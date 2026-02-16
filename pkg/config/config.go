package config

import (
    "fmt"
    "os"
)

type Config struct {
    APIKey      string
    PostgresDSN string
    RedisAddr   string
    OriginRoot  string
    SpoolRoot   string
    SegmentSec  int
    // control-api specific
    ReadCacheEnabled   bool
    ReadMaxWindowHours int
    // Scheduler-specific
    SchedSegmentSec int
    SchedTargetHorizonHours int
    SchedMinHorizonHours int
    SchedRefreshIntervalSec int
    SchedTrimPastHours int
    AdsURL      string
    ChannelID   string
    VariantID   string
    VariantBandwidth int
    VariantAverageBandwidth int
    VariantResolution string
    VariantFrameRate  float64
    VariantCodecs     string
    PlaylistWindowMin int
    PlaylistWindowMax int
    TargetDurationFloor int
    IOTmpSuffix string
    IOFsync     bool
    PreloadWindowS int
    TickMs         int
    SlateURI       string
    BumperStartURI string
}

func FromEnv() Config {
    c := Config{
        APIKey:      os.Getenv("API_KEY"),
        PostgresDSN: os.Getenv("POSTGRES_DSN"),
        RedisAddr:   getenvDefault("REDIS_ADDR", "redis:6379"),
        OriginRoot:  getenvDefault("ORIGIN_ROOT", "/var/www"),
        SpoolRoot:   getenvDefault("SPOOL_ROOT", "/spool"),
        SegmentSec:  2,
        ReadCacheEnabled: getenvDefault("READ_CACHE_ENABLED", "false") == "true",
        ReadMaxWindowHours: atoi(os.Getenv("READ_MAX_WINDOW_HOURS"), 12),
        SchedSegmentSec: atoi(os.Getenv("SCHED_SEGMENT_SEC"), 2),
        SchedTargetHorizonHours: atoi(os.Getenv("SCHED_TARGET_HORIZON_HOURS"), 12),
        SchedMinHorizonHours: atoi(os.Getenv("SCHED_MIN_HORIZON_HOURS"), 6),
        SchedRefreshIntervalSec: atoi(os.Getenv("SCHED_REFRESH_INTERVAL_SEC"), 60),
        SchedTrimPastHours: atoi(os.Getenv("SCHED_TRIM_PAST_HOURS"), 24),
        AdsURL:      getenvDefault("ADS_URL", "http://ads:8083/v1/ads/pod"),
        ChannelID:   getenvDefault("CHANNEL_ID", "c1"),
        VariantID:   getenvDefault("VARIANT_ID", "v720"),
        VariantBandwidth: atoi(os.Getenv("VARIANT_BANDWIDTH"), 3000000),
        VariantAverageBandwidth: atoi(os.Getenv("VARIANT_AVERAGE_BANDWIDTH"), 2600000),
        VariantResolution: getenvDefault("VARIANT_RESOLUTION", "1280x720"),
        VariantFrameRate: atof(os.Getenv("VARIANT_FRAME_RATE"), 25),
        VariantCodecs: getenvDefault("VARIANT_CODECS", "avc1.4d401f,mp4a.40.2"),
        PlaylistWindowMin: atoi(os.Getenv("PLAYLIST_WINDOW_MIN"), 60),
        PlaylistWindowMax: atoi(os.Getenv("PLAYLIST_WINDOW_MAX"), 90),
        TargetDurationFloor: atoi(os.Getenv("TARGETDURATION_FLOOR"), 2),
        IOTmpSuffix: getenvDefault("TMP_SUFFIX", ".tmp"),
        IOFsync:     getenvDefault("FSYNC", "true") == "true",
        PreloadWindowS: atoi(os.Getenv("PRELOAD_WINDOW_S"), 180),
        TickMs:         atoi(os.Getenv("TICK_MS"), 200),
        SlateURI:       getenvDefault("SLATE_URI", "s3://bucket/slates/default_2s_cmaf.m3u8"),
        BumperStartURI: getenvDefault("BUMPER_START_URI", ""),
    }
    if v := os.Getenv("SEGMENT_SEC"); v != "" { c.SegmentSec = atoi(v, 2) }
    return c
}

func getenvDefault(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func atoi(s string, def int) int { var n int; _, err := fmt.Sscanf(s, "%d", &n); if err != nil { return def }; return n }

func atof(s string, def float64) float64 { var n float64; _, err := fmt.Sscanf(s, "%f", &n); if err != nil { return def }; return n }


