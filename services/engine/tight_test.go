package main

// The hard variant: the playlist holds two segments, the one on the air and the next one, and a segment is
// released one second before it airs. There is no buffer to hide a slow download, a stalled tick or a
// half written file. These tests run the engine on fake time against real HTTP servers and check, after
// every step, what a player that polls the playlist would see.

import (
    "context"
    "errors"
    "fmt"
    "math/rand"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/fmp4"
    "ftvx/pkg/hls"
    "ftvx/pkg/models"
)

func tight(c *config.Config) {
    c.PlaylistWindowMax = 2
    c.LiveLeadS = 1
    c.RetainS = 10
    c.TickMs = 100
}

func newTightRig(t *testing.T) *rig { return newRigWith(t, tight) }

// --- the player ---

type seenSeg struct {
    seg  hls.Segment
    seq  int64
    data []byte
    init []byte
}

// watch plays the part of a player: it reads the playlist, checks everything the HLS rules ask of a live playlist,
// and downloads every new segment the moment it appears.
type watch struct {
    t      *testing.T
    r      *rig
    prev   *hls.Playlist
    got    map[int64]*seenSeg
    order  []int64
    polls  int
    window int
}

func newWatch(t *testing.T, r *rig) *watch {
    return &watch{t: t, r: r, got: map[int64]*seenSeg{}, window: r.e.cfg.PlaylistWindowMax}
}

func (w *watch) readPlaylist() (hls.Playlist, string, bool) {
    raw, err := os.ReadFile(w.r.e.mediaPath)
    if err != nil { return hls.Playlist{}, "", false }
    pl, err := hls.Parse(strings.NewReader(string(raw)), nil)
    if err != nil { w.t.Fatalf("the playlist does not parse: %v\n%s", err, raw) }
    return pl, string(raw), true
}

// poll checks the playlist as it is now.
func (w *watch) poll(now time.Time) *hls.Playlist {
    t := w.t
    t.Helper()
    pl, raw, ok := w.readPlaylist()
    if !ok { return nil }
    w.polls++
    if !strings.HasPrefix(raw, "#EXTM3U\n") { t.Fatalf("a playlist starts with #EXTM3U:\n%s", raw) }
    if strings.Contains(raw, "#EXT-X-ENDLIST") { t.Fatal("a live playlist has no ENDLIST") }
    if len(pl.Segments) > w.window { t.Fatalf("%d segments, the window is %d:\n%s", len(pl.Segments), w.window, raw) }
    maxDur := 0.0
    for _, sg := range pl.Segments { if sg.Duration > maxDur { maxDur = sg.Duration } }
    if pl.TargetDuration < maxDur { t.Fatalf("target duration %v is less than a segment of %v", pl.TargetDuration, maxDur) }

    for i, sg := range pl.Segments {
        if i > 0 && !sg.ProgramDateTime.After(pl.Segments[i-1].ProgramDateTime) { t.Fatalf("program date time does not grow:\n%s", raw) }
        // the file must be there at the moment the playlist names it
        if _, err := os.Stat(filepath.Join(w.r.e.st.dir, filepath.Base(sg.URI))); err != nil { t.Fatalf("the playlist names %s but it is not there", sg.URI) }
        if sg.MapURI != "" {
            if _, err := os.Stat(filepath.Join(w.r.e.st.dir, filepath.Base(sg.MapURI))); err != nil { t.Fatalf("the playlist names %s but it is not there", sg.MapURI) }
        }
    }
    if w.prev != nil {
        p := *w.prev
        if pl.MediaSequence < p.MediaSequence { t.Fatalf("media sequence went back: %d -> %d", p.MediaSequence, pl.MediaSequence) }
        // what a player has seen does not change under its hands
        for i, sg := range pl.Segments {
            seq := pl.MediaSequence + int64(i)
            if j := int(seq - p.MediaSequence); j >= 0 && j < len(p.Segments) {
                if p.Segments[j].URI != sg.URI || !p.Segments[j].ProgramDateTime.Equal(sg.ProgramDateTime) || p.Segments[j].Discontinuity != sg.Discontinuity {
                    t.Fatalf("segment %d changed after it was published: %+v -> %+v", seq, p.Segments[j], sg)
                }
            }
        }
        // the discontinuity sequence counts the discontinuities that left the playlist
        if removed := int(pl.MediaSequence - p.MediaSequence); removed <= len(p.Segments) {
            want := 0
            for _, sg := range p.Segments[:removed] { if sg.Discontinuity { want++ } }
            if got := int(pl.DiscontinuitySequence - p.DiscontinuitySequence); got != want {
                t.Fatalf("discontinuity sequence went %d -> %d, %d tagged segments left", p.DiscontinuitySequence, pl.DiscontinuitySequence, want)
            }
        }
    }
    // download what is new, as a player does
    for i, sg := range pl.Segments {
        seq := pl.MediaSequence + int64(i)
        if _, ok := w.got[seq]; ok { continue }
        data, err := os.ReadFile(filepath.Join(w.r.e.st.dir, filepath.Base(sg.URI)))
        if err != nil { t.Fatalf("segment %d: %v", seq, err) }
        init, err := os.ReadFile(filepath.Join(w.r.e.st.dir, filepath.Base(sg.MapURI)))
        if err != nil { t.Fatalf("init of segment %d: %v", seq, err) }
        w.got[seq] = &seenSeg{seg: sg, seq: seq, data: data, init: init}
        w.order = append(w.order, seq)
    }
    w.prev = &pl
    return &pl
}

// tickAndPoll moves the engine to now and looks at the result.
func (w *watch) tickAndPoll(now time.Time) *hls.Playlist {
    w.t.Helper()
    w.r.e.tick(context.Background(), now)
    return w.poll(now)
}

// stream checks what a player ends up with: segments in order and decode times that follow each other.
func (w *watch) stream() {
    t := w.t
    t.Helper()
    var prev map[string]uint64
    var prevSeg *seenSeg
    for _, seq := range w.order {
        cur := w.got[seq]
        in, err := fmp4.ParseInit(cur.init)
        if err != nil { t.Fatal(err) }
        frags, err := fmp4.Fragments(cur.data, in)
        if err != nil { t.Fatalf("segment %d: %v", seq, err) }
        if prevSeg != nil && seq != prevSeg.seq+1 { t.Fatalf("segment %d follows %d: a player would miss one", seq, prevSeg.seq) }
        // the picture is continuous whenever the segments are consecutive seconds; audio may be a few frames off at a change of source
        if prevSeg != nil && cur.seg.ProgramDateTime.Sub(prevSeg.seg.ProgramDateTime) == time.Second {
            for _, f := range frags {
                end, ok := prev[f.Track.Kind]
                if !ok { continue }
                gap := int64(f.Tfdt) - int64(end)
                limit := int64(0)
                if f.Track.Kind == "soun" && cur.seg.Discontinuity { limit = int64(f.Track.Timescale) / 10 }
                if gap > limit || gap < -limit { t.Fatalf("segment %d track %s starts %d ticks from where the previous one ended", seq, f.Track.Kind, gap) }
            }
        }
        prev = map[string]uint64{}
        for _, f := range frags { prev[f.Track.Kind] = f.Tfdt + f.Duration }
        prevSeg = cur
    }
}

// --- tests ---

func TestTight_PlaylistIsTheSegmentOnTheAirAndTheNextOne(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    w := newWatch(t, r)
    for ms := 0; ms <= 20000; ms += 250 { // four ticks a second
        now := r.base.Add(time.Duration(ms) * time.Millisecond)
        pl := w.tickAndPoll(now)
        if pl == nil { t.Fatal("no playlist after a tick") }
        if len(pl.Segments) != 2 { t.Fatalf("at %v the playlist has %d segments, want the current and the next", now, len(pl.Segments)) }
        cur, next := pl.Segments[0], pl.Segments[1]
        if want := now.Truncate(time.Second); !cur.ProgramDateTime.Equal(want) { t.Fatalf("at %v the first segment starts at %v, want the one on the air (%v)", now, cur.ProgramDateTime, want) }
        if !next.ProgramDateTime.Equal(cur.ProgramDateTime.Add(time.Second)) { t.Fatalf("the second segment must be the next second: %v after %v", next.ProgramDateTime, cur.ProgramDateTime) }
    }
    w.stream()
}

func TestTight_AFileStaysUntilPlayersAreDoneWithIt(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    w := newWatch(t, r)
    leftAt := map[int64]time.Time{}
    for n := 0; n <= 24; n++ {
        now := r.at(n)
        pl := w.tickAndPoll(now)
        in := map[int64]bool{}
        for i := range pl.Segments { in[pl.MediaSequence+int64(i)] = true }
        for seq := range w.got {
            if !in[seq] { if _, ok := leftAt[seq]; !ok { leftAt[seq] = now } }
        }
        for seq, at := range leftAt {
            file := filepath.Join(r.e.st.dir, filepath.Base(w.got[seq].seg.URI))
            _, err := os.Stat(file)
            age := now.Sub(at)
            if age < 10*time.Second && err != nil { t.Fatalf("segment %d left the playlist %v ago and its file is already gone", seq, age) }
        }
    }
    // after the retention time the files are gone and the directory does not grow
    entries, _ := os.ReadDir(r.e.st.dir)
    var segs int
    for _, e := range entries { if strings.HasPrefix(e.Name(), "seg_") { segs++ } }
    if segs > 2+10+2 { t.Fatalf("%d segment files after 25 seconds: the window is 2 and the retention 10 s", segs) }
    if segs < 2+10-2 { t.Fatalf("%d segment files: the retention keeps about 10 seconds of them", segs) }
    // the earliest ones are really deleted
    first := w.got[w.order[0]].seg.URI
    if _, err := os.Stat(filepath.Join(r.e.st.dir, filepath.Base(first))); err == nil { t.Fatalf("%s should be gone after 24 seconds", first) }
}

// Every second is a change of source: a discontinuity on every segment, and the counters must keep up.
func TestTight_EverySecondIsAChangeOfSource(t *testing.T) {
    r := newTightRig(t)
    var entries []models.TimelineEntry
    for i := 0; i < 40; i++ {
        name := "a"
        if i%2 == 1 { name = "ad" }
        entries = append(entries, r.content(name, i, 1))
    }
    r.sched.set(entries...)
    w := newWatch(t, r)
    for n := 0; n <= 36; n++ { w.tickAndPoll(r.at(n)) }
    // all segments after the first have a discontinuity, and the sequence numbers follow the seconds
    for i, seq := range w.order {
        if i > 0 && !w.got[seq].seg.Discontinuity { t.Fatalf("segment %d changes the source and must have a discontinuity", seq) }
    }
    pl := w.prev
    if pl.DiscontinuitySequence < 30 { t.Fatalf("after 36 changes the discontinuity sequence is %d", pl.DiscontinuitySequence) }
    w.stream()
}

func TestTight_AReaderNeverSeesAPartialPlaylist(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    var stop atomic.Bool
    var reads, bad atomic.Int64
    var wg sync.WaitGroup
    for i := 0; i < 4; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            for !stop.Load() {
                raw, err := os.ReadFile(r.e.mediaPath)
                if err != nil { continue } // before the first write
                reads.Add(1)
                if _, err := hls.Parse(strings.NewReader(string(raw)), nil); err != nil || !strings.HasSuffix(string(raw), "\n") { bad.Add(1) }
            }
        }()
    }
    for ms := 0; ms <= 24000; ms += 100 { r.e.tick(context.Background(), r.base.Add(time.Duration(ms)*time.Millisecond)) }
    stop.Store(true)
    wg.Wait()
    if reads.Load() < 100 { t.Fatalf("only %d reads happened, the test proves nothing", reads.Load()) }
    if bad.Load() != 0 { t.Fatalf("%d of %d reads saw a playlist that is cut off or does not parse", bad.Load(), reads.Load()) }
}

// A whole hour on fake time: movies with ad breaks. Nothing may leak, nothing may be missed.
func TestTight_OneHourOnTheAir(t *testing.T) {
    if testing.Short() { t.Skip("an hour of fake time") }
    r := newTightRig(t)
    var entries []models.TimelineEntry
    at := 0
    for at < 3600 {
        entries = append(entries, r.content("a", at, 25), r.adBreak(at+25, 3))
        at += 28
    }
    r.sched.set(entries...)
    w := newWatch(t, r)
    var slowest time.Duration
    for n := 0; n <= 3500; n++ {
        started := time.Now()
        w.tickAndPoll(r.at(n))
        if d := time.Since(started); d > slowest { slowest = d }
    }
    w.stream()
    if got := len(w.order); got < 3500 { t.Fatalf("a player got %d segments in 3500 seconds", got) }
    if slowest > time.Second { t.Fatalf("a tick took %v", slowest) }
    // nothing grows without a limit
    entries2, _ := os.ReadDir(r.e.st.dir)
    if len(entries2) > 2+10+8 { t.Fatalf("%d files in the output directory after an hour", len(entries2)) }
    r.e.res.mu.Lock()
    segBytes, segs := r.e.res.segBytes, len(r.e.res.segs)
    r.e.res.mu.Unlock()
    if segBytes > maxSegCacheBytes || segs > 400 { t.Fatalf("the segment cache holds %d files, %d bytes", segs, segBytes) }
    broker := r.e.pods.(notifyingPods).podSource.(*adBroker)
    broker.mu.Lock()
    pods := len(broker.pods)
    broker.mu.Unlock()
    if pods > 140 { t.Fatalf("%d ad pods are remembered after an hour", pods) }
    if r.ads.count() > 140 { t.Fatalf("the ads service was asked %d times for 125 breaks", r.ads.count()) }
}

// "Switch now": an operator changes the schedule. The change reaches the air one to two seconds later.
func TestTight_AChangeReachesTheAirWithinTwoSeconds(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    w := newWatch(t, r)
    change := r.base.Add(5100 * time.Millisecond)
    changed := false
    var firstChanged time.Time
    for ms := 0; ms <= 14000; ms += 100 {
        now := r.base.Add(time.Duration(ms) * time.Millisecond)
        if !changed && !now.Before(change) {
            // from second 7 on, five seconds of another source
            r.sched.set(r.content("a", 0, 7), r.content("ad", 7, 5), r.content("a", 12, 13))
            changed = true
        }
        w.tickAndPoll(now)
    }
    for _, seq := range w.order {
        s := w.got[seq]
        pic, _ := fmp4.MediaData(s.data)
        isNew := strings.HasPrefix(string(pic), "ad_")
        if isNew && firstChanged.IsZero() { firstChanged = s.seg.ProgramDateTime }
    }
    if want := r.at(7); !firstChanged.Equal(want) { t.Fatalf("the first changed segment airs at %v, want %v (the change was made at 5.1 s)", firstChanged, want) }
    // it airs 1.9 seconds after the change: inside the promised one to two seconds
    if lat := firstChanged.Sub(change); lat < time.Second || lat > 2*time.Second { t.Fatalf("latency %v", lat) }
    // what had been released before the change did not move
    for n := 0; n <= 6; n++ {
        pic, _ := fmp4.MediaData(w.got[w.order[n]].data)
        if want := fmt.Sprintf("a_%d", n); string(pic) != want { t.Fatalf("second %d shows %s, want %s: it was released before the change", n, pic, want) }
    }
    w.stream()
}

// "Ad break now": the pod is fetched in the background and the break starts exactly two seconds after the command.
func TestTight_AnAdBreakNowStartsWithinTwoSeconds(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    w := newWatch(t, r)
    change := r.base.Add(5100 * time.Millisecond)
    done := false
    for ms := 0; ms <= 14000; ms += 100 {
        now := r.base.Add(time.Duration(ms) * time.Millisecond)
        if !done && !now.Before(change) {
            r.sched.set(r.content("a", 0, 7), r.adBreak(7, 3), r.content("a", 10, 15))
            done = true
        }
        w.tickAndPoll(now)
    }
    var pics []string
    for _, seq := range w.order {
        pic, _ := fmp4.MediaData(w.got[seq].data)
        pics = append(pics, string(pic))
    }
    got := strings.Join(pics[:13], " ")
    if want := "a_0 a_1 a_2 a_3 a_4 a_5 a_6 ad_0 ad_1 ad_2 a_0 a_1 a_2"; got != want { t.Fatalf("the stream is\n %s\nwant\n %s", got, want) }
    w.stream()
}

// The ads service is down or very slow: the break airs as slate and the playlist never stops.
func TestTight_ADeadOrSlowAdsServiceNeverStopsThePlaylist(t *testing.T) {
    for name, delay := range map[string]time.Duration{"slow": 3 * time.Second, "dead": 0} {
        t.Run(name, func(t *testing.T) {
            r := newTightRig(t)
            r.ads.set(delay, name == "dead")
            r.sched.set(r.content("a", 0, 5), r.adBreak(5, 3), r.content("a", 8, 10))
            w := newWatch(t, r)
            var slowest time.Duration
            for n := 0; n <= 14; n++ {
                started := time.Now()
                w.tickAndPoll(r.at(n))
                if d := time.Since(started); d > slowest { slowest = d }
            }
            if slowest > 900*time.Millisecond { t.Fatalf("a tick took %v: the stream would freeze", slowest) }
            var pics []string
            for _, seq := range w.order {
                pic, _ := fmp4.MediaData(w.got[seq].data)
                pics = append(pics, string(pic))
            }
            if len(pics) < 14 { t.Fatalf("the playlist stopped: %v", pics) }
            for i := 5; i < 8; i++ {
                if !strings.HasPrefix(pics[i], "slate_") { t.Fatalf("second %d should be slate, got %s (%v)", i, pics[i], pics) }
            }
            if !strings.HasPrefix(pics[8], "a_") { t.Fatalf("the movie must come back after the break: %v", pics) }
            w.stream()
        })
    }
}

// A source that answers after seconds, or never: the slate airs, the playlist goes on, and the movie returns.
func TestTight_ASlowOrDeadSourceNeverStopsThePlaylist(t *testing.T) {
    for name, tc := range map[string]struct{ fail bool; delay time.Duration }{"slow": {false, 4 * time.Second}, "dead": {true, 0}} {
        t.Run(name, func(t *testing.T) {
            r := newTightRig(t)
            r.sched.set(r.content("a", 0, 5), r.content("ad", 5, 3), r.content("a", 8, 15))
            r.src.set("ad", tc.fail, tc.delay) // the middle source is the broken one
            w := newWatch(t, r)
            var slowest time.Duration
            for n := 0; n <= 16; n++ {
                started := time.Now()
                w.tickAndPoll(r.at(n))
                if d := time.Since(started); d > slowest { slowest = d }
            }
            if slowest > 1200*time.Millisecond { t.Fatalf("a tick took %v", slowest) }
            var pics []string
            for _, seq := range w.order {
                pic, _ := fmp4.MediaData(w.got[seq].data)
                pics = append(pics, string(pic))
            }
            if len(pics) < 16 { t.Fatalf("the playlist stopped: %v", pics) }
            for i := 5; i < 8; i++ {
                if !strings.HasPrefix(pics[i], "slate_") { t.Fatalf("second %d should be slate, got %s (%v)", i, pics[i], pics) }
            }
            if pics[8] != "a_0" { t.Fatalf("the movie must come back: %v", pics) }
            w.stream()
        })
    }
}

func TestTight_ASlowScheduleStoreNeverStopsThePlaylist(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    w := newWatch(t, r)
    for n := 0; n <= 3; n++ { w.tickAndPoll(r.at(n)) }
    r.sched.slow(5 * time.Second) // the database answers after five seconds
    var slowest time.Duration
    for n := 4; n <= 14; n++ {
        started := time.Now()
        w.tickAndPoll(r.at(n))
        if d := time.Since(started); d > slowest { slowest = d }
    }
    if slowest > 1500*time.Millisecond { t.Fatalf("a tick took %v waiting for the database", slowest) }
    var pics []string
    for _, seq := range w.order {
        pic, _ := fmp4.MediaData(w.got[seq].data)
        pics = append(pics, string(pic))
    }
    if len(pics) < 14 || pics[13] != "a_13" { t.Fatalf("the channel must keep airing what it knows: %v", pics) }
    w.stream()
}

// The process stalls for four seconds (a pause of the machine) or the clock steps back: no duplicates, no going back.
func TestTight_AStalledProcessAndAClockStepBack(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 28))
    w := newWatch(t, r)
    for n := 0; n <= 5; n++ { w.tickAndPoll(r.at(n)) }
    // four seconds without a tick
    pl := w.tickAndPoll(r.at(10))
    if len(pl.Segments) != 2 || !pl.Segments[0].ProgramDateTime.Equal(r.at(10)) { t.Fatalf("after the pause the playlist must show the present: %+v", pl.Segments) }
    if !pl.Segments[0].Discontinuity { t.Fatal("a hole in time is a discontinuity") }
    // the clock steps back by two seconds and then forward again
    for _, n := range []int{8, 9, 10, 11, 12} { w.tickAndPoll(r.at(n)) }
    for i := 1; i < len(w.order); i++ {
        a, b := w.got[w.order[i-1]].seg.ProgramDateTime, w.got[w.order[i]].seg.ProgramDateTime
        if !b.After(a) { t.Fatalf("time went back in the stream: %v then %v", a, b) }
    }
    last := w.got[w.order[len(w.order)-1]].seg.ProgramDateTime
    if !last.Equal(r.at(13)) { t.Fatalf("after the clock caught up the stream is at %v, want %v", last, r.at(13)) }
    w.stream()
}

// A restart in the middle of the air: the new process picks up the playlist and goes on, with one discontinuity.
func TestTight_ARestartContinuesTheStream(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 28))
    w := newWatch(t, r)
    for n := 0; n <= 10; n++ { w.tickAndPoll(r.at(n)) }
    again := newEngine(r.e.cfg, r.e.lg, r.sched, newResolver(r.e.cfg))
    if err := again.prepare(context.Background()); err != nil { t.Fatal(err) }
    r.e = again
    w.r = r
    for n := 11; n <= 20; n++ { w.tickAndPoll(r.at(n)) }
    w.stream()
    discs := 0
    for _, seq := range w.order { if w.got[seq].seg.Discontinuity { discs++ } }
    if discs != 1 { t.Fatalf("a restart is one discontinuity, got %d", discs) }
}

// Random trouble for ten minutes of fake time, many times over: sources and the ads service going down and slow,
// the schedule store failing, the process pausing, the clock stepping back, restarts. After every step the same
// invariants must hold, and the stream a player collected must be whole.
func TestTight_RandomTrouble(t *testing.T) {
    seeds := []int64{1, 2, 3, 4, 5, 6}
    seconds := 300
    if testing.Short() { seeds, seconds = seeds[:1], 120 }
    for _, seed := range seeds {
        seed := seed
        t.Run(fmt.Sprintf("seed%d", seed), func(t *testing.T) {
            rnd := rand.New(rand.NewSource(seed))
            r := newTightRig(t)
            // a schedule of random content, breaks and short items
            var entries []models.TimelineEntry
            at := 0
            for at < seconds+60 {
                switch rnd.Intn(4) {
                case 0:
                    entries = append(entries, r.adBreak(at, 3)); at += 3
                case 1:
                    n := 1 + rnd.Intn(3); entries = append(entries, r.content("ad", at, n)); at += n
                default:
                    n := 3 + rnd.Intn(20); entries = append(entries, r.content("a", at, n)); at += n
                }
            }
            r.sched.set(entries...)
            w := newWatch(t, r)
            now := r.base
            slowest := time.Duration(0)
            for sec := 0; sec < seconds; sec++ {
                // random trouble, for one second at a time
                r.src.set("a", rnd.Intn(25) == 0, time.Duration(rnd.Intn(3))*700*time.Millisecond*time.Duration(rnd.Intn(2)))
                r.src.set("ad", rnd.Intn(25) == 0, 0)
                r.ads.set(time.Duration(rnd.Intn(3))*time.Duration(rnd.Intn(2))*600*time.Millisecond, rnd.Intn(20) == 0)
                if rnd.Intn(30) == 0 { r.sched.fail(errors.New("db down")) } else if rnd.Intn(3) == 0 { r.sched.fail(nil) }
                switch rnd.Intn(40) {
                case 0: now = now.Add(time.Duration(2+rnd.Intn(4)) * time.Second) // the process stalls
                case 1: now = now.Add(-time.Duration(1+rnd.Intn(2)) * time.Second) // the clock steps back
                case 2: // a restart
                    again := newEngine(r.e.cfg, r.e.lg, r.sched, newResolver(r.e.cfg))
                    if err := again.prepare(context.Background()); err != nil { t.Fatal(err) }
                    r.e, w.r = again, r
                }
                for k := 0; k < 10; k++ { // ten ticks a second
                    started := time.Now()
                    w.tickAndPoll(now.Add(time.Duration(k*100) * time.Millisecond))
                    if d := time.Since(started); d > slowest { slowest = d }
                }
                now = now.Add(time.Second)
            }
            if slowest > 3*time.Second { t.Fatalf("a tick took %v", slowest) }
            w.stream()
            if len(w.order) < seconds/2 { t.Fatalf("only %d segments in %d seconds of trouble", len(w.order), seconds) }
        })
    }
}

// A hole in time must be marked even when the picture would seem to go on: the slate loops, and after a pause of
// five seconds it is at the segment that follows the one before the pause: the last clip released before the pause
// is second 6 (idx 6 mod 4), the first one after it is second 11 (11 mod 4 = 6 mod 4 + 1).
func TestTight_AHoleInTimeIsMarkedEvenWhenTheSourceSeemsToContinue(t *testing.T) {
    r := newTightRig(t) // no schedule: the slate airs
    w := newWatch(t, r)
    for n := 0; n <= 5; n++ { w.tickAndPoll(r.at(n)) }
    pl := w.tickAndPoll(r.at(11)) // six seconds without a tick
    first := pl.Segments[0]
    if !first.ProgramDateTime.Equal(r.at(11)) { t.Fatalf("the playlist must show the present: %+v", pl.Segments) }
    if !first.Discontinuity { t.Fatal("seconds 7..10 were never aired: that is a discontinuity, although the slate segment number follows") }
    w.stream()
}

// A clip that cannot be made at all (the source and the slate are down) is tried once, not on every tick.
func TestTight_ADroppedClipIsTriedOnce(t *testing.T) {
    r := newTightRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.src.set("a", true, 0)
    r.src.set("slate", true, 0)
    for ms := 0; ms <= 4000; ms += 100 { r.e.tick(context.Background(), r.base.Add(time.Duration(ms)*time.Millisecond)) }
    // about 4+1 clips, each asked of the source and of the slate; tens of ticks must not multiply that by ten
    if n := r.src.hitCount("a") + r.src.hitCount("slate"); n > 60 {
        t.Fatalf("the sources were asked %d times for 5 clips: a dropped clip is retried on every tick", n)
    }
}
