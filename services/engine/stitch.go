package main

import (
    "bufio"
    "bytes"
    "context"
    "crypto/sha1"
    "encoding/hex"
    "errors"
    "fmt"
    "io"
    "math"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "time"

    "github.com/rs/zerolog/log"

    "ftvx/pkg/config"
    "ftvx/pkg/fmp4"
    "ftvx/pkg/hls"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
    "ftvx/pkg/scte35"
)

// --- URI mapping ---

type prefixPair struct{ from, to string }

// parseMap reads "from=to,from=to". The first "=" of a pair splits it.
func parseMap(s string) []prefixPair {
    var out []prefixPair
    for _, part := range strings.Split(s, ",") {
        part = strings.TrimSpace(part)
        if i := strings.Index(part, "="); i > 0 { out = append(out, prefixPair{from: part[:i], to: part[i+1:]}) }
    }
    return out
}

func applyMap(uri string, m []prefixPair) string {
    for _, p := range m {
        if strings.HasPrefix(uri, p.from) { return p.to + strings.TrimPrefix(uri, p.from) }
    }
    return uri
}

// --- source playlists ---

const (
    okTTL      = 10 * time.Minute
    errTTL     = 5 * time.Second
    maxCached  = 512
    maxPlaylistBytes = 4 << 20
)

type cacheEntry struct {
    pl  hls.Playlist
    err error
    at  time.Time
}

// resolver downloads and caches source (VOD) media playlists.
type resolver struct {
    client   *http.Client
    fetch    []prefixPair
    fileRoot string          // file:// sources must be under it
    hosts    map[string]bool // http(s) sources must be on these hosts (nil: any)
    mu       sync.Mutex
    cache    map[string]cacheEntry
    inits    map[string]initEntry
    inflight map[string]bool
    segs     map[string]blob // downloaded source segments: prefetched before their time, and the slate that is used again and again
    segBytes int
    flight   map[string]chan struct{}
}

type blob struct {
    data []byte
    at   time.Time
}

const (
    maxSegCacheBytes = 64 << 20
    segTTL           = 2 * time.Minute
)

type initEntry struct {
    data   []byte
    parsed fmp4.Init
    name   string // file name in the output directory, derived from the content
    at     time.Time
}

func newResolver(cfg config.Config) *resolver {
    r := &resolver{
        client:   &http.Client{Timeout: time.Duration(cfg.SourceTimeoutMs) * time.Millisecond},
        fetch:    parseMap(cfg.SourceFetchMap),
        fileRoot: cfg.SourceFileRoot,
        hosts:    parseHosts(cfg.SourceAllowHosts),
        cache:    map[string]cacheEntry{},
        inits:    map[string]initEntry{},
        inflight: map[string]bool{},
        segs:     map[string]blob{},
        flight:   map[string]chan struct{}{},
    }
    // a redirect must not lead where the first address could not
    r.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
        if len(via) >= 5 { return errors.New("too many redirects") }
        return r.allowed(req.URL)
    }
    return r
}

func parseHosts(s string) map[string]bool {
    var m map[string]bool
    for _, h := range strings.Split(s, ",") {
        if h = strings.TrimSpace(strings.ToLower(h)); h != "" {
            if m == nil { m = map[string]bool{} }
            m[h] = true
        }
    }
    return m
}

// allowed says whether a source may be read: a file only under SOURCE_FILE_ROOT, a web address only on an allowed
// host (when there is a list). Sources come from whoever may write the schedule, so this is what stops a schedule
// from reading files of the server or from calling addresses inside the network.
func (r *resolver) allowed(u *url.URL) error {
    switch u.Scheme {
    case "http", "https":
        if r.hosts != nil && !r.hosts[strings.ToLower(u.Host)] && !r.hosts[strings.ToLower(u.Hostname())] {
            return fmt.Errorf("host %s is not in SOURCE_ALLOW_HOSTS", u.Host)
        }
        return nil
    case "file":
        if r.fileRoot == "" { return errors.New("file sources are off: set SOURCE_FILE_ROOT") }
        root, err := filepath.EvalSymlinks(r.fileRoot)
        if err != nil { return err }
        p, err := filepath.EvalSymlinks(filepath.Clean(u.Path))
        if err != nil { return err }
        if p != root && !strings.HasPrefix(p, root+string(os.PathSeparator)) { return fmt.Errorf("%s is outside SOURCE_FILE_ROOT", u.Path) }
        return nil
    }
    return fmt.Errorf("unsupported source URI %q: use http(s):// or file://, or map it with SOURCE_FETCH_MAP", u.String())
}

func (e cacheEntry) fresh(now time.Time) bool {
    if e.err != nil { return now.Sub(e.at) < errTTL }
    return now.Sub(e.at) < okTTL
}

func (r *resolver) playlist(ctx context.Context, src string) (hls.Playlist, error) {
    r.mu.Lock()
    if e, ok := r.cache[src]; ok && e.fresh(time.Now()) {
        r.mu.Unlock()
        return e.pl, e.err
    }
    r.mu.Unlock()
    pl, err := r.download(ctx, src)
    r.mu.Lock()
    if len(r.cache) >= maxCached {
        now := time.Now()
        for k, v := range r.cache { if !v.fresh(now) { delete(r.cache, k) } }
        if len(r.cache) >= maxCached { r.cache = map[string]cacheEntry{} }
    }
    r.cache[src] = cacheEntry{pl: pl, err: err, at: time.Now()}
    r.mu.Unlock()
    return pl, err
}

// prefetch warms the cache in the background so the stitching loop never waits for the network.
func (r *resolver) prefetch(ctx context.Context, src string) {
    r.mu.Lock()
    if e, ok := r.cache[src]; (ok && e.fresh(time.Now())) || r.inflight[src] {
        r.mu.Unlock()
        return
    }
    r.inflight[src] = true
    r.mu.Unlock()
    go func() {
        _, _ = r.playlist(ctx, src)
        r.mu.Lock()
        delete(r.inflight, src)
        r.mu.Unlock()
    }()
}

func (r *resolver) download(ctx context.Context, src string) (hls.Playlist, error) {
    target := applyMap(src, r.fetch)
    u, err := url.Parse(target)
    if err != nil { return hls.Playlist{}, err }
    b, err := r.get(ctx, target, maxPlaylistBytes)
    if err != nil { return hls.Playlist{}, err }
    pl, err := hls.Parse(bytes.NewReader(b), u)
    if err != nil { return hls.Playlist{}, fmt.Errorf("source %s: %w", target, err) }
    return pl, nil
}

const maxSegmentBytes = 64 << 20

// get downloads a source file; src may be a mapped URI (s3://...) or a plain URL.
func (r *resolver) get(ctx context.Context, src string, limit int64) ([]byte, error) {
    target := applyMap(src, r.fetch)
    u, err := url.Parse(target)
    if err != nil { return nil, err }
    if err := r.allowed(u); err != nil { return nil, err }
    if u.Scheme == "file" {
        f, err := os.Open(u.Path)
        if err != nil { return nil, err }
        defer f.Close()
        b, err := io.ReadAll(io.LimitReader(f, limit+1))
        if err != nil { return nil, err }
        if int64(len(b)) > limit { return nil, fmt.Errorf("source %s is larger than %d bytes", target, limit) }
        return b, nil
    }
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
    if err != nil { return nil, err }
    resp, err := r.client.Do(req)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK { return nil, fmt.Errorf("source %s: status %d", target, resp.StatusCode) }
    b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
    if err != nil { return nil, err }
    if int64(len(b)) > limit { return nil, fmt.Errorf("source %s is larger than %d bytes", target, limit) }
    return b, nil
}

// getCached returns a downloaded source file. A download that is already running is joined, not repeated, and one
// that finished earlier (a prefetch) is served from memory.
func (r *resolver) getCached(ctx context.Context, src string, limit int64) ([]byte, error) {
    for {
        r.mu.Lock()
        if b, ok := r.segs[src]; ok && time.Since(b.at) < segTTL {
            r.mu.Unlock()
            return b.data, nil
        }
        ch, running := r.flight[src]
        if !running {
            ch = make(chan struct{})
            r.flight[src] = ch
            r.mu.Unlock()
            break
        }
        r.mu.Unlock()
        select {
        case <-ch: // the other download is over: look at the cache again
        case <-ctx.Done(): return nil, ctx.Err()
        }
    }
    data, err := r.get(ctx, src, limit)
    r.mu.Lock()
    if err == nil { r.storeBlob(src, data) }
    ch := r.flight[src]
    delete(r.flight, src)
    r.mu.Unlock()
    close(ch)
    return data, err
}

// storeBlob keeps a downloaded file; the caller holds the lock. The oldest files go when the cache is full.
func (r *resolver) storeBlob(src string, data []byte) {
    if old, ok := r.segs[src]; ok { r.segBytes -= len(old.data) }
    r.segs[src] = blob{data: data, at: time.Now()}
    r.segBytes += len(data)
    for r.segBytes > maxSegCacheBytes && len(r.segs) > 1 {
        oldest, oldestAt := "", time.Now().Add(time.Hour)
        for k, v := range r.segs { if k != src && v.at.Before(oldestAt) { oldest, oldestAt = k, v.at } }
        if oldest == "" { break }
        r.segBytes -= len(r.segs[oldest].data)
        delete(r.segs, oldest)
    }
}

// prefetchBlob downloads a file in the background unless it is there or on its way.
func (r *resolver) prefetchBlob(ctx context.Context, src string, limit int64) {
    r.mu.Lock()
    if b, ok := r.segs[src]; (ok && time.Since(b.at) < segTTL) || r.flight[src] != nil {
        r.mu.Unlock()
        return
    }
    r.mu.Unlock()
    go func() { _, _ = r.getCached(ctx, src, limit) }()
}

// prefetchClip starts the downloads that a clip will need: its source playlist first, then its segment and init section.
func (r *resolver) prefetchClip(ctx context.Context, cfg config.Config, c models.ClipItem) {
    r.mu.Lock()
    e, ok := r.cache[c.URI]
    r.mu.Unlock()
    if !ok || !e.fresh(time.Now()) || e.err != nil {
        r.prefetch(ctx, c.URI)
        return
    }
    m, _, err := r.lookup(ctx, c.URI, c.OffsetS, c.Type == "slate", cfg.SegmentSec, cfg.DurationTolerance)
    if err != nil { return }
    r.prefetchBlob(ctx, m.URI, maxSegmentBytes)
    if m.MapURI != "" {
        r.mu.Lock()
        in, have := r.inits[m.MapURI]
        r.mu.Unlock()
        if !have || time.Since(in.at) >= okTTL { go func() { _, _ = r.initSection(ctx, m.MapURI) }() }
    }
}

// initSection downloads and parses an initialization section once per source.
func (r *resolver) initSection(ctx context.Context, mapURI string) (initEntry, error) {
    r.mu.Lock()
    if e, ok := r.inits[mapURI]; ok && time.Since(e.at) < okTTL {
        r.mu.Unlock()
        return e, nil
    }
    r.mu.Unlock()
    b, err := r.get(ctx, mapURI, 1<<20)
    if err != nil { return initEntry{}, err }
    parsed, err := fmp4.ParseInit(b)
    if err != nil { return initEntry{}, fmt.Errorf("init section %s: %w", mapURI, err) }
    sum := sha1.Sum(b)
    e := initEntry{data: b, parsed: parsed, name: "init_" + hex.EncodeToString(sum[:4]) + ".mp4", at: time.Now()}
    r.mu.Lock()
    if len(r.inits) >= maxCached { r.inits = map[string]initEntry{} }
    r.inits[mapURI] = e
    r.mu.Unlock()
    return e, nil
}

// --- clip -> segment ---

// SegmentMeta is one entry of the live media playlist.
type SegmentMeta struct {
    Seq             int64
    Duration        float64
    ProgramDateTime time.Time
    Discontinuity   bool
    Tags       []string  // lines written before the segment: the markers of an ad break (SCTE-35)
    AvailStart time.Time // the ad break of the schedule this segment belongs to, whatever was played in it
    AvailDur   int
    Kind       string    // content, bumper, ad or slate (not stored in the playlist)
    Title      string
    EntryStart time.Time
    BreakStart time.Time
    BreakDurS  int
    URI             string // path of the segment relative to the playlist
    MapURI          string // path of its init section relative to the playlist
    // where the segment came from; used to detect a change of source, not stored in the playlist
    src string
    idx int
}

// retimed is a source segment that is ready to be written: the bytes are already on the shared timeline.
type retimed struct {
    meta     SegmentMeta
    seg      []byte
    init     []byte
    initName string
    slate    bool // the slate was used in place of the clip
}

// lookup finds the source segment a clip points to. The segment is chosen by index (offset / segment length),
// not by summing durations, so sources with 2.002s segments do not drift on a long movie.
func (r *resolver) lookup(ctx context.Context, uri string, offsetS int, loop bool, segSec int, tol float64) (SegmentMeta, string, error) {
    pl, err := r.playlist(ctx, uri)
    if err != nil { return SegmentMeta{}, "fetch", err }
    idx := offsetS / segSec
    if loop { idx = ((idx % len(pl.Segments)) + len(pl.Segments)) % len(pl.Segments) }
    if idx < 0 || idx >= len(pl.Segments) {
        return SegmentMeta{}, "offset", fmt.Errorf("offset %ds is past the end of %s (%d segments)", offsetS, uri, len(pl.Segments))
    }
    seg := pl.Segments[idx]
    if math.Abs(seg.Duration-float64(segSec)) > tol {
        return SegmentMeta{}, "duration", fmt.Errorf("segment %d of %s lasts %.3fs, expected %ds", idx, uri, seg.Duration, segSec)
    }
    return SegmentMeta{Duration: seg.Duration, URI: seg.URI, MapURI: pl.MapURI, src: uri, idx: idx}, "", nil
}

// prepare turns a clip into a segment that is ready to be written: it finds the source segment, downloads it
// and puts it on the stream timeline at the moment of the clip.
func (r *resolver) prepare(ctx context.Context, cfg config.Config, uri string, offsetS int, loop bool, startAt time.Time) (retimed, string, error) {
    m, reason, err := r.lookup(ctx, uri, offsetS, loop, cfg.SegmentSec, cfg.DurationTolerance)
    if err != nil { return retimed{}, reason, err }
    if m.MapURI == "" { return retimed{}, "format", fmt.Errorf("source %s has no EXT-X-MAP: only fMP4 (CMAF) sources are supported", uri) }
    in, err := r.initSection(ctx, m.MapURI)
    if err != nil { return retimed{}, "fetch", err }
    raw, err := r.getCached(ctx, m.URI, maxSegmentBytes)
    if err != nil { return retimed{}, "fetch", err }
    // this segment is the idx-th segment of its file, so it is at second idx*SegmentSec of it
    out, err := fmp4.Retime(raw, in.parsed, startAt.Unix()-timelineEpoch, int64(m.idx*cfg.SegmentSec), gridTolerance)
    if err != nil {
        if errors.Is(err, fmp4.ErrOffGrid) { return retimed{}, "timeline", fmt.Errorf("segment %s: %w", m.URI, err) }
        return retimed{}, "retime", fmt.Errorf("segment %s: %w", m.URI, err)
    }
    return retimed{meta: m, seg: out, init: in.data, initName: in.name}, "", nil
}

// gridTolerance is how far (seconds) a track of a source segment may start from its whole second: the audio of a
// real file is up to one audio frame (21 ms) off, a few tenths of a second means the file is not normalized.
const gridTolerance = 0.25

// timelineEpoch is where the media timeline of the stream starts: 2020-01-01T00:00:00Z. Decode times are the
// wall clock distance from it, so they are the same after a restart and need no stored state.
const timelineEpoch = 1577836800

// resolve falls back to the slate when the clip cannot be prepared.
func (s *stitcher) resolve(ctx context.Context, c models.ClipItem) (retimed, bool) {
    cfg := s.cfg
    rt, reason, err := s.res.prepare(ctx, cfg, c.URI, c.OffsetS, c.Type == "slate", c.StartAt)
    if err == nil { return rt, true }
    metrics.PackagerStitchErrorsTotal.WithLabelValues(cfg.ChannelID, reason).Inc()
    if s.onFail != nil { s.onFail(reason, c.URI, c, err) }
    if c.Type == "slate" || cfg.SlateURI == "" || c.URI == cfg.SlateURI {
        metrics.PackagerClipsDroppedTotal.WithLabelValues(cfg.ChannelID).Inc()
        return retimed{}, false
    }
    rt, reason, err2 := s.res.prepare(ctx, cfg, cfg.SlateURI, int(c.StartAt.Unix()), true, c.StartAt)
    if err2 != nil {
        metrics.PackagerStitchErrorsTotal.WithLabelValues(cfg.ChannelID, "slate_"+reason).Inc()
        if s.onFail != nil { s.onFail("slate_"+reason, cfg.SlateURI, c, err2) }
        metrics.PackagerClipsDroppedTotal.WithLabelValues(cfg.ChannelID).Inc()
        return retimed{}, false
    }
    rt.slate = true
    return rt, true
}

// --- live window ---

type Window struct {
    Items   []SegmentMeta
    MaxSize int
    // DiscSeq counts discontinuities that already left the window (EXT-X-DISCONTINUITY-SEQUENCE).
    DiscSeq int64
}

// append adds a segment and returns the ones that left the window.
func (w *Window) append(m SegmentMeta) (removed []SegmentMeta) {
    w.Items = append(w.Items, m)
    for len(w.Items) > w.MaxSize {
        if w.Items[0].Discontinuity { w.DiscSeq++ }
        removed = append(removed, w.Items[0])
        w.Items = w.Items[1:]
    }
    return removed
}

func (w *Window) maxDuration() float64 {
    max := 0.0
    for _, it := range w.Items { if it.Duration > max { max = it.Duration } }
    return max
}

// stitcher turns clips into window entries and keeps the continuity state between them.
type stitcher struct {
    cfg       config.Config
    res       *resolver
    dir       string // output directory of the variant: segments and init sections are written here
    win       Window
    nextSeq   int64
    lastStart time.Time
    prevSrc   string
    prevIdx   int
    prevEnd   time.Time
    initChanges int // how many times the init section differed from the previous segment's
    prevAvail   time.Time // the ad break of the previous segment (zero outside a break)
    prevAvailDur int
    retired   []retiredFiles
    onFail    func(reason, uri string, c models.ClipItem, err error) // told when a source could not be used
}

// retiredFiles are files of segments that left the playlist. They stay for a while: a player that read the
// playlist a moment ago may still be downloading them (RFC 8216 asks servers to keep removed segments).
type retiredFiles struct {
    names []string // segment files and init sections
    at    time.Time // the stream time when they left
}

func newStitcher(cfg config.Config, res *resolver, dir string) *stitcher {
    return &stitcher{cfg: cfg, res: res, dir: dir, win: Window{MaxSize: cfg.PlaylistWindowMax}}
}

// add writes the segment of a clip and appends it to the window. It reports false when the clip is a duplicate,
// cannot be resolved or cannot be written.
func (s *stitcher) add(ctx context.Context, c models.ClipItem) bool {
    if !s.lastStart.IsZero() && !c.StartAt.After(s.lastStart) { return false }
    rt, ok := s.resolve(ctx, c)
    if !ok { return false }
    m := rt.meta
    m.Kind, m.Title, m.EntryStart, m.BreakStart, m.BreakDurS = c.Type, c.Title, c.EntryStart, c.BreakStart, c.BreakDurS
    m.AvailStart, m.AvailDur = c.BreakStart, c.BreakDurS
    m.Tags = s.breakTags(c)
    if rt.slate { m.Kind, m.Title, m.EntryStart, m.BreakStart, m.BreakDurS = "slate", "slate", time.Time{}, time.Time{}, 0 } // what airs is the slate
    // A change of source is a discontinuity (its own init section), and so is a jump inside a source or a hole in time.
    disc := c.Discontinuity
    switch {
    case len(s.win.Items) == 0:
        // the very first segment needs no discontinuity
        disc = false
    case s.prevSrc == "":
        disc = true // continuing a restored playlist: the predecessor is unknown
    case m.src != s.prevSrc || m.idx != s.prevIdx+1:
        disc = true
    }
    if !s.prevEnd.IsZero() && c.StartAt.Sub(s.prevEnd).Abs() > 500*time.Millisecond { disc = true }
    m.Discontinuity = disc
    m.ProgramDateTime = c.StartAt.UTC()
    m.Seq = s.nextSeq
    if m.Seq == 0 { m.Seq = c.Seq }

    variant := filepath.Base(s.dir)
    segName := fmt.Sprintf("seg_%d.m4s", m.Seq)
    if err := s.writeFiles(segName, rt); err != nil {
        metrics.PackagerFilesystemErrorsTotal.WithLabelValues(s.cfg.ChannelID, "segment_write").Inc()
        return false
    }
    m.URI, m.MapURI = variant+"/"+segName, variant+"/"+rt.initName

    // Sources encoded with different settings have different init sections. Players that re-initialize at a
    // discontinuity cope, but ffmpeg and many TVs keep the first parameter sets and fail to decode after the change.
    if n := len(s.win.Items); n > 0 && filepath.Base(s.win.Items[n-1].MapURI) != rt.initName {
        s.initChanges++
        metrics.PackagerInitChangesTotal.WithLabelValues(s.cfg.ChannelID).Inc()
        log.Warn().Str("channel", s.cfg.ChannelID).Str("source", m.src).Str("init", rt.initName).
            Msg("the init section differs from the previous source: encode all sources with tools/normalize.sh, strict players cannot decode across this change")
    }
    removed := s.win.append(m)
    s.cleanup(removed, c.StartAt)
    if disc { metrics.PackagerDiscontinuitiesTotal.WithLabelValues(s.cfg.ChannelID, s.cfg.VariantID).Inc() }
    s.nextSeq = m.Seq + 1
    s.lastStart = c.StartAt
    s.prevAvail, s.prevAvailDur = m.AvailStart, m.AvailDur
    s.prevSrc, s.prevIdx = m.src, m.idx
    s.prevEnd = c.StartAt.Add(time.Duration(c.DurationS) * time.Second)
    return true
}

func (s *stitcher) writeFiles(segName string, rt retimed) error {
    if err := os.MkdirAll(s.dir, 0o755); err != nil { return err }
    initPath := filepath.Join(s.dir, rt.initName)
    if _, err := os.Stat(initPath); err != nil {
        if err := atomicWrite(initPath, rt.init, s.cfg); err != nil { return err }
    }
    return atomicWrite(filepath.Join(s.dir, segName), rt.seg, s.cfg)
}

// cleanup retires the files of segments that left the window and deletes those that have been retired long enough,
// together with init sections nobody uses any more. now is the stream time of the clip that was just added.
func (s *stitcher) cleanup(removed []SegmentMeta, now time.Time) {
    if len(removed) > 0 {
        r := retiredFiles{at: now}
        for _, it := range removed {
            r.names = append(r.names, filepath.Base(it.URI))
            if it.MapURI != "" { r.names = append(r.names, filepath.Base(it.MapURI)) }
        }
        s.retired = append(s.retired, r)
    }
    keep := time.Duration(s.cfg.RetainS) * time.Second
    for len(s.retired) > 0 && now.Sub(s.retired[0].at) >= keep {
        s.deleteRetired(s.retired[0])
        s.retired = s.retired[1:]
    }
}

// deleteRetired removes the files unless the window or a later retirement still refers to them (init sections are shared).
func (s *stitcher) deleteRetired(r retiredFiles) {
    inUse := map[string]bool{}
    for _, it := range s.win.Items { inUse[filepath.Base(it.URI)], inUse[filepath.Base(it.MapURI)] = true, true }
    for _, other := range s.retired[1:] {
        for _, n := range other.names { inUse[n] = true }
    }
    for _, n := range r.names {
        if !inUse[n] { _ = os.Remove(filepath.Join(s.dir, n)) }
    }
}

// removeOrphans deletes files in the variant directory that the restored window does not use (left by an earlier run).
func (s *stitcher) removeOrphans() {
    used := map[string]bool{}
    for _, it := range s.win.Items { used[filepath.Base(it.URI)], used[filepath.Base(it.MapURI)] = true, true }
    entries, err := os.ReadDir(s.dir)
    if err != nil { return }
    for _, e := range entries {
        n := e.Name()
        if e.IsDir() || used[n] || !(strings.HasPrefix(n, "seg_") || strings.HasPrefix(n, "init_")) { continue }
        _ = os.Remove(filepath.Join(s.dir, n))
    }
}

// --- ad break markers ---

// breakTags returns the markers for a clip: the end of the previous ad break and the start of this one.
// They follow the schedule, not what was played: a break filled with slate is still a break.
func (s *stitcher) breakTags(c models.ClipItem) []string {
    mode := s.cfg.SCTE35Tags
    if mode == "" || mode == "off" { return nil }
    var tags []string
    if !s.prevAvail.IsZero() && !s.prevAvail.Equal(c.BreakStart) { // the break that was on the air is over
        tags = append(tags, endMarkers(mode, s.prevAvail, s.prevAvailDur)...)
    }
    // a break starts at its first second; a clip in the middle of one (after a restart) is not a start
    if !c.BreakStart.IsZero() && c.StartAt.Equal(c.BreakStart) {
        tags = append(tags, startMarkers(mode, c.BreakStart, c.BreakDurS, c.SCTE35)...)
    }
    return tags
}

func eventID(start time.Time) uint32 { return uint32(start.Unix()) }

const dateFormat = "2006-01-02T15:04:05.000Z"

func startMarkers(mode string, start time.Time, secs int, given string) []string {
    var out []string
    if mode == "daterange" || mode == "both" {
        payload := ""
        if raw, err := scte35.Parse(given); given != "" && err == nil {
            if _, err := scte35.Decode(raw); err == nil { payload = scte35.Hex(raw) } // a valid message from the schedule is used as it is
        }
        if payload == "" {
            if raw, err := scte35.Out(eventID(start), secs); err == nil { payload = scte35.Hex(raw) }
        }
        out = append(out, fmt.Sprintf(`#EXT-X-DATERANGE:ID="break-%d",START-DATE="%s",PLANNED-DURATION=%d.000,SCTE35-OUT=%s`, eventID(start), start.UTC().Format(dateFormat), secs, payload))
    }
    if mode == "cue" || mode == "both" { out = append(out, fmt.Sprintf("#EXT-X-CUE-OUT:%d", secs)) }
    return out
}

func endMarkers(mode string, start time.Time, secs int) []string {
    var out []string
    if mode == "daterange" || mode == "both" {
        payload := ""
        if raw, err := scte35.In(eventID(start)); err == nil { payload = scte35.Hex(raw) }
        out = append(out, fmt.Sprintf(`#EXT-X-DATERANGE:ID="break-%d",START-DATE="%s",DURATION=%d.000,SCTE35-IN=%s`, eventID(start), start.UTC().Format(dateFormat), secs, payload))
    }
    if mode == "cue" || mode == "both" { out = append(out, "#EXT-X-CUE-IN") }
    return out
}

// --- media playlist ---

func renderMediaPlaylist(cfg config.Config, w Window) string {
    td := int64(math.Ceil(w.maxDuration()))
    if td < int64(cfg.TargetDurationFloor) { td = int64(cfg.TargetDurationFloor) }
    b := &strings.Builder{}
    fmt.Fprintln(b, "#EXTM3U")
    fmt.Fprintln(b, "#EXT-X-VERSION:7")
    fmt.Fprintf(b, "#EXT-X-TARGETDURATION:%d\n", td)
    fmt.Fprintf(b, "#EXT-X-MEDIA-SEQUENCE:%d\n", w.Items[0].Seq)
    fmt.Fprintf(b, "#EXT-X-DISCONTINUITY-SEQUENCE:%d\n", w.DiscSeq)
    curMap := ""
    for i, it := range w.Items {
        if it.Discontinuity { fmt.Fprintln(b, "#EXT-X-DISCONTINUITY") }
        if it.MapURI != "" && (it.MapURI != curMap || it.Discontinuity || i == 0) {
            fmt.Fprintf(b, "#EXT-X-MAP:URI=\"%s\"\n", it.MapURI)
            curMap = it.MapURI
        }
        for _, tag := range it.Tags { fmt.Fprintln(b, tag) }
        fmt.Fprintf(b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", it.ProgramDateTime.UTC().Format("2006-01-02T15:04:05.000Z"))
        fmt.Fprintf(b, "#EXTINF:%.3f,\n", it.Duration)
        fmt.Fprintln(b, it.URI)
    }
    return b.String()
}

func renderMasterPlaylist(cfg config.Config) string {
    b := &strings.Builder{}
    fmt.Fprintln(b, "#EXTM3U")
    fmt.Fprintln(b, "#EXT-X-VERSION:7")
    fmt.Fprintln(b, "#EXT-X-INDEPENDENT-SEGMENTS")
    // single variant: the numbers must describe the sources
    fmt.Fprintf(b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%s,FRAME-RATE=%g,CODECS=\"%s\"\n",
        cfg.VariantBandwidth, cfg.VariantAverageBandwidth, cfg.VariantResolution, cfg.VariantFrameRate, cfg.VariantCodecs)
    fmt.Fprintf(b, "%s.m3u8\n", cfg.VariantID)
    return b.String()
}

// restoreWindow loads the window from a playlist written by an earlier run, so a restart continues the stream.
func restoreWindow(path string, s *stitcher) error {
    f, err := os.Open(path)
    if err != nil { return err }
    defer f.Close()
    var (
        seqBase  int64
        curMap   string
        dur      float64
        pdt      time.Time
        disc     bool
        idx      int64
        tags     []string
    )
    sc := bufio.NewScanner(f)
    sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
    for sc.Scan() {
        line := strings.TrimSpace(sc.Text())
        switch {
        case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
            seqBase, _ = strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
        case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
            s.win.DiscSeq, _ = strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
        case strings.HasPrefix(line, "#EXT-X-MAP:"):
            curMap = hls.Attr(line, "URI")
        case strings.HasPrefix(line, "#EXT-X-DATERANGE:") || strings.HasPrefix(line, "#EXT-X-CUE-"):
            tags = append(tags, line)
        case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY"):
            disc = true
        case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
            pdt, _ = time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"))
        case strings.HasPrefix(line, "#EXTINF:"):
            v := strings.TrimPrefix(line, "#EXTINF:")
            if i := strings.IndexByte(v, ','); i >= 0 { v = v[:i] }
            dur, _ = strconv.ParseFloat(v, 64)
        case line != "" && !strings.HasPrefix(line, "#"):
            if pdt.IsZero() || dur <= 0 { continue }
            s.win.Items = append(s.win.Items, SegmentMeta{Seq: seqBase + idx, Duration: dur, ProgramDateTime: pdt, Discontinuity: disc, URI: line, MapURI: curMap, Tags: tags})
            tags = nil
            idx++
            s.lastStart = pdt
            s.prevEnd = pdt.Add(time.Duration(dur * float64(time.Second)))
            s.nextSeq = seqBase + idx
            disc, pdt, dur = false, time.Time{}, 0
        }
    }
    return sc.Err()
}
