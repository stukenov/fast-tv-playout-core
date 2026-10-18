//go:build integration

package storage

import (
    "context"
    "os"
    "sync"
    "testing"
    "time"

    "ftvx/pkg/models"
)

// Run with: POSTGRES_DSN=postgres://... go test -tags integration ./pkg/storage/

func connect(t *testing.T) *Postgres {
    t.Helper()
    dsn := os.Getenv("POSTGRES_DSN")
    if dsn == "" { t.Skip("POSTGRES_DSN not set") }
    pg, err := ConnectPostgres(context.Background(), dsn)
    if err != nil { t.Fatalf("connect: %v", err) }
    t.Cleanup(pg.Close)
    return pg
}

func seed(t *testing.T, pg *Postgres, channel string) {
    t.Helper()
    ctx := context.Background()
    // entries of earlier tests reference the same assets
    _, _ = pg.pool.Exec(ctx, `DELETE FROM timeline_entry WHERE channel_id=$1 OR asset_id IN ('it_m1','it_m2')`, channel)
    _, _ = pg.pool.Exec(ctx, `DELETE FROM channel WHERE id=$1`, channel)
    _, _ = pg.pool.Exec(ctx, `DELETE FROM asset WHERE id IN ('it_m1','it_m2')`)
    if err := pg.InsertChannel(ctx, models.Channel{ID: channel, Name: "it", SegmentSec: 2, Ladder: []string{}, CreatedAt: time.Now()}); err != nil { t.Fatalf("channel: %v", err) }
    for _, a := range []models.Asset{
        {ID: "it_m1", URI: "http://origin/vod/it_m1/index.m3u8", DurationS: 600, Tags: []string{"it"}, CreatedAt: time.Now()},
        {ID: "it_m2", URI: "http://origin/vod/it_m2/index.m3u8", DurationS: 600, Tags: []string{"it"}, CreatedAt: time.Now()},
    } {
        if err := pg.InsertAsset(ctx, a); err != nil { t.Fatalf("asset: %v", err) }
    }
}

// Several services start together and all run migrations.
func TestPostgres_ConcurrentMigrationsAreSafe(t *testing.T) {
    dsn := os.Getenv("POSTGRES_DSN")
    if dsn == "" { t.Skip("POSTGRES_DSN not set") }
    var wg sync.WaitGroup
    errs := make(chan error, 6)
    for i := 0; i < 6; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            pg, err := ConnectPostgres(context.Background(), dsn)
            if err == nil { pg.Close() }
            errs <- err
        }()
    }
    wg.Wait()
    close(errs)
    for err := range errs { if err != nil { t.Fatalf("concurrent migrate: %v", err) } }
}

func TestPostgres_AdBreakWithoutAssetIsStored(t *testing.T) {
    pg := connect(t)
    seed(t, pg, "it1")
    ctx := context.Background()
    now := time.Now().UTC().Truncate(2 * time.Second)
    entries := []models.TimelineEntry{
        {Type: "content", AssetID: "it_m1", Start: now, DurationS: 20},
        {Type: "ad_break", Start: now.Add(20 * time.Second), DurationS: 10, Category: "midroll"},
    }
    if err := pg.ReplaceTimelineRange(ctx, "it1", now, now.Add(time.Hour), entries); err != nil { t.Fatalf("replace: %v", err) }
    if err := pg.UpsertTimeline(ctx, "it1", entries); err != nil { t.Fatalf("upsert: %v", err) }
    got, err := pg.GetTimelineRange(ctx, "it1", now, now.Add(time.Minute))
    if err != nil || len(got) != 2 { t.Fatalf("query: %v len=%d", err, len(got)) }
    if got[1].Type != "ad_break" || got[1].AssetID != "" { t.Fatalf("ad break: %+v", got[1]) }
}

func TestPostgres_EntryURIFallsBackToAssetURI(t *testing.T) {
    pg := connect(t)
    seed(t, pg, "it2")
    ctx := context.Background()
    now := time.Now().UTC().Truncate(2 * time.Second)
    entries := []models.TimelineEntry{
        {Type: "content", AssetID: "it_m1", Start: now, DurationS: 20},
        {Type: "content", AssetID: "it_m2", URI: "http://elsewhere/m2.m3u8", Start: now.Add(20 * time.Second), DurationS: 20},
    }
    if err := pg.ReplaceTimelineRange(ctx, "it2", now, now.Add(time.Hour), entries); err != nil { t.Fatal(err) }
    got, err := pg.GetTimelineRange(ctx, "it2", now, now.Add(time.Minute))
    if err != nil || len(got) != 2 { t.Fatalf("query: %v len=%d", err, len(got)) }
    if got[0].URI != "http://origin/vod/it_m1/index.m3u8" { t.Fatalf("asset URI expected, got %q", got[0].URI) }
    if got[1].URI != "http://elsewhere/m2.m3u8" { t.Fatalf("explicit URI must win, got %q", got[1].URI) }
}

// A movie that started 10 minutes ago is still on air and must be found by a window that starts now.
func TestPostgres_RunningEntryIsFoundByLaterWindow(t *testing.T) {
    pg := connect(t)
    seed(t, pg, "it3")
    ctx := context.Background()
    now := time.Now().UTC().Truncate(2 * time.Second)
    started := now.Add(-10 * time.Minute)
    entries := []models.TimelineEntry{
        {Type: "content", AssetID: "it_m1", Start: started, DurationS: 3600},
        {Type: "content", AssetID: "it_m2", Start: started.Add(-2 * time.Hour), DurationS: 60}, // long over
    }
    if err := pg.ReplaceTimelineRange(ctx, "it3", started.Add(-3*time.Hour), now.Add(time.Hour), entries); err != nil { t.Fatal(err) }
    got, err := pg.GetTimelineRange(ctx, "it3", now, now.Add(3*time.Minute))
    if err != nil { t.Fatal(err) }
    if len(got) != 1 || got[0].AssetID != "it_m1" { t.Fatalf("expected only the running movie, got %+v", got) }
}

func TestPostgres_HorizonAndTrim(t *testing.T) {
    pg := connect(t)
    seed(t, pg, "it4")
    ctx := context.Background()
    now := time.Now().UTC().Truncate(2 * time.Second)
    old := now.Add(-48 * time.Hour)
    entries := []models.TimelineEntry{
        {Type: "content", AssetID: "it_m1", Start: old, DurationS: 60},
        {Type: "content", AssetID: "it_m2", Start: now, DurationS: 120},
    }
    if err := pg.ReplaceTimelineRange(ctx, "it4", old, now.Add(time.Hour), entries); err != nil { t.Fatal(err) }
    h, err := pg.HorizonCoveredUntil(ctx, "it4")
    if err != nil || !h.Equal(now.Add(120*time.Second)) { t.Fatalf("horizon=%v err=%v", h, err) }
    n, err := pg.TrimTimelinePast(ctx, "it4", now.Add(-24*time.Hour))
    if err != nil || n != 1 { t.Fatalf("trim removed %d err=%v", n, err) }
}

// Regression: an asset posted without meta and tags used to fail with a NOT NULL violation.
func TestPostgres_AssetWithoutMetaAndTagsIsStored(t *testing.T) {
    pg := connect(t)
    ctx := context.Background()
    _, _ = pg.pool.Exec(ctx, `DELETE FROM asset WHERE id='it_bare'`)
    a := models.Asset{ID: "it_bare", URI: "http://origin/vod/it_bare/index.m3u8", DurationS: 30, CreatedAt: time.Now()}
    if err := pg.InsertAsset(ctx, a); err != nil { t.Fatalf("insert: %v", err) }
    got, err := pg.GetAssetByURI(ctx, a.URI)
    if err != nil || got.ID != "it_bare" { t.Fatalf("get: %v %+v", err, got) }
}
