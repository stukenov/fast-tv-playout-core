package main

import (
    "context"
    "fmt"
    "os"
    "path/filepath"
    "sync"
    "sync/atomic"
    "time"

    "github.com/rs/zerolog"

    "ftvx/pkg/config"
    "ftvx/pkg/events"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
)

// Schedule is what the engine needs from the store of the schedule. PostgreSQL implements it.
type Schedule interface {
    // GetTimelineRange returns the entries that overlap [from,to).
    GetTimelineRange(ctx context.Context, channelID string, from, to time.Time) ([]models.TimelineEntry, error)
}

type engineState struct {
    mu             sync.Mutex
    nowSeq         int64
    windowLen      int
    lastTimelineAt time.Time
    adState        string
    gaps           int
    segments       int
    lastPlaylistAt time.Time
}

func (s *engineState) snapshot() models.ChannelStatus {
    s.mu.Lock()
    defer s.mu.Unlock()
    return models.ChannelStatus{NowSeq: s.nowSeq, WindowLen: s.windowLen, LastTimelineAt: s.lastTimelineAt, AdState: s.adState, Gaps: s.gaps,
        Segments: s.segments, LastPlaylistAt: s.lastPlaylistAt}
}

// engine plays one channel: every tick it plans the next seconds from the schedule, puts the clips whose
// time has come into the live playlist, and writes the playlist. Nothing that has not been released yet is final,
// so a change of the schedule is picked up until a few seconds before it airs.
type engine struct {
    cfg       config.Config
    lg        zerolog.Logger
    sched     Schedule
    res       *resolver
    st        *stitcher
    state     *engineState
    pods      podSource
    entries   []models.TimelineEntry // the last schedule that was read successfully, aired through a database outage
    entriesAt time.Time
    entriesTo time.Time // the end of the window that entries was read for
    lastTried time.Time // the start of the last clip that was handed to the stitcher, whether it worked or not
    mediaPath string
    lastWrite time.Time
    ready     atomic.Bool
    hub        *events.Hub
    sender     *events.Sender
    onAir      onAir
    announcedTo time.Time
    failedAt   map[string]time.Time
    staleAt    time.Time
}

// newPods chooses where ad pods come from: an ad server, a registry file, or nowhere (breaks are filled with slate).
func newPods(cfg config.Config, lg zerolog.Logger) podSource {
    switch {
    case cfg.AdsURL != "": return newAdBroker(cfg, lg)
    case cfg.AdsRegistryFile != "": return newStaticFiller(cfg, lg)
    }
    return noAds{}
}

func newEngine(cfg config.Config, lg zerolog.Logger, sched Schedule, res *resolver) *engine {
    root := filepath.Join(cfg.OriginRoot, "hls", cfg.ChannelID)
    e := &engine{
        cfg: cfg, lg: lg, sched: sched, res: res,
        st:        newStitcher(cfg, res, filepath.Join(root, cfg.VariantID)),
        state:     &engineState{},
        mediaPath: filepath.Join(root, cfg.VariantID+".m3u8"),
        lastWrite: time.Now(),
        hub:       events.NewHub(1000),
        failedAt:  map[string]time.Time{},
    }
    e.pods = notifyingPods{podSource: newPods(cfg, lg), onGiveUp: func(start time.Time) { e.publish("ads.fallback", map[string]any{"break_start": start}) }}
    e.st.onFail = e.sourceFailed
    return e
}

// prepare creates the output, restores the window of an earlier run and checks the slate.
func (e *engine) prepare(ctx context.Context) error {
    root := filepath.Dir(e.mediaPath)
    if err := os.MkdirAll(root, 0o755); err != nil { return err }
    if err := atomicWrite(filepath.Join(root, "master.m3u8"), []byte(renderMasterPlaylist(e.cfg)), e.cfg); err != nil {
        metrics.PackagerFilesystemErrorsTotal.WithLabelValues(e.cfg.ChannelID, "master_write").Inc()
        e.lg.Error().Err(err).Msg("write master playlist")
    }
    if err := restoreWindow(e.mediaPath, e.st); err != nil && !os.IsNotExist(err) {
        e.lg.Warn().Err(err).Msg("cannot restore the window, starting fresh")
        e.st = newStitcher(e.cfg, e.res, e.st.dir)
        e.st.onFail = e.sourceFailed
    }
    e.st.removeOrphans()
    e.st.onFail = e.sourceFailed
    if n := len(e.st.win.Items); n > 0 {
        e.lg.Info().Int("segments", n).Msg("window restored")
        e.announcedTo = e.st.win.Items[n-1].ProgramDateTime // what was already on the air before the restart is not announced again
    }
    e.publish("engine.started", map[string]any{"channel": e.cfg.ChannelID, "variant": e.cfg.VariantID, "window": e.cfg.PlaylistWindowMax, "lead_s": e.cfg.LiveLeadS})
    if e.cfg.SlateURI != "" {
        // the slate is what airs when anything else fails, so it must be at hand: download it now
        pl, err := e.res.playlist(ctx, e.cfg.SlateURI)
        if err != nil {
            e.lg.Warn().Err(err).Str("slate", e.cfg.SlateURI).Msg("slate is not available, gaps cannot be filled")
        } else {
            for i, sg := range pl.Segments {
                if i >= 60 { break }
                e.res.prefetchBlob(ctx, sg.URI, maxSegmentBytes)
            }
            if pl.MapURI != "" { go func() { _, _ = e.res.initSection(ctx, pl.MapURI) }() }
        }
    }
    return nil
}

func (e *engine) run(ctx context.Context) {
    ticker := time.NewTicker(time.Duration(e.cfg.TickMs) * time.Millisecond)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case t := <-ticker.C:
            e.tick(ctx, t.UTC())
        }
    }
}

// tick is one step of the engine at the given moment.
func (e *engine) tick(ctx context.Context, now time.Time) {
    started := time.Now()
    cfg := e.cfg
    seg := time.Duration(cfg.SegmentSec) * time.Second
    slot := now.Truncate(seg)

    ahead := time.Duration(cfg.PublishAheadS) * time.Second
    if ahead <= 0 { ahead = time.Duration(cfg.PreloadWindowS) * time.Second }
    entries := e.entries
    // The schedule is read at most twice a second, and never for longer than the stream can wait: a slow database
    // must not stop the playlist. The channel keeps airing the last schedule it knows.
    if e.entriesAt.IsZero() || now.Sub(e.entriesAt) >= scheduleRefresh || e.entriesTo.Before(slot.Add(ahead)) {
        qctx, cancel := context.WithTimeout(ctx, scheduleTimeout)
        read, err := e.sched.GetTimelineRange(qctx, cfg.ChannelID, slot, slot.Add(time.Duration(cfg.PreloadWindowS)*time.Second))
        cancel()
        if err != nil {
            e.lg.Error().Err(err).Str("channel", cfg.ChannelID).Msg("read schedule, airing the last known one")
            if now.Sub(e.staleAt) >= 30*time.Second { // once in 30 seconds, not on every tick
                e.staleAt = now
                e.publish("schedule.stale", map[string]any{"error": err.Error(), "last_ok": e.entriesAt})
            }
        } else {
            entries = read
            e.entries, e.entriesAt, e.entriesTo = read, now, slot.Add(time.Duration(cfg.PreloadWindowS)*time.Second)
        }
    }
    if !e.entriesAt.IsZero() { metrics.PlayoutTimelineAgeSeconds.WithLabelValues(cfg.ChannelID).Set(now.Sub(e.entriesAt).Seconds()) }

    plan := buildPlan(slot, cfg, entries, e.pods, e.lg)
    e.pods.evict(slot)
    metrics.PlayoutClipsAhead.WithLabelValues(cfg.ChannelID).Set(float64(len(plan)))

    // Clips of the next seconds are downloaded ahead of time, so that releasing one is a copy in memory.
    prefetchUntil := now.Add(prefetchAhead)
    seen := map[string]bool{}
    for _, c := range plan {
        if c.StartAt.After(prefetchUntil) { break }
        key := fmt.Sprintf("%s#%d", c.URI, c.OffsetS)
        if !seen[key] { seen[key] = true; e.res.prefetchClip(ctx, cfg, c) }
    }

    // A clip goes into the playlist when its time comes, minus the lead that gives the player a buffer.
    // Clips that are not released yet may still change with the next tick. A source that does not answer within
    // the budget is replaced by the slate, and the tick as a whole never takes longer than tickBudget.
    release := now.Add(time.Duration(cfg.LiveLeadS) * time.Second)
    appended := 0
    for _, c := range plan {
        if c.StartAt.After(release) || !c.StartAt.After(e.lastTried) { continue }
        if time.Since(started) > tickBudget { break } // the rest follows on the next tick
        e.lastTried = c.StartAt
        cctx, cancel := context.WithTimeout(ctx, e.fetchBudget())
        ok := e.st.add(cctx, c)
        cancel()
        if ok { appended++ }
    }
    if appended > 0 { e.writePlaylist(now, appended) }
    e.announce(now)

    gaps, adState := 0, "none"
    for _, it := range plan {
        if it.Type == "slate" { gaps++ }
        if it.Type == "ad" { adState = "ad" }
    }
    segsAhead := 0
    for _, it := range e.st.win.Items { if it.ProgramDateTime.After(now) { segsAhead++ } }
    if len(e.st.win.Items) > 0 {
        metrics.PackagerSegmentsAhead.WithLabelValues(cfg.ChannelID).Set(float64(segsAhead))
        metrics.PackagerSegmentsInWindow.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(float64(len(e.st.win.Items)))
        metrics.PackagerPlaylistAgeSeconds.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(time.Since(e.lastWrite).Seconds())
    }
    s := e.state
    s.mu.Lock()
    s.nowSeq = quantSeq(slot, cfg.SegmentSec)
    s.windowLen, s.gaps, s.adState, s.segments = len(plan), gaps, adState, len(e.st.win.Items)
    if len(entries) > 0 { s.lastTimelineAt = time.Now() }
    s.mu.Unlock()
    metrics.PlayoutLoopLatencyMs.WithLabelValues(cfg.ChannelID).Set(float64(time.Since(started).Milliseconds()))
}

const (
    scheduleRefresh = 500 * time.Millisecond
    scheduleTimeout = 700 * time.Millisecond
    prefetchAhead   = 10 * time.Second
    tickBudget      = 1 * time.Second
)

// fetchBudget is how long the engine waits for the network for one clip: half the lead, but not less than 300 ms
// and not more than the configured timeout of a source.
func (e *engine) fetchBudget() time.Duration {
    ms := e.cfg.LiveLeadS * 500
    if ms < 300 { ms = 300 }
    if e.cfg.SourceTimeoutMs > 0 && ms > e.cfg.SourceTimeoutMs { ms = e.cfg.SourceTimeoutMs }
    return time.Duration(ms) * time.Millisecond
}

func (e *engine) writePlaylist(now time.Time, appended int) {
    if err := atomicWrite(e.mediaPath, []byte(renderMediaPlaylist(e.cfg, e.st.win)), e.cfg); err != nil {
        metrics.PackagerFilesystemErrorsTotal.WithLabelValues(e.cfg.ChannelID, "media_write").Inc()
        e.lg.Error().Err(err).Msg("write media playlist")
        return
    }
    e.lastWrite = time.Now()
    e.ready.Store(true)
    e.state.mu.Lock()
    e.state.lastPlaylistAt = e.lastWrite
    e.state.mu.Unlock()
    e.lg.Debug().Int("appended", appended).Int("window", len(e.st.win.Items)).Msg("playlist updated")
}

func atomicWrite(path string, data []byte, cfg config.Config) error {
    tmp := path + cfg.IOTmpSuffix
    f, err := os.Create(tmp)
    if err != nil { return err }
    if _, err := f.Write(data); err != nil { f.Close(); return err }
    if cfg.IOFsync { _ = f.Sync() }
    if err := f.Close(); err != nil { return err }
    return os.Rename(tmp, path)
}
