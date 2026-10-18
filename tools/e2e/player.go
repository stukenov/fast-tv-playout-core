package main

import (
    "context"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "sort"
    "strings"
    "sync"
    "time"

    "ftvx/pkg/hls"
)

// collected is a segment as a live player met it.
type collected struct {
    seg    hls.Segment
    seq    int64
    seenAt time.Time // when the playlist that names it was read
    doneAt time.Time // when the segment (and its init section) was downloaded
    first  bool      // it was in the playlist at the very first read: the player joined while it was already on the air
}

// livePlayer behaves like a simple HLS client: it reads the live playlist every 100 ms and downloads each new
// segment at once. What it ends up with is the stream that a viewer gets, and its timings show how much room
// the viewer has: a segment that is downloaded after its air time is a stall.
type livePlayer struct {
    url  string
    base *url.URL

    mu        sync.Mutex
    segs      []*collected
    maxSeq    int64
    cache     map[string][]byte
    polls     int
    pollErrs  int      // failed reads after the first successful one
    pollErrMsg string
    joined    bool     // the first playlist was read
    fetchErrs []string
    seqGaps   int // times the playlist moved past a segment the player had not seen
    maxWindow int
    minWindow int
    changes   []time.Time // when the content of the playlist changed
    lastRaw   string
    tags      map[string]bool // every DATERANGE and CUE line the playlists ever carried
    lastPDT   time.Time
}

func newLivePlayer(playlistURL string) *livePlayer {
    u, _ := url.Parse(playlistURL)
    return &livePlayer{url: playlistURL, base: u, cache: map[string][]byte{}, maxSeq: -1, minWindow: 1 << 30, tags: map[string]bool{}}
}

func (p *livePlayer) run(ctx context.Context) {
    for ctx.Err() == nil {
        p.poll()
        select {
        case <-ctx.Done():
            return
        case <-time.After(100 * time.Millisecond):
        }
    }
}

var playerClient = &http.Client{Timeout: 5 * time.Second}

func (p *livePlayer) get(u string) ([]byte, error) {
    resp, err := playerClient.Get(u)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    b, err := io.ReadAll(resp.Body)
    if err != nil { return nil, err }
    if resp.StatusCode != 200 { return nil, fmt.Errorf("%s: status %d", u, resp.StatusCode) }
    return b, nil
}

func (p *livePlayer) poll() {
    raw, err := p.get(p.url)
    now := time.Now()
    p.mu.Lock()
    p.polls++
    // before the first playlist exists the origin answers 404: that is not a failure of the stream
    if err != nil { if p.joined { p.pollErrs++; p.pollErrMsg = err.Error() }; p.mu.Unlock(); return }
    pl, perr := hls.Parse(strings.NewReader(string(raw)), p.base)
    if perr != nil { if p.joined { p.pollErrs++; p.pollErrMsg = perr.Error() }; p.mu.Unlock(); return }
    firstRead := !p.joined
    p.joined = true
    if string(raw) != p.lastRaw { p.changes = append(p.changes, now); p.lastRaw = string(raw) }
    for _, l := range strings.Split(string(raw), "\n") {
        if l = strings.TrimSpace(l); strings.HasPrefix(l, "#EXT-X-DATERANGE:") || strings.HasPrefix(l, "#EXT-X-CUE-") { p.tags[l] = true }
    }
    n := len(pl.Segments)
    if n > p.maxWindow { p.maxWindow = n }
    if n < p.minWindow { p.minWindow = n }
    if first := pl.MediaSequence; p.maxSeq >= 0 && first > p.maxSeq+1 { p.seqGaps++ }
    maxSeq := p.maxSeq
    p.mu.Unlock()

    for i, sg := range pl.Segments {
        seq := pl.MediaSequence + int64(i)
        if seq <= maxSeq { continue }
        urls := []string{sg.URI}
        if sg.MapURI != "" { urls = append(urls, sg.MapURI) }
        ok := true
        for _, u := range urls {
            p.mu.Lock()
            _, have := p.cache[u]
            p.mu.Unlock()
            if have { continue }
            b, err := p.get(u)
            if err != nil {
                p.mu.Lock()
                p.fetchErrs = append(p.fetchErrs, err.Error())
                p.mu.Unlock()
                ok = false
                break
            }
            p.mu.Lock()
            p.cache[u] = b
            p.mu.Unlock()
        }
        if !ok { return } // like a player: try again with the next playlist
        p.mu.Lock()
        p.segs = append(p.segs, &collected{seg: sg, seq: seq, seenAt: now, doneAt: time.Now(), first: firstRead})
        p.maxSeq = seq
        p.lastPDT = sg.ProgramDateTime
        maxSeq = seq
        p.mu.Unlock()
    }
}

func (p *livePlayer) lastSegmentTime() time.Time {
    p.mu.Lock()
    defer p.mu.Unlock()
    return p.lastPDT
}

// stream is everything the player collected, as one playlist with absolute URLs.
func (p *livePlayer) stream() hls.Playlist {
    p.mu.Lock()
    defer p.mu.Unlock()
    var pl hls.Playlist
    for _, c := range p.segs {
        pl.Segments = append(pl.Segments, c.seg)
        if c.seg.MapURI != "" { pl.MapURI = c.seg.MapURI }
    }
    if len(p.segs) > 0 { pl.MediaSequence = p.segs[0].seq }
    return pl
}

// bytes returns a file the player downloaded (files of the live playlist are deleted a while after they leave it).
func (p *livePlayer) bytes(u string) ([]byte, bool) {
    if p == nil { return nil, false }
    p.mu.Lock()
    defer p.mu.Unlock()
    b, ok := p.cache[u]
    return b, ok
}

// timings reports how much time the viewer has. slack is how long before its air time a segment could be
// downloaded: positive means in time, negative is a stall for a player without a buffer.
type timings struct {
    segments           int
    minSlack, medSlack time.Duration
    maxChangeGap       time.Duration
}

func (p *livePlayer) timings() timings {
    p.mu.Lock()
    defer p.mu.Unlock()
    var slack []time.Duration
    for _, c := range p.segs {
        if c.first { continue } // already on the air when the player joined
        slack = append(slack, c.seg.ProgramDateTime.Sub(c.doneAt))
    }
    sort.Slice(slack, func(i, j int) bool { return slack[i] < slack[j] })
    t := timings{segments: len(slack)}
    if len(slack) > 0 { t.minSlack, t.medSlack = slack[0], slack[len(slack)/2] }
    // the longest time without a new playlist, measured in the part where the stream is steady
    for i := 1; i < len(p.changes); i++ {
        if d := p.changes[i].Sub(p.changes[i-1]); d > t.maxChangeGap && i > 3 { t.maxChangeGap = d }
    }
    return t
}
