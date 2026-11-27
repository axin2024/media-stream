package web

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
)

type pulseCheck struct {
	AudioFound bool    `json:"audio_found"`
	VideoFound bool    `json:"video_found"`
	AudioPTS   float64 `json:"audio_onset_pts"`
	VideoPTS   int64   `json:"video_onset_pts"`
	DeltaMS    float64 `json:"absolute_delta_ms"`
}

func assessPulse(f *productionFixture, entry catalog.AsRunEntry, file string) pulseCheck {
	f.t.Helper()
	var result pulseCheck
	tracks := entry.Media[len(entry.Media)-1]
	const cropSamples = 64
	luma := f.decode(file, "-map", "0:v:0", "-vf", "crop=8:8:16:16,format=gray", "-an",
		"-c:v", "rawvideo", "-threads:v", "1", "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
	if len(luma) != tracks.VideoCount*cropSamples {
		f.t.Fatalf("expected %d decoded video markers, got %d bytes", tracks.VideoCount, len(luma))
	}
	for frame := 0; frame < tracks.VideoCount; frame++ {
		var sum int
		for _, b := range luma[frame*cropSamples : (frame+1)*cropSamples] {
			sum += int(b)
		}
		if sum > 220*cropSamples {
			result.VideoFound = true
			result.VideoPTS = tracks.VideoFirst + int64(frame)*3600
			break
		}
	}
	pcm := f.decode(file, "-map", "0:a:0", "-vn", "-ar", "48000", "-ac", "1", "-f", "s16le", "pipe:1")
	var loud int
	for sample := 0; sample < len(pcm)/2; sample++ {
		value := int(int16(binary.LittleEndian.Uint16(pcm[sample*2:])))
		if value >= 2000 || value <= -2000 {
			loud++
		} else {
			loud = 0
		}
		if loud == 8 {
			result.AudioFound = true
			result.AudioPTS = float64(tracks.AudioFirst) + float64(sample-loud+1)*90_000/48_000
			break
		}
	}
	if result.AudioFound && result.VideoFound {
		result.DeltaMS = math.Abs(float64(result.VideoPTS)-result.AudioPTS) / 90
	}
	return result
}

type uploadSample struct {
	Camera string    `json:"camera"`
	Frame  int       `json:"source_frame"`
	Start  time.Time `json:"request_at"`
	MS     float64   `json:"elapsed_ms"`
	HTTP   int       `json:"status"`
	Error  string    `json:"error,omitempty"`
}

func TestMySQLProductionCapacity(t *testing.T) {
	report := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_LOAD_REPORT")
	if report == "" {
		t.Skip("set MEDIA_STREAM_TEST_PRODUCTION_LOAD_REPORT to run the four-camera capacity test")
	}
	seconds := 3600
	if override := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_LOAD_SECONDS"); override != "" {
		var err error
		seconds, err = strconv.Atoi(override)
		must(t, err)
		if seconds < 20 || seconds > 3600 || seconds%5 != 0 {
			t.Fatal("test duration must be between 20 and 3,600 seconds and divisible by 5")
		}
	}
	f := newProductionFixture(t, 5000)
	cameraIDs := []string{"CAM-A", "CAM-B", "CAM-C", "CAM-D"}
	cameraColors := []string{"red", "blue", "purple", "green"}
	var input [][]byte
	for i, id := range cameraIDs {
		f.slot(id, id, "live", "stereo")
		f.clock(id, 0)
		input = append(input, f.source(id, cameraColors[i], 125, 0, 340+i*150))
	}
	fixtureJSON[catalog.Rundown](f, "PUT", f.base+"/rundown", f.owner,
		catalog.RundownChange{Expected: f.production.RundownRev, Items: []catalog.RundownItem{{ID: "LIVE", Title: "Capacity programme", Slot: cameraIDs[0],
			Kind: "live", In: 0, Out: int64(seconds * 25)}}}, "", 200)
	lease := f.control(f.owner, 0, 90)
	warmup := time.Now()
	warmupCode, _, warmupErr := f.input(cameraIDs[0], 0, 125, input[0])
	must(t, warmupErr)
	if warmupCode != 200 {
		t.Fatalf("warm-up upload returned %d", warmupCode)
	}
	warmupMS := time.Since(warmup).Milliseconds()
	state := fixtureJSON[catalog.Production](f, "POST", f.base+"/start", f.owner, map[string]uint64{"expected_control_epoch": lease.ControlEpoch}, "", 202)
	if state.Started == nil {
		t.Fatal("expected start timestamp")
	}
	origin := *state.Started
	t.Logf("capacity capture started %s, target %d wall seconds with %d cameras", origin.Format(time.RFC3339Nano), seconds, len(cameraIDs))

	var uploads []uploadSample
	var takes []catalog.ProductionTake
	var inputFailures, completed, nextCamera int
	for chunk := range seconds / 5 {
		cadence := origin.Add(time.Duration(chunk+1) * 5 * time.Second)
		waitUntil(t, cadence)
		startGate := make(chan struct{})
		arrived := make(chan uploadSample, len(cameraIDs))
		for index, id := range cameraIDs {
			go func() {
				<-startGate
				begin := time.Now()
				code, out, err := f.input(id, chunk*125, 125, input[index])
				sample := uploadSample{Camera: id, Frame: chunk * 125, Start: begin, MS: float64(time.Since(begin).Microseconds()) / 1000, HTTP: code}
				if err != nil {
					sample.Error = err.Error()
				} else if code != 200 {
					sample.Error = string(out)
				}
				arrived <- sample
			}()
		}
		close(startGate)
		for range cameraIDs {
			sample := <-arrived
			uploads = append(uploads, sample)
			if sample.HTTP == 200 && sample.Error == "" {
				completed++
			} else {
				inputFailures++
				t.Errorf("camera upload failed: %+v", sample)
			}
		}
		if chunk%12 == 11 && chunk < seconds/5-1 {
			lease = f.control(f.owner, lease.ControlEpoch, 90)
		}
		if chunk%3 == 1 && chunk < seconds/5-1 {
			nextCamera = (nextCamera + 1) % len(cameraIDs)
			selected := cameraIDs[nextCamera]
			fixtureJSON[catalog.Production](f, "POST", f.base+"/preview", f.owner,
				productionCommand{Slot: selected, Epoch: lease.ControlEpoch}, "", 200)
			key := fmt.Sprintf("take-%d", chunk)
			take := fixtureJSON[catalog.ProductionTake](f, "POST", f.base+"/takes", f.owner, catalog.TakeRequest{
				Slot: selected, Epoch: lease.ControlEpoch, Transition: "cut", Command: key}, key, 202)
			takes = append(takes, take)
		}
		if (chunk+1)%12 == 0 {
			t.Logf("%d wall seconds, accepted %d / %d inputs, queued %d TAKEs, playhead %d", (chunk+1)*5, completed, len(uploads), len(takes), f.read().Frame)
		}
	}
	wall := time.Since(origin)
	sealed := f.waitProduction(func(s catalog.Production) bool { return s.State == "sealed" }, 30*time.Second)
	entries := f.asRun()
	samples := make(map[string]pulseCheck)
	var latency []float64
	var cursor, fallbackFrames int64
	var lastAudioPTS int64 = -1
	for _, entry := range entries {
		if entry.Start != cursor || entry.End <= entry.Start || entry.Sequence == nil || len(entry.Media) != 2 {
			t.Fatalf("playout discontinuity at frame %d: %+v", cursor, entry)
		}
		track := entry.Media[1]
		if track.VideoFirst != 90_000+entry.Start*3600 || track.VideoLast != 90_000+(entry.End-1)*3600 ||
			track.VideoCount != int(entry.End-entry.Start) || track.AudioFirst <= lastAudioPTS || track.AudioRate != 48000 {
			t.Fatalf("non-continuous 25 fps / 48 kHz output: %+v", track)
		}
		cursor = entry.End
		lastAudioPTS = track.AudioLast
		if entry.Plan.Input.BuiltIn || entry.Plan.Reason == "source_unavailable" {
			fallbackFrames += entry.End - entry.Start
		}
		if entry.Take == nil {
			continue
		}
		path := filepath.Join(f.work, "switch.ts")
		must(t, os.WriteFile(path, f.segment(entry), 0600))
		sample := assessPulse(f, entry, path)
		samples[*entry.Take] = sample
		if !sample.AudioFound || !sample.VideoFound {
			t.Errorf("switch %s at frame %d missing at least one content marker: %+v", *entry.Take, entry.Start, sample)
			continue
		}
		latency = append(latency, sample.DeltaMS)
	}
	for i := range takes {
		takes[i] = fixtureJSON[catalog.ProductionTake](f, "GET", f.base+"/takes/"+takes[i].ID, f.owner, nil, "", 200)
		if takes[i].State != "published" || takes[i].PTS == nil || takes[i].Actual == nil || *takes[i].PTS != *takes[i].Actual*3600 {
			t.Errorf("TAKE did not publish once at a verified frame: %+v", takes[i])
		}
	}
	slices.Sort(latency)
	var p95 *float64
	if len(latency) > 0 {
		value := latency[int(math.Ceil(0.95*float64(len(latency))))-1]
		p95 = &value
	}
	var timeouts int
	for _, sample := range uploads {
		if sample.HTTP == 0 && strings.Contains(strings.ToLower(sample.Error), "timeout") {
			timeouts++
		}
	}
	ffVersion, err := exec.CommandContext(t.Context(), f.ff.Path, "-version").Output()
	must(t, err)
	passed := p95 != nil && *p95 <= 120 && len(latency) == len(takes) && cursor == int64(seconds*25) &&
		wall >= time.Duration(seconds)*time.Second && sealed.Frame == cursor && fallbackFrames == 0 && inputFailures == 0 && !t.Failed()
	writeProductionEvidence(t, report, map[string]any{
		"passed": passed, "full_length": seconds == 3600, "go": runtime.Version(), "goos": runtime.GOOS, "goarch": runtime.GOARCH,
		"cpu_logical": runtime.NumCPU(), "ffmpeg": strings.TrimSpace(strings.SplitN(string(ffVersion), "\n", 2)[0]),
		"producer":  "four distinct generated 1280×720 H.264 streams, 25 fps/48 kHz; 5 s live-time fragment cadence over loopback",
		"warmup_ms": warmupMS, "sample_seconds": seconds, "elapsed_wall_ms": wall.Milliseconds(), "started_at": origin,
		"recorded_frames": cursor, "fallback_frames": fallbackFrames, "accepted_uploads": completed, "rejected_uploads": inputFailures - timeouts,
		"timeout_uploads": timeouts, "take_count": len(takes), "p95_absolute_av_ms": p95, "switches": takes, "measurements": samples, "uploads": uploads,
	})
	if !passed {
		t.Fatalf("capacity acceptance failed: wall=%s frames=%d fallback=%d uploads=%d/%d samples=%d/%d p95=%v (see %s)",
			wall, cursor, fallbackFrames, completed, len(uploads), len(latency), len(takes), latency, report)
	}
	t.Logf("elapsed %s, %d frames, %d accepted uploads, %d verified switches, A/V P95 %.3f ms", wall, cursor, completed, len(takes), *p95)
}
