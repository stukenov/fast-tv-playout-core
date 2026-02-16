//go:build integration

package storage

import (
    "context"
    "os"
    "testing"
    "time"

    "ftvx/pkg/models"
)

func TestPostgres_TimelineUpsertAndQuery(t *testing.T) {
    dsn := os.Getenv("POSTGRES_DSN")
    if dsn == "" {
        t.Skip("POSTGRES_DSN not set")
    }
    ctx := context.Background()
    pg, err := ConnectPostgres(ctx, dsn)
    if err != nil { t.Fatalf("connect: %v", err) }
    defer pg.Close()
    chID := "c_test"
    // seed channel required by FK only at app level; table does not require FK for timeline
    now := time.Now().UTC().Truncate(2 * time.Second)
    entries := []models.TimelineEntry{
        {Type: "content", AssetID: "m1", Start: now, DurationS: 20},
        {Type: "ad_break", Start: now.Add(20 * time.Second), DurationS: 10},
    }
    if err := pg.UpsertTimeline(ctx, chID, entries); err != nil { t.Fatalf("upsert: %v", err) }
    got, err := pg.GetTimelineRange(ctx, chID, now.Add(-time.Second), now.Add(60*time.Second))
    if err != nil { t.Fatalf("query: %v", err) }
    if len(got) < 2 { t.Fatalf("expected >=2 entries, got %d", len(got)) }
}


