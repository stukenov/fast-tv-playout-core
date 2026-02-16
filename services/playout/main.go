package main

import (
    "context"
    "encoding/json"
    "fmt"
    "net/http"
    "os"
    "path"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/rs/zerolog"

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
    rd := storage.ConnectRedis(cfg.RedisAddr)
    pg, _ := storage.ConnectPostgres(context.Background(), cfg.PostgresDSN)

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(15*time.Second))
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
        ctx, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
        defer cancel()
        // ping Redis as readiness check (S3 skip in MVP)
        if err := rd.Ping(ctx); err != nil {
            httpx.WriteProblem(w, 503, "unready", "redis unavailable")
            return
        }
        w.WriteHeader(200)
    })
    r.Handle("/metrics", metrics.Handler())
    // status endpoint
    st := &engineState{}
    r.Get("/v1/channels/{id}/status", func(w http.ResponseWriter, r *http.Request) {
        _ = json.NewEncoder(w).Encode(models.ChannelStatus{
            NowSeq:         st.nowSeq,
            WindowLen:      st.windowLen,
            LastTimelineAt: st.lastTimelineAt,
            AdState:        st.adState,
            Gaps:           st.gaps,
        })
    })

    // playout engine loop
    go runEngine(context.Background(), lg, cfg, rd, pg, st)

    addr := ":8085"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    _ = http.ListenAndServe(addr, r)
}

// --- Engine implementation ---

type engineState struct {
    lastSeqPublished int64
    nowSeq           int64
    windowLen        int
    lastTimelineAt   time.Time
    adState          string
    gaps             int
}

func runEngine(ctx context.Context, lg zerolog.Logger, cfg config.Config, rd *storage.Redis, pg *storage.Postgres, st *engineState) {
    ticker := time.NewTicker(time.Duration(cfg.TickMs) * time.Millisecond)
    defer ticker.Stop()
    adsCache := map[time.Time][]models.AdItem{}
    for {
        select {
        case <-ctx.Done():
            return
        case t := <-ticker.C:
            started := time.Now()
            now := t.UTC().Truncate(time.Duration(cfg.SegmentSec) * time.Second)
            st.nowSeq = quantSeq(now, cfg.SegmentSec)
            // fetch timeline window
            var entries []models.TimelineEntry
            var err error
            windowDur := time.Duration(cfg.PreloadWindowS) * time.Second
            if pg != nil {
                entries, err = pg.GetTimelineRange(ctx, cfg.ChannelID, now, now.Add(windowDur))
                if err == nil && len(entries) > 0 {
                    st.lastTimelineAt = time.Now()
                }
            }
            // build window plan
            plan := buildPlan(now, cfg, entries, adsCache, lg)
            st.windowLen = len(plan)
            metrics.PlayoutClipsAhead.WithLabelValues(cfg.ChannelID).Set(float64(len(plan)))
            // publish new items to Redis Stream (idempotent by seq)
            for _, it := range plan {
                if it.Seq <= st.lastSeqPublished { continue }
                if _, err := rd.PublishClip(ctx, cfg.ChannelID, it); err != nil {
                    lg.Error().Err(err).Str("channel", cfg.ChannelID).Int64("seq", it.Seq).Msg("publish clip")
                    continue
                }
                st.lastSeqPublished = it.Seq
            }
            // also enqueue current segment for existing packager demo
            curFile := "stream_" + fmt.Sprintf("%05d", st.nowSeq%100000) + ".m4s"
            seg := models.SegmentDescriptor{ChannelID: cfg.ChannelID, Sequence: st.nowSeq, ProgramDateTime: now, DurationS: cfg.SegmentSec, Filename: curFile}
            if err := rd.EnqueueSegment(ctx, seg); err != nil { lg.Error().Err(err).Msg("enqueue seg") }
            metrics.PlayoutLoopLatencyMs.WithLabelValues(cfg.ChannelID).Set(float64(time.Since(started).Milliseconds()))
        }
    }
}

func buildPlan(now time.Time, cfg config.Config, entries []models.TimelineEntry, adsCache map[time.Time][]models.AdItem, lg zerolog.Logger) []models.ClipItem {
    seg := time.Duration(cfg.SegmentSec) * time.Second
    end := now.Add(time.Duration(cfg.PreloadWindowS) * time.Second)
    var out []models.ClipItem
    // helper to find entry covering a time
    findEntry := func(ts time.Time) (models.TimelineEntry, bool) {
        for _, e := range entries {
            st := e.Start
            en := e.Start.Add(time.Duration(e.DurationS) * time.Second)
            if !ts.Before(st) && ts.Before(en) { return e, true }
        }
        return models.TimelineEntry{}, false
    }
    prevType := ""
    for ts := now; !ts.After(end.Add(-seg)); ts = ts.Add(seg) {
        seq := quantSeq(ts, cfg.SegmentSec)
        e, ok := findEntry(ts)
        var item models.ClipItem
        item.Seq = seq
        item.StartAt = ts
        item.DurationS = cfg.SegmentSec
        if !ok {
            // fallback slate
            item.Type = "slate"
            item.URI = cfg.SlateURI
        } else {
            switch e.Type {
            case "content", "bumper":
                item.Type = map[string]string{"content":"content","bumper":"bumper"}[e.Type]
                item.URI = e.URI
            case "slate":
                item.Type = "slate"
                item.URI = cfg.SlateURI
            case "ad_break":
                // resolve ad pod for break start
                pod, has := adsCache[e.Start]
                if !has {
                    metrics.PlayoutAdRequestsTotal.WithLabelValues(cfg.ChannelID).Inc()
                    podResp, err := callAds(cfg.AdsURL, cfg.ChannelID, e, cfg.APIKey)
                    if err != nil {
                        metrics.PlayoutAdErrorsTotal.WithLabelValues(cfg.ChannelID, "request_error").Inc()
                        pod = nil
                    } else {
                        pod = podResp.Items
                    }
                    adsCache[e.Start] = pod
                }
                // build quantum list for full break
                breakItems := buildAdBreakQuanta(e, pod, cfg)
                // pick the item matching current ts
                idx := int(ts.Sub(e.Start) / seg)
                if idx >= 0 && idx < len(breakItems) {
                    item = breakItems[idx]
                    item.Seq = seq
                    item.StartAt = ts
                    item.DurationS = cfg.SegmentSec
                } else {
                    // safety fallback
                    item.Type = "slate"
                    item.URI = cfg.SlateURI
                }
            default:
                item.Type = "slate"
                item.URI = cfg.SlateURI
            }
        }
        // discontinuity markers at ad boundaries
        if item.Type == "ad" && prevType != "ad" {
            item.Discontinuity = true
        }
        if item.Type != "ad" && prevType == "ad" {
            // mark discontinuity on first post-ad item
            item.Discontinuity = true
        }
        out = append(out, item)
        prevType = item.Type
    }
    return out
}

func buildAdBreakQuanta(br models.TimelineEntry, items []models.AdItem, cfg config.Config) []models.ClipItem {
    seg := cfg.SegmentSec
    quantaTotal := br.DurationS / seg
    var out []models.ClipItem
    // Expand ad items into quanta
    for _, it := range items {
        n := it.DurationS / seg
        for i := 0; i < n; i++ {
            out = append(out, models.ClipItem{URI: it.URI, DurationS: seg, Type: "ad"})
        }
    }
    // pad with slate if not enough
    for len(out) < quantaTotal {
        out = append(out, models.ClipItem{URI: cfg.SlateURI, DurationS: seg, Type: "slate"})
    }
    // trim if too many
    if len(out) > quantaTotal {
        out = out[:quantaTotal]
    }
    // mark discontinuity on first item of break; exit will be marked by caller on next clip
    if len(out) > 0 {
        out[0].Discontinuity = true
    }
    return out
}

func quantSeq(ts time.Time, segSec int) int64 {
    return ts.Unix() / int64(segSec)
}

func callAds(url, channelID string, br models.TimelineEntry, apiKey string) (models.AdPodResponse, error) {
    cli := http.Client{Timeout: 2 * time.Second}
    reqBody := models.AdPodRequest{ChannelID: channelID, Break: models.AdBreak{Start: br.Start, Duration: br.DurationS, Category: br.Category}}
    bs, _ := json.Marshal(reqBody)
    req, _ := http.NewRequest("POST", url, strings.NewReader(string(bs)))
    req.Header.Set("Content-Type", "application/json")
    if apiKey != "" { req.Header.Set("X-API-Key", apiKey) }
    resp, err := cli.Do(req)
    if err != nil { return models.AdPodResponse{}, err }
    defer resp.Body.Close()
    if resp.StatusCode != 200 {
        return models.AdPodResponse{}, fmt.Errorf("ads status %d", resp.StatusCode)
    }
    var out models.AdPodResponse
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return models.AdPodResponse{}, err }
    // normalize URIs if needed
    for i := range out.Items {
        out.Items[i].URI = normalizeURI(out.Items[i].URI)
    }
    return out, nil
}

func normalizeURI(u string) string {
    // allow plain s3:// or http(s) URLs; for relative paths, prepend s3://bucket placeholder
    if strings.HasPrefix(u, "s3://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") { return u }
    return path.Clean("s3://" + u)
}


