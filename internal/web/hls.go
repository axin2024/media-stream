package web

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/axin2024/media-stream/internal/catalog"
)

const liveWindow = 8

type counted struct {
	http.ResponseWriter
	status int
	bytes  int64
	failed bool
}

func (w *counted) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *counted) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	w.failed = w.failed || err != nil
	return n, err
}

func (s *Server) play(w http.ResponseWriter, r *http.Request, user catalog.Identity) {
	id, kind, name := r.PathValue("id"), r.PathValue("kind"), r.PathValue("file")
	if kind == "clip" {
		s.clipMedia(w, r, user, id, name)
		return
	}
	if kind != "live" && kind != "vod" {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	stream, err := s.DB.Playback(r.Context(), user, id, kind == "vod")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writer := &counted{ResponseWriter: w}
	s.streamMedia(writer, r, user, stream, name)
	s.measure(r, stream.ID, writer.bytes, writer.failed || writer.status >= 400)
}

func (s *Server) streamMedia(w http.ResponseWriter, r *http.Request, user catalog.Identity, stream catalog.Session, name string) {
	if parts := strings.Split(name, "/"); len(parts) > 1 {
		switch {
		case len(parts) == 3 && parts[0] == "parts":
			s.playSegment(w, r, stream, parts[1], parts[2])
		case len(parts) == 2 && parts[0] == "captions" && stream.State == "archived":
			s.playCaption(w, r, user, stream, parts[1])
		default:
			s.fail(w, r, catalog.ErrMissing)
		}
		return
	}
	if !strings.HasSuffix(name, ".m3u8") {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	snapshot, err := s.snapshot(r.Context(), stream)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(snapshot.canonical) == 0 {
		w.Header().Set("Retry-After", "2")
		reply(w, 503, map[string]string{"code": "playlist_unavailable", "message": "waiting for a contiguous set of complete fragments"})
		return
	}
	track := strings.TrimSuffix(name, ".m3u8")
	if track == "index" {
		var captions []catalog.Caption
		if snapshot.finished {
			captions, err = s.DB.Captions(r.Context(), user, stream.ID)
			if err != nil {
				s.fail(w, r, err)
				return
			}
		}
		writePlaylist(w, playlistMaster(snapshot, catalog.Profiles(stream.Preset), captions))
		return
	}
	segments := snapshot.tracks[track]
	if track == "source" {
		segments = snapshot.canonical
	}
	if len(segments) == 0 {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	writePlaylist(w, playlistMedia(snapshot, segments, stream.StartedAt))
}

type manifest struct {
	finished           bool
	first              uint64
	discontinuity      uint64
	firstDiscontinuity bool
	targetTime         int
	tracks             map[string][]catalog.Segment
	canonical          []catalog.Segment
}

func (s *Server) snapshot(ctx context.Context, stream catalog.Session) (manifest, error) {
	finished := stream.State == "archived"
	first := uint64(0)
	if !finished && stream.NextSeq > liveWindow {
		first = stream.NextSeq - liveWindow
	}
	result := manifest{finished: finished, first: first, targetTime: (stream.SegmentMS + 999) / 1_000,
		tracks: make(map[string][]catalog.Segment)}
	if stream.NextSeq == 0 {
		return result, nil
	}
	values, err := s.DB.Segments(ctx, stream.ID, first, stream.NextSeq)
	if err != nil {
		return manifest{}, err
	}
	return s.buildManifest(ctx, stream, result, values)
}

func (s *Server) buildManifest(ctx context.Context, stream catalog.Session, result manifest, values []catalog.Segment) (manifest, error) {
	var best []catalog.Segment
	for _, seg := range values {
		if !s.Files.Available(seg.Blob) {
			continue
		}
		result.targetTime = max(result.targetTime, (seg.DurationMS+999)/1_000)
		result.tracks[seg.Track] = append(result.tracks[seg.Track], seg)
		if last := len(best) - 1; last >= 0 && best[last].Seq == seg.Seq {
			if trackHeight(seg.Track) > trackHeight(best[last].Track) {
				best[last] = seg
			}
		} else {
			best = append(best, seg)
		}
	}
	count := int(stream.NextSeq - result.first)
	if len(best) != count {
		return result, nil
	}
	for name, lane := range result.tracks {
		if len(lane) != count {
			delete(result.tracks, name)
		}
	}
	result.canonical = best
	if result.first > 0 {
		var err error
		result.discontinuity, err = s.DB.PriorDiscontinuities(ctx, stream.ID, result.first)
		if err != nil {
			return manifest{}, err
		}
		result.firstDiscontinuity, err = s.DB.LocalDiscontinuity(ctx, stream.ID, result.first)
		return result, err
	}
	result.firstDiscontinuity = best[0].GapMS > 0
	return result, nil
}

func trackHeight(name string) int {
	size, _ := strconv.Atoi(strings.TrimSuffix(name, "p"))
	return size
}

func playlistMaster(value manifest, preset []catalog.Profile, captions []catalog.Caption) string {
	var text strings.Builder
	text.WriteString("#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n")
	for _, caption := range captions {
		fmt.Fprintf(&text, "#EXT-X-MEDIA:TYPE=SUBTITLES,GROUP-ID=\"text\",NAME=\"%s\",LANGUAGE=\"%s\",AUTOSELECT=YES,URI=\"captions/%s.m3u8\"\n",
			caption.Language, caption.Language, caption.ID)
	}
	subtitleGroup := ""
	if len(captions) > 0 {
		subtitleGroup = ",SUBTITLES=\"text\""
	}
	if len(value.tracks) == 0 {
		maxBandwidth := 0
		for _, profile := range preset {
			maxBandwidth = max(maxBandwidth, bandwidth(profile))
		}
		fmt.Fprintf(&text, "#EXT-X-STREAM-INF:BANDWIDTH=%d%s\nsource.m3u8\n", maxBandwidth, subtitleGroup)
		return text.String()
	}
	slices.SortFunc(preset, func(a, b catalog.Profile) int { return a.VideoKbps - b.VideoKbps })
	for _, profile := range preset {
		if len(value.tracks[profile.Track]) != 0 {
			fmt.Fprintf(&text, "#EXT-X-STREAM-INF:BANDWIDTH=%d,RESOLUTION=%dx%d,FRAME-RATE=25.000%s\n%s.m3u8\n",
				bandwidth(profile), profile.Width, profile.Height, subtitleGroup, profile.Track)
		}
	}
	return text.String()
}

func bandwidth(p catalog.Profile) int { return (p.VideoKbps + p.AudioKbps) * 1_250 }

func playlistMedia(value manifest, segments []catalog.Segment, start time.Time) string {
	var text strings.Builder
	fmt.Fprintf(&text, "#EXTM3U\n#EXT-X-VERSION:6\n#EXT-X-INDEPENDENT-SEGMENTS\n#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n",
		value.targetTime, value.first)
	fmt.Fprintf(&text, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", value.discontinuity)
	if value.finished {
		text.WriteString("#EXT-X-PLAYLIST-TYPE:VOD\n")
	}
	for index, seg := range segments {
		boundary := value.firstDiscontinuity
		if index > 0 {
			prior := segments[index-1]
			boundary = seg.SourceEpoch != prior.SourceEpoch || seg.GapMS > 0 || prior.Track != seg.Track
		}
		if boundary {
			text.WriteString("#EXT-X-DISCONTINUITY\n")
		}
		if boundary || index == 0 {
			at := start.Add(time.Duration(seg.OffsetMS) * time.Millisecond)
			fmt.Fprintf(&text, "#EXT-X-PROGRAM-DATE-TIME:%s\n", at.UTC().Format(time.RFC3339Nano))
		}
		fmt.Fprintf(&text, "#EXTINF:%.3f,\nparts/%s/%d.ts\n", float64(seg.DurationMS)/1_000, seg.Track, seg.Seq)
	}
	if value.finished {
		text.WriteString("#EXT-X-ENDLIST\n")
	}
	return text.String()
}

func writePlaylist(w http.ResponseWriter, value string) {
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl; charset=utf-8")
	_, _ = w.Write([]byte(value))
}

func (s *Server) playSegment(w http.ResponseWriter, r *http.Request, stream catalog.Session, track, name string) {
	raw, valid := strings.CutSuffix(name, ".ts")
	seq, err := number(raw)
	if !valid || err != nil || seq >= stream.NextSeq {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	value, err := s.DB.ReadSegment(r.Context(), stream.ID, track, seq)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.file(w, r, value.Blob, name, "video/mp2t")
}

func (s *Server) playCaption(w http.ResponseWriter, r *http.Request, caller catalog.Identity, stream catalog.Session, name string) {
	id, extension, found := strings.Cut(name, ".")
	if !found || (extension != "m3u8" && extension != "vtt") {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	value, err := s.DB.Caption(r.Context(), caller, stream.ID, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if extension == "vtt" {
		s.file(w, r, value.Blob, name, "text/vtt; charset=utf-8")
		return
	}
	if !s.Files.Available(value.Blob) {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	writePlaylist(w, fmt.Sprintf("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:%d\n#EXT-X-PLAYLIST-TYPE:VOD\n#EXTINF:%.3f,\n%s.vtt\n#EXT-X-ENDLIST\n",
		(stream.MediaDurationMS+999)/1_000, float64(stream.MediaDurationMS)/1_000, value.ID))
}

func (s *Server) clipMedia(w http.ResponseWriter, r *http.Request, user catalog.Identity, id, name string) {
	kind := "video/mp4"
	if name == "subtitles.vtt" {
		kind = "text/vtt; charset=utf-8"
	} else if name != "video.mp4" {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	edit, blob, err := s.DB.EditMedia(r.Context(), user, id, name == "subtitles.vtt")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writer := &counted{ResponseWriter: w}
	s.file(writer, r, blob, name, kind)
	s.measure(r, edit.Session, writer.bytes, writer.failed || writer.status >= 400)
}

func (s *Server) file(w http.ResponseWriter, r *http.Request, blob catalog.Blob, name, contentType string) {
	file, err := s.Files.Open(blob)
	if err != nil {
		s.fail(w, r, catalog.ErrMissing)
		return
	}
	defer file.Close()
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", "\""+hex.EncodeToString(blob.Digest[:])+"\"")
	http.ServeContent(w, r, name, time.Time{}, file)
}
