package httpx

import (
    "context"
    "errors"
    "net/http"
    "os"
    "os/signal"
    "syscall"
    "time"
)

// SignalContext is cancelled on SIGINT or SIGTERM, which is how Docker stops a container.
func SignalContext() (context.Context, context.CancelFunc) {
    return signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
}

// Addr returns ":PORT" from the PORT variable or the default.
func Addr(def string) string {
    if v := os.Getenv("PORT"); v != "" { return ":" + v }
    return def
}

// Serve runs the handler with sane timeouts and shuts it down when ctx is cancelled.
func Serve(ctx context.Context, addr string, h http.Handler) error {
    srv := &http.Server{
        Addr:              addr,
        Handler:           h,
        ReadHeaderTimeout: 5 * time.Second,
        ReadTimeout:       30 * time.Second,
        WriteTimeout:      60 * time.Second,
        IdleTimeout:       120 * time.Second,
    }
    errc := make(chan error, 1)
    go func() { errc <- srv.ListenAndServe() }()
    select {
    case err := <-errc:
        if errors.Is(err, http.ErrServerClosed) { return nil }
        return err
    case <-ctx.Done():
        shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
        defer cancel()
        return srv.Shutdown(shutdown)
    }
}
