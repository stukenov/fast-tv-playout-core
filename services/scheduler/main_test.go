package main

import (
    "testing"
    "time"

    "ftvx/pkg/models"
)

// A-001 Scheduler-quantization: all start/duration align to 2s
func TestQuantizeAndValidate_QuantizesStartAndDuration(t *testing.T) {
    seg := 2
    now := time.Now().UTC().Truncate(time.Second)
    in := []models.TimelineEntry{
        {Type: "content", AssetID: "m1", Start: now.Add(1300 * time.Millisecond), DurationS: 5},
        {Type: "content", AssetID: "m2", Start: now.Add(7 * time.Second), DurationS: 3},
    }
    out, err := quantizeAndValidate(in, seg)
    if err != nil { t.Fatalf("unexpected error: %v", err) }
    if len(out) != 2 { t.Fatalf("len=%d, want 2", len(out)) }
    // Start should be truncated to nearest 2s quantum (down)
    if out[0].Start.Unix()%int64(seg) != 0 {
        t.Fatalf("start not aligned: %s", out[0].Start)
    }
    if out[1].Start.Unix()%int64(seg) != 0 {
        t.Fatalf("start not aligned: %s", out[1].Start)
    }
    // Durations should be floored to multiple of seg and positive
    if out[0].DurationS%seg != 0 || out[0].DurationS <= 0 {
        t.Fatalf("duration not quantized: %d", out[0].DurationS)
    }
    if out[1].DurationS%seg != 0 || out[1].DurationS <= 0 {
        t.Fatalf("duration not quantized: %d", out[1].DurationS)
    }
}

// A-002 Scheduler-without gaps: overlap detection returns error
func TestQuantizeAndValidate_OverlapError(t *testing.T) {
    seg := 2
    base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
    // Two entries overlapping after quantization
    in := []models.TimelineEntry{
        {Type: "content", AssetID: "m1", Start: base, DurationS: 4},
        {Type: "content", AssetID: "m2", Start: base.Add(3 * time.Second), DurationS: 4},
    }
    if _, err := quantizeAndValidate(in, seg); err == nil {
        t.Fatalf("expected overlap error, got nil")
    }
}


