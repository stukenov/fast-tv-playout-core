// Command engine plays one channel: it turns the schedule into a live HLS stream of one second segments.
// It needs a schedule store (PostgreSQL) and the sources; it writes files and tells what is on the air (SSE, webhooks).
package main

import (
    "context"
    "net/http"
    "os"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"

    "ftvx/pkg/auth"
    "ftvx/pkg/config"
    "ftvx/pkg/events"
    "ftvx/pkg/httpx"
    "ftvx/pkg/logger"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
    "ftvx/pkg/storage"
)

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    ctx, stop := httpx.SignalContext()
    defer stop()
    keys, err := auth.Parse(os.Getenv("API_KEYS"), cfg.APIKey)
    if err != nil { lg.Fatal().Err(err).Msg("api keys") }
    endpoints, err := events.ParseEndpoints(os.Getenv("WEBHOOK_URLS"))
    if err != nil { lg.Fatal().Err(err).Msg("webhooks") }
    pg, err := storage.ConnectPostgresWait(ctx, cfg.PostgresDSN, 60*time.Second)
    if err != nil { lg.Fatal().Err(err).Msg("postgres connect") }
    defer pg.Close()

    e := newEngine(cfg, lg, pg, newResolver(cfg))
    if err := e.prepare(ctx); err != nil { lg.Fatal().Err(err).Msg("prepare") }
    if len(endpoints) > 0 {
        e.sender = &events.Sender{Lg: lg}
        e.sender.Run(e.hub, endpoints, ctx.Done())
        lg.Info().Int("endpoints", len(endpoints)).Msg("webhooks on")
    }

    go e.run(ctx)

    lg.Info().Str("channel", cfg.ChannelID).Str("variant", cfg.VariantID).Msg("engine started")
    if err := httpx.Serve(ctx, httpx.Addr(":8085"), e.routes(keys, pg.Ping)); err != nil { lg.Fatal().Err(err).Msg("http") }
}

type webhookStats struct {
    Delivered int64 `json:"delivered"`
    Failed    int64 `json:"failed"`
    Dropped   int64 `json:"dropped"`
}

type statusReply struct {
    models.ChannelStatus
    Webhooks *webhookStats `json:"webhooks,omitempty"`
}

func (e *engine) status() statusReply {
    r := statusReply{ChannelStatus: e.state.snapshot()}
    if e.sender != nil { r.Webhooks = &webhookStats{Delivered: e.sender.Delivered(), Failed: e.sender.Failed(), Dropped: e.sender.Dropped()} }
    return r
}

// routes is the HTTP interface of the engine: health and metrics are open, status and events need a key that may read the channel.
func (e *engine) routes(keys *auth.Keys, ping func(context.Context) error) http.Handler {
    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", func(w http.ResponseWriter, r *http.Request) {
        c, cancel := context.WithTimeout(r.Context(), 500*time.Millisecond)
        defer cancel()
        if err := ping(c); err != nil { httpx.WriteProblem(w, 503, "unready", "postgres unavailable"); return }
        if !e.ready.Load() { httpx.WriteProblem(w, 503, "unready", "no playlist written yet"); return }
        w.WriteHeader(200)
    })
    r.Method("GET", "/metrics", metrics.Handler())

    channel := func(r *http.Request) string { return chi.URLParam(r, "channel_id") }
    read := keys.Require(auth.Read, channel)
    mine := func(next http.HandlerFunc) http.HandlerFunc { // the engine plays one channel
        return func(w http.ResponseWriter, r *http.Request) {
            if chi.URLParam(r, "channel_id") != e.cfg.ChannelID { httpx.WriteProblem(w, 404, "Not Found", "this engine does not play that channel"); return }
            next(w, r)
        }
    }
    r.With(read).Get("/v1/channels/{channel_id}/status", mine(func(w http.ResponseWriter, r *http.Request) { httpx.WriteJSON(w, 200, e.status()) }))
    r.With(read).Get("/v1/channels/{channel_id}/events", mine(e.hub.ServeSSE(e.cfg.ChannelID)))
    return r
}
