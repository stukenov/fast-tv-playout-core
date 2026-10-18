package config

import (
    "fmt"
    "os"
)

// SegmentSec is the length of every segment of the stream, in seconds: exactly one, never more and never less.
// It is a rule of the system, not a setting: the schedule, the ads, the sources and the playlist all count in
// whole seconds of this length.
const SegmentSec = 1

type Config struct {
    APIKey      string
    PostgresDSN string
    OriginRoot  string
    SegmentSec  int
    AdsURL      string // an ad server that is asked for the pod of every break (see x-ads-webhook in api/openapi.yaml)
    AdsRegistryFile string // or a registry file the engine fills breaks from itself
    AdsAPIKey   string // sent to AdsURL as X-API-Key
    ChannelID   string
    VariantID   string
    VariantBandwidth int
    VariantAverageBandwidth int
    VariantResolution string
    VariantFrameRate  float64
    VariantCodecs     string
    PlaylistWindowMax int
    TargetDurationFloor int
    IOTmpSuffix string
    IOFsync     bool
    PreloadWindowS int
    TickMs         int
    SlateURI       string
    // playout
    PublishAheadS int
    // packager / stitcher
    SourceFetchMap   string  // "s3://bucket/=http://minio:9000/bucket/,..." where the packager downloads source playlists
    SourceTimeoutMs  int
    DurationTolerance float64 // allowed difference between a source segment and SEGMENT_SEC, seconds
    LiveLeadS        int     // how far ahead of wall clock the playlist edge is published
    RetainS          int     // how long the file of a segment stays after it left the playlist
    SCTE35Tags       string  // "daterange" (default), "cue", "both" or "off": how ad breaks are marked in the playlist
    SourceFileRoot   string  // sources may be file:// URIs under this folder (empty: files are refused)
    SourceAllowHosts string  // comma separated hosts[:port] that http(s) sources may be on (empty: any)
}

func FromEnv() Config {
    c := Config{
        APIKey:      os.Getenv("API_KEY"),
        PostgresDSN: os.Getenv("POSTGRES_DSN"),
        OriginRoot:  getenvDefault("ORIGIN_ROOT", "/var/www"),
        SegmentSec:  SegmentSec,
        AdsURL:      os.Getenv("ADS_URL"),
        AdsRegistryFile: os.Getenv("ADS_REGISTRY_FILE"),
        AdsAPIKey:   os.Getenv("ADS_API_KEY"),
        ChannelID:   getenvDefault("CHANNEL_ID", "c1"),
        VariantID:   getenvDefault("VARIANT_ID", "v720"),
        VariantBandwidth: atoi(os.Getenv("VARIANT_BANDWIDTH"), 3000000),
        VariantAverageBandwidth: atoi(os.Getenv("VARIANT_AVERAGE_BANDWIDTH"), 2600000),
        VariantResolution: getenvDefault("VARIANT_RESOLUTION", "1280x720"),
        VariantFrameRate: atof(os.Getenv("VARIANT_FRAME_RATE"), 25),
        VariantCodecs: getenvDefault("VARIANT_CODECS", "avc1.4d401f,mp4a.40.2"),
        PlaylistWindowMax: atoi(os.Getenv("PLAYLIST_WINDOW_MAX"), 180),
        TargetDurationFloor: atoi(os.Getenv("TARGETDURATION_FLOOR"), 1),
        IOTmpSuffix: getenvDefault("TMP_SUFFIX", ".tmp"),
        IOFsync:     getenvDefault("FSYNC", "true") == "true",
        PreloadWindowS: atoi(os.Getenv("PRELOAD_WINDOW_S"), 180),
        TickMs:         atoi(os.Getenv("TICK_MS"), 500),
        SlateURI:       getenvDefault("SLATE_URI", "s3://bucket/slates/default_2s_cmaf.m3u8"),
        PublishAheadS:  atoi(os.Getenv("PUBLISH_AHEAD_S"), 30),
        SourceFetchMap:  os.Getenv("SOURCE_FETCH_MAP"),
        SourceTimeoutMs: atoi(os.Getenv("SOURCE_TIMEOUT_MS"), 3000),
        DurationTolerance: atof(os.Getenv("SOURCE_DURATION_TOLERANCE_S"), 0.01),
        LiveLeadS:       atoi(os.Getenv("LIVE_LEAD_S"), 6),
        RetainS:         atoi(os.Getenv("SEGMENT_RETAIN_S"), 30),
        SCTE35Tags:      getenvDefault("SCTE35_TAGS", "daterange"),
        SourceFileRoot:  os.Getenv("SOURCE_FILE_ROOT"),
        SourceAllowHosts: os.Getenv("SOURCE_ALLOW_HOSTS"),
    }
    return c
}

func getenvDefault(k, d string) string { if v := os.Getenv(k); v != "" { return v }; return d }

func atoi(s string, def int) int { var n int; _, err := fmt.Sscanf(s, "%d", &n); if err != nil { return def }; return n }

func atof(s string, def float64) float64 { var n float64; _, err := fmt.Sscanf(s, "%f", &n); if err != nil { return def }; return n }


