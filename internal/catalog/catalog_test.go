package catalog

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"os"
	"sync"
	"testing"
	"time"
)

func localCatalog(t *testing.T, name string, steps int) *Store {
	t.Helper()
	dsn := os.Getenv(name)
	if dsn == "" {
		t.Skipf("set %s to an empty MySQL 8.4 database", name)
	}
	if err := Migrate(dsn, steps); err != nil {
		t.Fatal(err)
	}
	db, err := Open(context.Background(), dsn, []byte("test-key-that-is-at-least-thirty-two-bytes"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func require[T any](t *testing.T, found T, err error) T {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return found
}

func ok(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func provision(t *testing.T, db *Store, tenant, label, role string) Identity {
	t.Helper()
	grant, err := db.Provision(t.Context(), tenant, label, role, "", time.Now().Add(time.Hour))
	ok(t, err)
	id, err := db.Identify(t.Context(), grant.Secret)
	return require(t, id, err)
}

func fakeBlob(text string) Blob {
	return Blob{Key: key(), Digest: sha256.Sum256([]byte(text)), Bytes: int64(len(text))}
}

func openTestChannel(t *testing.T, db *Store, owner Identity, channel string) (Channel, Opened) {
	t.Helper()
	request := Channel{Key: channel, Title: "Studio " + channel, From: time.Now().Add(-time.Minute), Until: time.Now().Add(time.Hour)}
	created, err := db.CreateChannel(t.Context(), owner, request)
	ok(t, err)
	stream, err := db.OpenSession(t.Context(), owner, created.Key, "open-test", Opening{
		Preset: "standard", SegmentMS: 1_000, PrimaryZeroMS: 10_000, BackupZeroMS: 20_000, ExpiresIn: 600})
	ok(t, err)
	return created, stream
}

func TestMySQLPublicationFailoverAndRecovery(t *testing.T) {
	db := localCatalog(t, "MEDIA_STREAM_TEST_DSN", 0)
	ctx := t.Context()
	ok(t, db.Ready(ctx))
	owner := provision(t, db, "alpha", "host", "owner")
	viewer := provision(t, db, "alpha", "audience", "viewer")
	editor := provision(t, db, "alpha", "producer", "editor")
	other := provision(t, db, "beta", "host", "owner")
	channel, live := openTestChannel(t, db, owner, "show")
	openTestChannel(t, db, other, "show")
	stream := live.Session.ID
	_, err := db.Session(ctx, other, stream)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("cross-tenant read returned %v", err)
	}
	input := Opening{Preset: "standard", SegmentMS: 1_000, PrimaryZeroMS: 10_000, BackupZeroMS: 20_000, ExpiresIn: 600}
	replay, err := db.OpenSession(ctx, owner, channel.Key, "open-test", input)
	ok(t, err)
	if replay.Session.ID != stream || replay.Inputs["primary"].Secret != live.Inputs["primary"].Secret {
		t.Fatal("lost opening response changed result")
	}
	input.SegmentMS = 2_000
	_, err = db.OpenSession(ctx, owner, channel.Key, "open-test", input)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("changed idempotent request returned %v", err)
	}
	_, err = db.Ingest(ctx, viewer, stream, 10_000, 1_000, fakeBlob("video"))
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer push returned %v", err)
	}
	primary, err := db.Identify(ctx, live.Inputs["primary"].Secret)
	ok(t, err)
	backup, err := db.Identify(ctx, live.Inputs["backup"].Secret)
	ok(t, err)
	raw := fakeBlob("primary at zero")
	_, err = db.Ingest(ctx, primary, stream, 10_000, 1_000, raw)
	ok(t, err)
	duplicate, err := db.Ingest(ctx, primary, stream, 10_000, 1_000, fakeBlob("primary at zero"))
	ok(t, err)
	if !duplicate.Replay {
		t.Fatal("replayed source not recognized")
	}
	_, err = db.Ingest(ctx, primary, stream, 10_000, 1_000, fakeBlob("different"))
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("same offset different data returned %v", err)
	}
	_, err = db.Ingest(ctx, backup, stream, 20_000, 1_000, fakeBlob("backup at zero"))
	ok(t, err)
	ok(t, db.RegisterNode(ctx, "node-1", 1, true))
	ok(t, db.RegisterNode(ctx, "node-2", 1, true))
	ok(t, db.Schedule(ctx, stream))
	first, err := db.Claim(ctx, "node-1")
	ok(t, err)
	if first == nil || first.Source != "primary" || first.Seq != 0 {
		t.Fatalf("unexpected first claim: %+v", first)
	}
	_, err = db.db.ExecContext(ctx, "UPDATE transcode_jobs SET lease_until=UTC_TIMESTAMP(6) WHERE id=?", first.ID)
	ok(t, err)
	second, err := db.Claim(ctx, "node-2")
	ok(t, err)
	if second == nil || second.ID != first.ID || second.Attempt != first.Attempt+1 {
		t.Fatalf("expected fenced retry: %+v", second)
	}
	stale := db.Complete(ctx, *first, []Rendition{{"720p", fakeBlob("stale")}}, "")
	if !errors.Is(stale, ErrFenced) {
		t.Fatalf("stale publication returned %v", stale)
	}
	ok(t, db.Complete(ctx, *second, []Rendition{{"360p", fakeBlob("p0 low")}, {"720p", fakeBlob("p0 high")}}, ""))
	_, err = db.Ingest(ctx, primary, stream, 11_000, 1_000, fakeBlob("another primary"))
	ok(t, err)
	ok(t, db.Schedule(ctx, stream))
	obsolete, err := db.Claim(ctx, "node-1")
	ok(t, err)
	if obsolete == nil || obsolete.Source != "primary" {
		t.Fatalf("unexpected current activity: %+v", obsolete)
	}
	// The backup continues on its own source clock. There is a one-second source gap.
	_, err = db.Ingest(ctx, backup, stream, 22_000, 1_000, fakeBlob("backup selected"))
	ok(t, err)
	ok(t, db.InputLost(ctx, primary, stream))
	ok(t, db.Schedule(ctx, stream))
	takeover, err := db.Claim(ctx, "node-2")
	ok(t, err)
	if takeover == nil || takeover.Source != "backup" || takeover.SourceEpoch <= obsolete.SourceEpoch || takeover.GapMS != 1_000 {
		t.Fatalf("standby did not take over: %+v", takeover)
	}
	if err := db.Complete(ctx, *obsolete, []Rendition{{"360p", fakeBlob("late")}}, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("late source completion returned %v", err)
	}
	ok(t, db.Complete(ctx, *takeover, []Rendition{{"360p", fakeBlob("b2 low")}}, "720p unavailable"))
	session, err := db.Session(ctx, owner, stream)
	ok(t, err)
	if session.State != "degraded" || session.NextOffsetMS != 3_000 || session.MediaDurationMS != 2_000 {
		t.Fatalf("unexpected timeline: %+v", session)
	}
	_, err = db.Ingest(ctx, primary, stream, 12_000, 1_000, fakeBlob("too late"))
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("late packet returned %v", err)
	}
	rotated, err := db.RotateInput(ctx, owner, stream, "primary", "rotate-one", Rotation{Generation: 1, OriginMS: 10_000, ExpiresIn: 600})
	ok(t, err)
	_, err = db.Identify(ctx, live.Inputs["primary"].Secret)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("revoked token returned %v", err)
	}
	_, err = db.db.ExecContext(ctx, "UPDATE credentials SET expires_at=UTC_TIMESTAMP(6) WHERE digest=?", digestBytesForTest(rotated.Secret))
	ok(t, err)
	_, err = db.Identify(ctx, rotated.Secret)
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("expired token returned %v", err)
	}
	signal := QoE{Device: "device", Key: "stall-1", Kind: "stall", Duration: 1_222, MediaTime: 1_000}
	fresh, err := db.RecordQoE(ctx, viewer, stream, signal)
	ok(t, err)
	prior, err := db.RecordQoE(ctx, viewer, stream, signal)
	ok(t, err)
	if fresh || !prior {
		t.Fatal("QoE replay accounting failed")
	}
	measured, err := db.Measure(ctx, owner, stream)
	ok(t, err)
	if measured.Stalls != 1 || measured.StalledMS != 1_222 || measured.Switches != 1 {
		t.Fatalf("unexpected measurement %+v", measured)
	}
	_, err = db.End(ctx, owner, stream)
	ok(t, err)
	ok(t, db.Schedule(ctx, stream))
	_, err = db.End(ctx, owner, stream)
	ok(t, err)
	ok(t, db.Schedule(ctx, stream))
	rec, err := db.Recording(ctx, viewer, stream)
	ok(t, err)
	if rec.State != "published" || rec.DurationMS != 2_000 {
		t.Fatalf("incorrect archive %+v", rec)
	}
	caption, err := db.AddCaption(ctx, editor, rec.ID, "en", "caption-demo", fakeBlob("WEBVTT"))
	ok(t, err)
	_, err = db.CreateEdit(ctx, editor, rec.ID, "invalid", EditRequest{Revision: rec.Revision, FromMS: 500, ToMS: 400})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid range returned %v", err)
	}
	selection := EditRequest{Revision: rec.Revision, FromMS: 200, ToMS: 1_400, Caption: caption.ID}
	edit, err := db.CreateEdit(ctx, editor, rec.ID, "demo-edit", selection)
	ok(t, err)
	_, err = db.SetVisibility(ctx, editor, rec.ID, rec.Revision, nil, true)
	ok(t, err)
	_, err = db.Recording(ctx, viewer, rec.ID)
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("hidden recording returned %v", err)
	}
	_, err = db.db.ExecContext(ctx, "UPDATE recordings SET hidden_at=TIMESTAMPADD(MINUTE,-2,UTC_TIMESTAMP(6)) WHERE id=?", stream)
	ok(t, err)
	purged, err := db.PurgeHidden(ctx, time.Minute)
	ok(t, err)
	if purged != 0 {
		t.Fatal("pending edit lost its source")
	}
	claimed, err := db.ClaimEdit(ctx, "node-1")
	ok(t, err)
	if claimed == nil || claimed.ID != edit.ID {
		t.Fatalf("unexpected edit claim %+v", claimed)
	}
	parts, captions, err := db.EditInputs(ctx, *claimed)
	ok(t, err)
	if captions == nil || len(parts) != 2 {
		t.Fatalf("incomplete edit source: %v / %v", parts, captions)
	}
	ok(t, db.CompleteEdit(ctx, *claimed, fakeBlob("delivery"), fakeBlob("caption delivery"), 200, 1_400, ""))
	purged, err = db.PurgeHidden(ctx, time.Minute)
	ok(t, err)
	if purged != 1 {
		t.Fatalf("expected one retired recording, got %d", purged)
	}
	published, asset, err := db.EditMedia(ctx, viewer, edit.ID, false)
	ok(t, err)
	if published.State != "published" || asset.Digest != fakeBlob("delivery").Digest {
		t.Fatalf("independent clip was lost: %+v", published)
	}
	_, err = db.db.ExecContext(ctx, "UPDATE blobs SET created_at=TIMESTAMPADD(MINUTE,-3,UTC_TIMESTAMP(6))")
	ok(t, err)
	retire, err := db.Collectible(ctx, time.Minute)
	ok(t, err)
	if len(retire) < 4 {
		t.Fatalf("expected stale assets to collect, got %v", retire)
	}
	for _, key := range retire {
		if key == asset.Key {
			t.Fatal("collection selected a published clip")
		}
		ok(t, db.MarkDeleted(ctx, key))
	}
}

func digestBytesForTest(secret string) []byte {
	sum := digestSecret(secret)
	return sum[:]
}

func TestMySQLUpgradePreservesSessionAndCredential(t *testing.T) {
	db := localCatalog(t, "MEDIA_STREAM_TEST_UPGRADE_DSN", 1)
	ctx := t.Context()
	owner := provision(t, db, "upgrade", "host", "owner")
	channel, opened := openTestChannel(t, db, owner, "legacy")
	ok(t, Migrate(os.Getenv("MEDIA_STREAM_TEST_UPGRADE_DSN"), 2))
	ok(t, Migrate(os.Getenv("MEDIA_STREAM_TEST_UPGRADE_DSN"), 0))
	ok(t, db.Ready(ctx))
	session, err := db.Session(ctx, owner, opened.Session.ID)
	ok(t, err)
	if session.Channel != channel.Key || session.Revision != opened.Session.Revision {
		t.Fatalf("prior session changed: %+v", session)
	}
	identity, err := db.Identify(ctx, opened.Inputs["primary"].Secret)
	ok(t, err)
	_, err = db.Ingest(ctx, identity, session.ID, 10_000, 1_000, fakeBlob("first after upgrade"))
	ok(t, err)
	ok(t, db.Schedule(ctx, session.ID))
}

func TestMySQLVisibilityBatchContinuesAfterAnItemCommits(t *testing.T) {
	db := localCatalog(t, "MEDIA_STREAM_TEST_DSN", 0)
	ctx := t.Context()
	owner := provision(t, db, "batch-studio", "host", "owner")
	editor := provision(t, db, owner.Tenant, "producer", "editor")
	peer := provision(t, db, owner.Tenant, editor.Label, "editor")
	other := provision(t, db, "batch-other", editor.Label, "editor")
	viewer := provision(t, db, owner.Tenant, "audience", "viewer")
	_, first := openTestChannel(t, db, owner, "show")
	stream := first.Session.ID
	_, err := db.db.ExecContext(ctx, `INSERT INTO recordings(id,session_id,state,duration_ms) VALUES(?,?,'published',1000)`, stream, stream)
	ok(t, err)

	// Change the selected version after preparation to exercise a conflict
	// independently of the successful item in the same batch.
	channel, err := db.CreateChannel(ctx, owner, Channel{
		Key: "changed", Title: "Changed", From: time.Now().Add(-time.Minute), Until: time.Now().Add(time.Hour),
	})
	ok(t, err)
	second, err := db.OpenSession(ctx, owner, channel.Key, "open-changed", Opening{Preset: "standard", SegmentMS: 1_000, ExpiresIn: 600})
	ok(t, err)
	stale := second.Session.ID
	_, err = db.db.ExecContext(ctx, `INSERT INTO recordings(id,session_id,state,duration_ms) VALUES(?,?,'published',1000)`, stale, stale)
	ok(t, err)

	const requestKey = "hide-selected"
	input := VisibilityBatchRequest{Hide: true, Targets: []VisibilityTarget{{stream, 1}, {stale, 1}, {key(), 1}}}
	prepared, err := db.PrepareVisibilityBatch(ctx, editor, requestKey, input)
	ok(t, err)
	if prepared.State != "prepared" || prepared.Processed != 0 || len(prepared.Items) != 3 || prepared.Items[0].State != "not_started" {
		t.Fatalf("unexpected preparation: %+v", prepared)
	}
	until := time.Now().Add(time.Hour)
	_, err = db.SetVisibility(ctx, editor, stale, 1, &until, false)
	ok(t, err)
	_, err = db.Recording(ctx, viewer, stream)
	ok(t, err)
	for _, caller := range []Identity{peer, other} {
		_, err := db.VisibilityBatch(ctx, caller, requestKey)
		if !errors.Is(err, ErrMissing) {
			t.Fatalf("receipt from another credential returned %v", err)
		}
		visible, err := db.VisibilityBatches(ctx, caller, 0)
		ok(t, err)
		if len(visible) != 0 {
			t.Fatalf("batch history leaked: %+v", visible)
		}
	}
	if _, err := db.ExecuteVisibilityBatch(ctx, viewer, requestKey); !errors.Is(err, ErrDenied) {
		t.Fatalf("viewer execution returned %v", err)
	}

	done, err := transaction(ctx, db, func(tx *sql.Tx) (bool, error) {
		return executeVisibilityItem(ctx, tx, editor, requestKey)
	})
	ok(t, err)
	if done {
		t.Fatal("first item completed the whole batch")
	}
	reopened, err := Open(ctx, os.Getenv("MEDIA_STREAM_TEST_DSN"), nil)
	ok(t, err)
	t.Cleanup(func() { reopened.Close() })
	partial, err := reopened.VisibilityBatch(ctx, editor, requestKey)
	ok(t, err)
	if partial.State != "running" || partial.Processed != 1 || partial.Items[0].State != "applied" ||
		partial.Items[1].State != "not_started" || partial.Items[2].State != "not_started" {
		t.Fatalf("failed to recover the partial batch: %+v", partial)
	}
	var work sync.WaitGroup
	errs := make(chan error, 2)
	for _, runner := range []*Store{db, reopened} {
		work.Go(func() {
			_, err := runner.ExecuteVisibilityBatch(ctx, editor, requestKey)
			errs <- err
		})
	}
	work.Wait()
	close(errs)
	for err := range errs {
		ok(t, err)
	}
	finished, err := db.PrepareVisibilityBatch(ctx, editor, requestKey, input)
	ok(t, err)
	if finished.ID != prepared.ID || finished.State != "completed" || finished.Processed != 3 ||
		finished.Items[0].AppliedRevision == nil || *finished.Items[0].AppliedRevision != 2 ||
		finished.Items[1].Reason == nil || *finished.Items[1].Reason != "revision_or_state_changed" ||
		finished.Items[2].Reason == nil || *finished.Items[2].Reason != "not_found" {
		t.Fatalf("unexpected recovered result: %+v", finished)
	}
	input.Targets = input.Targets[:1]
	if _, err := db.PrepareVisibilityBatch(ctx, editor, requestKey, input); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed selection returned %v", err)
	}
	var auditCount, revision int
	ok(t, db.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM session_events WHERE session_id=? AND kind='recording_visibility'
		AND actor=? AND details->>'$.batch_key'=?`, stream, editor.Label, requestKey).Scan(&auditCount))
	ok(t, db.db.QueryRowContext(ctx, "SELECT revision FROM recordings WHERE id=?", stream).Scan(&revision))
	if auditCount != 1 || revision != 2 {
		t.Fatalf("visibility applied %d times, revision=%d", auditCount, revision)
	}
	deadline := time.Now().UTC().Add(30 * time.Minute).Truncate(time.Microsecond)
	_, err = db.PrepareVisibilityBatch(ctx, editor, "set-deadline", VisibilityBatchRequest{
		Targets: []VisibilityTarget{{stale, 2}}, Until: &deadline,
	})
	ok(t, err)
	timed, err := db.ExecuteVisibilityBatch(ctx, editor, "set-deadline")
	ok(t, err)
	visible, err := db.Recording(ctx, viewer, stale)
	ok(t, err)
	if timed.Items[0].State != "applied" || visible.Revision != 3 || visible.VisibleUntil == nil || !visible.VisibleUntil.Equal(deadline) {
		t.Fatalf("deadline receipt disagreed with public record: %+v / %+v", timed, visible)
	}
	_, err = db.db.ExecContext(ctx, "UPDATE recordings SET visible_until=UTC_TIMESTAMP(6) WHERE id=?", stale)
	ok(t, err)
	ok(t, db.ExpirePublications(ctx))
	if _, err := db.Recording(ctx, viewer, stale); !errors.Is(err, ErrMissing) {
		t.Fatalf("expired recording was returned to the viewer: %v", err)
	}
	expired, err := db.Recording(ctx, editor, stale)
	ok(t, err)
	if expired.State != "hidden" || expired.Revision != 4 {
		t.Fatalf("expiry did not persist the lifecycle transition: %+v", expired)
	}
	t.Log("reopened a partial recording batch and resumed concurrently with one visibility change, one stale and one missing rejection")
}
