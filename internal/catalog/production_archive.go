package catalog

import (
	"context"
)

func (s *Store) ProductionArchiveSegments(ctx context.Context, stream Session, track string, from, to int64) ([]Segment, error) {
	available := false
	for _, profile := range Profiles(stream.Preset) {
		available = available || track == profile.Track
	}
	if !available || from < 0 || from >= to || to > ProductionMaxFrames || to-from > 10*60*ProductionFPS {
		return nil, &ProductionInputError{"range", "choose a programme track and an interval of one frame to ten minutes"}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT m.seq,m.track,m.source,m.source_epoch,m.source_ms,m.offset_ms,m.media_ms,m.duration_ms,
		m.gap_ms,m.published_at,b.blob_key,b.digest,b.bytes FROM production_frames f JOIN productions p ON p.id=f.production_id
		JOIN segments m ON m.session_id=p.session_id AND m.seq=f.sequence_no JOIN blobs b ON b.blob_key=m.blob_key AND b.state='ready'
		WHERE p.session_id=? AND m.track=? AND f.start_frame>=? AND f.start_frame<? AND f.start_frame+f.frame_count>?
		ORDER BY f.start_frame`, stream.ID, track, max(0, from-249), to, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]Segment, 0)
	for rows.Next() {
		segment, err := scanSegment(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, segment)
	}
	return values, rows.Err()
}
