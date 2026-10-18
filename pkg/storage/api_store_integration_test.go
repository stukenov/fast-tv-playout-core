//go:build integration

package storage

import (
    "context"
    "errors"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "ftvx/pkg/models"
)

func freshChannel(t *testing.T, pg *Postgres, id string) {
    t.Helper()
    ctx := context.Background()
    _ = pg.DeleteChannel(ctx, id)
    if err := pg.InsertChannelChecked(ctx, models.Channel{ID: id, Name: id, SegmentSec: 1, Ladder: []string{}, CreatedAt: time.Now()}); err != nil { t.Fatal(err) }
}

func entry(start time.Time, secs int) models.TimelineEntry {
    return models.TimelineEntry{Type: "slate", Start: start, DurationS: secs}
}

func TestApply_VersionAndIfMatch(t *testing.T) {
    pg := connect(t)
    freshChannel(t, pg, "ap1")
    ctx := context.Background()
    now := time.Now().UTC().Truncate(time.Second)
    req := ApplyRequest{ChannelID: "ap1", From: now, To: now.Add(time.Minute), Entries: []models.TimelineEntry{entry(now, 10)}}
    res, err := pg.ApplySchedule(ctx, req)
    if err != nil || res.Version != 1 { t.Fatalf("first apply: %+v %v", res, err) }
    zero := int64(0)
    req.IfMatch = &zero
    if _, err := pg.ApplySchedule(ctx, req); !errors.As(err, new(VersionMismatch)) { t.Fatalf("a stale If-Match must be refused, got %v", err) }
    one := int64(1)
    req.IfMatch = &one
    if res, err := pg.ApplySchedule(ctx, req); err != nil || res.Version != 2 { t.Fatalf("matching If-Match: %+v %v", res, err) }
    ch, _ := pg.GetChannelChecked(ctx, "ap1")
    if ch.ScheduleVersion != 2 { t.Fatalf("version %d", ch.ScheduleVersion) }
}

func TestApply_IdempotentUnderConcurrency(t *testing.T) {
    pg := connect(t)
    freshChannel(t, pg, "ap2")
    now := time.Now().UTC().Truncate(time.Second)
    var applied, replayed atomic.Int32
    var wg sync.WaitGroup
    for i := 0; i < 8; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            res, err := pg.ApplySchedule(context.Background(), ApplyRequest{ChannelID: "ap2", From: now, To: now.Add(time.Minute),
                Entries: []models.TimelineEntry{entry(now, 10)}, IdemKey: "k1", BodyHash: "h1",
                // the answer is built inside the transaction: holding it open makes the other seven requests really overlap with it
                Response: func(v int64) []byte { time.Sleep(60 * time.Millisecond); return []byte(`{"version":` + string(rune('0'+v)) + `}`) }})
            if err != nil { t.Error(err); return }
            if res.Replayed { replayed.Add(1) } else { applied.Add(1) }
        }()
    }
    wg.Wait()
    if applied.Load() != 1 || replayed.Load() != 7 { t.Fatalf("applied %d, replayed %d: exactly one request may be applied", applied.Load(), replayed.Load()) }
    ch, _ := pg.GetChannelChecked(context.Background(), "ap2")
    if ch.ScheduleVersion != 1 { t.Fatalf("the schedule changed %d times", ch.ScheduleVersion) }
    // the same key with another body is an error, not a silent second change
    _, err := pg.ApplySchedule(context.Background(), ApplyRequest{ChannelID: "ap2", From: now, To: now.Add(time.Minute), IdemKey: "k1", BodyHash: "other"})
    if !errors.Is(err, ErrIdempotencyConflict) { t.Fatalf("got %v", err) }
}

func TestApply_UnknownChannelAndUnknownAsset(t *testing.T) {
    pg := connect(t)
    freshChannel(t, pg, "ap3")
    now := time.Now().UTC().Truncate(time.Second)
    if _, err := pg.ApplySchedule(context.Background(), ApplyRequest{ChannelID: "nope", From: now, To: now.Add(time.Minute)}); !errors.Is(err, ErrNotFound) { t.Fatalf("got %v", err) }
    bad := models.TimelineEntry{Type: "content", AssetID: "no-such-asset", Start: now, DurationS: 5}
    if _, err := pg.ApplySchedule(context.Background(), ApplyRequest{ChannelID: "ap3", From: now, To: now.Add(time.Minute), Entries: []models.TimelineEntry{bad}}); !errors.Is(err, ErrUnknownReference) { t.Fatalf("got %v", err) }
    // a failed apply changes nothing, not even the version
    ch, _ := pg.GetChannelChecked(context.Background(), "ap3")
    if ch.ScheduleVersion != 0 { t.Fatalf("version %d after a failed apply", ch.ScheduleVersion) }
}

func TestOffsetIsStoredAndReturned(t *testing.T) {
    pg := connect(t)
    freshChannel(t, pg, "ap4")
    now := time.Now().UTC().Truncate(time.Second)
    e := entry(now, 10)
    e.OffsetS = 42
    if _, err := pg.ApplySchedule(context.Background(), ApplyRequest{ChannelID: "ap4", From: now, To: now.Add(time.Minute), Entries: []models.TimelineEntry{e}}); err != nil { t.Fatal(err) }
    got, err := pg.GetTimelineRange(context.Background(), "ap4", now, now.Add(time.Minute))
    if err != nil || len(got) != 1 || got[0].OffsetS != 42 { t.Fatalf("%v %+v", err, got) }
}

func TestChannelsAndAssets_CRUD(t *testing.T) {
    pg := connect(t)
    ctx := context.Background()
    freshChannel(t, pg, "crud1")
    name := "Renamed"
    c, err := pg.UpdateChannel(ctx, "crud1", &name, []string{"720p"})
    if err != nil || c.Name != "Renamed" || len(c.Ladder) != 1 { t.Fatalf("%+v %v", c, err) }
    c, err = pg.UpdateChannel(ctx, "crud1", nil, nil)
    if err != nil || c.Name != "Renamed" || len(c.Ladder) != 1 { t.Fatalf("an update without fields keeps them: %+v %v", c, err) }
    if _, err := pg.UpdateChannel(ctx, "missing", &name, nil); !errors.Is(err, ErrNotFound) { t.Fatalf("got %v", err) }

    a := models.Asset{ID: "crud_a1", URI: "http://x/crud_a1.m3u8", DurationS: 10, Tags: []string{"crudtag"}, CreatedAt: time.Now()}
    _ = pg.DeleteAsset(ctx, a.ID)
    if err := pg.InsertAssetChecked(ctx, a); err != nil { t.Fatal(err) }
    if err := pg.InsertAssetChecked(ctx, a); !errors.Is(err, ErrConflict) { t.Fatalf("a duplicate must be ErrConflict, got %v", err) }
    list, err := pg.ListAssets(ctx, "crudtag", "", 10)
    if err != nil || len(list) != 1 || list[0].ID != "crud_a1" { t.Fatalf("%v %+v", err, list) }
    now := time.Now().UTC().Truncate(time.Second)
    use := models.TimelineEntry{Type: "content", AssetID: a.ID, Start: now, DurationS: 5}
    if _, err := pg.ApplySchedule(ctx, ApplyRequest{ChannelID: "crud1", From: now, To: now.Add(time.Minute), Entries: []models.TimelineEntry{use}}); err != nil { t.Fatal(err) }
    if err := pg.DeleteAsset(ctx, a.ID); !errors.Is(err, ErrAssetInUse) { t.Fatalf("an asset in the schedule must not be deleted, got %v", err) }
    if err := pg.DeleteChannel(ctx, "crud1"); err != nil { t.Fatal(err) }
    if _, err := pg.GetChannelChecked(ctx, "crud1"); !errors.Is(err, ErrNotFound) { t.Fatalf("got %v", err) }
    if err := pg.DeleteAsset(ctx, a.ID); err != nil { t.Fatalf("after the channel (and its schedule) is gone the asset can go: %v", err) }
    if err := pg.DeleteChannel(ctx, "crud1"); !errors.Is(err, ErrNotFound) { t.Fatalf("got %v", err) }
}

func TestChannelPagination(t *testing.T) {
    pg := connect(t)
    ctx := context.Background()
    for _, id := range []string{"pg_a", "pg_b", "pg_c"} { freshChannel(t, pg, id) }
    first, err := pg.ListChannelsPage(ctx, "pg_", 2)
    if err != nil || len(first) != 2 || first[0].ID != "pg_a" || first[1].ID != "pg_b" { t.Fatalf("%v %+v", err, first) }
    second, err := pg.ListChannelsPage(ctx, first[1].ID, 2)
    if err != nil || len(second) < 1 || second[0].ID != "pg_c" { t.Fatalf("%v %+v", err, second) }
}
