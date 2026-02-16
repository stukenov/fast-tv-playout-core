package main

import (
    "context"
    "errors"
    "fmt"
    "net/http"
    "os"
    "sort"
    "strconv"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"

    "ftvx/pkg/config"
    "ftvx/pkg/metrics"
    "ftvx/pkg/httpx"
    "ftvx/pkg/logger"
    "ftvx/pkg/models"
    "ftvx/pkg/storage"
)

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    ctx := context.Background()
    pg, err := storage.ConnectPostgres(ctx, cfg.PostgresDSN)
    if err != nil { lg.Fatal().Err(err).Msg("pg connect") }
    defer pg.Close()
    rd := storage.ConnectRedis(cfg.RedisAddr)

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(15*time.Second))

    // Metrics endpoint
    r.Handle("/metrics", metrics.Handler())

    // Health and readiness
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
        if err := rd.Ping(r.Context()); err != nil { httpx.WriteProblem(w, 503, "redis", err.Error()); return }
        if _, err := pg.HorizonCoveredUntil(r.Context(), cfg.ChannelID); err != nil { httpx.WriteProblem(w, 503, "postgres", err.Error()); return }
        w.WriteHeader(200)
    })

    // Simple API-key auth for mutating routes
    auth := func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if cfg.APIKey != "" && r.Header.Get("X-API-Key") != cfg.APIKey {
                httpx.WriteProblem(w, 401, "unauthorized", "invalid api key")
                return
            }
            next.ServeHTTP(w, r)
        })
    }

    r.Route("/v1", func(r chi.Router) {
        r.Group(func(r chi.Router) {
            r.Use(auth)
            r.Post("/schedule/{channel_id}", func(w http.ResponseWriter, r *http.Request) {
                channelID := chi.URLParam(r, "channel_id")
                var req models.ScheduleRequest
                if err := httpx.DecodeJSON(r, &req); err != nil { httpx.WriteProblem(w, 400, "bad json", err.Error()); return }
                entries, nowItem, nextItem, err := handleSchedule(r.Context(), pg, rd, channelID, req, cfg)
                if err != nil {
                    var pe *problemError
                    if errors.As(err, &pe) {
                        httpx.WriteProblem(w, pe.status, pe.title, pe.detail)
                    } else {
                        httpx.WriteProblem(w, 500, "internal", err.Error())
                    }
                    return
                }
                // Build response
                horizon, _ := pg.HorizonCoveredUntil(r.Context(), channelID)
                resp := map[string]any{
                    "channel_id": channelID,
                    "written": map[string]any{"from": req.From, "to": req.To},
                    "entries_count": len(entries),
                    "horizon_covered_until": horizon,
                }
                if nowItem != nil && nextItem != nil {
                    resp["now_next"] = map[string]any{
                        "now":  nowItem,
                        "next": nextItem,
                    }
                }
                httpx.WriteJSON(w, 200, resp)
            })
        })

        // Preview GET (read-only)
        r.Get("/schedule/{channel_id}", func(w http.ResponseWriter, r *http.Request) {
            channelID := chi.URLParam(r, "channel_id")
            qs := r.URL.Query()
            fromStr := qs.Get("from")
            toStr := qs.Get("to")
            if fromStr == "" || toStr == "" { httpx.WriteProblem(w, 400, "missing", "from/to required"); return }
            from, err1 := time.Parse(time.RFC3339, fromStr)
            to, err2 := time.Parse(time.RFC3339, toStr)
            if err1 != nil || err2 != nil { httpx.WriteProblem(w, 400, "bad_time", "RFC3339 required"); return }
            entries, err := pg.GetTimelineRange(r.Context(), channelID, from.UTC(), to.UTC())
            if err != nil { httpx.WriteProblem(w, 500, "db", err.Error()); return }
            httpx.WriteJSON(w, 200, map[string]any{"channel_id": channelID, "from": from, "to": to, "entries": entries})
        })
    })

    // Background rolling refresh
    go func() {
        ticker := time.NewTicker(time.Duration(getenvInt("SCHED_REFRESH_INTERVAL_SEC", 60)) * time.Second)
        defer ticker.Stop()
        for {
            <-ticker.C
            targetH := getenvInt("SCHED_TARGET_HORIZON_HOURS", cfg.SchedTargetHorizonHours)
            minH := getenvInt("SCHED_MIN_HORIZON_HOURS", cfg.SchedMinHorizonHours)
            seg := getenvInt("SCHED_SEGMENT_SEC", cfg.SchedSegmentSec)
            channelID := cfg.ChannelID
            // compute current horizon and extend if needed
            now := time.Now().UTC()
            covered, err := pg.HorizonCoveredUntil(ctx, channelID)
            if err != nil { lg.Error().Err(err).Msg("horizon"); continue }
            needUntil := now.Add(time.Duration(targetH) * time.Hour)
            metrics.SchedulerHorizonSeconds.WithLabelValues(channelID).Set(covered.Sub(now).Seconds())
            if covered.After(needUntil) { continue }
            // Use simple rule: use library tag from env LIBRARY or "prime"
            defaultRules := &models.ScheduleRules{SegmentSec: seg}
            defaultRules.AdBreaks.DefaultDurationS = 120
            defaultRules.AdBreaks.AdsEvery = "12m"
            defaultRules.Blocks = []struct{ Range string "json:\"range\""; Library string "json:\"library\"" }{{Range: "00:00-24:00", Library: getenv("LIBRARY", "prime")}}
            from := now
            if covered.After(now) { from = covered }
            to := now.Add(time.Duration(minH) * time.Hour)
            if to.Before(from) { to = from }
            req := models.ScheduleRequest{From: from, To: to, Rules: defaultRules, Mode: "replace"}
            start := time.Now()
            if _, _, _, err := handleSchedule(ctx, pg, rd, channelID, req, cfg); err != nil {
                lg.Error().Err(err).Str("channel", channelID).Msg("rolling refresh")
                metrics.SchedulerGenerationDurationSeconds.WithLabelValues(channelID, "error").Observe(time.Since(start).Seconds())
            } else {
                metrics.SchedulerGenerationDurationSeconds.WithLabelValues(channelID, "ok").Observe(time.Since(start).Seconds())
                metrics.SchedulerLastSuccessTimestampSeconds.WithLabelValues(channelID).Set(float64(time.Now().Unix()))
            }
            // Trim past entries
            cutoff := now.Add(-time.Duration(getenvInt("SCHED_TRIM_PAST_HOURS", cfg.SchedTrimPastHours)) * time.Hour)
            if _, err := pg.TrimTimelinePast(ctx, channelID, cutoff); err != nil {
                lg.Error().Err(err).Str("channel", channelID).Msg("trim")
            }
        }
    }()

    addr := ":8084"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    _ = http.ListenAndServe(addr, r)
}

type problemError struct{ status int; title string; detail string }

func (e *problemError) Error() string { return fmt.Sprintf("%s: %s", e.title, e.detail) }

func handleSchedule(ctx context.Context, pg *storage.Postgres, rd *storage.Redis, channelID string, req models.ScheduleRequest, cfg config.Config) ([]models.TimelineEntry, *models.TimelineEntry, *models.TimelineEntry, error) {
    if req.Mode != "" && req.Mode != "replace" { return nil, nil, nil, &problemError{status: 400, title: "validation", detail: "only mode=replace supported"} }
    if channelID == "" { return nil, nil, nil, &problemError{status: 400, title: "validation", detail: "channel_id required"} }
    if req.From.IsZero() || req.To.IsZero() || !req.To.After(req.From) { return nil, nil, nil, &problemError{status: 400, title: "validation", detail: "invalid from/to"} }

    // Lock
    got, err := rd.TryLock(ctx, "sched:lock:"+channelID, 300*time.Second)
    if err != nil { return nil, nil, nil, err }
    if !got {
        metrics.SchedulerLockContentionTotal.WithLabelValues(channelID).Inc()
        return nil, nil, nil, &problemError{status: 503, title: "locked", detail: "scheduler lock contention"}
    }

    seg := getenvInt("SCHED_SEGMENT_SEC", cfg.SegmentSec)

    // If explicit entries provided (variant A): trust and quantize
    var entries []models.TimelineEntry
    if len(req.Entries) > 0 {
        entries, err = quantizeAndValidate(req.Entries, seg)
        if err != nil { return nil, nil, nil, &problemError{status: 400, title: "validation", detail: err.Error()} }
    } else {
        rules := req.Rules
        if rules == nil { return nil, nil, nil, &problemError{status: 400, title: "validation", detail: "rules required in MVP"} }
        entries, err = generateWindow(ctx, pg, *rules, req.From, req.To)
        if err != nil {
            var pe *problemError
            if errors.As(err, &pe) { return nil, nil, nil, pe }
            return nil, nil, nil, err
        }
    }

    // Replace in DB
    if err := pg.ReplaceTimelineRange(ctx, channelID, req.From.UTC(), req.To.UTC(), entries); err != nil { return nil, nil, nil, err }

    // Compute Now/Next from just-written window if spans now; otherwise from DB
    now := time.Now().UTC()
    var nowItem, nextItem *models.TimelineEntry
    for i := range entries {
        e := entries[i]
        if e.Type != "content" { continue }
        if !now.Before(e.Start) && now.Before(e.Start.Add(time.Duration(e.DurationS)*time.Second)) { nowItem = &e }
        if nowItem != nil && e.Start.After(now) { nextItem = &e; break }
        if nowItem == nil && e.Start.After(now) { nextItem = &e; break }
    }
    if nowItem != nil && nextItem != nil { _ = rd.SetNowNext(ctx, channelID, *nowItem, *nextItem) }

    // Publish timeline window ahead 60-180 minutes for cache
    windowTo := req.From.Add(120 * time.Minute)
    if req.To.Before(windowTo) { windowTo = req.To }
    // select only future entries
    var pub []models.TimelineEntry
    for _, e := range entries { if e.Start.After(now.Add(-1*time.Minute)) && e.Start.Before(windowTo) { pub = append(pub, e) } }
    payload := map[string]any{"channel_id": channelID, "generated_at": time.Now().UTC(), "valid_until": windowTo, "entries": pub}
    _ = rd.PublishTimeline(ctx, channelID, payload, 120*time.Second)

    // Metrics: gap count (slate insertions)
    gaps := 0
    for _, e := range entries { if e.Type == "slate" { gaps++ } }
    if gaps > 0 { metrics.SchedulerGapCountTotal.WithLabelValues(channelID).Add(float64(gaps)) }

    return entries, nowItem, nextItem, nil
}

func quantizeAndValidate(in []models.TimelineEntry, seg int) ([]models.TimelineEntry, error) {
    q := make([]models.TimelineEntry, 0, len(in))
    quantum := time.Duration(seg) * time.Second
    for _, e := range in {
        e.Start = e.Start.UTC().Truncate(quantum)
        if e.DurationS%seg != 0 { e.DurationS = (e.DurationS/seg)*seg }
        if e.DurationS <= 0 { return nil, fmt.Errorf("non-positive duration after quantize") }
        q = append(q, e)
    }
    // Ensure non-overlap by sorting and checking
    sort.Slice(q, func(i, j int) bool { return q[i].Start.Before(q[j].Start) })
    for i := 1; i < len(q); i++ {
        prev := q[i-1]
        prevEnd := prev.Start.Add(time.Duration(prev.DurationS) * time.Second).UTC()
        if prevEnd.After(q[i].Start.UTC()) {
            return nil, fmt.Errorf("overlap at %s", q[i].Start.Format(time.RFC3339))
        }
    }
    return q, nil
}

func generateWindow(ctx context.Context, pg *storage.Postgres, rules models.ScheduleRules, from time.Time, to time.Time) ([]models.TimelineEntry, error) {
    if !to.After(from) { return nil, &problemError{status: 400, title: "validation", detail: "to must be after from"} }
    seg := rules.SegmentSec
    if seg <= 0 { seg = 2 }
    quantum := time.Duration(seg) * time.Second
    from = from.UTC().Truncate(quantum)
    to = to.UTC().Truncate(quantum)
    if rules.AdBreaks.DefaultDurationS%seg != 0 { rules.AdBreaks.DefaultDurationS = (rules.AdBreaks.DefaultDurationS/seg)*seg }
    adsEvery, err := time.ParseDuration(rules.AdBreaks.AdsEvery)
    if err != nil { return nil, &problemError{status: 400, title: "validation", detail: "bad ads_every"} }

    // Build blocks timeline [from,to)
    type block struct{ start time.Time; end time.Time; library string }
    blocks := []block{}
    cur := from
    for cur.Before(to) {
        // For the day of cur, compute blocks
        dayStart := time.Date(cur.Year(), cur.Month(), cur.Day(), 0, 0, 0, 0, time.UTC)
        dayEnd := dayStart.Add(24 * time.Hour)
        for _, b := range rules.Blocks {
            st, en, err := parseRange(b.Range, dayStart)
            if err != nil { return nil, &problemError{status: 400, title: "validation", detail: "bad block range"} }
            if en.After(dayEnd) { en = dayEnd }
            // intersect with [from,to)
            if en.After(from) && st.Before(to) {
                s := maxTime(st, from)
                e := minTime(en, to)
                if e.After(s) { blocks = append(blocks, block{start: s, end: e, library: b.Library}) }
            }
        }
        cur = dayEnd
    }
    sort.Slice(blocks, func(i, j int) bool { return blocks[i].start.Before(blocks[j].start) })

    entries := []models.TimelineEntry{}
    for _, bl := range blocks {
        // Asset pool
        assets, err := pg.SelectAssetsByTag(ctx, bl.library, seg)
        if err != nil { return nil, err }
        if len(assets) == 0 { return nil, &problemError{status: 422, title: "no_assets", detail: "asset pool empty for library: " + bl.library} }

        // Ad grid points within block
        adpoints := []time.Time{}
        for t := bl.start.Add(adsEvery); t.Before(bl.end) || t.Equal(bl.end); t = t.Add(adsEvery) {
            adpoints = append(adpoints, t.Truncate(quantum))
        }

        // Fill content avoiding intersections with adpoints
        tcur := bl.start
        poolIdx := 0
        nextAdIdx := 0
        placedFirstContent := false
        for tcur.Before(bl.end) {
            // If it's time for ad_break
            if nextAdIdx < len(adpoints) && !tcur.Before(adpoints[nextAdIdx]) {
                // insert ad_break
                entries = append(entries, models.TimelineEntry{Type: "ad_break", Start: tcur, DurationS: rules.AdBreaks.DefaultDurationS, Category: "midroll"})
                tcur = tcur.Add(time.Duration(rules.AdBreaks.DefaultDurationS) * time.Second)
                nextAdIdx++
                continue
            }

            // Remaining before next boundary
            boundary := bl.end
            if nextAdIdx < len(adpoints) { boundary = adpoints[nextAdIdx] }
            remaining := int(boundary.Sub(tcur).Seconds())
            if remaining <= 0 { break }

            // Choose asset that fits
            chosen := -1
            // try rotation starting from poolIdx, wrap once
            for scan := 0; scan < len(assets); scan++ {
                cand := assets[(poolIdx+scan)%len(assets)]
                d := cand.DurationS
                if d%seg != 0 { d = (d/seg)*seg }
                if d > 0 && d <= remaining { chosen = (poolIdx+scan)%len(assets); break }
            }

            if chosen >= 0 {
                a := assets[chosen]
                d := a.DurationS
                if d%seg != 0 { d = (d/seg)*seg }
                entries = append(entries, models.TimelineEntry{Type: "content", AssetID: a.ID, URI: a.URI, Start: tcur, DurationS: d})
                tcur = tcur.Add(time.Duration(d) * time.Second)
                poolIdx = (chosen + 1) % len(assets)
                placedFirstContent = true
                continue
            }

            // No asset fits. If remainder < 30s and slate configured -> slate
            if remaining < 30 && rules.SlateURI != nil && *rules.SlateURI != "" {
                d := (remaining/seg)*seg
                if d > 0 {
                    entries = append(entries, models.TimelineEntry{Type: "slate", URI: *rules.SlateURI, Start: tcur, DurationS: d})
                    tcur = tcur.Add(time.Duration(d) * time.Second)
                    continue
                }
            }

            // Otherwise try to advance to boundary by inserting smallest asset <= remaining (already tried). As fallback, if slate exists use it even if >=30s.
            if rules.SlateURI != nil && *rules.SlateURI != "" {
                d := (remaining/seg)*seg
                if d > 0 {
                    entries = append(entries, models.TimelineEntry{Type: "slate", URI: *rules.SlateURI, Start: tcur, DurationS: d})
                    tcur = tcur.Add(time.Duration(d) * time.Second)
                    continue
                }
            }

            // If cannot place anything, break to avoid infinite loop
            break
        }
        _ = placedFirstContent // bumper optional in MVP
    }

    // Final overlap check
    entries, err = quantizeAndValidate(entries, seg)
    if err != nil { return nil, &problemError{status: 409, title: "conflict", detail: err.Error()} }
    return entries, nil
}

func parseRange(s string, dayStart time.Time) (time.Time, time.Time, error) {
    // format HH:MM-HH:MM in local dayStart (UTC)
    if len(s) != 11 || s[5] != '-' { return time.Time{}, time.Time{}, fmt.Errorf("bad range") }
    h1, _ := strconv.Atoi(s[0:2])
    m1, _ := strconv.Atoi(s[3:5])
    h2, _ := strconv.Atoi(s[6:8])
    m2, _ := strconv.Atoi(s[9:11])
    st := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), h1, m1, 0, 0, time.UTC)
    en := time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), h2, m2, 0, 0, time.UTC)
    if !en.After(st) { en = en.Add(24 * time.Hour) }
    return st, en, nil
}

func minTime(a, b time.Time) time.Time { if a.Before(b) { return a } else { return b } }
func maxTime(a, b time.Time) time.Time { if a.After(b) { return a } else { return b } }

func getenvInt(k string, def int) int { if v := os.Getenv(k); v != "" { if n, err := strconv.Atoi(v); err == nil { return n } } ; return def }
func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
