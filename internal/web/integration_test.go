package web

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
	"github.com/axin2024/media-stream/internal/media"
	"github.com/axin2024/media-stream/internal/pipeline"
)

type testClient struct {
	t       *testing.T
	handler http.Handler
}

func (c testClient) request(method, path, token string, input io.Reader, headers map[string]string, want int) *httptest.ResponseRecorder {
	c.t.Helper()
	request := httptest.NewRequest(method, path, input)
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	response := httptest.NewRecorder()
	c.handler.ServeHTTP(response, request)
	if response.Code != want {
		c.t.Fatalf("%s %s: got %d, want %d: %s", method, path, response.Code, want, response.Body.String())
	}
	return response
}

func jsonInput(t *testing.T, value any) io.Reader {
	t.Helper()
	buf, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.NewReader(buf)
}

func parsed[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestMySQLFFmpegHTTPPlaybackAndClip(t *testing.T) {
	dsn, executable := os.Getenv("MEDIA_STREAM_TEST_HTTP_DSN"), os.Getenv("MEDIA_STREAM_TEST_FFMPEG")
	if dsn == "" || executable == "" {
		t.Skip("set MEDIA_STREAM_TEST_HTTP_DSN and MEDIA_STREAM_TEST_FFMPEG")
	}
	ctx := t.Context()
	must(t, catalog.Migrate(dsn, 0))
	db, err := catalog.Open(ctx, dsn, []byte("test-use-only-signing-key-32-bytes"))
	must(t, err)
	defer db.Close()
	storage, err := media.OpenFiles(t.TempDir())
	must(t, err)
	defer storage.Close()
	ffmpeg := media.FFmpeg{Path: executable, Threads: 1}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	server := &Server{DB: db, Files: storage, FFmpeg: ffmpeg, Log: logger, IngestSlots: make(chan struct{}, 2)}
	client := testClient{t, server.Handler()}
	worker := &pipeline.Worker{Catalog: db, Files: storage, FFmpeg: ffmpeg, Log: logger, Node: "fixture-node"}
	must(t, db.RegisterNode(ctx, worker.Node, 1, true))
	provision := func(tenant, role string) string {
		value, err := db.Provision(ctx, tenant, role, role, "", time.Now().Add(time.Hour))
		must(t, err)
		return value.Secret
	}
	owner, viewer, editor, outsider := provision("studio", "owner"), provision("studio", "viewer"), provision("studio", "editor"), provision("other", "viewer")
	client.request("GET", "/", "", nil, nil, 200)
	client.request("GET", "/assets/hls.min.js", "", nil, nil, 200)
	cookie := client.request("POST", "/api/playback-cookie", viewer, nil, nil, 204).Header().Get("Set-Cookie")
	if !strings.Contains(cookie, "HttpOnly") || !strings.Contains(cookie, "SameSite=Strict") || strings.Contains(cookie, "Secure") {
		t.Fatalf("unexpected playback cookie policy: %s", cookie)
	}
	client.request("POST", "/api/channels", viewer, jsonInput(t, catalog.Channel{Key: "main"}), nil, 403)
	client.request("POST", "/api/channels", owner, jsonInput(t, catalog.Channel{
		Key: "main", Title: "Acceptance studio", From: time.Now().Add(-time.Minute), Until: time.Now().Add(time.Hour)}), nil, 201)
	open := catalog.Opening{Preset: "standard", SegmentMS: 1_000, ExpiresIn: 900, BackupZeroMS: 15_000}
	opened := parsed[catalog.Opened](t, client.request("POST", "/api/channels/main/sessions", owner, jsonInput(t, open),
		map[string]string{"Idempotency-Key": "opening-1"}, 201))
	id := opened.Session.ID
	primary, backup := opened.Inputs["primary"].Secret, opened.Inputs["backup"].Secret
	base := "/api/sessions/" + id
	origin := "/media/live/" + id
	client.request("GET", origin+"/index.m3u8", outsider, nil, nil, 404)
	client.request("GET", origin+"/index.m3u8", primary, nil, nil, 404)
	client.request("PUT", base+"/input", viewer, bytes.NewReader([]byte("malformed")), nil, 403)
	client.request("GET", origin+"/index.m3u8", viewer, nil, nil, 503)
	dir := t.TempDir()
	assertSource := filepath.Join(dir, "source.mp4")
	must(t, ffmpeg.Sample(ctx, assertSource, 4, 320, 180))
	must(t, ffmpeg.Partition(ctx, assertSource, dir, 1_000, true))
	mediaAt := func(n int) []byte {
		content, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("part-%08d.ts", n)))
		must(t, err)
		return content
	}
	push := func(secret string, index int, at int) {
		client.request("PUT", base+"/input", secret, bytes.NewReader(mediaAt(index)),
			map[string]string{"X-Source-Time-Ms": fmt.Sprint(at), "X-Source-Duration-Ms": "1000"}, 200)
	}
	client.request("PUT", base+"/input", primary, strings.NewReader("invalid mpeg ts"),
		map[string]string{"X-Source-Time-Ms": "0", "X-Source-Duration-Ms": "1000"}, 400)
	push(primary, 0, 0)
	push(backup, 0, 15_000)
	must(t, db.Schedule(ctx, id))
	must(t, worker.Step(ctx))
	master := client.request("GET", origin+"/index.m3u8", viewer, nil, nil, 200).Body.String()
	if !strings.Contains(master, "360p.m3u8") || !strings.Contains(master, "720p.m3u8") {
		t.Fatalf("incomplete master ladder: %s", master)
	}
	part := client.request("GET", origin+"/parts/720p/0.ts", viewer, nil, map[string]string{"Range": "bytes=0-187"}, 206)
	if part.Body.Len() != 188 || part.Body.Bytes()[0] != 0x47 {
		t.Fatal("wrong MPEG-TS range")
	}
	push(backup, 1, 16_000)
	client.request("POST", base+"/input-lost", primary, nil, nil, 204)
	must(t, db.Schedule(ctx, id))
	must(t, worker.Step(ctx))
	continued := client.request("GET", origin+"/360p.m3u8", viewer, nil, nil, 200).Body.String()
	if !strings.Contains(continued, "#EXT-X-DISCONTINUITY\n") || !strings.Contains(continued, "parts/360p/1.ts") {
		t.Fatalf("manifest missing source boundary: %s", continued)
	}
	push(backup, 2, 17_000)
	must(t, db.Schedule(ctx, id))
	worker.ExcludedTracks = map[string]bool{"720p": true}
	must(t, worker.Step(ctx))
	master = client.request("GET", origin+"/index.m3u8", viewer, nil, nil, 200).Body.String()
	if strings.Contains(master, "720p.m3u8") || !strings.Contains(master, "360p.m3u8") {
		t.Fatalf("degraded ladder wrong: %s", master)
	}
	high, err := db.ReadSegment(ctx, id, "720p", 1)
	must(t, err)
	must(t, storage.Remove(high.Blob.Key))
	client.request("GET", origin+"/parts/720p/1.ts", viewer, nil, nil, 404)
	client.request("GET", origin+"/index.m3u8", viewer, nil, nil, 200)
	client.request("POST", base+"/end", owner, nil, nil, 202)
	must(t, db.Schedule(ctx, id))
	client.request("POST", base+"/end", owner, nil, nil, 202)
	rec := parsed[catalog.Recording](t, client.request("GET", "/api/recordings/"+id, viewer, nil, nil, 200))
	if rec.DurationMS != 3_000 || rec.State != "published" {
		t.Fatalf("bad recording: %+v", rec)
	}
	vod := "/media/vod/" + id
	manifest := client.request("GET", vod+"/360p.m3u8", viewer, nil, nil, 200).Body.String()
	if !strings.Contains(manifest, "#EXT-X-ENDLIST") || strings.Count(manifest, "#EXTINF:") != 3 {
		t.Fatalf("bad VOD index: %s", manifest)
	}
	caption := parsed[catalog.Caption](t, client.request("POST", "/api/recordings/"+id+"/captions/en", editor,
		strings.NewReader("WEBVTT\n\n00:00:00.700 --> 00:00:01.800\nSwitch\n\n00:00:02.000 --> 00:00:03.000\nEnd\n"),
		map[string]string{"Idempotency-Key": "sub-1"}, 201))
	master = client.request("GET", vod+"/index.m3u8", viewer, nil, nil, 200).Body.String()
	if !strings.Contains(master, "captions/"+caption.ID+".m3u8") {
		t.Fatalf("missing subtitle group: %s", master)
	}
	client.request("GET", vod+"/captions/"+caption.ID+".vtt", viewer, nil, nil, 200)
	request := catalog.EditRequest{Revision: rec.Revision, FromMS: 800, ToMS: 2_400, Caption: caption.ID}
	headers := map[string]string{"Idempotency-Key": "edit-1"}
	edit := parsed[catalog.Edit](t, client.request("POST", "/api/recordings/"+id+"/edits", editor, jsonInput(t, request), headers, 202))
	replay := parsed[catalog.Edit](t, client.request("POST", "/api/recordings/"+id+"/edits", editor, jsonInput(t, request), headers, 202))
	if edit.ID != replay.ID {
		t.Fatalf("edit replay differs: %q %q", edit.ID, replay.ID)
	}
	must(t, worker.Step(ctx))
	result := parsed[catalog.Edit](t, client.request("GET", "/api/edits/"+edit.ID, editor, nil, nil, 200))
	if result.State != "published" || result.ActualFrom == nil || *result.ActualFrom != request.FromMS || *result.ActualTo != request.ToMS {
		t.Fatalf("clip not delivered: %+v", result)
	}
	clip := "/media/clip/" + edit.ID
	content := client.request("GET", clip+"/video.mp4", "", nil, map[string]string{"Cookie": strings.Split(cookie, ";")[0]}, 200)
	if !bytes.Contains(content.Body.Bytes()[:32], []byte("ftyp")) {
		t.Fatal("expected MP4 header")
	}
	subtitles := client.request("GET", clip+"/subtitles.vtt", viewer, nil, nil, 200).Body.String()
	if !strings.Contains(subtitles, "00:00:00.000 --> 00:00:01.000") ||
		!strings.Contains(subtitles, "00:00:01.200 --> 00:00:01.600") {
		t.Fatalf("incorrect trimmed subtitles: %s", subtitles)
	}
	const batchPath = "/api/visibility-batches/live-replay-removal"
	requestBatch := catalog.VisibilityBatchRequest{Hide: true, Targets: []catalog.VisibilityTarget{
		{ID: rec.ID, Revision: rec.Revision},
		{ID: "ffffffffffffffffffffffff", Revision: 1},
	}}
	client.request("PUT", batchPath, viewer, jsonInput(t, requestBatch), nil, 403)
	prepared := parsed[catalog.VisibilityBatch](t, client.request("PUT", batchPath, editor, jsonInput(t, requestBatch), nil, 200))
	if prepared.State != "prepared" || len(prepared.Items) != 2 {
		t.Fatalf("unexpected recording batch: %+v", prepared)
	}
	client.request("GET", batchPath, provision("other", "editor"), nil, nil, 404)
	client.request("GET", batchPath, owner, nil, nil, 404)
	client.request("GET", vod+"/index.m3u8", viewer, nil, nil, 200)
	// Discard a successful execute response and query by the original key.
	client.request("POST", batchPath+"/execute", editor, nil, nil, 200)
	saved := parsed[catalog.VisibilityBatch](t, client.request("GET", batchPath, editor, nil, nil, 200))
	repeated := parsed[catalog.VisibilityBatch](t, client.request("POST", batchPath+"/execute", editor, nil, nil, 200))
	if saved.State != "completed" || repeated.ID != saved.ID || saved.Processed != 2 || repeated.Processed != 2 ||
		saved.Items[0].State != "applied" || saved.Items[0].AppliedRevision == nil || *saved.Items[0].AppliedRevision != rec.Revision+1 ||
		saved.Items[1].State != "rejected" || saved.Items[1].Reason == nil || *saved.Items[1].Reason != "not_found" {
		t.Fatalf("wrong media visibility receipts: %+v / %+v", saved, repeated)
	}
	client.request("GET", vod+"/index.m3u8", viewer, nil, nil, 404)
	client.request("GET", clip+"/video.mp4", viewer, nil, nil, 200)
	client.request("GET", clip+"/video.mp4", outsider, nil, nil, 404)
	measurements := parsed[catalog.Measurements](t, client.request("GET", base+"/quality", owner, nil, nil, 200))
	if measurements.Switches != 1 || measurements.EgressBytes == 0 || measurements.EgressErrors < 2 {
		t.Fatalf("unexpected playback measurements: %+v", measurements)
	}
	t.Logf("live→backup→VOD→MP4 with two cue trims, %d bytes served, %d origin errors",
		measurements.EgressBytes, measurements.EgressErrors)
}
