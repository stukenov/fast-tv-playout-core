package main

import (
    "bytes"
    "compress/gzip"
    "context"
    "encoding/json"
    "io"
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

type timelineSource string

const (
    srcRedis timelineSource = "redis"
    srcHTTP  timelineSource = "http"
)

var (
    rd                 *storage.Redis
    apiKey             string
    timelineMode       timelineSource
    timelineHTTPBase   string
    defaultWindowHours int
    xmltvEnabled       bool
    xmltvEvery         time.Duration
    xmltvOutput        string // file://path
    xmltvPublic        bool
)

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    if cfg.RedisAddr != "" { rd = storage.ConnectRedis(cfg.RedisAddr) }

    // EPG-specific env (simple local parsing to avoid changing shared config)
    apiKey = getenv("API_KEY", "")
    timelineMode = timelineSource(getenv("EPG_TIMELINE_SOURCE", "redis"))
    timelineHTTPBase = getenv("EPG_TIMELINE_HTTP", "http://scheduler:8084/v1/schedule")
    defaultWindowHours = atoi(getenv("DEFAULT_WINDOW_HOURS", "24"), 24)
    xmltvEnabled = getenv("XMLTV_EXPORT_ENABLED", "true") == "true"
    if d, err := time.ParseDuration(getenv("XMLTV_EXPORT_EVERY", "15m")); err == nil { xmltvEvery = d } else { xmltvEvery = 15 * time.Minute }
    xmltvOutput = getenv("XMLTV_OUTPUT", "file:///var/epg/xmltv")
    xmltvPublic = getenv("XMLTV_PUBLIC", "true") == "true"

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(15*time.Second))

    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Handle("/metrics", metrics.Handler())

    // API key guard for private endpoints
    auth := func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
            if apiKey == "" { next.ServeHTTP(w, r); return }
            if r.Header.Get("X-API-Key") != apiKey { httpx.WriteProblem(w, 401, "unauthorized", "invalid api key"); return }
            next.ServeHTTP(w, r)
        })
    }

    r.Route("/v1/epg", func(r chi.Router) {
        // Now/Next
        r.With(auth).Get("/{channel_id}/now-next", handleNowNext)
        // Schedule JSON
        r.With(auth).Get("/{channel_id}/schedule", handleScheduleJSON)
        // XMLTV pull (public optional)
        r.Group(func(r chi.Router) {
            if !xmltvPublic { r.Use(auth) }
            r.Get("/xmltv", handleXMLTV)
        })
    })

    // Background XMLTV exporter (file:// only for MVP)
    if xmltvEnabled && strings.HasPrefix(xmltvOutput, "file://") {
        go xmltvExporterLoop()
    }

    addr := ":8082"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    lg.Info().Str("addr", addr).Msg("epg listening")
    _ = http.ListenAndServe(addr, r)
}

func handleNowNext(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    ch := chi.URLParam(r, "channel_id")
    metrics.EPGNowNextRequestsTotal.WithLabelValues(ch).Inc()
    // Prefer Redis cache
    if rd != nil {
        if nn, err := rd.GetNowNext(r.Context(), ch); err == nil && (nn.Now.StartsAt.After(time.Time{}) || nn.Now.StartsAt2.After(time.Time{})) {
            // Age metric
            st := nn.Now.StartsAt
            if st.IsZero() { st = nn.Now.StartsAt2 }
            if !st.IsZero() { metrics.EPGNowNextAgeSeconds.WithLabelValues(ch).Set(time.Since(st).Seconds()) }
            metrics.EPGNowNextLatencySeconds.WithLabelValues(ch).Observe(time.Since(start).Seconds())
            httpx.WriteJSON(w, 200, map[string]any{
                "channel_id": ch,
                "server_time": time.Now().UTC(),
                "now":  nn.Now,
                "next": nn.Next,
            })
            return
        }
    }
    // Fallback: compute from timeline source
    now, next, err := computeNowNext(r.Context(), ch)
    if err != nil {
        httpx.WriteProblem(w, 503, "timeline_unavailable", err.Error())
        return
    }
    if rd != nil && now != nil && next != nil {
        _ = rd.SetNowNext(r.Context(), ch, *now, *next)
    }
    resp := map[string]any{
        "channel_id": ch,
        "server_time": time.Now().UTC(),
        "now":  toNowNextItem(now),
        "next": toNowNextItem(next),
    }
    if t := resp["now"].(models.NowNextItem).StartsAt; !t.IsZero() { metrics.EPGNowNextAgeSeconds.WithLabelValues(ch).Set(time.Since(t).Seconds()) }
    metrics.EPGNowNextLatencySeconds.WithLabelValues(ch).Observe(time.Since(start).Seconds())
    httpx.WriteJSON(w, 200, resp)
}

func handleScheduleJSON(w http.ResponseWriter, r *http.Request) {
    ch := chi.URLParam(r, "channel_id")
    qs := r.URL.Query()
    fromStr := qs.Get("from")
    toStr := qs.Get("to")
    var from, to time.Time
    var err error
    if fromStr == "" || toStr == "" {
        from = time.Now().UTC()
        to = from.Add(time.Duration(defaultWindowHours) * time.Hour)
    } else {
        from, err = time.Parse(time.RFC3339, fromStr); if err != nil { httpx.WriteProblem(w, 400, "bad_time", "from must be RFC3339"); return }
        to, err = time.Parse(time.RFC3339, toStr); if err != nil { httpx.WriteProblem(w, 400, "bad_time", "to must be RFC3339"); return }
    }
    if !to.After(from) { httpx.WriteProblem(w, 400, "bad_range", "to must be after from"); return }
    // Cache layer
    cacheKey := "epg:json:" + ch + ":" + strconv.FormatInt(from.Unix(), 10) + ":" + strconv.FormatInt(to.Unix(), 10)
    if rd != nil {
        if s, err := rd.GetString(r.Context(), cacheKey); err == nil && s != "" {
            metrics.EPGScheduleCacheHitsTotal.WithLabelValues(ch).Inc()
            w.Header().Set("Content-Type", "application/json")
            _, _ = io.WriteString(w, s)
            return
        }
    }
    metrics.EPGScheduleCacheMissesTotal.WithLabelValues(ch).Inc()
    programs, err := buildPrograms(r.Context(), ch, from, to)
    if err != nil { httpx.WriteProblem(w, 503, "timeline_unavailable", err.Error()); return }
    bs, _ := json.Marshal(programs)
    if rd != nil { _ = rd.SetJSON(r.Context(), cacheKey, programs, 5*time.Minute) }
    w.Header().Set("Content-Type", "application/json")
    _, _ = w.Write(bs)
}

func handleXMLTV(w http.ResponseWriter, r *http.Request) {
    qs := r.URL.Query()
    channels := strings.Split(qs.Get("channels"), ",")
    hours := atoi(getenv("XMLTV_HOURS_DEFAULT", qs.Get("hours")), defaultWindowHours)
    if len(channels) == 1 && channels[0] == "" { httpx.WriteProblem(w, 400, "missing", "channels required"); return }

    from := time.Now().UTC()
    to := from.Add(time.Duration(min(hours, 48)) * time.Hour)

    var buf bytes.Buffer
    buf.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?><tv>")
    for _, ch := range channels {
        ch = strings.TrimSpace(ch)
        if ch == "" { continue }
        // Channel header
        buf.WriteString("<channel id=\"" + ch + "\">")
        buf.WriteString("<display-name>" + ch + "</display-name>")
        buf.WriteString("</channel>")
        // Programs
        progs, err := buildPrograms(r.Context(), ch, from, to)
        if err != nil {
            metrics.EPGTimelinePullErrorsTotal.WithLabelValues(string(timelineMode), ch, "fetch").Inc()
            continue
        }
        for _, p := range progs {
            start := p.Start.Format("20060102 150405 +0000")
            stop := p.End.Format("20060102 150405 +0000")
            buf.WriteString("<programme start=\"" + start + "\" stop=\"" + stop + "\" channel=\"" + ch + "\">")
            buf.WriteString("<title>" + xmlEscape(p.Title) + "</title>")
            buf.WriteString("<desc></desc>")
            buf.WriteString("</programme>")
        }
    }
    buf.WriteString("</tv>")

    w.Header().Set("Content-Type", "application/xml; charset=utf-8")
    if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
        w.Header().Set("Content-Encoding", "gzip")
        gz := gzip.NewWriter(w)
        _, _ = gz.Write(buf.Bytes())
        _ = gz.Close()
        return
    }
    _, _ = w.Write(buf.Bytes())
}

// Background exporter writing combined XMLTV file under file:// path
func xmltvExporterLoop() {
    // Only supports single channel via CHANNEL_ID in MVP; extend to multiple later
    out := strings.TrimPrefix(xmltvOutput, "file://")
    _ = os.MkdirAll(out, 0o755)
    ticker := time.NewTicker(xmltvEvery)
    defer ticker.Stop()
    for {
        <-ticker.C
        channels := getenv("XMLTV_CHANNELS", getenv("CHANNEL_ID", "c1"))
        chs := strings.Split(channels, ",")
        req, _ := http.NewRequest("GET", "/ignored", nil)
        // Build XML
        var buf bytes.Buffer
        buf.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?><tv>")
        from := time.Now().UTC()
        to := from.Add(time.Duration(defaultWindowHours) * time.Hour)
        for _, ch := range chs {
            ch = strings.TrimSpace(ch)
            if ch == "" { continue }
            buf.WriteString("<channel id=\"" + ch + "\"><display-name>" + ch + "</display-name></channel>")
            progs, err := buildPrograms(context.Background(), ch, from, to)
            if err != nil { metrics.EPGXMLTVPublishErrorsTotal.WithLabelValues("file").Inc(); continue }
            for _, p := range progs {
                start := p.Start.Format("20060102 150405 +0000")
                stop := p.End.Format("20060102 150405 +0000")
                buf.WriteString("<programme start=\"" + start + "\" stop=\"" + stop + "\" channel=\"" + ch + "\"><title>" + xmlEscape(p.Title) + "</title><desc></desc></programme>")
            }
        }
        buf.WriteString("</tv>")
        // Publish
        ts := time.Now().UTC().Format("20060102_1504")
        fn := out + string(os.PathSeparator) + "xmltv_" + ts + ".xml"
        tmp := fn + ".tmp"
        start := time.Now()
        if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err == nil {
            if err := os.Rename(tmp, fn); err != nil { metrics.EPGXMLTVPublishErrorsTotal.WithLabelValues("file").Inc() }
        } else {
            metrics.EPGXMLTVPublishErrorsTotal.WithLabelValues("file").Inc()
        }
        metrics.EPGXMLTVGenerateSeconds.Observe(time.Since(start).Seconds())
        _ = req // quiet linter for unused vars in minimal loop
    }
}

// Helper: compute Now/Next from source timeline
func computeNowNext(ctx context.Context, channelID string) (*models.TimelineEntry, *models.TimelineEntry, error) {
    now := time.Now().UTC()
    entries, err := readTimeline(ctx, channelID, now.Add(-30*time.Minute), now.Add(6*time.Hour))
    if err != nil { return nil, nil, err }
    // Filter content only, sort
    items := []models.TimelineEntry{}
    for _, e := range entries { if e.Type == "content" { items = append(items, e) } }
    sort.Slice(items, func(i, j int) bool { return items[i].Start.Before(items[j].Start) })
    var cur, nxt *models.TimelineEntry
    for i := range items {
        e := items[i]
        end := e.Start.Add(time.Duration(e.DurationS) * time.Second)
        if !now.Before(e.Start) && now.Before(end) { cur = &items[i] }
        if e.Start.After(now) { nxt = &items[i]; break }
    }
    return cur, nxt, nil
}

// Helper: build EPG programs for window [from,to)
type epgProgram struct {
    ChannelID string    `json:"channel_id"`
    Start     time.Time `json:"start_at"`
    End       time.Time `json:"end_at"`
    Title     string    `json:"title"`
    AssetID   string    `json:"asset_id,omitempty"`
}

func buildPrograms(ctx context.Context, channelID string, from, to time.Time) ([]epgProgram, error) {
    entries, err := readTimeline(ctx, channelID, from, to)
    if err != nil { return nil, err }
    progs := make([]epgProgram, 0, len(entries))
    for _, e := range entries {
        if e.Type != "content" { continue }
        st := e.Start.UTC()
        en := e.Start.Add(time.Duration(e.DurationS) * time.Second).UTC()
        if !en.After(st) || st.Before(from.Add(-24*time.Hour)) { continue }
        title := e.AssetID
        progs = append(progs, epgProgram{ChannelID: channelID, Start: st, End: en, Title: title, AssetID: e.AssetID})
    }
    sort.Slice(progs, func(i, j int) bool { return progs[i].Start.Before(progs[j].Start) })
    return progs, nil
}

// Read timeline from configured source
func readTimeline(ctx context.Context, channelID string, from, to time.Time) ([]models.TimelineEntry, error) {
    switch timelineMode {
    case srcRedis:
        // Expect key tl:<channel_id> with {entries:[]}
        if rd == nil { return nil, io.EOF }
        s, err := rd.GetString(ctx, "tl:"+channelID)
        if err != nil { return nil, err }
        var payload struct{ Entries []models.TimelineEntry `json:"entries"` }
        if err := json.Unmarshal([]byte(s), &payload); err != nil { return nil, err }
        return payload.Entries, nil
    case srcHTTP:
        base := strings.TrimSuffix(timelineHTTPBase, "/")
        u := base + "/" + channelID + "?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
        req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
        resp, err := http.DefaultClient.Do(req)
        if err != nil { return nil, err }
        defer resp.Body.Close()
        if resp.StatusCode != 200 { return nil, io.ErrUnexpectedEOF }
        var data struct{ Entries []models.TimelineEntry `json:"entries"` }
        if err := json.NewDecoder(resp.Body).Decode(&data); err != nil { return nil, err }
        return data.Entries, nil
    default:
        return nil, io.EOF
    }
}

func toNowNextItem(e *models.TimelineEntry) models.NowNextItem {
    if e == nil { return models.NowNextItem{} }
    st := e.Start.UTC()
    en := e.Start.Add(time.Duration(e.DurationS) * time.Second).UTC()
    title := e.AssetID
    return models.NowNextItem{Title: title, StartsAt: st, StartsAt2: st, EndsAt: en}
}

func getenv(k, def string) string { if v := os.Getenv(k); v != "" { return v }; return def }
func atoi(s string, def int) int { var n int; _, err := strconv.Atoi(s); if err != nil { return def }; return n }
func min(a, b int) int { if a < b { return a } else { return b } }
func xmlEscape(s string) string { r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "\"", "&quot;", "'", "&apos;"); return r.Replace(s) }

