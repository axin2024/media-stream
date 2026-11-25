package catalog

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"time"
)

type Channel struct {
	Key       string    `json:"key"`
	Title     string    `json:"title"`
	Owner     string    `json:"owner"`
	From      time.Time `json:"available_from"`
	Until     time.Time `json:"available_until"`
	SessionID string    `json:"session_id,omitempty"`
}

func (s *Store) CreateChannel(ctx context.Context, caller Identity, input Channel) (Channel, error) {
	if !caller.Allows(caller.Tenant, input.Key, "", "owner") {
		return Channel{}, ErrDenied
	}
	input.Owner, input.SessionID = caller.Label, ""
	input.From, input.Until = input.From.UTC(), input.Until.UTC()
	if !identifier.MatchString(input.Key) || !strictText(input.Title, 160) || input.From.Year() < 2000 ||
		!input.Until.After(input.From) || !input.Until.After(time.Now()) || input.Until.Year() > 2099 {
		return Channel{}, ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO channels(tenant,channel,owner,title,available_from,available_until)
		VALUES(?,?,?,?,?,?)`, caller.Tenant, input.Key, input.Owner, input.Title, input.From, input.Until)
	if duplicate(err) {
		return Channel{}, ErrConflict
	}
	return input, err
}

func (s *Store) Channels(ctx context.Context, caller Identity, after string) ([]Channel, error) {
	if caller.Role == "push" || caller.Session != "" || (after != "" && !identifier.MatchString(after)) {
		return nil, ErrDenied
	}
	rows, err := s.db.QueryContext(ctx, `SELECT channel,title,owner,available_from,available_until,COALESCE(session_id,'')
		FROM channels WHERE tenant=? AND (?='' OR channel=?) AND channel>? ORDER BY channel LIMIT 101`,
		caller.Tenant, caller.Channel, caller.Channel, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	channels := make([]Channel, 0)
	for rows.Next() {
		var channel Channel
		if err := rows.Scan(&channel.Key, &channel.Title, &channel.Owner, &channel.From, &channel.Until, &channel.SessionID); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

type Session struct {
	ID              string     `json:"id"`
	Tenant          string     `json:"tenant"`
	Channel         string     `json:"channel"`
	State           string     `json:"state"`
	ActiveSource    string     `json:"active_source,omitempty"`
	SourceEpoch     uint64     `json:"source_epoch"`
	SourceFloorMS   int64      `json:"-"`
	NextOffsetMS    int64      `json:"source_watermark_ms"`
	SourceEndMS     int64      `json:"source_covered_until_ms"`
	MediaDurationMS int64      `json:"media_duration_ms"`
	NextSeq         uint64     `json:"next_sequence"`
	Revision        uint64     `json:"revision"`
	SegmentMS       int        `json:"segment_ms"`
	Preset          string     `json:"preset"`
	PreferredSource string     `json:"preferred_source,omitempty"`
	StartedAt       time.Time  `json:"started_at"`
	EndedAt         *time.Time `json:"ended_at,omitempty"`
}

const sessionSelect = `SELECT id,tenant,channel,state,active_source,source_epoch,source_floor_ms,next_offset_ms,source_end_ms,
	media_duration_ms,next_seq,revision,segment_ms,preset,preferred_source,started_at,ended_at FROM sessions WHERE id=?`

func readSession(ctx context.Context, db queryer, id string, lock bool) (Session, error) {
	query := sessionSelect
	if lock {
		query += " FOR UPDATE"
	}
	var result Session
	err := db.QueryRowContext(ctx, query, id).Scan(&result.ID, &result.Tenant, &result.Channel, &result.State,
		&result.ActiveSource, &result.SourceEpoch, &result.SourceFloorMS, &result.NextOffsetMS, &result.SourceEndMS, &result.MediaDurationMS,
		&result.NextSeq, &result.Revision, &result.SegmentMS, &result.Preset, &result.PreferredSource, &result.StartedAt, &result.EndedAt)
	return result, missing(err)
}

func (s *Store) Session(ctx context.Context, caller Identity, id string) (Session, error) {
	stream, err := readSession(ctx, s.db, id, false)
	if err == nil && !caller.Allows(stream.Tenant, stream.Channel, id, "owner", "editor", "director", "viewer", "operator") {
		return Session{}, ErrMissing
	}
	return stream, err
}

type Opening struct {
	Preset        string `json:"preset"`
	SegmentMS     int    `json:"segment_ms"`
	PrimaryZeroMS int64  `json:"primary_source_zero_ms"`
	BackupZeroMS  int64  `json:"backup_source_zero_ms"`
	ExpiresIn     int    `json:"push_ttl_seconds"`
}

type Opened struct {
	Session Session          `json:"session"`
	Inputs  map[string]Grant `json:"inputs"`
}

func (s *Store) OpenSession(ctx context.Context, caller Identity, channel, idempotency string, input Opening) (Opened, error) {
	if !caller.Allows(caller.Tenant, channel, "", "owner") {
		return Opened{}, ErrDenied
	}
	if !identifier.MatchString(channel) || !requestKey.MatchString(idempotency) || len(s.signing) < 32 ||
		(input.Preset != "standard" && input.Preset != "low-bandwidth") || input.SegmentMS < 1_000 || input.SegmentMS > 10_000 ||
		input.SegmentMS%1_000 != 0 || input.PrimaryZeroMS < 0 || input.BackupZeroMS < 0 ||
		input.ExpiresIn < 30 || input.ExpiresIn > 86_400 {
		return Opened{}, ErrInvalid
	}
	fingerprint := digest(struct {
		Channel string
		Opening
	}{channel, input})
	return transaction(ctx, s, func(tx *sql.Tx) (Opened, error) {
		var owner, previous string
		var from, until, now time.Time
		err := tx.QueryRowContext(ctx, `SELECT owner,COALESCE(session_id,''),available_from,available_until,UTC_TIMESTAMP(6)
			FROM channels WHERE tenant=? AND channel=? FOR UPDATE`, caller.Tenant, channel).Scan(&owner, &previous, &from, &until, &now)
		if err != nil {
			return Opened{}, missing(err)
		}
		if owner != caller.Label {
			return Opened{}, ErrMissing
		}
		id, found, err := receipt(ctx, tx, caller.Tenant, "open", idempotency, fingerprint)
		if err != nil {
			return Opened{}, err
		}
		if !found {
			if now.Before(from) || !now.Before(until) {
				return Opened{}, ErrConflict
			}
			if previous != "" {
				active, err := readSession(ctx, tx, previous, false)
				if err != nil {
					return Opened{}, err
				}
				if active.State != "archived" {
					return Opened{}, ErrConflict
				}
			}
			id = key()
			if _, err := tx.ExecContext(ctx, `INSERT INTO sessions(id,tenant,channel,state,segment_ms,preset) VALUES(?,?,?,'waiting',?,?)`,
				id, caller.Tenant, channel, input.SegmentMS, input.Preset); err != nil {
				return Opened{}, err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE channels SET session_id=? WHERE tenant=? AND channel=?", id, caller.Tenant, channel); err != nil {
				return Opened{}, err
			}
			expires := now.Add(time.Duration(input.ExpiresIn) * time.Second)
			if expires.After(until) {
				expires = until
			}
			initial := map[string]int64{"primary": input.PrimaryZeroMS, "backup": input.BackupZeroMS}
			for _, source := range []string{"primary", "backup"} {
				if _, err := tx.ExecContext(ctx, "INSERT INTO source_inputs(session_id,source,generation,clock_origin_ms) VALUES(?,?,1,?)",
					id, source, initial[source]); err != nil {
					return Opened{}, err
				}
				if _, err := s.newPushGrant(ctx, tx, caller.Tenant, channel, id, source, 1, expires); err != nil {
					return Opened{}, err
				}
			}
			if err := saveReceipt(ctx, tx, caller.Tenant, "open", idempotency, fingerprint, id); err != nil {
				return Opened{}, err
			}
			if err := audit(ctx, tx, id, "session_opened", caller.Label, input); err != nil {
				return Opened{}, err
			}
		}
		result := Opened{Inputs: make(map[string]Grant)}
		result.Session, err = readSession(ctx, tx, id, false)
		if err != nil {
			return Opened{}, err
		}
		for _, source := range []string{"primary", "backup"} {
			secret := s.pushSecret(id, source, 1)
			sum := digestSecret(secret)
			var expires time.Time
			var revoked *time.Time
			err := tx.QueryRowContext(ctx, "SELECT expires_at,revoked_at FROM credentials WHERE digest=?", sum[:]).Scan(&expires, &revoked)
			if err != nil {
				return Opened{}, err
			}
			if revoked == nil && now.Before(expires) {
				result.Inputs[source] = Grant{Secret: secret, Generation: 1, Expires: expires}
			}
		}
		return result, nil
	})
}

func receipt(ctx context.Context, db queryer, tenant, operation, request string, fingerprint [32]byte) (string, bool, error) {
	var prior []byte
	var id string
	err := db.QueryRowContext(ctx, `SELECT request_digest,object_id FROM operation_receipts WHERE tenant=? AND operation=? AND request_key=?`,
		tenant, operation, request).Scan(&prior, &id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if !bytes.Equal(fingerprint[:], prior) {
		return "", false, ErrConflict
	}
	return id, true, nil
}

func saveReceipt(ctx context.Context, tx *sql.Tx, tenant, operation, request string, fingerprint [32]byte, id string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO operation_receipts(tenant,operation,request_key,request_digest,object_id) VALUES(?,?,?,?,?)`,
		tenant, operation, request, fingerprint[:], id)
	if duplicate(err) {
		return ErrConflict
	}
	return err
}

func (s *Store) newPushGrant(ctx context.Context, tx *sql.Tx, tenant, channel, session, source string, generation uint64, expires time.Time) (Grant, error) {
	grant := Grant{Secret: s.pushSecret(session, source, generation), Generation: generation, Expires: expires}
	err := insertGrant(ctx, tx, Identity{Tenant: tenant, Label: source, Role: "push", Channel: channel,
		Session: session, Source: source, SourceGeneration: generation}, grant)
	return grant, err
}
