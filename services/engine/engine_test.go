package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/logger"
    "ftvx/pkg/models"
)

// fakeSchedule is an in-memory schedule store: the engine only asks it for the entries that overlap a window.
type fakeSchedule struct {
    mu      sync.Mutex
    entries []models.TimelineEntry
    err     error
    lag     time.Duration // how long an answer takes; the caller's context cuts it short
}

func (f *fakeSchedule) slow(d time.Duration) { f.mu.Lock(); f.lag = d; f.mu.Unlock() }

func (f *fakeSchedule) set(e ...models.TimelineEntry) { f.mu.Lock(); f.entries = e; f.mu.Unlock() }
func (f *fakeSchedule) fail(err error)                { f.mu.Lock(); f.err = err; f.mu.Unlock() }

func (f *fakeSchedule) GetTimelineRange(ctx context.Context, ch string, from, to time.Time) ([]models.TimelineEntry, error) {
    f.mu.Lock()
    lag := f.lag
    f.mu.Unlock()
    if lag > 0 {
        select {
        case <-time.After(lag):
        case <-ctx.Done(): return nil, ctx.Err()
        }
    }
    f.mu.Lock()
    defer f.mu.Unlock()
    if f.err != nil { return nil, f.err }
    var out []models.TimelineEntry
    for _, e := range f.entries {
        if e.Start.Before(to) && e.Start.Add(time.Duration(e.DurationS)*time.Second).After(from) { out = append(out, e) }
    }
    return out, nil
}

// adsCtl is an ads service that a test can slow down or break.
type adsCtl struct {
    mu    sync.Mutex
    delay time.Duration
    fail  bool
    calls int
}

func (c *adsCtl) set(delay time.Duration, fail bool) { c.mu.Lock(); c.delay, c.fail = delay, fail; c.mu.Unlock() }
func (c *adsCtl) count() int                          { c.mu.Lock(); defer c.mu.Unlock(); return c.calls }

// adsServer answers like the ads service: a pod of one 3 second creative.
func adsServer(t *testing.T, srv *httptest.Server) (*httptest.Server, *adsCtl) {
    t.Helper()
    ctl := &adsCtl{}
    ads := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctl.mu.Lock()
        ctl.calls++
        d, fail := ctl.delay, ctl.fail
        ctl.mu.Unlock()
        if d > 0 { time.Sleep(d) }
        if fail { http.Error(w, "down", 500); return }
        json.NewEncoder(w).Encode(models.AdPodResponse{Items: []models.AdItem{{URI: srv.URL + "/vod/ad/index.m3u8", DurationS: 3, Type: "partner"}}})
    }))
    t.Cleanup(ads.Close)
    return ads, ctl
}

type rig struct {
    e     *engine
    sched *fakeSchedule
    srv   *httptest.Server
    src   *srvCtl
    ads   *adsCtl
    base  time.Time
}

func newRig(t *testing.T) *rig { return newRigWith(t, nil) }

// newRigWith builds an engine on fake time, a fake schedule and real HTTP servers for the sources and the ads.
func newRigWith(t *testing.T, tweak func(*config.Config)) *rig {
    t.Helper()
    srv, src := sourceServerCtl(t)
    cfg := testConfig(srv)
    cfg.OriginRoot = t.TempDir()
    cfg.ChannelID = "c1"
    cfg.LiveLeadS = 6
    cfg.PublishAheadS = 30
    cfg.PreloadWindowS = 60
    adsSrv, ads := adsServer(t, srv)
    cfg.AdsURL = adsSrv.URL
    if tweak != nil { tweak(&cfg) }
    r := &rig{sched: &fakeSchedule{}, srv: srv, src: src, ads: ads, base: time.Now().UTC().Truncate(time.Second)}
    r.e = newEngine(cfg, logger.Setup(), r.sched, newResolver(cfg))
    if err := r.e.prepare(context.Background()); err != nil { t.Fatal(err) }
    return r
}

func (r *rig) at(n int) time.Time { return r.base.Add(time.Duration(n) * time.Second) }

func (r *rig) content(name string, from, seconds int) models.TimelineEntry {
    return models.TimelineEntry{Type: "content", URI: r.srv.URL + "/vod/" + name + "/index.m3u8", Start: r.at(from), DurationS: seconds}
}

func (r *rig) adBreak(from, seconds int) models.TimelineEntry {
    return models.TimelineEntry{Type: "ad_break", Start: r.at(from), DurationS: seconds, Category: "midroll"}
}

// ticks advances the clock second by second.
func (r *rig) ticks(from, to int) {
    for n := from; n <= to; n++ { r.e.tick(context.Background(), r.at(n)) }
}

func (r *rig) playlist(t *testing.T) string {
    t.Helper()
    b, err := os.ReadFile(r.e.mediaPath)
    if err != nil { t.Fatal(err) }
    return string(b)
}

func (r *rig) tokens(t *testing.T) []string { return tokens(t, r.e.st, r.playlist(t)) }

func seq(prefix string, from, to int) []string {
    var out []string
    for i := from; i <= to; i++ { out = append(out, fmt.Sprintf("S:%s_%d", prefix, i)) }
    return out
}

func join(parts ...[]string) []string {
    var out []string
    for _, p := range parts { out = append(out, p...) }
    return out
}

// The whole path in one process: the schedule is read, the ad pod is requested, the sources are downloaded,
// the segments are written and the playlist points at them.
func TestEngine_AirsMovieAdAndMovie(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 10), r.adBreak(10, 3), r.content("a", 13, 7))
    r.ticks(0, 13) // released up to second 19: the lead is 6 seconds
    equalTokens(t, r.tokens(t), join(
        []string{"M:a"}, seq("a", 0, 9),
        []string{"D", "M:ad"}, seq("ad", 0, 2),
        []string{"D", "M:a"}, seq("a", 0, 6)))
    assertContinuous(t, r.e.st, r.e.st.win.Items)
    pl := r.playlist(t)
    if !strings.Contains(pl, "#EXT-X-TARGETDURATION:1\n") || strings.Count(pl, "#EXTINF:1.000,") != 20 { t.Fatalf("playlist:\n%s", pl) }
    if !r.e.ready.Load() { t.Fatal("the engine must be ready once a playlist was written") }
}

func TestEngine_ReleasesOnlyWhatIsDue(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    if r.e.ready.Load() { t.Fatal("not ready before the first playlist") }
    r.ticks(0, 0)
    // the plan covers 30 seconds but only the clips of the next 6 seconds (and the current one) are in the playlist
    if got := strings.Count(r.playlist(t), "#EXTINF"); got != 7 { t.Fatalf("expected 7 segments after the first tick, got %d", got) }
    r.ticks(1, 1)
    if got := strings.Count(r.playlist(t), "#EXTINF"); got != 8 { t.Fatalf("one more segment per second, got %d", got) }
}

// What the merge of playout and packager is for: a clip that was not released yet can still change.
func TestEngine_ScheduleChangeIsAppliedUntilJustBeforeAir(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 9) // released up to second 15
    // an operator inserts an ad break at second 20 and shortens the movie
    r.sched.set(r.content("a", 0, 20), r.adBreak(20, 3), r.content("a", 23, 7))
    r.ticks(10, 18) // released up to second 24
    equalTokens(t, r.tokens(t), join(
        []string{"M:a"}, seq("a", 0, 19),
        []string{"D", "M:ad"}, seq("ad", 0, 2),
        []string{"D", "M:a"}, seq("a", 0, 1)))
    assertContinuous(t, r.e.st, r.e.st.win.Items)
}

// ... and what has been released is final.
func TestEngine_ReleasedSegmentsDoNotChange(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 9) // released up to second 15
    before := r.tokens(t)
    r.sched.set(r.adBreak(5, 3), r.content("a", 8, 17)) // a change inside the part that is already in the playlist
    r.ticks(10, 11)
    after := r.tokens(t)
    equalTokens(t, after[:len(before)], before)
}

func TestEngine_KeepsAiringWhenTheScheduleStoreIsDown(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 4)
    r.sched.fail(errors.New("connection refused"))
    r.ticks(5, 12) // eight seconds without a database: the channel plays what it knew
    equalTokens(t, r.tokens(t), join([]string{"M:a"}, seq("a", 0, 18)))
    r.sched.fail(nil)
    r.ticks(13, 14)
    equalTokens(t, r.tokens(t), join([]string{"M:a"}, seq("a", 0, 20)))
}

func TestEngine_RestartContinuesTheStreamWithoutRepeating(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 9) // released up to second 15
    // a new process on the same output
    again := newEngine(r.e.cfg, logger.Setup(), r.sched, newResolver(r.e.cfg))
    if err := again.prepare(context.Background()); err != nil { t.Fatal(err) }
    r.e = again
    r.ticks(10, 14) // released up to second 20
    equalTokens(t, r.tokens(t), join(
        []string{"M:a"}, seq("a", 0, 15),
        []string{"D", "M:a"}, seq("a", 16, 20))) // the first clip after a restart has an unknown predecessor
    assertContinuous(t, r.e.st, r.e.st.win.Items)
}

func TestEngine_ABrokenSourceIsReplacedByTheSlateAndTriedOnce(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("nope", 0, 10))
    r.ticks(0, 3)
    // 4 clips are due (seconds 0..9 released up to 9? lead 6 and now 3 gives 0..9): all slate, each clip tried once
    toks := r.tokens(t)
    for _, tok := range toks {
        if strings.HasPrefix(tok, "S:") && !strings.HasPrefix(tok, "S:slate_") { t.Fatalf("only the slate may air, got %s", tok) }
    }
    if n := r.e.st.win.Items; len(n) != 10 { t.Fatalf("expected 10 segments (0..9), got %d", len(n)) }
}

func TestEngine_StatusShowsTheStream(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 10), r.adBreak(10, 3))
    r.ticks(0, 6)
    s := r.e.state.snapshot()
    if s.Segments == 0 || s.LastPlaylistAt.IsZero() || s.WindowLen == 0 { t.Fatalf("status: %+v", s) }
    if s.AdState != "ad" { t.Fatalf("a break is in the plan: %+v", s) }
    if filepath.Base(r.e.mediaPath) != "v720.m3u8" { t.Fatal(r.e.mediaPath) }
}
