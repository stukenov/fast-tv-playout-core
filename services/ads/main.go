package main

import (
    "context"
    "crypto/sha1"
    "encoding/hex"
    "encoding/json"
    "net/http"
    "os"
    "sort"
    "strconv"
    "strings"
    "sync"
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

type creative struct {
    ID        string   `json:"id"`
    Type      string   `json:"type"`      // partner|house
    Category  []string `json:"category"`
    Duration  int      `json:"duration"`
    URI       string   `json:"uri"`
    Codecs    string   `json:"codecs,omitempty"`
    GOP       int      `json:"gop,omitempty"`
    Tags      []string `json:"tags,omitempty"`
    VastMap   map[string]string `json:"vast_map,omitempty"`
    Active    bool     `json:"active"`
}

type registry struct {
    Version   int         `json:"version"`
    UpdatedAt time.Time   `json:"updated_at"`
    Creatives []creative  `json:"creatives"`
}

type state struct {
    cfg        config.Config
    rd         *storage.Redis
    regMu      sync.RWMutex
    reg        *registry
    ready      bool
    segSec     int
    selectOrder []int
    apiKey     string
    vastTimeoutMs  int
    vastDeadlineMs int
}

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    rd := storage.ConnectRedis(cfg.RedisAddr)

    st := &state{cfg: cfg, rd: rd, segSec: cfg.SegmentSec}
    st.apiKey = os.Getenv("API_KEY")
    if st.apiKey == "" { st.apiKey = cfg.APIKey }
    st.vastTimeoutMs = getenvInt("VAST_TIMEOUT_MS", 350)
    st.vastDeadlineMs = getenvInt("VAST_DEADLINE_MS", 400)
    st.selectOrder = parseOrder(getenvDefault("SELECTION_ORDER", "30,15,10,5,2"))
    if len(st.selectOrder) == 0 { st.selectOrder = []int{30,15,10,5,2} }

    // initial registry load (from local file path for dev)
    if err := st.loadRegistry(); err != nil {
        lg.Warn().Err(err).Msg("ads: registry not loaded on start")
    }

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(2*time.Second))
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
        if st.isReady() { w.WriteHeader(200) } else { w.WriteHeader(503) }
    })
    r.Handle("/metrics", metrics.Handler())

    // secure group
    r.Group(func(pr chi.Router) {
        pr.Use(st.requireAPIKey)
        pr.Post("/v1/ads/pod", st.handleAdPod)
        pr.Post("/v1/admin/reload", st.handleReload)
    })

    addr := ":8083"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    _ = http.ListenAndServe(addr, r)
}

func (s *state) isReady() bool {
    s.regMu.RLock()
    defer s.regMu.RUnlock()
    return s.reg != nil && s.ready
}

func (s *state) requireAPIKey(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if s.apiKey == "" { next.ServeHTTP(w, r); return }
        if r.Header.Get("X-API-Key") != s.apiKey {
            httpx.WriteProblem(w, 401, "unauthorized", "missing or invalid X-API-Key")
            return
        }
        next.ServeHTTP(w, r)
    })
}

func (s *state) handleReload(w http.ResponseWriter, r *http.Request) {
    if err := s.loadRegistry(); err != nil {
        httpx.WriteProblem(w, 500, "reload_failed", err.Error())
        return
    }
    w.WriteHeader(204)
}

func (s *state) handleAdPod(w http.ResponseWriter, r *http.Request) {
    start := time.Now()
    var req models.AdPodRequest
    if err := httpx.DecodeJSON(r, &req); err != nil { httpx.WriteProblem(w, 400, "invalid json", err.Error()); return }
    ch := req.ChannelID
    metrics.AdsPodRequestsTotal.WithLabelValues(ch).Inc()

    // defaults
    if req.Break.Category == "" { req.Break.Category = "midroll" }
    if req.Constraints == nil { req.Constraints = &models.AdConstraints{NoBackToBack: true, AllowHouse: true} }

    // validate
    if req.Break.Duration < 2 || req.Break.Duration > 300 || req.Break.Duration%2 != 0 {
        httpx.WriteProblem(w, 422, "invalid duration", "duration must be even in [2;300]")
        return
    }
    if !oneOf(req.Break.Category, []string{"midroll","preroll","postroll","any"}) {
        httpx.WriteProblem(w, 422, "invalid category", "unsupported category")
        return
    }
    if !s.isReady() {
        httpx.WriteProblem(w, 503, "unready", "registry not loaded")
        return
    }

    // VAST check
    vastStatus, houseOnly := s.checkVAST(r.Context(), req.VastTags)

    // Build candidates
    cands := s.selectCandidates(req, houseOnly)
    if len(cands) == 0 && !req.Constraints.AllowHouse {
        w.WriteHeader(204)
        return
    }

    // Selection
    items, filled, housePad := s.fillPod(req, cands)
    fillRatio := 0.0
    if req.Break.Duration > 0 { fillRatio = float64(filled) / float64(req.Break.Duration) }
    metrics.AdFillRatio.WithLabelValues(ch).Set(fillRatio)
    if housePad > 0 { metrics.AdsHousePadSecondsTotal.WithLabelValues(ch).Add(float64(housePad)) }

    // Anti back-to-back: store last shown
    if len(items) > 0 {
        last := items[len(items)-1]
        _ = s.rd.SetString(r.Context(), "ads:last:"+ch, last.CreativeID, 30*time.Minute)
    }

    httpx.WriteJSON(w, 200, models.AdPodResponse{
        Items: items,
        FilledDuration: filled,
        FillRatio: fillRatio,
        Strategy: "greedy_dp_then_house_pad",
        Vast: &models.VastStatus{Status: vastStatus.status, CheckedTags: vastStatus.checked},
    })

    metrics.AdsPodLatencySeconds.WithLabelValues(ch).Observe(time.Since(start).Seconds())
}

func (s *state) fillPod(req models.AdPodRequest, cands []creative) ([]models.AdItem, int, int) {
    target := req.Break.Duration
    // Split partner/house
    var partner, house []creative
    for _, c := range cands {
        if c.Type == "house" { house = append(house, c) } else { partner = append(partner, c) }
    }
    order := s.selectOrder
    pickGreedy := func(pool []creative, need int) ([]models.AdItem, int) {
        out := []models.AdItem{}
        remain := need
        for _, dur := range order {
            for remain >= dur {
                if cr, ok := firstCreativeByDuration(pool, dur); ok {
                    out = append(out, models.AdItem{CreativeID: cr.ID, URI: cr.URI, DurationS: cr.Duration, Type: cr.Type})
                    remain -= dur
                } else {
                    break
                }
            }
        }
        return out, need - remain
    }

    // 1) Greedy partner
    items, filled := pickGreedy(partner, target)
    // 2) DP exact if not matched
    if filled < target {
        if exact := solveExactSum(partner, target-filled); len(exact) > 0 {
            for _, cr := range exact {
                items = append(items, models.AdItem{CreativeID: cr.ID, URI: cr.URI, DurationS: cr.Duration, Type: cr.Type})
                filled += cr.Duration
                if filled >= target { break }
            }
        }
    }
    // 3) Greedy house if allowed
    housePad := 0
    if filled < target && req.Constraints.AllowHouse {
        hItems, hFilled := pickGreedy(house, target-filled)
        items = append(items, hItems...)
        filled += hFilled
    }
    // 4) Pad with 2s house if still short
    if filled < target && req.Constraints.AllowHouse {
        // find 2s house
        var pad creative
        for _, c := range house {
            if c.Duration == 2 { pad = c; break }
        }
        if pad.ID != "" {
            for filled < target {
                items = append(items, models.AdItem{CreativeID: pad.ID, URI: pad.URI, DurationS: pad.Duration, Type: pad.Type})
                filled += pad.Duration
                housePad += pad.Duration
            }
        }
    }
    return items, filled, housePad
}

func firstCreativeByDuration(pool []creative, dur int) (creative, bool) {
    for _, c := range pool {
        if c.Duration == dur { return c, true }
    }
    return creative{}, false
}

func solveExactSum(pool []creative, target int) []creative {
    if target <= 0 { return nil }
    // DP by durations with backtracking; limited N
    if len(pool) > 50 { pool = pool[:50] }
    n := len(pool)
    dp := make([]bool, target+1)
    prev := make([]int, target+1)
    for i := range prev { prev[i] = -1 }
    dp[0] = true
    for i := 0; i < n; i++ {
        d := pool[i].Duration
        for s := target; s >= d; s-- {
            if dp[s-d] && !dp[s] {
                dp[s] = true
                prev[s] = i
            }
        }
    }
    if !dp[target] { return nil }
    // reconstruct
    res := []creative{}
    s := target
    used := make([]bool, n)
    for s > 0 {
        i := prev[s]
        if i < 0 { break }
        if used[i] { // avoid infinite loop
            break
        }
        used[i] = true
        res = append(res, pool[i])
        s -= pool[i].Duration
    }
    return res
}

func (s *state) selectCandidates(req models.AdPodRequest, houseOnly bool) []creative {
    s.regMu.RLock()
    reg := s.reg
    s.regMu.RUnlock()
    if reg == nil { return nil }

    // no back-to-back
    var last string
    if req.Constraints != nil && req.Constraints.NoBackToBack {
        if s.rd != nil {
            if v, err := s.rd.GetString(context.Background(), "ads:last:"+req.ChannelID); err == nil { last = v }
        }
    }

    // category matching
    want := req.Break.Category
    var out []creative
    for _, c := range reg.Creatives {
        if !c.Active { continue }
        if c.Duration%2 != 0 { continue }
        if last != "" && c.ID == last { continue }
        if houseOnly && c.Type != "house" { continue }
        if want != "any" && !hasCategory(c.Category, want) && !hasCategory(c.Category, "any") { continue }
        out = append(out, c)
    }
    // prefer partner first by stable sort
    sort.SliceStable(out, func(i, j int) bool { return out[i].Type < out[j].Type })
    return out
}

type vastResult struct { status string; checked int }

func (s *state) checkVAST(ctx context.Context, tags []string) (vastResult, bool) {
    if len(tags) == 0 { return vastResult{status: "skipped", checked: 0}, false }
    deadline := time.Duration(s.vastDeadlineMs) * time.Millisecond
    ctx2, cancel := context.WithTimeout(ctx, deadline)
    defer cancel()
    cli := http.Client{Timeout: time.Duration(s.vastTimeoutMs) * time.Millisecond}
    checked := 0
    for _, t := range tags {
        checked++
        // cache key
        h := sha1.Sum([]byte(t))
        key := "vast:cache:" + hex.EncodeToString(h[:])
        if v, err := s.rd.GetString(ctx2, key); err == nil {
            if v == "ok" { return vastResult{status: "ok", checked: checked}, false }
            if v == "timeout" || v == "error" { continue }
        }
        req, _ := http.NewRequestWithContext(ctx2, "GET", t, nil)
        resp, err := cli.Do(req)
        if err != nil {
            _ = s.rd.SetString(ctx2, key, "timeout", 60*time.Second)
            metrics.VastRequestsTotal.WithLabelValues("timeout", "").Inc()
            continue
        }
        _ = resp.Body.Close()
        code := strconv.Itoa(resp.StatusCode)
        if resp.StatusCode == 200 {
            _ = s.rd.SetString(ctx2, key, "ok", 90*time.Second)
            metrics.VastRequestsTotal.WithLabelValues("ok", code).Inc()
            return vastResult{status: "ok", checked: checked}, false
        }
        _ = s.rd.SetString(ctx2, key, "error", 60*time.Second)
        metrics.VastRequestsTotal.WithLabelValues("error", code).Inc()
    }
    return vastResult{status: "error", checked: checked}, true // house-only on failure
}

func (s *state) loadRegistry() error {
    // prefer ADS_REGISTRY_FILE; otherwise try ORIGIN_ROOT/ads/_registry.json
    path := getenvDefault("ADS_REGISTRY_FILE", "")
    if path == "" {
        root := getenvDefault("ADS_PREFIX_ROOT", s.cfg.OriginRoot)
        path = strings.TrimRight(root, "/") + "/ads/_registry.json"
    }
    bs, err := os.ReadFile(path)
    if err != nil { s.setReady(false); return err }
    var reg registry
    if err := json.Unmarshal(bs, &reg); err != nil { s.setReady(false); return err }
    // metrics for registry
    activeHouse := 0
    activePartner := 0
    for i := range reg.Creatives {
        if !reg.Creatives[i].Active { continue }
        if reg.Creatives[i].Type == "house" { activeHouse++ } else { activePartner++ }
    }
    metrics.AdsRegistryCreatives.WithLabelValues("house","active").Set(float64(activeHouse))
    metrics.AdsRegistryCreatives.WithLabelValues("partner","active").Set(float64(activePartner))
    s.regMu.Lock()
    s.reg = &reg
    s.regMu.Unlock()
    s.setReady(true)
    return nil
}

func (s *state) setReady(v bool) { s.ready = v }

func oneOf(v string, allowed []string) bool {
    for _, a := range allowed { if v == a { return true } }
    return false
}

func hasCategory(cats []string, want string) bool {
    for _, c := range cats { if c == want { return true } }
    return false
}

func getenvInt(k string, def int) int {
    if v := os.Getenv(k); v != "" {
        if n, err := strconv.Atoi(v); err == nil { return n }
    }
    return def
}

func getenvDefault(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func parseOrder(s string) []int {
    parts := strings.Split(s, ",")
    out := make([]int, 0, len(parts))
    for _, p := range parts {
        p = strings.TrimSpace(p)
        if p == "" { continue }
        if n, err := strconv.Atoi(p); err == nil { out = append(out, n) }
    }
    return out
}

