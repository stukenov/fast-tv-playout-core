package main

import (
    "context"
    "errors"
    "log"
    "net/http"
    "os"
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"

    "ftvx/pkg/config"
    "ftvx/pkg/httpx"
    "ftvx/pkg/logger"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
    "ftvx/pkg/storage"
)

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    ctx := context.Background()
    // storage
    var pg *storage.Postgres
    if cfg.PostgresDSN != "" {
        p, err := storage.ConnectPostgres(ctx, cfg.PostgresDSN)
        if err != nil { lg.Fatal().Err(err).Msg("postgres connect") }
        pg = p
        defer pg.Close()
    }
    var rd *storage.Redis
    if cfg.RedisAddr != "" { rd = storage.ConnectRedis(cfg.RedisAddr) }
    // expose to handlers
    pgStore = pg
    rdStore = rd

    r := chi.NewRouter()
    r.Use(middleware.RequestID)
    r.Use(middleware.RealIP)
    r.Use(middleware.Recoverer)
    r.Use(middleware.Timeout(30 * time.Second))

    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", handleReadyz)
    r.Handle("/metrics", metrics.Handler())

    r.Route("/v1", func(r chi.Router) {
        r.Use(apiKeyMiddleware(cfg.APIKey))
        r.Post("/assets", handlePostAsset)
        r.Get("/assets/{asset_id}", handleGetAsset)
        r.Post("/channels", handlePostChannel)
        r.Get("/channels/{channel_id}", handleGetChannel)
        r.Get("/channels", handleListChannels)
        r.Post("/schedule/{channel_id}:replace", handleScheduleReplace)
        r.Get("/schedule/{channel_id}", handleGetSchedule)
        r.Get("/status/{channel_id}", handleGetStatus)
    })

    addr := ":8081"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    lg.Info().Str("addr", addr).Msg("control-api listening")
    srv := &http.Server{Addr: addr, Handler: r}
    if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
        log.Fatal(err)
    }
}

// In-memory storage for MVP; replace with Postgres/Redis later
var (
    assets   = map[string]models.Asset{}
    channels = map[string]models.Channel{}
    timelines = map[string][]models.TimelineEntry{}
    pgStore *storage.Postgres
    rdStore *storage.Redis
)

func handleReadyz(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
    defer cancel()
    if pgStore != nil {
        if err := pgStore.Ping(ctx); err != nil { httpx.WriteProblem(w, 503, "postgres not ready", err.Error()); return }
    }
    if rdStore != nil {
        if err := rdStore.Ping(ctx); err != nil { httpx.WriteProblem(w, 503, "redis not ready", err.Error()); return }
    }
    w.WriteHeader(200)
}

func handlePostAsset(w http.ResponseWriter, r *http.Request) {
    var in struct{
        URI string `json:"uri"`
        DurationS int `json:"duration_s"`
        Tags []string `json:"tags"`
        Meta map[string]any `json:"meta"`
    }
    if err := httpx.DecodeJSON(r, &in); err != nil { httpx.WriteProblem(w, 400, "Validation error", "invalid json"); return }
    if len(in.URI) < 5 || in.DurationS < 1 { httpx.WriteProblem(w, 400, "Validation error", "uri or duration invalid"); return }
    // check unique by URI
    if pgStore != nil {
        if ex, err := pgStore.GetAssetByURI(r.Context(), in.URI); err == nil && ex.ID != "" {
            httpx.WriteProblem(w, 409, "Conflict", "asset uri already exists")
            return
        }
    }
    a := models.Asset{ID: generateID("m"), URI: in.URI, DurationS: in.DurationS, Tags: in.Tags, Meta: in.Meta, CreatedAt: time.Now().UTC()}
    if pgStore != nil {
        if err := pgStore.InsertAsset(r.Context(), a); err != nil { httpx.WriteProblem(w, 500, "db error", err.Error()); return }
    }
    httpx.WriteJSON(w, 201, map[string]string{"asset_id": a.ID})
}

func handleGetAsset(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "asset_id")
    if id == "" { httpx.WriteProblem(w, 400, "Validation error", "asset_id required"); return }
    if pgStore == nil { httpx.WriteProblem(w, 404, "Not Found", "asset not found"); return }
    a, err := pgStore.GetAsset(r.Context(), id)
    if err != nil { httpx.WriteProblem(w, 404, "Not Found", "asset not found"); return }
    httpx.WriteJSON(w, 200, a)
}

func handlePostChannel(w http.ResponseWriter, r *http.Request) {
    var in struct{
        ID string `json:"id"`
        Name string `json:"name"`
        SegmentSec int `json:"segment_sec"`
        Ladder []int `json:"ladder"`
    }
    if err := httpx.DecodeJSON(r, &in); err != nil { httpx.WriteProblem(w, 400, "Validation error", "invalid json"); return }
    if in.ID == "" || len(in.Name) == 0 { httpx.WriteProblem(w, 400, "Validation error", "missing id or name"); return }
    if in.SegmentSec == 0 { in.SegmentSec = 2 }
    // convert ladder to []string for storage model
    ladderStr := make([]string, 0, len(in.Ladder))
    for _, v := range in.Ladder { ladderStr = append(ladderStr, strconv.Itoa(v)) }
    c := models.Channel{ID: in.ID, Name: in.Name, SegmentSec: in.SegmentSec, Ladder: ladderStr, CreatedAt: time.Now().UTC()}
    if pgStore != nil {
        if err := pgStore.InsertChannel(r.Context(), c); err != nil {
            httpx.WriteProblem(w, 409, "Conflict", err.Error())
            return
        }
    }
    httpx.WriteJSON(w, 201, map[string]string{"status": "created"})
}

func handleGetChannel(w http.ResponseWriter, r *http.Request) {
    id := chi.URLParam(r, "channel_id")
    if id == "" { httpx.WriteProblem(w, 400, "Validation error", "channel_id required"); return }
    if pgStore == nil { httpx.WriteProblem(w, 404, "Not Found", "channel not found"); return }
    c, err := pgStore.GetChannel(r.Context(), id)
    if err != nil { httpx.WriteProblem(w, 404, "Not Found", "channel not found"); return }
    httpx.WriteJSON(w, 200, c)
}

func handleListChannels(w http.ResponseWriter, r *http.Request) {
    if pgStore == nil { httpx.WriteJSON(w, 200, []any{}); return }
    items, err := pgStore.ListChannels(r.Context(), 100)
    if err != nil { httpx.WriteProblem(w, 500, "db error", err.Error()); return }
    httpx.WriteJSON(w, 200, items)
}

func handleScheduleReplace(w http.ResponseWriter, r *http.Request) {
    channelID := chi.URLParam(r, "channel_id")
    if channelID == "" { httpx.WriteProblem(w, 400, "Validation error", "channel_id required"); return }
    // ensure channel exists
    if pgStore != nil {
        if _, err := pgStore.GetChannel(r.Context(), channelID); err != nil { httpx.WriteProblem(w, 404, "Not Found", "channel not found"); return }
    }
    var in struct{
        Window struct{ From string `json:"from"`; To string `json:"to"` } `json:"window"`
        Entries []struct{
            Type string `json:"type"`
            AssetID string `json:"asset_id"`
            Start string `json:"start"`
            Duration int `json:"duration"`
            Category string `json:"category"`
            SCTE35 string `json:"scte35"`
        } `json:"entries"`
    }
    if err := httpx.DecodeJSON(r, &in); err != nil { httpx.WriteProblem(w, 400, "Validation error", "invalid json"); return }
    if in.Window.From == "" || in.Window.To == "" || len(in.Entries) == 0 { httpx.WriteProblem(w, 400, "Validation error", "missing window or entries"); return }
    from, err1 := time.Parse(time.RFC3339, in.Window.From)
    to, err2 := time.Parse(time.RFC3339, in.Window.To)
    if err1 != nil || err2 != nil || !to.After(from) { httpx.WriteProblem(w, 400, "Validation error", "invalid window"); return }
    if to.Sub(from) > 12*time.Hour { httpx.WriteProblem(w, 400, "Validation error", "window too wide"); return }
    // load channel to get segment_sec (default 2)
    seg := 2
    if pgStore != nil { if ch, err := pgStore.GetChannel(r.Context(), channelID); err == nil && ch.SegmentSec > 0 { seg = ch.SegmentSec } }
    // build entries
    entries := make([]models.TimelineEntry, 0, len(in.Entries))
    for i, e := range in.Entries {
        if e.Type == "content" || e.Type == "bumper" || e.Type == "slate" {
            if strings.TrimSpace(e.AssetID) == "" { httpx.WriteProblem(w, 400, "Validation error", "asset_id required for non-ad_break"); return }
        } else if e.Type == "ad_break" {
            if e.AssetID != "" { httpx.WriteProblem(w, 400, "Validation error", "asset_id must be empty for ad_break"); return }
        } else {
            httpx.WriteProblem(w, 400, "Validation error", "invalid type"); return }
        if e.Duration <= 0 || e.Duration%seg != 0 { httpx.WriteProblem(w, 400, "Validation error", "duration not multiple of segment_sec"); return }
        st, err := time.Parse(time.RFC3339, e.Start)
        if err != nil || st.Before(from) || !st.Before(to) { httpx.WriteProblem(w, 400, "Validation error", "start outside window"); return }
        if int(st.Unix())%seg != 0 { httpx.WriteProblem(w, 400, "Validation error", "start not aligned to segment_sec"); return }
        entries = append(entries, models.TimelineEntry{Type: e.Type, AssetID: e.AssetID, Start: st, DurationS: e.Duration, Category: e.Category, SCTE35: e.SCTE35})
        _ = i // keep index for future detailed error paths
    }
    // sort by start and check overlaps
    sort.Slice(entries, func(i, j int) bool { return entries[i].Start.Before(entries[j].Start) })
    if overlap := detectOverlap(entries); overlap != "" { httpx.WriteProblem(w, 400, "Validation error", overlap); return }
    // write in one transaction
    if pgStore != nil {
        if err := pgStore.ReplaceTimelineRange(r.Context(), channelID, from, to, entries); err != nil { httpx.WriteProblem(w, 500, "db error", err.Error()); return }
    }
    // publish redis stream (best-effort)
    if rdStore != nil { _, _ = rdStore.PublishScheduleChanged(r.Context(), channelID, from, to, len(entries)) }
    // optional: cache key tl:<channel_id>
    if rdStore != nil { _ = rdStore.PublishTimeline(r.Context(), channelID, map[string]any{"from": in.Window.From, "to": in.Window.To, "entries": entries}, 0) }
    httpx.WriteJSON(w, 200, map[string]any{"replaced": len(entries), "warnings": []any{}})
}

func handleGetSchedule(w http.ResponseWriter, r *http.Request) {
    channelID := chi.URLParam(r, "channel_id")
    if channelID == "" { httpx.WriteProblem(w, 400, "Validation error", "channel_id required"); return }
    q := r.URL.Query()
    fromS := q.Get("from")
    toS := q.Get("to")
    var from, to time.Time
    var err error
    if fromS == "" {
        httpx.WriteProblem(w, 400, "Validation error", "from required"); return
    }
    from, err = time.Parse(time.RFC3339, fromS)
    if err != nil { httpx.WriteProblem(w, 400, "Validation error", "invalid from"); return }
    if toS == "" { to = from.Add(6 * time.Hour) } else { to, err = time.Parse(time.RFC3339, toS); if err != nil { httpx.WriteProblem(w, 400, "Validation error", "invalid to"); return } }
    if to.Sub(from) > 12*time.Hour { httpx.WriteProblem(w, 400, "Validation error", "window too wide"); return }
    if pgStore == nil { httpx.WriteJSON(w, 200, []any{}); return }
    items, err := pgStore.GetTimelineRange(r.Context(), channelID, from, to)
    if err != nil { httpx.WriteProblem(w, 500, "db error", err.Error()); return }
    httpx.WriteJSON(w, 200, items)
}

func handleGetStatus(w http.ResponseWriter, r *http.Request) {
    channelID := chi.URLParam(r, "channel_id")
    if channelID == "" { httpx.WriteProblem(w, 400, "Validation error", "channel_id required"); return }
    var lastWrite time.Time
    // Cheap heuristic: use Redis key TTL or simply now
    if rdStore != nil {
        if s, err := rdStore.GetString(r.Context(), "tl:"+channelID); err == nil && s != "" {
            lastWrite = time.Now().UTC()
        }
    }
    // Range and count from DB (last 6h)
    from := time.Now().UTC()
    to := from.Add(6 * time.Hour)
    if pgStore != nil {
        items, err := pgStore.GetTimelineRange(r.Context(), channelID, from, to)
        if err != nil && !errors.Is(err, context.DeadlineExceeded) { httpx.WriteProblem(w, 500, "db error", err.Error()); return }
        httpx.WriteJSON(w, 200, map[string]any{
            "channel_id": channelID,
            "timeline": map[string]any{
                "last_write_at": lastWrite.Format(time.RFC3339),
                "range": map[string]string{"from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339)},
                "count": len(items),
            },
        })
        return
    }
    httpx.WriteJSON(w, 200, map[string]any{"channel_id": channelID})
}

func storeNowNext(ctx context.Context, channelID string, now, next models.TimelineEntry) error {
    if rdStore == nil { return nil }
    return rdStore.SetNowNext(ctx, channelID, now, next)
}

func generateID(prefix string) string {
    return prefix + "_" + time.Now().UTC().Format("20060102T150405.000000000")
}

func apiKeyMiddleware(key string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if key == "" { next.ServeHTTP(w, r); return }
            if r.Header.Get("X-API-Key") != key { httpx.WriteProblem(w, 401, "unauthorized", "invalid api key"); return }
            next.ServeHTTP(w, r)
        })
    }
}

func detectOverlap(entries []models.TimelineEntry) string {
    if len(entries) < 2 { return "" }
    for i := 1; i < len(entries); i++ {
        prevEnd := entries[i-1].Start.Add(time.Duration(entries[i-1].DurationS) * time.Second)
        if !entries[i].Start.After(prevEnd) && !entries[i].Start.Equal(prevEnd) {
            return "overlap detected"
        }
    }
    return ""
}


