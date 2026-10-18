package main

import (
    "context"
    "crypto/sha1"
    "encoding/hex"
    "fmt"
    "math"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "sync"
    "testing"
    "time"


    "ftvx/pkg/config"
    "ftvx/pkg/fmp4"
    "ftvx/pkg/fmp4/fmp4test"
    "ftvx/pkg/models"
)

// --- sources ---

type source struct {
    name  string
    segs  int
    dur   string // EXTINF of the playlist
    noMap bool
}

var (
    // every source has its own init section (the default duration differs), like different files do
    initBytes = map[string][]byte{}
    sources   = []source{
        {name: "a", segs: 30, dur: "1.000"},
        {name: "ad", segs: 5, dur: "1.000"},
        {name: "slate", segs: 4, dur: "1.000"},
        {name: "ntsc", segs: 30, dur: "1.001"}, // 29.97 fps: a segment of 30 frames lasts 1.001 s
        {name: "long", segs: 5, dur: "2.000"},
        {name: "ts", segs: 3, dur: "1.000", noMap: true},
    }
)

func init() {
    for i, s := range sources {
        initBytes[s.name] = fmp4test.Init(
            fmp4test.TrackSpec{ID: 1, Kind: "vide", Timescale: 12800, DefaultDur: uint32(i + 1)},
            fmp4test.TrackSpec{ID: 2, Kind: "soun", Timescale: 48000})
    }
}

// audioStart is where audio of the k-th second of a real file starts: the muxer cuts at a whole AAC frame
// (1024 samples), a second holds 46.875 of them, so the start is off the whole second by up to 21 ms.
func audioStart(k int) uint64 { return uint64(math.Round(float64(k)*46.875)) * 1024 }

// segment i of a source: the picture is the name, the timeline of a file starts at 0
func (s source) segment(i int) []byte {
    frames := int(audioStart(i+1)-audioStart(i)) / 1024
    return fmp4test.Segment([]byte(fmt.Sprintf("%s_%d", s.name, i)),
        fmp4test.FragSpec{TrackID: 1, Tfdt: uint64(i) * 12800, Samples: 25, SampleDur: 512},
        fmp4test.FragSpec{TrackID: 2, Tfdt: audioStart(i), Samples: frames, SampleDur: 1024})
}

func (s source) playlist() string {
    b := &strings.Builder{}
    fmt.Fprintf(b, "#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:3\n")
    if !s.noMap { fmt.Fprintf(b, "#EXT-X-MAP:URI=\"init.mp4\"\n") }
    for i := 0; i < s.segs; i++ { fmt.Fprintf(b, "#EXTINF:%s,\nseg_%d.m4s\n", s.dur, i) }
    b.WriteString("#EXT-X-ENDLIST\n")
    return b.String()
}

// srvCtl lets a test break the sources: make one fail, or answer slowly with its segments and init sections.
type srvCtl struct {
    mu    sync.Mutex
    fail  map[string]bool
    delay map[string]time.Duration
    hits  map[string]int // requests per source, to see how often the engine asks
}

func (c *srvCtl) hitCount(name string) int { c.mu.Lock(); defer c.mu.Unlock(); return c.hits[name] }

func (c *srvCtl) set(name string, fail bool, delay time.Duration) {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.fail[name], c.delay[name] = fail, delay
}

// gate returns false when the source is down; media files (not playlists) are also delayed.
func (c *srvCtl) gate(name string, media bool) bool {
    c.mu.Lock()
    c.hits[name]++
    fail, d := c.fail[name], c.delay[name]
    c.mu.Unlock()
    if media && d > 0 { time.Sleep(d) }
    return !fail
}

func sourceServer(t *testing.T) *httptest.Server {
    srv, _ := sourceServerCtl(t)
    return srv
}

func sourceServerCtl(t *testing.T) (*httptest.Server, *srvCtl) {
    t.Helper()
    ctl := &srvCtl{fail: map[string]bool{}, delay: map[string]time.Duration{}, hits: map[string]int{}}
    mux := http.NewServeMux()
    for _, s := range sources {
        s := s
        mux.HandleFunc("/vod/"+s.name+"/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
            if !ctl.gate(s.name, false) { http.Error(w, "down", 503); return }
            fmt.Fprint(w, s.playlist())
        })
        mux.HandleFunc("/vod/"+s.name+"/init.mp4", func(w http.ResponseWriter, r *http.Request) {
            if !ctl.gate(s.name, true) { http.Error(w, "down", 503); return }
            w.Write(initBytes[s.name])
        })
        for i := 0; i < s.segs; i++ {
            i := i
            mux.HandleFunc(fmt.Sprintf("/vod/%s/seg_%d.m4s", s.name, i), func(w http.ResponseWriter, r *http.Request) {
                if !ctl.gate(s.name, true) { http.Error(w, "down", 503); return }
                w.Write(s.segment(i))
            })
        }
    }
    srv := httptest.NewServer(mux)
    t.Cleanup(srv.Close)
    return srv, ctl
}

func initFile(name string) string {
    sum := sha1.Sum(initBytes[name])
    return "init_" + hex.EncodeToString(sum[:4]) + ".mp4"
}

// --- helpers ---

func testConfig(srv *httptest.Server) config.Config {
    return config.Config{
        OriginRoot: "", ChannelID: "c1", VariantID: "v720", SegmentSec: 1,
        PlaylistWindowMax: 50, TargetDurationFloor: 1, IOTmpSuffix: ".tmp",
        DurationTolerance: 0.0005, SourceTimeoutMs: 2000, LiveLeadS: 600, RetainS: 0,
        SlateURI: srv.URL + "/vod/slate/index.m3u8",
        VariantBandwidth: 3000000, VariantAverageBandwidth: 2600000, VariantResolution: "1280x720", VariantFrameRate: 25, VariantCodecs: "avc1.4d401f,mp4a.40.2",
    }
}

var t0 = time.Now().UTC().Truncate(time.Second).Add(-20 * time.Second)

func clip(srv *httptest.Server, typ, name string, offset, n int) models.ClipItem {
    uri := srv.URL + "/vod/" + name + "/index.m3u8"
    return models.ClipItem{Seq: t0.Unix() + int64(n), URI: uri, StartAt: t0.Add(time.Duration(n) * time.Second), DurationS: 1, OffsetS: offset, Type: typ}
}

func newTestStitcher(t *testing.T, srv *httptest.Server) (*stitcher, config.Config) {
    t.Helper()
    cfg := testConfig(srv)
    cfg.OriginRoot = t.TempDir()
    return newStitcher(cfg, newResolver(cfg), filepath.Join(cfg.OriginRoot, "hls", "c1", cfg.VariantID)), cfg
}

// tokens flattens a rendered playlist into: D (discontinuity), M:<source of the init section>, S:<picture of the segment>.
// The picture is read from the file the playlist points at, so this checks what a player would download.
func tokens(t *testing.T, st *stitcher, pl string) []string {
    t.Helper()
    byInit := map[string]string{}
    for n := range initBytes { byInit[initFile(n)] = n }
    var out []string
    for _, l := range strings.Split(pl, "\n") {
        switch {
        case l == "#EXT-X-DISCONTINUITY": out = append(out, "D")
        case strings.HasPrefix(l, "#EXT-X-MAP:URI=\""):
            f := filepath.Base(strings.TrimSuffix(strings.TrimPrefix(l, "#EXT-X-MAP:URI=\""), "\""))
            if _, err := os.Stat(filepath.Join(st.dir, f)); err != nil { t.Fatalf("the playlist points at %s which does not exist", f) }
            out = append(out, "M:"+byInit[f])
        case l != "" && !strings.HasPrefix(l, "#"):
            b, err := os.ReadFile(filepath.Join(st.dir, filepath.Base(l)))
            if err != nil { t.Fatalf("the playlist points at %s which does not exist: %v", l, err) }
            pic, _ := fmp4.MediaData(b)
            out = append(out, "S:"+string(pic))
        }
    }
    return out
}

func equalTokens(t *testing.T, got, want []string) {
    t.Helper()
    if strings.Join(got, " ") != strings.Join(want, " ") {
        t.Fatalf("playlist tokens differ\n got: %s\nwant: %s", strings.Join(got, " "), strings.Join(want, " "))
    }
}

// assertContinuous checks the property the whole design is for: whatever the sources were, the decode time of
// every segment starts where the previous one of the same track ended. Inside a source it is exact; at a change of
// source the audio may be off by one audio frame (the files were cut at different places), which is why there is
// a discontinuity there.
func assertContinuous(t *testing.T, st *stitcher, items []SegmentMeta) {
    t.Helper()
    const audioFrame = 1024
    prev := map[string]uint64{}
    for i, it := range items {
        seg, err := os.ReadFile(filepath.Join(st.dir, filepath.Base(it.URI)))
        if err != nil { t.Fatal(err) }
        raw, err := os.ReadFile(filepath.Join(st.dir, filepath.Base(it.MapURI)))
        if err != nil { t.Fatal(err) }
        in, err := fmp4.ParseInit(raw)
        if err != nil { t.Fatal(err) }
        frags, err := fmp4.Fragments(seg, in)
        if err != nil { t.Fatal(err) }
        for _, f := range frags {
            if end, ok := prev[f.Track.Kind]; ok {
                gap := int64(f.Tfdt) - int64(end)
                limit := int64(0)
                if it.Discontinuity && f.Track.Kind == "soun" { limit = audioFrame }
                if gap > limit || gap < -limit {
                    t.Fatalf("segment %d (%s) track %s starts at %d but the previous one ended at %d (off by %d ticks)", i, it.URI, f.Track.Kind, f.Tfdt, end, gap)
                }
            }
            prev[f.Track.Kind] = f.Tfdt + f.Duration
        }
    }
}

// --- tests ---

func TestStitch_ContentAdContent_ContinuousTimelineAndDiscontinuityFollowsTheSource(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    ctx := context.Background()
    plan := []models.ClipItem{
        clip(srv, "content", "a", 0, 0), clip(srv, "content", "a", 1, 1), clip(srv, "content", "a", 2, 2),
        clip(srv, "ad", "ad", 0, 3), clip(srv, "ad", "ad", 1, 4),
        clip(srv, "content", "a", 5, 5), clip(srv, "content", "a", 6, 6),
    }
    plan[3].Discontinuity = true
    plan[5].Discontinuity = true
    for i, c := range plan { if !st.add(ctx, c) { t.Fatalf("clip %d was not added", i) } }

    pl := renderMediaPlaylist(cfg, st.win)
    equalTokens(t, tokens(t, st, pl), []string{
        "M:a", "S:a_0", "S:a_1", "S:a_2",
        "D", "M:ad", "S:ad_0", "S:ad_1",
        "D", "M:a", "S:a_5", "S:a_6",
    })
    assertContinuous(t, st, st.win.Items)
    if !strings.Contains(pl, "#EXT-X-TARGETDURATION:1\n") { t.Fatalf("target duration:\n%s", pl) }
    if !strings.Contains(pl, "#EXTINF:1.000,") { t.Fatalf("every segment lasts one second:\n%s", pl) }
    if !strings.Contains(pl, fmt.Sprintf("#EXT-X-MEDIA-SEQUENCE:%d\n", plan[0].Seq)) { t.Fatalf("media sequence must start at the first clip seq:\n%s", pl) }
    if !strings.Contains(pl, "#EXT-X-PROGRAM-DATE-TIME:"+t0.Format("2006-01-02T15:04:05.000Z")) { t.Fatalf("program date time:\n%s", pl) }
    if st.win.Items[6].Seq != st.win.Items[0].Seq+6 { t.Fatal("sequence numbers must be consecutive") }
}

func TestStitch_SegmentIsPlacedOnTheClock(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    // the 7th second of the file airs at t0: its decode time is t0, not 7 s
    st.add(context.Background(), clip(srv, "content", "a", 7, 0))
    frags := fragmentsOf(t, st, 0)
    if want := uint64(t0.Unix()-timelineEpoch) * 12800; frags[0].Tfdt != want { t.Fatalf("video starts at %d, want %d (the clock minus 2020-01-01)", frags[0].Tfdt, want) }
}

func fragmentsOf(t *testing.T, st *stitcher, i int) []fmp4.Fragment {
    t.Helper()
    raw, err := os.ReadFile(filepath.Join(st.dir, filepath.Base(st.win.Items[i].MapURI)))
    if err != nil { t.Fatal(err) }
    in, err := fmp4.ParseInit(raw)
    if err != nil { t.Fatal(err) }
    frags, err := fmp4.Fragments(readSeg(t, st, i), in)
    if err != nil { t.Fatal(err) }
    return frags
}

// Audio and video must keep the offset they have in the source; nothing may accumulate over many files.
func TestStitch_AudioStaysInSyncWithVideoAcrossManyFiles(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    ctx := context.Background()
    n := 0
    for round := 0; round < 12; round++ { // twelve "movies" one after another, each starting in another place of its file
        for k := 0; k < 3; k++ {
            if !st.add(ctx, clip(srv, "content", "a", round+k, n)) { t.Fatalf("clip %d", n) }
            n++
        }
    }
    for i := range st.win.Items {
        idx := i % 3 + i/3 // the index inside the file used for item i
        f := fragmentsOf(t, st, i)
        // compared in whole numbers over the common denominator 48000*12800, so that nothing is rounded
        got := int64(f[1].Tfdt)*12800 - int64(f[0].Tfdt)*48000
        want := (int64(audioStart(idx)) - int64(idx)*48000) * 12800
        if got != want { t.Fatalf("item %d: audio-video offset %d, the source has %d", i, got, want) }
    }
}

func TestStitch_SourceFileIsNotModified(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    st.add(context.Background(), clip(srv, "content", "a", 0, 0))
    out := readSeg(t, st, 0)
    orig := sources[0].segment(0)
    a, _ := fmp4.MediaData(out)
    b, _ := fmp4.MediaData(orig)
    if string(a) != string(b) { t.Fatal("the picture must be copied untouched") }
    if len(out) != len(orig) { t.Fatal("only decode times may change") }
}

func TestStitch_ContinuousPlaybackOfOneSourceHasNoDiscontinuity(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    for i := 0; i < 8; i++ { st.add(context.Background(), clip(srv, "content", "a", i, i)) }
    for _, tok := range tokens(t, st, renderMediaPlaylist(cfg, st.win)) {
        if tok == "D" { t.Fatal("unexpected discontinuity inside one source") }
    }
    assertContinuous(t, st, st.win.Items)
}

// 29.97 fps cannot give a segment of exactly one second, so such a source is refused up front, not 25 s into a movie.
func TestStitch_SourceWithSegmentsOf1001msIsRefused(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    if !st.add(context.Background(), clip(srv, "content", "ntsc", 0, 0)) { t.Fatal("the slate must fill in") }
    pic, _ := fmp4.MediaData(readSeg(t, st, 0))
    if !strings.HasPrefix(string(pic), "slate_") { t.Fatalf("expected a slate segment, got %s", pic)}
}

func readSeg(t *testing.T, st *stitcher, i int) []byte {
    t.Helper()
    b, err := os.ReadFile(filepath.Join(st.dir, filepath.Base(st.win.Items[i].URI)))
    if err != nil { t.Fatal(err) }
    return b
}

func TestStitch_FallsBackToSlate(t *testing.T) {
    srv := sourceServer(t)
    cases := map[string]models.ClipItem{
        "source is missing":        clip(srv, "content", "nope", 0, 0),
        "offset is past the end":   clip(srv, "content", "a", 40, 0),
        "segments are not 1s long": clip(srv, "content", "long", 0, 0),
        "source is not fMP4":       clip(srv, "content", "ts", 0, 0),
    }
    for name, c := range cases {
        t.Run(name, func(t *testing.T) {
            st, _ := newTestStitcher(t, srv)
            if !st.add(context.Background(), c) { t.Fatal("expected the slate to be used") }
            pic, _ := fmp4.MediaData(readSeg(t, st, 0))
            if !strings.HasPrefix(string(pic), "slate_") { t.Fatalf("expected a slate segment, got %s", pic) }
        })
    }
}

func TestStitch_SlateLoopsWithoutLosingContinuity(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    for i := 0; i < 5; i++ {
        c := clip(srv, "slate", "slate", 1000+i, i) // the slate has 4 segments
        if !st.add(context.Background(), c) { t.Fatalf("clip %d", i) }
    }
    equalTokens(t, tokens(t, st, renderMediaPlaylist(cfg, st.win)), []string{
        "M:slate", "S:slate_0", "S:slate_1", "S:slate_2", "S:slate_3",
        "D", "M:slate", "S:slate_0",
    })
    assertContinuous(t, st, st.win.Items) // the loop wraps in the source, but not on the stream timeline
}

func TestStitch_IgnoresDuplicatesAndMarksGaps(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    ctx := context.Background()
    st.add(ctx, clip(srv, "content", "a", 0, 0))
    if st.add(ctx, clip(srv, "content", "a", 0, 0)) { t.Fatal("a repeated clip must be ignored") }
    st.add(ctx, clip(srv, "content", "a", 1, 1))
    st.add(ctx, clip(srv, "content", "a", 4, 4)) // clips 2 and 3 never arrived
    equalTokens(t, tokens(t, st, renderMediaPlaylist(cfg, st.win)), []string{
        "M:a", "S:a_0", "S:a_1", "D", "M:a", "S:a_4",
    })
}

func TestStitch_DiscontinuitySequenceAdvancesWhenOneLeavesTheWindow(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    st.win.MaxSize = 3
    ctx := context.Background()
    st.add(ctx, clip(srv, "content", "a", 0, 0))
    st.add(ctx, clip(srv, "ad", "ad", 0, 1)) // discontinuity here
    st.add(ctx, clip(srv, "ad", "ad", 1, 2))
    if st.win.DiscSeq != 0 { t.Fatalf("DiscSeq=%d before the discontinuity left", st.win.DiscSeq) }
    st.add(ctx, clip(srv, "content", "a", 3, 3)) // back to the movie: a second discontinuity; a_0 leaves
    st.add(ctx, clip(srv, "content", "a", 4, 4)) // the first ad segment leaves together with its tag
    if st.win.DiscSeq != 1 { t.Fatalf("DiscSeq=%d, want 1", st.win.DiscSeq) }
    st.add(ctx, clip(srv, "content", "a", 5, 5))
    st.add(ctx, clip(srv, "content", "a", 6, 6)) // the segment that carried the second tag leaves
    if pl := renderMediaPlaylist(cfg, st.win); !strings.Contains(pl, "#EXT-X-DISCONTINUITY-SEQUENCE:2\n") {
        t.Fatalf("both discontinuities left the window, expected sequence 2:\n%s", pl)
    }
}

func TestStitch_FilesLeaveWithTheirSegments(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    st.win.MaxSize = 3
    ctx := context.Background()
    st.add(ctx, clip(srv, "content", "a", 0, 0))
    st.add(ctx, clip(srv, "ad", "ad", 0, 1))
    for i := 2; i < 8; i++ { st.add(ctx, clip(srv, "content", "a", i, i)) }
    entries, _ := os.ReadDir(st.dir)
    var segs, inits []string
    for _, e := range entries {
        if strings.HasPrefix(e.Name(), "seg_") { segs = append(segs, e.Name()) }
        if strings.HasPrefix(e.Name(), "init_") { inits = append(inits, e.Name()) }
    }
    if len(segs) != 3 { t.Fatalf("window holds 3 segments, directory has %v", segs) }
    if len(inits) != 1 || inits[0] != initFile("a") { t.Fatalf("only the init section of the movie is still used, directory has %v", inits) }
}

func TestStitch_RestoredPlaylistContinuesTheStream(t *testing.T) {
    srv := sourceServer(t)
    st, cfg := newTestStitcher(t, srv)
    ctx := context.Background()
    st.add(ctx, clip(srv, "content", "a", 0, 0))
    st.add(ctx, clip(srv, "ad", "ad", 0, 1))
    st.add(ctx, clip(srv, "ad", "ad", 1, 2))
    path := filepath.Join(t.TempDir(), "v720.m3u8")
    if err := os.WriteFile(path, []byte(renderMediaPlaylist(cfg, st.win)), 0o644); err != nil { t.Fatal(err) }

    again := newStitcher(cfg, newResolver(cfg), st.dir)
    if err := restoreWindow(path, again); err != nil { t.Fatal(err) }
    if len(again.win.Items) != 3 || again.nextSeq != st.nextSeq || !again.lastStart.Equal(st.lastStart) {
        t.Fatalf("restored %d items, nextSeq %d (want %d), lastStart %v (want %v)", len(again.win.Items), again.nextSeq, st.nextSeq, again.lastStart, st.lastStart)
    }
    if renderMediaPlaylist(cfg, again.win) != renderMediaPlaylist(cfg, st.win) { t.Fatal("restored window must render identically") }
    if again.add(ctx, clip(srv, "ad", "ad", 1, 2)) { t.Fatal("a clip that is already in the playlist must be ignored") }
    if !again.add(ctx, clip(srv, "content", "a", 3, 3)) { t.Fatal("next clip must be added") }
    if !again.win.Items[3].Discontinuity { t.Fatal("the first clip after a restart is marked as a discontinuity") }
    if again.win.Items[3].Seq != st.nextSeq { t.Fatalf("media sequence must continue, got %d want %d", again.win.Items[3].Seq, st.nextSeq) }
    assertContinuous(t, again, again.win.Items) // including across the restart: the timeline is the clock, no state is needed
}

func TestStitch_OrphanFilesOfAnEarlierRunAreRemoved(t *testing.T) {
    srv := sourceServer(t)
    st, _ := newTestStitcher(t, srv)
    st.add(context.Background(), clip(srv, "content", "a", 0, 0))
    stale := filepath.Join(st.dir, "seg_1.m4s")
    other := filepath.Join(st.dir, "notes.txt")
    os.WriteFile(stale, []byte("old"), 0o644)
    os.WriteFile(other, []byte("keep"), 0o644)
    st.removeOrphans()
    if _, err := os.Stat(stale); err == nil { t.Fatal("a segment that is not in the playlist must be removed") }
    if _, err := os.Stat(other); err != nil { t.Fatal("foreign files must stay") }
    if _, err := os.Stat(filepath.Join(st.dir, filepath.Base(st.win.Items[0].URI))); err != nil { t.Fatal("files of the window must stay") }
}

func TestResolver_FetchMapReachesSourcesBehindAnotherScheme(t *testing.T) {
    srv := sourceServer(t)
    cfg := testConfig(srv)
    cfg.OriginRoot = t.TempDir()
    cfg.SourceFetchMap = "s3://bucket/=" + srv.URL + "/vod/"
    st := newStitcher(cfg, newResolver(cfg), filepath.Join(cfg.OriginRoot, "v720"))
    c := clip(srv, "content", "a", 0, 0)
    c.URI = "s3://bucket/a/index.m3u8"
    if !st.add(context.Background(), c) { t.Fatal("not added") }
    pic, _ := fmp4.MediaData(readSeg(t, st, 0))
    if string(pic) != "a_0" { t.Fatalf("got %s", pic) }
}

func TestParseMap(t *testing.T) {
    m := parseMap(" s3://b/=http://minio:9000/b/ , http://o:8080/vod/=/vod/ ,broken")
    if len(m) != 2 { t.Fatalf("pairs: %v", m) }
    if got := applyMap("s3://b/x/y.m3u8", m); got != "http://minio:9000/b/x/y.m3u8" { t.Fatal(got) }
    if got := applyMap("https://other/x", m); got != "https://other/x" { t.Fatal(got) }
}
