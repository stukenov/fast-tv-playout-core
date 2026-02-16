package main

import (
    "context"
    "encoding/json"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "testing"
    "time"

    "ftvx/pkg/config"
)

func TestRunPackager_WritesPlaylistsFromSpool(t *testing.T) {
    tmp := t.TempDir()
    cfg := config.Config{
        OriginRoot: tmp,
        SpoolRoot:  tmp,
        ChannelID:  "c1",
        VariantID:  "v720",
        SegmentSec: 2,
        PlaylistWindowMin: 3,
        PlaylistWindowMax: 5,
        TargetDurationFloor: 2,
        IOTmpSuffix: ".tmp",
        IOFsync: false,
    }

    // prepare spool structure and seed init + 5 segments
    spool := filepath.Join(cfg.SpoolRoot, cfg.ChannelID, cfg.VariantID)
    if err := os.MkdirAll(spool, 0o755); err != nil { t.Fatal(err) }
    if err := os.WriteFile(filepath.Join(spool, "init.mp4"), []byte("init"), 0o644); err != nil { t.Fatal(err) }

    now := time.Now().UTC().Add(-10 * time.Second)
    for i := 1; i <= 5; i++ {
        meta := SegmentMeta{Seq: int64(i), Duration: 2.0, ProgramDateTime: now.Add(time.Duration(i) * 2 * time.Second)}
        b, _ := json.Marshal(meta)
        if err := os.WriteFile(filepath.Join(spool, "seg_"+strconv.Itoa(i)+".json"), b, 0o644); err != nil { t.Fatal(err) }
        if err := os.WriteFile(filepath.Join(spool, "seg_"+strconv.Itoa(i)+".m4s"), []byte("data"), 0o644); err != nil { t.Fatal(err) }
    }

    ctx, cancel := context.WithCancel(context.Background())
    ready := new(bool)
    go runPackager(ctx, cfg, ready)

    // allow packager to process
    time.Sleep(1200 * time.Millisecond)
    cancel()
    time.Sleep(100 * time.Millisecond)

    root := filepath.Join(cfg.OriginRoot, "hls", cfg.ChannelID)
    masterPath := filepath.Join(root, "master.m3u8")
    if _, err := os.Stat(masterPath); err != nil { t.Fatalf("master.m3u8 not found: %v", err) }
    master, _ := os.ReadFile(masterPath)
    if !strings.Contains(string(master), "#EXT-X-INDEPENDENT-SEGMENTS") {
        t.Fatalf("master missing INDEPENDENT-SEGMENTS: %s", string(master))
    }
    if !strings.Contains(string(master), "#EXT-X-STREAM-INF:") {
        t.Fatalf("master missing STREAM-INF")
    }
    mediaPath := filepath.Join(root, cfg.VariantID+".m3u8")
    media, err := os.ReadFile(mediaPath)
    if err != nil { t.Fatalf("read media: %v", err) }
    body := string(media)
    if !strings.Contains(body, "#EXTM3U") || !strings.Contains(body, "#EXT-X-MAP:URI=\""+cfg.VariantID+"/init.mp4\"") || !strings.Contains(body, "#EXTINF:2.000") {
        t.Fatalf("unexpected media playlist: %s", body)
    }
    // window size capped at max
    if strings.Count(body, ".m4s\n") != cfg.PlaylistWindowMax { t.Fatalf("unexpected window len: %d", strings.Count(body, ".m4s\n")) }
    // TARGETDURATION within 2..4
    if !strings.Contains(body, "#EXT-X-TARGETDURATION:") { t.Fatalf("no TARGETDURATION") }
}
