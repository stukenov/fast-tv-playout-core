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
    // Packager-specific metrics per spec
    PackagerPlaylistAgeSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_playlist_age_seconds", Help: "now() - PDT of last segment in playlist"},
        []string{"channel","variant"},
    )
    PackagerSegmentsInWindow = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_segments_in_window", Help: "Number of segments in current media playlist window"},
        []string{"channel","variant"},
    )
    PackagerPublishLatencySeconds = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{Name: "packager_publish_latency_seconds", Help: "Latency from sidecar appearance to playlist update", Buckets: prometheus.DefBuckets},
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
    PackagerSpoolLagSegments = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "packager_spool_lag_segments", Help: "(last seq in spool) - (last in playlist)"},
        []string{"channel","variant"},
    )
    PackagerWriteBytesTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "packager_write_bytes_total", Help: "Bytes written to origin"},
        []string{"channel"},
    )
    AdFillRatio = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "ad_fill_ratio", Help: "Ad fill ratio"},
        []string{"channel"},
    )
    VastErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "vast_errors_total", Help: "VAST errors (legacy)"},
        []string{"code"},
    )

    // Ads service specific metrics per spec
    AdsPodRequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "ads_pod_requests_total", Help: "Ad pod requests"},
        []string{"channel"},
    )
    AdsPodLatencySeconds = prometheus.NewSummaryVec(
        prometheus.SummaryOpts{Name: "ads_pod_latency_seconds", Help: "Latency of /v1/ads/pod", Objectives: map[float64]float64{0.5: 0.01, 0.95: 0.005, 0.99: 0.001}},
        []string{"channel"},
    )
    AdsHousePadSecondsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "ads_house_pad_seconds_total", Help: "House padding seconds added"},
        []string{"channel"},
    )
    VastRequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "vast_requests_total", Help: "VAST requests by result and code"},
        []string{"result","code"},
    )
    AdsRegistryCreatives = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "ads_registry_creatives", Help: "Creatives by type and status"},
        []string{"type","status"},
    )

    // Scheduler metrics
    SchedulerLastSuccessTimestampSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "scheduler_last_success_timestamp_seconds", Help: "Unix timestamp of last successful generation"},
        []string{"channel"},
    )
    SchedulerHorizonSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "scheduler_horizon_seconds", Help: "Seconds from now() to horizon covered"},
        []string{"channel"},
    )
    SchedulerGenerationDurationSeconds = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{Name: "scheduler_generation_duration_seconds", Help: "Generation phase duration", Buckets: prometheus.DefBuckets},
        []string{"channel","phase"},
    )
    SchedulerGapCountTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "scheduler_gap_count_total", Help: "Slate gap insertions"},
        []string{"channel"},
    )
    SchedulerLockContentionTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "scheduler_lock_contention_total", Help: "Failed lock attempts"},
        []string{"channel"},
    )

    // EPG metrics (MVP)
    EPGNowNextRequestsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "epg_nownext_requests_total", Help: "Now/Next requests"},
        []string{"channel"},
    )
    EPGNowNextLatencySeconds = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{Name: "epg_nownext_latency_seconds", Help: "Latency of Now/Next handler", Buckets: prometheus.DefBuckets},
        []string{"channel"},
    )
    EPGNowNextAgeSeconds = prometheus.NewGaugeVec(
        prometheus.GaugeOpts{Name: "epg_nownext_age_seconds", Help: "server_time - now.started_at"},
        []string{"channel"},
    )
    EPGTimelinePullErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "epg_timeline_pull_errors_total", Help: "Timeline source pull errors"},
        []string{"source","channel","code"},
    )
    EPGXMLTVGenerateSeconds = prometheus.NewHistogram(
        prometheus.HistogramOpts{Name: "epg_xmltv_generate_seconds", Help: "XMLTV generation time", Buckets: prometheus.DefBuckets},
    )
    EPGXMLTVPublishErrorsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "epg_xmltv_publish_errors_total", Help: "XMLTV publish errors"},
        []string{"target"},
    )
    EPGScheduleCacheHitsTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "epg_schedule_cache_hits_total", Help: "Schedule cache hits"},
        []string{"channel"},
    )
    EPGScheduleCacheMissesTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{Name: "epg_schedule_cache_misses_total", Help: "Schedule cache misses"},
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
        PackagerPublishLatencySeconds,
        PackagerFilesystemErrorsTotal,
        PackagerDiscontinuitiesTotal,
        PackagerSpoolLagSegments,
        PackagerWriteBytesTotal,
        AdFillRatio,
        VastErrorsTotal,
        AdsPodRequestsTotal,
        AdsPodLatencySeconds,
        AdsHousePadSecondsTotal,
        VastRequestsTotal,
        AdsRegistryCreatives,
        SchedulerLastSuccessTimestampSeconds,
        SchedulerHorizonSeconds,
        SchedulerGenerationDurationSeconds,
        SchedulerGapCountTotal,
        SchedulerLockContentionTotal,
        EPGNowNextRequestsTotal,
        EPGNowNextLatencySeconds,
        EPGNowNextAgeSeconds,
        EPGTimelinePullErrorsTotal,
        EPGXMLTVGenerateSeconds,
        EPGXMLTVPublishErrorsTotal,
        EPGScheduleCacheHitsTotal,
        EPGScheduleCacheMissesTotal,
    )
}

func Handler() http.Handler { return promhttp.HandlerFor(Registry, promhttp.HandlerOpts{}) }


