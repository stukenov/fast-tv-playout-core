// Command control-api is the API of the system: channels, assets, schedule and commands for the air.
// It needs PostgreSQL only. The engine picks up what it writes.
package main

import (
    "os"
    "strconv"
    "time"

    "ftvx/pkg/auth"
    "ftvx/pkg/config"
    "ftvx/pkg/httpx"
    "ftvx/pkg/logger"
    "ftvx/pkg/storage"
)

func main() {
    lg := logger.Setup()
    cfg := config.FromEnv()
    ctx, stop := httpx.SignalContext()
    defer stop()
    keys, err := auth.Parse(os.Getenv("API_KEYS"), cfg.APIKey)
    if err != nil { lg.Fatal().Err(err).Msg("api keys") }
    pg, err := storage.ConnectPostgresWait(ctx, cfg.PostgresDSN, 60*time.Second)
    if err != nil { lg.Fatal().Err(err).Msg("postgres connect") }
    defer pg.Close()

    cue := 3
    if v, err := strconv.Atoi(os.Getenv("CUE_LEAD_S")); err == nil && v >= 0 { cue = v }
    s := newServer(pg, keys, lg, time.Duration(cue)*time.Second)
    lg.Info().Int("cue_lead_s", cue).Msg("control-api listening")
    if err := httpx.Serve(ctx, httpx.Addr(":8081"), s.routes()); err != nil { lg.Fatal().Err(err).Msg("http") }
}
