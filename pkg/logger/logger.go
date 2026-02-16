package logger

import (
    "os"
    "strings"

    "github.com/rs/zerolog"
    "github.com/rs/zerolog/log"
)

func Setup() zerolog.Logger {
    levelStr := strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL")))
    level := zerolog.InfoLevel
    if lvl, err := zerolog.ParseLevel(levelStr); err == nil {
        level = lvl
    }
    zerolog.SetGlobalLevel(level)
    log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stdout})
    return log.Logger
}


