package web

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/axin2024/media-stream/internal/catalog"
)

type archiveInterval struct {
	From   int64   `json:"from_frame"`
	To     int64   `json:"to_frame"`
	Source string  `json:"source"`
	Seq    *uint64 `json:"sequence,omitempty"`
}

type archiveTrack struct {
	Sequence uint64 `json:"sequence"`
	From     int64  `json:"from_frame"`
	To       int64  `json:"to_frame"`
	Bytes    int64  `json:"bytes"`
	SHA256   string `json:"sha256"`
	URL      string `json:"url"`
}

type productionArchive struct {
	Production string            `json:"production_id"`
	AsRun      uint64            `json:"asrun_rev"`
	State      string            `json:"state"`
	Track      string            `json:"track"`
	From       int64             `json:"requested_from_frame"`
	To         int64             `json:"requested_to_frame"`
	ActualFrom *int64            `json:"actual_from_frame,omitempty"`
	ActualTo   *int64            `json:"actual_to_frame,omitempty"`
	Missing    []archiveInterval `json:"missing"`
	Segments   []archiveTrack    `json:"segments"`
	Digest     string            `json:"archive_digest,omitempty"`
}

func (s *Server) productionArchive(w http.ResponseWriter, r *http.Request, actor catalog.Identity) {
	p, _, err := s.DB.ProductionAsRun(r.Context(), actor, r.PathValue("production"), 0, 1)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	stream, err := s.DB.Playback(r.Context(), actor, p.Output, true)
	if err != nil {
		if p.State != "sealed" {
			err = catalog.ErrConflict
		}
		s.fail(w, r, err)
		return
	}
	query := r.URL.Query()
	from, fromErr := strconv.ParseInt(query.Get("from_frame"), 10, 64)
	to, toErr := strconv.ParseInt(query.Get("to_frame"), 10, 64)
	track := query.Get("track")
	format := query.Get("format")
	if strings.HasPrefix(r.URL.Path, "/media/archive/") {
		format = "playlist"
	}
	if fromErr != nil || toErr != nil || (format != "" && format != "playlist") {
		s.fail(w, r, &catalog.ProductionInputError{Field: "range", Cause: "from_frame and to_frame are required"})
		return
	}
	segments, err := s.DB.ProductionArchiveSegments(r.Context(), stream, track, from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	result := productionArchive{Production: p.ID, AsRun: p.AsRunRev, State: "incomplete", Track: track, From: from, To: to,
		Missing: []archiveInterval{}, Segments: []archiveTrack{}}
	var intact []catalog.Segment
	cursor := from
	for _, segment := range segments {
		start := segment.MediaMS / catalog.ProductionFrameMS
		end := start + int64(segment.DurationMS/catalog.ProductionFrameMS)
		if start > cursor {
			result.Missing = append(result.Missing, archiveInterval{From: cursor, To: min(start, to), Source: "program_output"})
		}
		if _, err := s.Files.Path(segment.Blob, true); err != nil {
			result.Missing = append(result.Missing, archiveInterval{From: max(start, from), To: min(end, to), Source: "unreadable_fragment", Seq: &segment.Seq})
		} else {
			intact = append(intact, segment)
			result.Segments = append(result.Segments, archiveTrack{Sequence: segment.Seq, From: start, To: end,
				Bytes: segment.Blob.Bytes, SHA256: hex.EncodeToString(segment.Blob.Digest[:]), URL: fmt.Sprintf("/media/vod/%s/parts/%s/%d.ts", p.Output, track, segment.Seq)})
		}
		cursor = max(cursor, end)
	}
	if cursor < to {
		result.Missing = append(result.Missing, archiveInterval{From: cursor, To: to, Source: "program_output"})
	}
	if len(result.Missing) > 0 || len(intact) == 0 {
		status := 200
		if format == "playlist" {
			status = 409
		}
		reply(w, status, result)
		return
	}
	result.ActualFrom = &result.Segments[0].From
	result.ActualTo = &result.Segments[len(result.Segments)-1].To
	result.State = "complete"
	encoded, err := json.Marshal(result.Segments)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	digest := sha256.Sum256(encoded)
	result.Digest = hex.EncodeToString(digest[:])
	if format != "playlist" {
		reply(w, 200, result)
		return
	}
	value := manifest{finished: true, first: intact[0].Seq, targetTime: (stream.SegmentMS + 999) / 1000}
	list := playlistMedia(value, intact, *p.Started)
	list = strings.ReplaceAll(list, "\nparts/", "\n/media/vod/"+p.Output+"/parts/")
	w.Header().Set("X-Archive-SHA256", result.Digest)
	writePlaylist(w, list)
}
