package storage

import (
    "context"
    "encoding/json"
    "errors"
    "strconv"
    "time"

    redis "github.com/redis/go-redis/v9"

    "ftvx/pkg/models"
)

type Redis struct { client *redis.Client }

func ConnectRedis(addr string) *Redis {
    return &Redis{client: redis.NewClient(&redis.Options{Addr: addr})}
}

func (r *Redis) Ping(ctx context.Context) error { return r.client.Ping(ctx).Err() }

func (r *Redis) SetNowNext(ctx context.Context, ch string, now models.TimelineEntry, next models.TimelineEntry) error {
    key := "nn:" + ch
    payload := map[string]any{
        "channel_id": ch,
        "now": map[string]any{"title": now.AssetID, "started_at": now.Start, "ends_at": now.Start.Add(time.Duration(now.DurationS) * time.Second)},
        "next": map[string]any{"title": next.AssetID, "starts_at": next.Start, "ends_at": next.Start.Add(time.Duration(next.DurationS) * time.Second)},
    }
    return r.client.Set(ctx, key, mustJSON(payload), 10*time.Second).Err()
}

func (r *Redis) GetNowNext(ctx context.Context, ch string) (models.NowNext, error) {
    var out models.NowNext
    bs, err := r.client.Get(ctx, "nn:"+ch).Bytes()
    if err != nil { return out, err }
    _ = json.Unmarshal(bs, &out)
    return out, nil
}

// Generic helpers for JSON cache
func (r *Redis) SetJSON(ctx context.Context, key string, value any, ttl time.Duration) error {
    return r.client.Set(ctx, key, mustJSON(value), ttl).Err()
}

func (r *Redis) GetString(ctx context.Context, key string) (string, error) {
    return r.client.Get(ctx, key).Result()
}

// SetString sets a string value with TTL
func (r *Redis) SetString(ctx context.Context, key string, value string, ttl time.Duration) error {
    return r.client.Set(ctx, key, value, ttl).Err()
}

// TryLock acquires a Redis lock key with TTL. Returns true if lock was acquired.
func (r *Redis) TryLock(ctx context.Context, key string, ttl time.Duration) (bool, error) {
    ok, err := r.client.SetNX(ctx, key, "1", ttl).Result()
    return ok, err
}

// PublishTimeline stores the timeline window for channel with short TTL
func (r *Redis) PublishTimeline(ctx context.Context, channelID string, payload any, ttl time.Duration) error {
    return r.client.Set(ctx, "tl:"+channelID, mustJSON(payload), ttl).Err()
}

// PublishScheduleChanged emits a Redis Stream entry for schedule changes per spec
// Stream: sched:changed
// Fields: channel_id, window_from, window_to, entries, op="replace_window"
func (r *Redis) PublishScheduleChanged(ctx context.Context, channelID string, windowFrom time.Time, windowTo time.Time, entries int) (string, error) {
    if r == nil || r.client == nil { return "", errors.New("redis not configured") }
    fields := map[string]any{
        "channel_id":  channelID,
        "window_from": windowFrom.Format(time.RFC3339),
        "window_to":   windowTo.Format(time.RFC3339),
        "entries":     strconv.Itoa(entries),
        "op":          "replace_window",
    }
    args := &redis.XAddArgs{Stream: "sched:changed", Values: fields}
    return r.client.XAdd(ctx, args).Result()
}

func mustJSON(v any) string { bs, _ := json.Marshal(v); return string(bs) }

// Queue operations for playout->packager
func (r *Redis) EnqueueSegment(ctx context.Context, seg models.SegmentDescriptor) error {
    key := "q:seg:" + seg.ChannelID
    return r.client.RPush(ctx, key, mustJSON(seg)).Err()
}

func (r *Redis) DequeueSegment(ctx context.Context, channelID string, timeout time.Duration) (models.SegmentDescriptor, error) {
    var seg models.SegmentDescriptor
    key := "q:seg:" + channelID
    res, err := r.client.BLPop(ctx, timeout, key).Result()
    if err != nil { return seg, err }
    if len(res) != 2 { return seg, redis.Nil }
    _ = json.Unmarshal([]byte(res[1]), &seg)
    return seg, nil
}

// ClipPlan stream helpers (Redis Streams)
// Key: clips:<channel_id>
func (r *Redis) PublishClip(ctx context.Context, ch string, item models.ClipItem) (string, error) {
    key := "clips:" + ch
    fields := map[string]any{
        "seq":        strconv.FormatInt(item.Seq, 10),
        "uri":        item.URI,
        "start_at":   item.StartAt.Format(time.RFC3339),
        "duration_s": strconv.Itoa(item.DurationS),
        "discontinuity": func() string { if item.Discontinuity { return "true" } else { return "false" } }(),
        "type":       item.Type,
    }
    args := &redis.XAddArgs{Stream: key, Values: fields, Approx: false}
    return r.client.XAdd(ctx, args).Result()
}

func (r *Redis) TrimClipStream(ctx context.Context, ch string, maxLen int64) error {
    key := "clips:" + ch
    return r.client.XTrimMaxLen(ctx, key, maxLen).Err()
}


