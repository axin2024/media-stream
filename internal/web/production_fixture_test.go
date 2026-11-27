package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
	"github.com/axin2024/media-stream/internal/pipeline"
	"golang.org/x/image/font/gofont/goregular"
)

type productionFixture struct {
	t          *testing.T
	work       string
	ff         media.FFmpeg
	files      *media.Files
	db         *catalog.Store
	server     *httptest.Server
	client     *http.Client
	production catalog.Production
	owner      string
	base       string
}

func newProductionFixture(t *testing.T, segmentMS int) *productionFixture {
	t.Helper()
	dsn, ffPath := os.Getenv("MEDIA_STREAM_TEST_PRODUCTION_DSN"), os.Getenv("MEDIA_STREAM_TEST_FFMPEG")
	if dsn == "" || ffPath == "" {
		t.Skip("set MEDIA_STREAM_TEST_PRODUCTION_DSN and MEDIA_STREAM_TEST_FFMPEG")
	}
	work := t.TempDir()
	fontPath := filepath.Join(work, "regular.ttf")
	must(t, os.WriteFile(fontPath, goregular.TTF, 0600))
	ffPath, err := exec.LookPath(ffPath)
	must(t, err)
	probe := "ffprobe"
	if runtime.GOOS == "windows" {
		probe += ".exe"
	}
	ff := media.FFmpeg{Path: ffPath, ProbePath: filepath.Join(filepath.Dir(ffPath), probe), Threads: 2}
	must(t, catalog.Migrate(dsn, 0))
	db, err := catalog.Open(t.Context(), dsn, []byte("production-fixture-only-signing-key-32-bytes"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	files, err := media.OpenFiles(filepath.Join(work, "blobs"))
	must(t, err)
	t.Cleanup(func() { _ = files.Close() })
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	graphics := media.Graphics{FontPath: fontPath}
	server := httptest.NewServer((&Server{DB: db, Files: files, FFmpeg: ff, Graphics: graphics, Log: logger,
		IngestSlots: make(chan struct{}, 8)}).Handler())
	t.Cleanup(server.Close)
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	renderer := pipeline.ProductionRenderer{Catalog: db, Files: files, FFmpeg: ff, Graphics: graphics, Log: logger}
	go func() {
		defer close(stopped)
		_ = renderer.Run(ctx)
	}()
	t.Cleanup(func() { cancel(); <-stopped })
	client := server.Client()
	client.Timeout = 9 * time.Second
	f := &productionFixture{t: t, work: work, ff: ff, files: files, db: db, server: server, client: client}
	f.owner = f.provision("studio", "owner", "studio-producer")
	now := time.Now()
	fixtureJSON[catalog.Channel](f, "POST", "/api/channels", f.owner,
		catalog.Channel{Key: "main", Title: "Programme test", From: now.Add(-time.Minute), Until: now.Add(2 * time.Hour)}, "", 201)
	opened := fixtureJSON[catalog.Opened](f, "POST", "/api/channels/main/sessions", f.owner,
		catalog.Opening{Preset: "standard", SegmentMS: segmentMS, ExpiresIn: 7_200}, "fixture-opening", 201)
	f.production = fixtureJSON[catalog.Production](f, "POST", "/api/productions", f.owner, catalog.ProductionOpening{
		Session: opened.Session.ID, Title: "Studio relay", Timezone: "Asia/Shanghai", Fallback: "CONT"}, "fixture-production", 201)
	f.base = "/api/productions/" + f.production.ID
	f.slot("CONT", "Continuity", "slate", "mute")
	return f
}

func (f *productionFixture) provision(tenant, role, label string) string {
	f.t.Helper()
	credential, err := f.db.Provision(f.t.Context(), tenant, label, role, "", time.Now().Add(2*time.Hour))
	must(f.t, err)
	return credential.Secret
}

func (f *productionFixture) request(method, path, token string, body io.Reader, headers map[string]string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(f.t.Context(), method, f.server.URL+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	return response.StatusCode, data, err
}

func (f *productionFixture) expect(method, path, token string, body io.Reader, headers map[string]string, want int) []byte {
	f.t.Helper()
	code, data, err := f.request(method, path, token, body, headers)
	must(f.t, err)
	if code != want {
		f.t.Fatalf("%s %s returned %d, wanted %d: %s", method, path, code, want, data)
	}
	return data
}

func fixtureDecode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	must(t, json.Unmarshal(data, &value))
	return value
}

func fixtureJSON[T any](f *productionFixture, method, path, token string, in any, key string, want int) T {
	f.t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if key != "" {
		headers["Idempotency-Key"] = key
	}
	data := f.expect(method, path, token, jsonInput(f.t, in), headers, want)
	if want == 204 {
		var zero T
		return zero
	}
	return fixtureDecode[T](f.t, data)
}

func (f *productionFixture) slot(id, title, kind, audio string) {
	f.t.Helper()
	fixtureJSON[catalog.ProductionSlot](f, "PUT", f.base+"/slots/"+id, f.owner,
		catalog.SlotSettings{Title: title, Kind: kind, Audio: audio}, "", 200)
}

func (f *productionFixture) clock(id string, offset int64) catalog.ProductionSlot {
	f.t.Helper()
	var samples []catalog.ClockObservation
	for _, ts := range []int64{1000, 2000, 3000} {
		samples = append(samples, catalog.ClockObservation{SourceMS: ts - offset, ProgramMS: ts})
	}
	return fixtureJSON[catalog.ProductionSlot](f, "PUT", f.base+"/slots/"+id+"/clock", f.owner,
		catalog.SlotClock{Samples: samples}, "", 200)
}

func (f *productionFixture) source(name, color string, frames, firstSecond, frequency int) []byte {
	f.t.Helper()
	seconds := float64(frames) / 25
	timecode := fmt.Sprintf("%02d\\:%02d\\:%02d\\:00", firstSecond/3600, firstSecond/60%60, firstSecond%60)
	filter := "drawbox=x=8:y=8:w=32:h=32:color=white:t=fill:enable='lt(mod(n,25),3)'," +
		"drawtext=fontfile=regular.ttf:timecode='" + timecode + "':timecode_rate=25:fontsize=64:fontcolor=white:box=1:boxcolor=black:x=64:y=40"
	signal := fmt.Sprintf("aevalsrc=if(lt(mod(t\\,1)\\,0.12)\\,0.44*sin(2*PI*%d*t)\\,0):s=48000:d=%.3f", frequency, seconds)
	file := filepath.Join(f.work, name+".ts")
	command := exec.CommandContext(f.t.Context(), f.ff.Path, "-v", "error", "-nostdin", "-y", "-f", "lavfi", "-i",
		fmt.Sprintf("color=c=%s:s=1280x720:r=25:d=%.3f", color, seconds), "-f", "lavfi", "-i", signal,
		"-vf", filter, "-filter_threads", "2", "-c:v", "libx264", "-threads:v", "2", "-preset", "ultrafast", "-bf", "0",
		"-g", fmt.Sprint(frames), "-c:a", "aac", "-ar", "48000", "-muxdelay", "0", "-f", "mpegts", file)
	command.Dir = f.work
	if output, err := command.CombinedOutput(); err != nil {
		f.t.Fatalf("create source %s: %v %s", name, err, output)
	}
	must(f.t, f.ff.VerifyProductionInput(f.t.Context(), file, frames, "stereo"))
	data, err := os.ReadFile(file)
	must(f.t, err)
	return data
}

func (f *productionFixture) input(slot string, sourceFrame, frames int, data []byte) (int, []byte, error) {
	return f.request("PUT", fmt.Sprintf("%s/slots/%s/input/%d", f.base, slot, sourceFrame), f.owner,
		bytes.NewReader(data), map[string]string{"X-Frame-Count": fmt.Sprint(frames), "Content-Type": "video/mp2t"})
}

func (f *productionFixture) upload(slot string, sourceFrame, frames int, data []byte) catalog.ProductionInput {
	f.t.Helper()
	code, body, err := f.input(slot, sourceFrame, frames, data)
	must(f.t, err)
	if code != 200 {
		f.t.Fatalf("upload %s at %d returned %d: %s", slot, sourceFrame, code, body)
	}
	return fixtureDecode[catalog.ProductionInput](f.t, body)
}

func (f *productionFixture) read() catalog.Production {
	f.t.Helper()
	return fixtureJSON[catalog.Production](f, "GET", f.base, f.owner, nil, "", 200)
}

func (f *productionFixture) control(token string, epoch uint64, seconds int) catalog.Production {
	f.t.Helper()
	return fixtureJSON[catalog.Production](f, "POST", f.base+"/control", token,
		catalog.ControlRequest{Epoch: epoch, Seconds: seconds}, "", 200)
}

func (f *productionFixture) waitProduction(predicate func(catalog.Production) bool, timeout time.Duration) catalog.Production {
	f.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		state := f.read()
		if predicate(state) {
			return state
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("production did not reach the expected state: %+v", state)
		}
		waitUntil(f.t, time.Now().Add(100*time.Millisecond))
	}
}

type asRunPage struct {
	Entries []catalog.AsRunEntry `json:"items"`
	Next    *int64               `json:"next_frame"`
}

func (f *productionFixture) asRun() []catalog.AsRunEntry {
	f.t.Helper()
	var entries []catalog.AsRunEntry
	var cursor int64
	for {
		page := fixtureJSON[asRunPage](f, "GET", fmt.Sprintf("%s/as-run?from_frame=%d", f.base, cursor), f.owner, nil, "", 200)
		entries = append(entries, page.Entries...)
		if page.Next == nil {
			return entries
		}
		if *page.Next <= cursor {
			f.t.Fatalf("as-run cursor did not advance: %v", page)
		}
		cursor = *page.Next
	}
}

func waitUntil(t *testing.T, at time.Time) {
	t.Helper()
	timer := time.NewTimer(time.Until(at))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal(t.Context().Err())
	}
}

func (f *productionFixture) segment(entry catalog.AsRunEntry) []byte {
	f.t.Helper()
	if entry.Sequence == nil {
		f.t.Fatalf("as-run entry has no media: %+v", entry)
	}
	return f.expect("GET", fmt.Sprintf("/media/vod/%s/parts/720p/%d.ts", f.production.Output, *entry.Sequence), f.owner, nil, nil, 200)
}

func (f *productionFixture) decode(file string, args ...string) []byte {
	f.t.Helper()
	command := exec.CommandContext(f.t.Context(), f.ff.Path, append([]string{"-v", "error", "-nostdin", "-threads", "2", "-i", file}, args...)...)
	data, err := command.Output()
	if failure, ok := err.(*exec.ExitError); ok {
		f.t.Fatalf("decode %s: %v %s", filepath.Base(file), err, strings.TrimSpace(string(failure.Stderr)))
	}
	must(f.t, err)
	return data
}
