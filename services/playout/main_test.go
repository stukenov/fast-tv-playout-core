package main

import (
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/models"
    "ftvx/pkg/logger"
)

// A-003/A-004: media-sequence monotonicity and discontinuity only at ad boundaries
func TestBuildPlan_SequenceMonotonic_And_DiscontinuityOnAdBoundaries(t *testing.T) {
    cfg := config.Config{SegmentSec: 2, PreloadWindowS: 20, SlateURI: "s3://bucket/slates/default_2s_cmaf.m3u8", ChannelID: "c1"}
    now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    entries := []models.TimelineEntry{
        {Type: "content", URI: "s3://bucket/content/a/index.m3u8", Start: now, DurationS: 10},
        {Type: "ad_break", Start: now.Add(10 * time.Second), DurationS: 6, Category: "midroll"},
        {Type: "content", URI: "s3://bucket/content/b/index.m3u8", Start: now.Add(16 * time.Second), DurationS: 6},
    }
    // Seed ads cache with exact 6s pod (3x2s)
    adsCache := map[time.Time][]models.AdItem{
        entries[1].Start: {
            {URI: "s3://bucket/ads/x/index.m3u8", DurationS: 2, Type: "partner"},
            {URI: "s3://bucket/ads/y/index.m3u8", DurationS: 2, Type: "partner"},
            {URI: "s3://bucket/ads/z/index.m3u8", DurationS: 2, Type: "partner"},
        },
    }

    // pass a configured logger
    plan := buildPlan(now, cfg, entries, adsCache, logger.Setup())
    if len(plan) == 0 { t.Fatalf("empty plan") }
    // Check sequence monotonicity
    for i := 1; i < len(plan); i++ {
        if !(plan[i].Seq == plan[i-1].Seq+1 || plan[i].Seq == plan[i-1].Seq) {
            // allow equal if PreloadWindowS not multiple of seg boundary; but generally expect +1 step per item
            t.Fatalf("media sequence not monotonic: %d -> %d", plan[i-1].Seq, plan[i].Seq)
        }
    }
    // Find indices around ad break boundary (10s and 16s)
    seg := time.Duration(cfg.SegmentSec) * time.Second
    // item at 10s should be ad with discontinuity
    idx10 := int(10 * time.Second / seg)
    if idx10 < 0 || idx10 >= len(plan) { t.Fatalf("index out of range for 10s") }
    if plan[idx10].Type != "ad" || !plan[idx10].Discontinuity {
        t.Fatalf("expected ad with discontinuity at 10s, got type=%s disc=%v", plan[idx10].Type, plan[idx10].Discontinuity)
    }
    // item at 16s (post-ad first content segment) should have discontinuity too
    idx16 := int(16 * time.Second / seg)
    if idx16 < 0 || idx16 >= len(plan) { t.Fatalf("index out of range for 16s") }
    if !plan[idx16].Discontinuity {
        t.Fatalf("expected discontinuity at first post-ad segment")
    }
    // No other discontinuities inside continuous content before/after
    for i := 0; i < len(plan); i++ {
        if i == idx10 || i == idx16 { continue }
        if plan[i].Type == "content" && plan[i].Discontinuity {
            t.Fatalf("unexpected discontinuity within content at i=%d", i)
        }
    }
}


