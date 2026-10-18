package main

import (
    "encoding/json"
    "os"
    "sort"
    "sync"
    "time"

    "github.com/rs/zerolog"

    "ftvx/pkg/config"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
)

// creative is one ad in the registry file.
type creative struct {
    ID       string   `json:"id"`
    Type     string   `json:"type"` // "partner" (paid) or "house" (our own, fills what partners cannot)
    Category []string `json:"category"`
    Duration int      `json:"duration"` // seconds, a whole number of segments
    URI      string   `json:"uri"`
    Active   bool     `json:"active"`
}

type registryFile struct {
    Creatives []creative `json:"creatives"`
}

// staticFiller fills ad breaks from a registry file, without any service: it is the simplest way to have ads.
// The file is read again when it changes. For anything smarter point ADS_URL at your own ad server.
type staticFiller struct {
    path string
    seg  int
    lg   zerolog.Logger
    ch   string

    mu      sync.Mutex
    reg     []creative
    mtime   time.Time
    last    string // the creative that ended the previous break: it does not start the next one
    pods    map[time.Time][]models.AdItem
}

func newStaticFiller(cfg config.Config, lg zerolog.Logger) *staticFiller {
    return &staticFiller{path: cfg.AdsRegistryFile, seg: cfg.SegmentSec, lg: lg, ch: cfg.ChannelID, pods: map[time.Time][]models.AdItem{}}
}

func (f *staticFiller) reload() {
    st, err := os.Stat(f.path)
    if err != nil {
        if f.reg != nil { f.lg.Warn().Err(err).Str("file", f.path).Msg("ad registry is gone, breaks are filled with slate") }
        f.reg, f.mtime = nil, time.Time{}
        return
    }
    if st.ModTime().Equal(f.mtime) { return }
    b, err := os.ReadFile(f.path)
    var rf registryFile
    if err == nil { err = json.Unmarshal(b, &rf) }
    if err != nil {
        f.lg.Error().Err(err).Str("file", f.path).Msg("cannot read the ad registry, keeping the previous one")
        return
    }
    f.reg, f.mtime = rf.Creatives, st.ModTime()
    f.lg.Info().Int("creatives", len(rf.Creatives)).Msg("ad registry loaded")
}

// pod never waits: the registry is a local file. A break gets the same pod for as long as it is in the plan.
func (f *staticFiller) pod(e models.TimelineEntry, due bool) ([]models.AdItem, podState) {
    f.mu.Lock()
    defer f.mu.Unlock()
    if items, ok := f.pods[e.Start]; ok { return items, podReady }
    f.reload()
    metrics.PlayoutAdRequestsTotal.WithLabelValues(f.ch).Inc()
    items := fillBreak(f.reg, e.DurationS, e.Category, f.last, f.seg)
    if n := len(items); n > 0 { f.last = items[n-1].CreativeID }
    filled := 0
    for _, it := range items { filled += it.DurationS }
    if e.DurationS > 0 { metrics.AdFillRatio.WithLabelValues(f.ch).Set(float64(filled) / float64(e.DurationS)) }
    f.pods[e.Start] = items
    return items, podReady
}

func (f *staticFiller) giveUp(time.Time) {}

func (f *staticFiller) evict(now time.Time) {
    cutoff := now.Add(-time.Hour)
    f.mu.Lock()
    defer f.mu.Unlock()
    for k := range f.pods { if k.Before(cutoff) { delete(f.pods, k) } }
}

// noAds is used when neither ADS_URL nor ADS_REGISTRY_FILE is set: an ad break is filled with slate.
type noAds struct{}

func (noAds) pod(models.TimelineEntry, bool) ([]models.AdItem, podState) { return nil, podReady }
func (noAds) giveUp(time.Time)                                          {}
func (noAds) evict(time.Time)                                           {}

// fillBreak chooses creatives for a break of duration seconds:
//  1. partner creatives of the category whose lengths add up to as much of the break as possible, each used once
//     (fewest creatives on a tie), so the same ad is never shown twice in a row;
//  2. house creatives for what is left, the longest first;
//  3. the shortest house creative repeated for the rest. The last creative may run past the end of the break, the planner cuts it.
//
// last is the creative that ended the previous break; it is not used again at once, unless nothing else can be played
// (a registry with a single ad would otherwise leave every second break empty).
func fillBreak(reg []creative, duration int, category, last string, seg int) []models.AdItem {
    items := fillBreakExcluding(reg, duration, category, last, seg)
    if len(items) == 0 && last != "" { items = fillBreakExcluding(reg, duration, category, "", seg) }
    return items
}

func fillBreakExcluding(reg []creative, duration int, category, last string, seg int) []models.AdItem {
    if duration <= 0 || seg <= 0 { return nil }
    var partner, house []creative
    for _, c := range reg {
        if !c.Active || c.Duration <= 0 || c.Duration%seg != 0 || c.ID == last { continue }
        if category != "" && category != "any" && !hasCategory(c.Category, category) { continue }
        if c.Type == "house" { house = append(house, c) } else { partner = append(partner, c) }
    }
    byDuration := func(l []creative) {
        sort.SliceStable(l, func(i, j int) bool {
            if l[i].Duration != l[j].Duration { return l[i].Duration > l[j].Duration }
            return l[i].ID < l[j].ID
        })
    }
    byDuration(partner)
    byDuration(house)

    var items []models.AdItem
    add := func(c creative) { items = append(items, models.AdItem{CreativeID: c.ID, URI: c.URI, DurationS: c.Duration, Type: c.Type}) }
    filled := 0
    for _, c := range bestSubset(partner, duration) { add(c); filled += c.Duration }
    // the house creatives that are left, each once, longest first
    for _, c := range house {
        if filled+c.Duration <= duration { add(c); filled += c.Duration }
    }
    if filled < duration && len(house) > 0 {
        pad := house[len(house)-1] // the shortest
        for filled < duration { add(pad); filled += pad.Duration }
    }
    return items
}

// bestSubset picks creatives, each at most once, whose durations add up to the largest sum not above limit,
// using the fewest creatives for that sum. The pool is expected sorted so that the result is the same every time.
func bestSubset(pool []creative, limit int) []creative {
    if len(pool) == 0 || limit <= 0 { return nil }
    if len(pool) > 60 { pool = pool[:60] }
    // reach[s] = fewest creatives that give exactly s seconds, -1 when impossible; choice[s][i] tells how s was reached
    const none = -1
    reach := make([]int, limit+1)
    for i := range reach { reach[i] = none }
    reach[0] = 0
    from := make([][]int, len(pool)) // from[i][s] = 1 when creative i was used to reach s in the pass of creative i
    for i, c := range pool {
        from[i] = make([]int, limit+1)
        for s := limit; s >= c.Duration; s-- {
            if reach[s-c.Duration] != none && (reach[s] == none || reach[s-c.Duration]+1 < reach[s]) {
                reach[s] = reach[s-c.Duration] + 1
                from[i][s] = 1
            }
        }
    }
    best := 0
    for s := limit; s > 0; s-- { if reach[s] != none { best = s; break } }
    // walk back: the creative that last improved the cell, from the last creative to the first
    var out []creative
    s := best
    for i := len(pool) - 1; i >= 0 && s > 0; i-- {
        if from[i][s] == 1 { out = append(out, pool[i]); s -= pool[i].Duration }
    }
    sort.SliceStable(out, func(a, b int) bool { return out[a].Duration > out[b].Duration })
    return out
}

func hasCategory(cats []string, want string) bool {
    for _, c := range cats { if c == want || c == "any" { return true } }
    return false
}
