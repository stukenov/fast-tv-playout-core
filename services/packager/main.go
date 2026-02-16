package main

import (
    "bufio"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "regexp"
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/rs/zerolog/log"

    "ftvx/pkg/config"
    "ftvx/pkg/metrics"
)

type SegmentMeta struct {
    Seq             int64     `json:"seq"`
    Duration        float64   `json:"duration"`
    ProgramDateTime time.Time `json:"program_date_time"`
    Discontinuity   bool      `json:"discontinuity"`
}

type Window struct {
    FirstSeq int64
    Items    []SegmentMeta
    MaxSize  int
    MinSize  int
}

func (w *Window) append(meta SegmentMeta) (trimmed []SegmentMeta) {
    w.Items = append(w.Items, meta)
    if len(w.Items) == 1 { w.FirstSeq = meta.Seq }
    if len(w.Items) > w.MaxSize {
        trimmed = append(trimmed, w.Items[:len(w.Items)-w.MaxSize]...)
        w.Items = w.Items[len(w.Items)-w.MaxSize:]
        w.FirstSeq = w.Items[0].Seq
    }
    return
}

func (w *Window) maxDuration() float64 {
    max := 0.0
    for _, it := range w.Items {
        if it.Duration > max { max = it.Duration }
    }
    return max
}

var (
    segJSONRe = regexp.MustCompile(`^seg_(\d+)\.json$`)
    segM4sRe  = regexp.MustCompile(`^seg_(\d+)\.m4s$`)
)

func main() {
    cfg := config.FromEnv()

    ready := new(bool)
    *ready = false

    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(15*time.Second))
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
        if *ready { w.WriteHeader(200) } else { w.WriteHeader(503) }
    })
    r.Handle("/metrics", metrics.Handler())

    ctx, cancel := context.WithCancel(context.Background())
    defer cancel()
    go runPackager(ctx, cfg, ready)

    addr := ":8086"
    if v := os.Getenv("PORT"); v != "" { addr = ":" + v }
    _ = http.ListenAndServe(addr, r)
}

func runPackager(ctx context.Context, cfg config.Config, ready *bool) {
    originRoot := filepath.Join(cfg.OriginRoot, "hls", cfg.ChannelID)
    variantDir := filepath.Join(originRoot, cfg.VariantID)
    spoolDir := filepath.Join(cfg.SpoolRoot, cfg.ChannelID, cfg.VariantID)
    _ = os.MkdirAll(variantDir, 0o755)

    // Ensure master exists on start
    if err := writeMasterPlaylist(originRoot, cfg); err != nil {
        log.Error().Err(err).Str("channel", cfg.ChannelID).Msg("fs_error: write master")
        metrics.PackagerFilesystemErrorsTotal.WithLabelValues(cfg.ChannelID, "master_write").Inc()
    }

    // Try to restore window from existing media playlist
    window := Window{MaxSize: cfg.PlaylistWindowMax, MinSize: cfg.PlaylistWindowMin}
    lastSeq, err := restoreWindowFromPlaylist(filepath.Join(originRoot, cfg.VariantID+".m3u8"), &window)
    if err != nil && !errors.Is(err, os.ErrNotExist) {
        log.Warn().Err(err).Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Msg("failed to restore window; starting fresh")
    }
    expectedSeq := lastSeq + 1

    // Ensure init.mp4 exists if present in spool
    ensureInitFromSpool(spoolDir, variantDir, cfg, false)

    *ready = len(window.Items) > 0

    for {
        select {
        case <-ctx.Done():
            return
        default:
        }

        // Find next sidecar
        metaPath := filepath.Join(spoolDir, fmt.Sprintf("seg_%d.json", expectedSeq))
        m4sPath := filepath.Join(spoolDir, fmt.Sprintf("seg_%d.m4s", expectedSeq))
        metaInfo, metaErr := os.Stat(metaPath)
        if metaErr != nil {
            // update spool lag metric if possible
            if maxSeq := findMaxSeqInSpool(spoolDir); maxSeq > 0 && expectedSeq > 0 {
                lag := float64(maxSeq - (expectedSeq - 1))
                metrics.PackagerSpoolLagSegments.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(lag)
            }
            time.Sleep(200 * time.Millisecond)
            continue
        }
        if _, err := os.Stat(m4sPath); err != nil {
            time.Sleep(50 * time.Millisecond)
            continue
        }

        // Read sidecar JSON
        sc, err := os.ReadFile(metaPath)
        if err != nil {
            metrics.PackagerFilesystemErrorsTotal.WithLabelValues(cfg.ChannelID, "sidecar_read").Inc()
            log.Error().Err(err).Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Int64("seq", expectedSeq).Msg("fs_error: read sidecar")
            time.Sleep(100 * time.Millisecond)
            continue
        }
        var meta SegmentMeta
        if err := json.Unmarshal(sc, &meta); err != nil {
            log.Error().Err(err).Msg("invalid sidecar json")
            time.Sleep(100 * time.Millisecond)
            continue
        }
        // Verify sequence is monotonic
        if meta.Seq != expectedSeq {
            // Gap or out-of-order. Do not advance playlist, just wait.
            log.Warn().Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Int64("expected", expectedSeq).Int64("got", meta.Seq).Msg("spool_gap_detected")
            metrics.PackagerSpoolLagSegments.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(float64(meta.Seq-(expectedSeq-1)))
            time.Sleep(200 * time.Millisecond)
            continue
        }

        // Ensure init.mp4 exists (and re-copy on discontinuity)
        if meta.Discontinuity { metrics.PackagerDiscontinuitiesTotal.WithLabelValues(cfg.ChannelID, cfg.VariantID).Inc() }
        if err := ensureInitFromSpool(spoolDir, variantDir, cfg, meta.Discontinuity); err != nil {
            log.Error().Err(err).Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Msg("fs_error: ensure init")
            metrics.PackagerFilesystemErrorsTotal.WithLabelValues(cfg.ChannelID, "init_copy").Inc()
        }

        // Publish segment atomically
        finalSegRel := fmt.Sprintf("%s/seg_%d.m4s", cfg.VariantID, meta.Seq)
        finalSegAbs := filepath.Join(originRoot, finalSegRel)
        if err := atomicCopy(m4sPath, finalSegAbs, cfg); err != nil {
            metrics.PackagerFilesystemErrorsTotal.WithLabelValues(cfg.ChannelID, "segment_write").Inc()
            log.Error().Err(err).Str("file", finalSegAbs).Msg("fs_error: write segment")
            time.Sleep(100 * time.Millisecond)
            continue
        }
        // Update write bytes metric
        if st, err := os.Stat(finalSegAbs); err == nil {
            metrics.PackagerWriteBytesTotal.WithLabelValues(cfg.ChannelID).Add(float64(st.Size()))
        }

        // Update window
        trimmed := window.append(meta)
        if len(trimmed) > 0 {
            for _, old := range trimmed {
                // Remove old segment files
                _ = os.Remove(filepath.Join(originRoot, fmt.Sprintf("%s/seg_%d.m4s", cfg.VariantID, old.Seq)))
            }
            log.Info().Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Int("trimmed", len(trimmed)).Msg("window_trimmed")
        }

        // Render and publish media playlist
        startWrite := time.Now()
        if err := writeMediaPlaylist(originRoot, cfg, window); err != nil {
            metrics.PackagerFilesystemErrorsTotal.WithLabelValues(cfg.ChannelID, "media_write").Inc()
            log.Error().Err(err).Msg("fs_error: write media playlist")
        } else {
            latency := time.Since(metaInfo.ModTime()).Seconds()
            metrics.PackagerPublishLatencySeconds.WithLabelValues(cfg.ChannelID, cfg.VariantID).Observe(latency)
            metrics.PackagerSegmentsInWindow.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(float64(len(window.Items)))
            lastPDT := window.Items[len(window.Items)-1].ProgramDateTime
            metrics.PackagerPlaylistAgeSeconds.WithLabelValues(cfg.ChannelID, cfg.VariantID).Set(time.Since(lastPDT).Seconds())
            log.Info().Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Int64("seq", meta.Seq).Int("latency_ms", int(time.Since(startWrite).Milliseconds())).Msg("playlist_updated")
            *ready = true
        }

        expectedSeq++
    }
}

func writeMasterPlaylist(originRoot string, cfg config.Config) error {
    b := &strings.Builder{}
    fmt.Fprintln(b, "#EXTM3U")
    fmt.Fprintln(b, "#EXT-X-VERSION:7")
    fmt.Fprintln(b, "#EXT-X-INDEPENDENT-SEGMENTS")
    // Single-variant MVP
    fmt.Fprintf(b, "#EXT-X-STREAM-INF:BANDWIDTH=%d,AVERAGE-BANDWIDTH=%d,RESOLUTION=%s,FRAME-RATE=%g,CODECS=\"%s\"\n",
        cfg.VariantBandwidth, cfg.VariantAverageBandwidth, cfg.VariantResolution, cfg.VariantFrameRate, cfg.VariantCodecs)
    fmt.Fprintf(b, "%s.m3u8\n", cfg.VariantID)
    path := filepath.Join(originRoot, "master.m3u8")
    return atomicWrite(path, []byte(b.String()), cfg)
}

func writeMediaPlaylist(originRoot string, cfg config.Config, w Window) error {
    if len(w.Items) == 0 { return nil }
    maxDur := w.maxDuration()
    if maxDur < float64(cfg.TargetDurationFloor) { maxDur = float64(cfg.TargetDurationFloor) }
    if maxDur > 4 { maxDur = 4 }
    td := int64(time.Duration(maxDur*1000+0.999) * time.Millisecond / time.Second) // ceil

    b := &strings.Builder{}
    fmt.Fprintln(b, "#EXTM3U")
    fmt.Fprintln(b, "#EXT-X-VERSION:7")
    fmt.Fprintf(b, "#EXT-X-TARGETDURATION:%d\n", td)
    fmt.Fprintf(b, "#EXT-X-MEDIA-SEQUENCE:%d\n", w.FirstSeq)
    fmt.Fprintf(b, "#EXT-X-MAP:URI=\"%s/init.mp4\"\n", cfg.VariantID)
    for _, it := range w.Items {
        if it.Discontinuity { fmt.Fprintln(b, "#EXT-X-DISCONTINUITY") }
        fmt.Fprintf(b, "#EXT-X-PROGRAM-DATE-TIME:%s\n", it.ProgramDateTime.UTC().Format(time.RFC3339Nano))
        fmt.Fprintf(b, "#EXTINF:%.3f,\n", it.Duration)
        fmt.Fprintf(b, "%s/seg_%d.m4s\n", cfg.VariantID, it.Seq)
    }
    path := filepath.Join(originRoot, cfg.VariantID+".m3u8")
    return atomicWrite(path, []byte(b.String()), cfg)
}

func restoreWindowFromPlaylist(mediaPath string, w *Window) (lastSeq int64, err error) {
    f, err := os.Open(mediaPath)
    if err != nil { return 0, err }
    defer f.Close()
    var seqBase int64 = 0
    scanner := bufio.NewScanner(f)
    var pendingDur float64
    var pendingPDT time.Time
    var pendingDisc bool
    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())
        if strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:") {
            v := strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:")
            if n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64); err == nil { seqBase = n }
        } else if strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:") {
            v := strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:")
            if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(v)); err == nil { pendingPDT = t }
        } else if strings.HasPrefix(line, "#EXTINF:") {
            v := strings.TrimPrefix(line, "#EXTINF:")
            v = strings.TrimSuffix(v, ",")
            if f64, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil { pendingDur = f64 }
        } else if strings.HasPrefix(line, "#EXT-X-DISCONTINUITY") {
            pendingDisc = true
        } else if segM4sRe.MatchString(filepath.Base(line)) {
            // derive seq from filename
            base := filepath.Base(line)
            m := segM4sRe.FindStringSubmatch(base)
            if len(m) == 2 {
                if n, err := strconv.ParseInt(m[1], 10, 64); err == nil {
                    meta := SegmentMeta{Seq: n, Duration: pendingDur, ProgramDateTime: pendingPDT, Discontinuity: pendingDisc}
                    w.append(meta)
                    pendingDisc = false
                }
            }
        }
    }
    if len(w.Items) > 0 {
        lastSeq = w.Items[len(w.Items)-1].Seq
    } else if seqBase > 0 {
        lastSeq = seqBase - 1
    }
    return lastSeq, nil
}

func atomicWrite(path string, data []byte, cfg config.Config) error {
    tmp := path + cfg.IOTmpSuffix
    f, err := os.Create(tmp)
    if err != nil { return err }
    if _, err := f.Write(data); err != nil { f.Close(); return err }
    if cfg.IOFsync {
        _ = f.Sync()
    }
    if err := f.Close(); err != nil { return err }
    return os.Rename(tmp, path)
}

func atomicCopy(src, dst string, cfg config.Config) error {
    if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil { return err }
    in, err := os.Open(src)
    if err != nil { return err }
    defer in.Close()
    tmp := dst + cfg.IOTmpSuffix
    out, err := os.Create(tmp)
    if err != nil { return err }
    if _, err := io.Copy(out, in); err != nil { out.Close(); return err }
    if cfg.IOFsync { _ = out.Sync() }
    if err := out.Close(); err != nil { return err }
    return os.Rename(tmp, dst)
}

func ensureInitFromSpool(spoolDir, variantDir string, cfg config.Config, force bool) error {
    src := filepath.Join(spoolDir, "init.mp4")
    if _, err := os.Stat(src); err != nil { return nil }
    dst := filepath.Join(variantDir, "init.mp4")
    if _, err := os.Stat(dst); err == nil && !force { return nil }
    // overwrite via atomic rename: write to tmp then rename to dst
    tmp := dst + cfg.IOTmpSuffix
    if err := atomicCopy(src, tmp, cfg); err != nil { return err }
    return os.Rename(tmp, dst)
}

func findMaxSeqInSpool(spoolDir string) int64 {
    entries, err := os.ReadDir(spoolDir)
    if err != nil { return 0 }
    var seqs []int64
    for _, e := range entries {
        if e.IsDir() { continue }
        name := e.Name()
        if m := segJSONRe.FindStringSubmatch(name); len(m) == 2 {
            if n, err := strconv.ParseInt(m[1], 10, 64); err == nil { seqs = append(seqs, n) }
        }
    }
    if len(seqs) == 0 { return 0 }
    sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
    return seqs[len(seqs)-1]
}

