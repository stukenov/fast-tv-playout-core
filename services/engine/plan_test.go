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
    pods := newAdBroker(cfg, logger.Setup())
    pods.seed(entries[1].Start, []models.AdItem{
        {URI: "s3://bucket/ads/x/index.m3u8", DurationS: 2, Type: "partner"},
        {URI: "s3://bucket/ads/y/index.m3u8", DurationS: 2, Type: "partner"},
        {URI: "s3://bucket/ads/z/index.m3u8", DurationS: 2, Type: "partner"},
    })

    // pass a configured logger
    plan := buildPlan(now, cfg, entries, pods, logger.Setup())
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



// The packager picks the source segment by the offset, so it must count from the start of the entry,
// including when the entry began before "now" (a movie that is already running).
func TestBuildPlan_OffsetsCountFromTheStartOfTheEntry(t *testing.T) {
    cfg := config.Config{SegmentSec: 2, PreloadWindowS: 12, SlateURI: "s3://bucket/slate.m3u8", ChannelID: "c1"}
    movieStart := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    now := movieStart.Add(100 * time.Second)
    entries := []models.TimelineEntry{
        {Type: "content", URI: "s3://bucket/movie.m3u8", Start: movieStart, DurationS: 108},
        {Type: "ad_break", Start: movieStart.Add(108 * time.Second), DurationS: 6, Category: "midroll"},
    }
    pods := newAdBroker(cfg, logger.Setup())
    pods.seed(entries[1].Start, []models.AdItem{{URI: "s3://bucket/ad1.m3u8", DurationS: 4, Type: "partner"}})
    plan := buildPlan(now, cfg, entries, pods, logger.Setup())
    if len(plan) != 6 { t.Fatalf("expected 6 clips, got %d", len(plan)) }
    wantOffsets := []int{100, 102, 104, 106} // the movie
    for i, w := range wantOffsets {
        if plan[i].Type != "content" || plan[i].OffsetS != w {
            t.Fatalf("clip %d: type=%s offset=%d, want content at %d", i, plan[i].Type, plan[i].OffsetS, w)
        }
    }
    // ad: 4s creative = offsets 0 and 2, then 2s of slate padding
    if plan[4].Type != "ad" || plan[4].OffsetS != 0 || plan[5].Type != "ad" || plan[5].OffsetS != 2 {
        t.Fatalf("ad clips: %+v %+v", plan[4], plan[5])
    }
}

func TestBuildPlan_GapIsFilledWithSlate(t *testing.T) {
    cfg := config.Config{SegmentSec: 2, PreloadWindowS: 6, SlateURI: "s3://bucket/slate.m3u8", ChannelID: "c1"}
    now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    plan := buildPlan(now, cfg, nil, newAdBroker(cfg, logger.Setup()), logger.Setup())
    if len(plan) != 3 { t.Fatalf("expected 3 clips, got %d", len(plan)) }
    for _, c := range plan {
        if c.Type != "slate" || c.URI != cfg.SlateURI { t.Fatalf("unexpected clip %+v", c) }
    }
    if plan[1].OffsetS-plan[0].OffsetS != 2 { t.Fatal("slate offsets must advance by one segment per clip") }
}

func TestBuildPlan_PublishAheadLimitsThePlan(t *testing.T) {
    cfg := config.Config{SegmentSec: 2, PreloadWindowS: 180, PublishAheadS: 10, SlateURI: "s3://bucket/slate.m3u8", ChannelID: "c1"}
    now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    plan := buildPlan(now, cfg, nil, newAdBroker(cfg, logger.Setup()), logger.Setup())
    if len(plan) != 5 { t.Fatalf("expected 5 clips (10s ahead), got %d", len(plan)) }
}

// If the ad server is down, a break that is still far away is not turned into slate:
// the plan stops before it, so a later tick can try again. A break that is about to air gets slate, once.
func TestBuildPlan_AdServerDown(t *testing.T) {
    cfg := config.Config{SegmentSec: 2, PreloadWindowS: 30, PublishAheadS: 30, LiveLeadS: 6, AdsURL: "http://127.0.0.1:1/v1/ads/pod", SlateURI: "s3://bucket/slate.m3u8", ChannelID: "c1"}
    now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

    far := []models.TimelineEntry{
        {Type: "content", URI: "s3://bucket/m.m3u8", Start: now, DurationS: 20},
        {Type: "ad_break", Start: now.Add(20 * time.Second), DurationS: 6, Category: "midroll"},
    }
    pods := newAdBroker(cfg, logger.Setup())
    plan := buildPlan(now, cfg, far, pods, logger.Setup())
    if len(plan) != 10 { t.Fatalf("the plan must stop at the ad break (10 clips), got %d", len(plan)) }
    pods.mu.Lock()
    _, known := pods.pods[far[1].Start]
    pods.mu.Unlock()
    if !known { t.Fatal("the request must have been started in the background") }

    near := []models.TimelineEntry{{Type: "ad_break", Start: now, DurationS: 6, Category: "midroll"}}
    plan = buildPlan(now, cfg, near, newAdBroker(cfg, logger.Setup()), logger.Setup())
    if len(plan) < 3 { t.Fatalf("an imminent break must be filled, got %d clips", len(plan)) }
    for i := 0; i < 3; i++ {
        if plan[i].Type != "slate" { t.Fatalf("clip %d must be slate, got %s", i, plan[i].Type) }
    }
}
