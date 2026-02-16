package storage

import (
    "context"
    "testing"
    "time"

    miniredis "github.com/alicebob/miniredis/v2"
    "ftvx/pkg/models"
)

func TestRedisNowNext_SetAndGet(t *testing.T) {
    mr := miniredis.RunT(t)
    r := ConnectRedis(mr.Addr())
    ctx := context.Background()
    now := models.TimelineEntry{AssetID: "a1", Start: time.Now().UTC(), DurationS: 20}
    next := models.TimelineEntry{AssetID: "a2", Start: now.Start.Add(20 * time.Second), DurationS: 20}
    if err := r.SetNowNext(ctx, "c1", now, next); err != nil { t.Fatalf("SetNowNext error: %v", err) }
    got, err := r.GetNowNext(ctx, "c1")
    if err != nil { t.Fatalf("GetNowNext error: %v", err) }
    if got.ChannelID != "" && got.ChannelID != "c1" {
        t.Fatalf("unexpected channel id: %q", got.ChannelID)
    }
}

func TestRedisQueue_EnqueueDequeue(t *testing.T) {
    mr := miniredis.RunT(t)
    r := ConnectRedis(mr.Addr())
    ctx := context.Background()
    seg := models.SegmentDescriptor{ChannelID: "c1", Sequence: 100, ProgramDateTime: time.Now().UTC(), DurationS: 2, Filename: "stream_00100.m4s"}
    if err := r.EnqueueSegment(ctx, seg); err != nil { t.Fatalf("enqueue: %v", err) }
    got, err := r.DequeueSegment(ctx, "c1", time.Second)
    if err != nil { t.Fatalf("dequeue: %v", err) }
    if got.Sequence != seg.Sequence || got.Filename != seg.Filename {
        t.Fatalf("unexpected seg: %+v", got)
    }
}


