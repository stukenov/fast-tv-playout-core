package metrics

import (
    "net/http"

    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/collectors"
    "github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
    Registry = prometheus.NewRegistry()

    PlayoutPlaylistAge = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "playout_playlist_age_seconds", Help: "HLS playlist age"},
        []string{"channel"},
    )
    PlayoutClipsAhead = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "playout_clips_ahead", Help: "Clip quanta ahead in window"},
        []string{"channel"},
    )
    PlayoutGapCountTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "playout_gap_count_total", Help: "Number of content gaps replaced by slate"},
        []string{"channel"},
    )
    PlayoutAdRequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "playout_ad_requests_total", Help: "Ad pod requests"},
        []string{"channel"},
    )
    PlayoutAdErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "playout_ad_errors_total", Help: "Ad pod errors"},
        []string{"channel","code"},
    )
    PlayoutTimelineAgeSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "playout_timeline_age_seconds", Help: "Age of timeline vs now"},
        []string{"channel"},
    )
    PlayoutLoopLatencyMs = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "playout_loop_latency_ms", Help: "Main loop latency per tick"},
        []string{"channel"},
    )
    PackagerSegmentsAhead = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_segments_ahead", Help: "Segments ahead of wall clock"},
        []string{"channel"},
    )
    PackagerPlaylistAgeSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_playlist_age_seconds", Help: "Seconds since the media playlist was last updated"},
        []string{"channel","variant"},
    )
    PackagerSegmentsInWindow = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_segments_in_window", Help: "Number of segments in current media playlist window"},
        []string{"channel","variant"},
    )
    PackagerFilesystemErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_filesystem_errors_total", Help: "Filesystem operation errors"},
        []string{"channel","code"},
    )
    PackagerDiscontinuitiesTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_discontinuities_total", Help: "Number of discontinuity tags emitted"},
        []string{"channel","variant"},
    )
    PackagerStitchErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_stitch_errors_total", Help: "Clips that could not be resolved to a source segment"},
        []string{"channel","reason"},
    )
    PackagerInitChangesTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_init_changes_total", Help: "Times the init section differs from the previous segment's: the sources were not encoded with the same settings"},
        []string{"channel"},
    )
    PackagerClipsDroppedTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_clips_dropped_total", Help: "Clips that were skipped entirely (even the slate fallback failed)"},
        []string{"channel"},
    )
    EventsPublishedTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "events_published_total", Help: "Events published to SSE and webhooks"},
        []string{"channel","type"},
    )
    AdFillRatio = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "ad_fill_ratio", Help: "Ad fill ratio"},
        []string{"channel"},
    )
)

func init() {
    Registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
    Registry.MustRegister(collectors.NewGoCollector())
    Registry.MustRegister(
        PlayoutPlaylistAge,
        PlayoutClipsAhead,
        PlayoutGapCountTotal,
        PlayoutAdRequestsTotal,
        PlayoutAdErrorsTotal,
        PlayoutTimelineAgeSeconds,
        PlayoutLoopLatencyMs,
        PackagerSegmentsAhead,
        PackagerPlaylistAgeSeconds,
        PackagerSegmentsInWindow,
        PackagerFilesystemErrorsTotal,
        PackagerDiscontinuitiesTotal,
        PackagerStitchErrorsTotal,
        PackagerClipsDroppedTotal,
        PackagerInitChangesTotal,
        AdFillRatio,
        EventsPublishedTotal,
    )
}

func Handler() http.Handler { return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}) }


