package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
)

func TestMySQLProductionRehearsal(t *testing.T) {
	f := newProductionFixture(t, 5000)
	director := f.provision("studio", "director", "first-director")
	relief := f.provision("studio", "director", "relief-director")
	editor := f.provision("studio", "editor", "graphics-operator")
	outsider := f.provision("outside", "owner", "outside-producer")
	f.expect("GET", f.base, outsider, nil, nil, 404)

	f.slot("INTRO", "Opening", "clip", "stereo")
	for _, id := range []string{"CAM-A", "CAM-B", "CAM-C", "CAM-D"} {
		f.slot(id, id, "live", "stereo")
	}
	if slot := f.clock("CAM-A", -200); slot.Alignment != -200 || slot.ZeroMS == nil || *slot.ZeroMS != 200 {
		t.Fatalf("A1 expected -200 ms alignment: %+v", slot)
	}
	f.clock("CAM-B", 0)
	f.clock("CAM-D", 0)
	f.expect("PUT", f.base+"/slots/CAM-C/input/0", f.owner, bytes.NewReader([]byte("no clock")), map[string]string{"X-Frame-Count": "25"}, 422)
	f.expect("PUT", f.base+"/slots/CAM-D/clock", f.owner, jsonInput(t, catalog.SlotClock{Samples: []catalog.ClockObservation{
		{SourceMS: 1000, ProgramMS: 1000}, {SourceMS: 2000, ProgramMS: 2000}, {SourceMS: 3400, ProgramMS: 3000},
	}}), map[string]string{"Content-Type": "application/json"}, 422)
	t.Log("A1/A2: confirmed -200 ms, uncalibrated input and clock jump rejected")

	items := []catalog.RundownItem{
		{ID: "live", Title: "Live desk", Slot: "CAM-A", Kind: "live", In: 0, Out: 100},
		{ID: "MOVABLE", Title: "Segments", Slot: "INTRO", Kind: "clip", In: 100, Out: 200},
		{ID: "AD", Title: "Fixed break", Slot: "INTRO", Kind: "advert", In: 1000, Out: 1200, Fixed: true},
	}
	change := func(change catalog.RundownChange, status int) catalog.Rundown {
		return fixtureJSON[catalog.Rundown](f, "PUT", f.base+"/rundown", director, change, "", status)
	}
	plan := change(catalog.RundownChange{Expected: f.production.RundownRev, Items: items}, 200)
	plan = change(catalog.RundownChange{Expected: plan.Revision, Extend: "live", By: 750}, 200)
	if plan.Items[0].Out != 850 || plan.Items[1].In != 850 || plan.Items[1].Out != 950 || plan.Items[2].In != 1000 {
		t.Fatalf("A3 incorrect movable or fixed frames: %+v", plan)
	}
	conflict := change(catalog.RundownChange{Expected: plan.Revision, Extend: "live", By: 100}, 409)
	if len(conflict.Conflict) != 1 || conflict.Conflict[0].Against != "AD" || conflict.Conflict[0].By != 50 || f.read().RundownRev != plan.Revision {
		t.Fatalf("A4 incorrect conflict or the revision changed: %+v", conflict)
	}
	plan = change(catalog.RundownChange{Expected: plan.Revision, Items: []catalog.RundownItem{
		{ID: "OPEN", Title: "Opening titles", Slot: "INTRO", Kind: "clip", In: 0, Out: 750},
		{ID: "LIVE", Title: "Studio show", Slot: "CAM-A", Kind: "live", In: 750, Out: 1_300},
	}}, 200)
	t.Log("A3/A4: extended live by 750 frames, preserved the advert at 1000, identified exactly 50 overlapping frames")

	intros := make(map[int][]byte)
	for second := 0; second < 30; second += 5 {
		name := fmt.Sprintf("opening-%02d", second)
		intros[second] = f.source(name, "orange", 125, second, 310)
		f.upload("INTRO", second*25, 125, intros[second])
	}
	aFirst := f.source("camera-a-first", "red", 75, 30, 440)
	aReturn := f.source("camera-a-return", "red", 125, 43, 440)
	aTail := f.source("camera-a-tail", "red", 100, 48, 440)
	bFirst := f.source("camera-b-monitor", "blue", 125, 30, 880)
	bReturn := f.source("camera-b-return", "blue", 125, 43, 880)
	bTail := f.source("camera-b-tail", "blue", 100, 48, 880)
	dMonitor := f.source("camera-d-monitor", "purple", 125, 30, 660)
	cMonitor := f.source("camera-c-monitor", "green", 125, 30, 550)
	firstReceipt := f.upload("CAM-A", 755, 75, aFirst)
	if firstReceipt.StartFrame != 750 || !f.upload("CAM-A", 755, 75, aFirst).Replay {
		t.Fatalf("A1 expected aligned first frame and replay: %+v", firstReceipt)
	}
	f.upload("CAM-B", 750, 125, bFirst)
	f.upload("CAM-D", 750, 125, dMonitor)
	initial := f.control(director, 0, 3)
	on := int64(125)
	overlay := catalog.OverlayRequest{Template: "lower_third", Content: "Studio guest", Size: 32, Start: &on, Length: 125}
	fixtureJSON[map[string]any](f, "POST", f.base+"/overlays", editor,
		catalog.OverlayRequest{Template: "lower_third", Content: "🧪", Size: 32, Start: &on, Length: 125}, "", 422)
	fixtureJSON[map[string]any](f, "POST", f.base+"/overlays", editor,
		catalog.OverlayRequest{Template: "lower_third", Content: strings.Repeat("W", 50), Size: 62, Start: &on, Length: 125}, "", 422)
	graphic := f.expect("POST", f.base+"/overlays/preview.png", editor, jsonInput(t, overlay),
		map[string]string{"Content-Type": "application/json"}, 200)
	if !bytes.HasPrefix(graphic, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatal("A8 overlay preview returned no PNG")
	}
	approved := fixtureJSON[catalog.Overlay](f, "POST", f.base+"/overlays", editor, overlay, "", 201)
	if approved.On != 125 || approved.Off != 250 {
		t.Fatalf("A7 bad overlay duration %+v", approved)
	}
	initial = f.control(director, initial.ControlEpoch, 3)
	start := fixtureJSON[catalog.Production](f, "POST", f.base+"/start", director, map[string]uint64{"expected_control_epoch": initial.ControlEpoch}, "", 202)
	if start.Started == nil {
		t.Fatal("expected server start timestamp")
	}
	f.waitProduction(func(s catalog.Production) bool { return s.Frame >= 125 }, 12*time.Second)
	active := f.control(relief, initial.ControlEpoch, 100)
	if active.ControlEpoch != initial.ControlEpoch+1 || active.Controller != "relief-director" {
		t.Fatalf("A10 new control lease %+v", active)
	}
	oldTake := catalog.TakeRequest{Slot: "CAM-B", Epoch: initial.ControlEpoch, Transition: "cut", Command: "obsolete-command"}
	stale := fixtureJSON[struct{ Current catalog.ControlConflict }](f, "POST", f.base+"/takes", director, oldTake, "obsolete-request", 409)
	if stale.Current.Epoch != active.ControlEpoch {
		t.Fatalf("A6 wrong current epoch: %+v", stale)
	}
	setPreview := func(slot string) catalog.Production {
		return fixtureJSON[catalog.Production](f, "POST", f.base+"/preview", relief, productionCommand{Slot: slot, Epoch: active.ControlEpoch}, "", 200)
	}
	setPreview("CAM-C")
	badTake := catalog.TakeRequest{Slot: "CAM-C", Epoch: active.ControlEpoch, Transition: "cut", Command: "uncalibrated-command"}
	fixtureJSON[map[string]any](f, "POST", f.base+"/takes", relief, badTake, "uncalibrated-request", 422)
	f.clock("CAM-C", 0)
	cliUpload := uploadProductionCamera(f, cMonitor)
	preview := setPreview("CAM-B")
	if preview.Preview != "CAM-B" || preview.Program != "INTRO" {
		t.Fatalf("A5 preview changed the program: %+v", preview)
	}
	monitor := f.expect("GET", f.base+"/slots/CAM-B/monitor.mp4", relief, nil, nil, 200)
	if len(monitor) < 1024 || !bytes.Contains(monitor[:128], []byte("ftyp")) {
		t.Fatal("A5 preview returned no playable MP4")
	}
	f.waitProduction(func(s catalog.Production) bool { return s.Frame >= 825 }, 36*time.Second)
	lostAt := time.Now()
	f.expect("POST", f.base+"/slots/CAM-A/lost", f.owner, nil, nil, 204)
	waitUntil(t, lostAt.Add(10*time.Second))
	returnReceipt := f.upload("CAM-A", 1080, 125, aReturn)
	outage := time.Since(lostAt)
	if returnReceipt.StartFrame != 1075 {
		t.Fatalf("A9 recovery did not align to frame 1075: %+v", returnReceipt)
	}
	f.upload("CAM-A", 1205, 100, aTail)
	f.upload("CAM-B", 1075, 125, bReturn)
	f.upload("CAM-B", 1200, 100, bTail)
	f.waitProduction(func(s catalog.Production) bool { return s.Frame >= 1080 && s.Program == "CAM-A" }, 11*time.Second)
	takeIn := catalog.TakeRequest{Slot: "CAM-B", Epoch: active.ControlEpoch, Transition: "dissolve", Frames: 10, Command: "verified-command"}
	take := fixtureJSON[catalog.ProductionTake](f, "POST", f.base+"/takes", relief, takeIn, "verified-request", 202)
	if take.ID == "" || take.PTS != nil {
		t.Fatalf("A5 expected a queued receipt without an actual timestamp: %+v", take)
	}
	_, _, _ = f.request("POST", f.base+"/takes", relief, jsonInput(t, takeIn),
		map[string]string{"Idempotency-Key": "verified-request", "Content-Type": "application/json"})
	takeIn.Transition, takeIn.Frames = "cut", 0
	fixtureJSON[map[string]any](f, "POST", f.base+"/takes", relief, takeIn, "verified-request", 409)
	f.waitProduction(func(s catalog.Production) bool { return s.State == "sealed" }, 21*time.Second)
	published := fixtureJSON[catalog.ProductionTake](f, "GET", f.base+"/takes/"+take.ID, relief, nil, "", 200)
	if published.State != "published" || published.PTS == nil || published.Actual == nil || *published.PTS != *published.Actual*3600 {
		t.Fatalf("A5 expected a real applied timestamp: %+v", published)
	}
	stillActive := f.read()
	if stillActive.Frame != 1300 || stillActive.Controller != active.Controller || stillActive.ControlEpoch != active.ControlEpoch {
		t.Fatalf("A10 older director changed production: %+v", stillActive)
	}

	entries := f.asRun()
	var eligible, covered, takeCount, graphicFrames int
	var gapStart, gapEnd int64
	var raw []map[string]any
	for _, entry := range entries {
		if entry.Start != int64(covered) || entry.End <= entry.Start || entry.Sequence == nil || len(entry.Media) != 2 {
			t.Fatalf("A11 missing continuous published evidence: %+v", entry)
		}
		covered = int(entry.End)
		if entry.Take != nil && *entry.Take == take.ID {
			takeCount++
		}
		if entry.Plan.Reason == "source_unavailable" && entry.Plan.Input.Slot == "CONT" {
			if gapEnd == 0 {
				gapStart = entry.Start
			}
			if gapEnd != 0 && gapEnd != entry.Start {
				t.Fatalf("A9 fallback interval split into separate events: %+v", entry)
			}
			gapEnd = entry.End
		}
		for _, graphic := range entry.Plan.Overlays {
			if graphic.ID == approved.ID {
				graphicFrames += int(min(entry.End, graphic.Off) - max(entry.Start, graphic.On))
			}
		}
		if entry.Start == 0 || entry.Start == 750 || entry.Start == gapStart && gapEnd != 0 || entry.Start == 1075 || entry.Take != nil {
			data := f.segment(entry)
			digest := sha256.Sum256(data)
			if entry.Media[1].SHA256 != hex.EncodeToString(digest[:]) || entry.Media[1].VideoFirst != 90_000+entry.Start*3600 {
				t.Fatalf("A11 media bytes disagree with as-run tracks: %+v", entry.Media)
			}
			path := filepath.Join(f.work, fmt.Sprintf("output-%d.ts", entry.Start))
			must(t, os.WriteFile(path, data, 0600))
			sample := assessPulse(f, entry, path)
			if entry.Plan.Input.BuiltIn {
				if sample.AudioFound || sample.VideoFound {
					t.Fatalf("A13 slate shows camera markers: %+v", sample)
				}
			} else if entry.Take == nil && (!sample.AudioFound || !sample.VideoFound || sample.DeltaMS > 120) {
				t.Fatalf("A13 content markers disagree: %+v", sample)
			}
			eligible++
			raw = append(raw, map[string]any{"entry": entry, "pulse": sample})
		}
	}
	if covered != 1300 || gapStart != 825 || gapEnd != 1075 || takeCount != 1 || graphicFrames != 125 || eligible < 5 {
		t.Fatalf("A5/A7/A9/A13 covered=%d fallback=%d–%d take=%d graphic=%d decoded=%d", covered, gapStart, gapEnd, takeCount, graphicFrames, eligible)
	}
	rangeURL := f.base + "/archive?from_frame=825&to_frame=1075&track=720p"
	result := fixtureJSON[productionArchive](f, "GET", rangeURL, editor, nil, "", 200)
	if result.State != "complete" || result.Digest == "" || result.ActualFrom == nil || result.ActualTo == nil ||
		*result.ActualFrom != 825 || *result.ActualTo != 1075 {
		t.Fatalf("A11 archive range %+v", result)
	}
	list := f.expect("GET", rangeURL+"&format=playlist", editor, nil, nil, 200)
	if !bytes.Contains(list, []byte("#EXT-X-ENDLIST")) || !bytes.Contains(list, []byte("/media/vod/"+start.Output+"/parts/720p/")) {
		t.Fatalf("archive manifest invalid: %s", list)
	}
	browser := runProductionBrowser(f, 825, 1075, result.Digest)
	archiveSegment := result.Segments[0]
	matches, err := filepath.Glob(filepath.Join(f.work, "blobs", "media", "*", "*"))
	must(t, err)
	var removed string
	for _, match := range matches {
		data, err := os.ReadFile(match)
		must(t, err)
		digest := sha256.Sum256(data)
		if hex.EncodeToString(digest[:]) == archiveSegment.SHA256 {
			must(t, os.Remove(match))
			removed = match
			break
		}
	}
	if removed == "" {
		t.Fatal("A12 selected programme file did not exist")
	}
	missing := fixtureJSON[productionArchive](f, "GET", rangeURL, editor, nil, "", 200)
	if missing.State != "incomplete" || missing.Digest != "" || len(missing.Missing) != 1 ||
		missing.Missing[0].From != archiveSegment.From || missing.Missing[0].To != archiveSegment.To ||
		missing.Missing[0].Source != "unreadable_fragment" {
		t.Fatalf("A12 wrong missing interval: %+v", missing)
	}
	f.expect("GET", rangeURL+"&format=playlist", editor, nil, nil, 409)
	t.Logf("A5–A13: %d frames, %d as-run entries, one TAKE at %d, 125 graphic frames, real outage %.3fs, exactly %d contiguous slate frames", covered, len(entries), *published.Actual, outage.Seconds(), gapEnd-gapStart)
	if file := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_REPORT"); file != "" {
		opening := make([]map[string]any, 0, len(intros))
		for second := 0; second < 30; second += 5 {
			data := intros[second]
			opening = append(opening, map[string]any{"start_frame": second * 25, "frames": 125,
				"bytes": len(data), "sha256": fmt.Sprintf("%x", sha256.Sum256(data))})
		}
		report := map[string]any{"production": stillActive, "rundown": plan, "clock_alignment_ms": -200, "introduction_frames": 750,
			"take": published, "overlay": approved, "wall_outage_ms": outage.Milliseconds(), "fallback_from_frame": gapStart, "fallback_to_frame": gapEnd,
			"archive": result, "missing_archive": missing, "decoded_output": raw, "as_run": entries, "browser": browser,
			"opening_inputs": opening, "camera_a_first": firstReceipt, "camera_a_recovered": returnReceipt, "camera_c_cli": cliUpload}
		writeProductionEvidence(t, file, report)
	}
}

func uploadProductionCamera(f *productionFixture, direct []byte) bool {
	f.t.Helper()
	cli := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_CLI")
	if cli == "" {
		f.upload("CAM-C", 750, 125, direct)
		return false
	}
	command := exec.CommandContext(f.t.Context(), cli, "push", "-origin", f.server.URL,
		"-production", f.production.ID, "-slot", "CAM-C", "-source", filepath.Join(f.work, "camera-c-monitor.ts"),
		"-segment-ms", "5000", "-clock-start-ms", "30000", "-ffmpeg", f.ff.Path, "-ffprobe", f.ff.ProbePath, "-fast")
	command.Env = append(os.Environ(), "MEDIA_STREAM_PRODUCTION_TOKEN="+f.owner)
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("production camera CLI: %v\n%s", err, output)
	}
	slots := fixtureJSON[[]catalog.ProductionSlot](f, "GET", f.base+"/slots", f.owner, nil, "", 200)
	for _, slot := range slots {
		if slot.ID == "CAM-C" && slot.LastFrame != nil && *slot.LastFrame == 750 {
			f.t.Logf("Camera CLI published CAM-C at frame %d", *slot.LastFrame)
			return true
		}
	}
	f.t.Fatalf("camera CLI did not publish the expected source frame: %+v", slots)
	return false
}

func writeProductionEvidence(t *testing.T, file string, data any) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(file), 0755))
	out, err := json.MarshalIndent(data, "", "  ")
	must(t, err)
	must(t, os.WriteFile(file, append(out, '\n'), 0644))
}

func runProductionBrowser(f *productionFixture, from, to int64, digest string) map[string]any {
	f.t.Helper()
	script := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_BROWSER")
	if script == "" {
		return nil
	}
	command := exec.CommandContext(f.t.Context(), "node", script)
	command.Env = append(os.Environ(), "STUDIO_TEST_URL="+f.server.URL, "STUDIO_TEST_TOKEN="+f.owner, "STUDIO_TEST_PRODUCTION="+f.production.ID,
		fmt.Sprintf("STUDIO_TEST_RANGE=%d,%d", from, to), "STUDIO_TEST_DIGEST="+digest)
	output, err := command.CombinedOutput()
	if err != nil {
		f.t.Fatalf("production browser: %v\n%s", err, output)
	}
	f.t.Log(strings.TrimSpace(string(output)))
	return fixtureDecode[map[string]any](f.t, output)
}
