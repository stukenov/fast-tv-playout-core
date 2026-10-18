package events

import (
    "bytes"
    "crypto/hmac"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "strings"
    "sync/atomic"
    "time"

    "github.com/rs/zerolog"
)

// Endpoint is a webhook receiver. With a Secret every request carries X-FTVX-Signature: sha256=<HMAC-SHA256 of the body>.
type Endpoint struct {
    URL    string
    Secret string
}

// ParseEndpoints reads "url|secret;url|secret" (the secret is optional).
func ParseEndpoints(s string) ([]Endpoint, error) {
    var out []Endpoint
    for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == '\n' }) {
        part = strings.TrimSpace(part)
        if part == "" { continue }
        url, secret, _ := strings.Cut(part, "|")
        url = strings.TrimSpace(url)
        if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") { return nil, fmt.Errorf("webhook %q: the URL must start with http:// or https://", url) }
        out = append(out, Endpoint{URL: url, Secret: strings.TrimSpace(secret)})
    }
    return out, nil
}

// Sign returns the value of X-FTVX-Signature for a body.
func Sign(secret string, body []byte) string {
    m := hmac.New(sha256.New, []byte(secret))
    m.Write(body)
    return "sha256=" + hex.EncodeToString(m.Sum(nil))
}

// Sender delivers the events of a hub to webhook endpoints, in order, per endpoint. A delivery that fails is retried
// with growing pauses; a 4xx answer (except 408 and 429) is final. An endpoint that stays down does not hold the others:
// each has its own queue, and when a queue is full the oldest event is dropped and counted.
type Sender struct {
    Retries int           // attempts after the first (default 5)
    Backoff time.Duration // the pause before the first retry, doubled every time (default 1 s)
    Timeout time.Duration // per request (default 5 s)
    Client  *http.Client
    Lg      zerolog.Logger

    delivered atomic.Int64
    failed    atomic.Int64
    dropped   atomic.Int64
}

// Delivered, Failed and Dropped count events over all endpoints.
func (s *Sender) Delivered() int64 { return s.delivered.Load() }
func (s *Sender) Failed() int64    { return s.failed.Load() }
func (s *Sender) Dropped() int64   { return s.dropped.Load() }

func (s *Sender) defaults() {
    if s.Retries == 0 { s.Retries = 5 }
    if s.Backoff == 0 { s.Backoff = time.Second }
    if s.Timeout == 0 { s.Timeout = 5 * time.Second }
    if s.Client == nil { s.Client = &http.Client{Timeout: s.Timeout} }
}

// Run subscribes to the hub and delivers until stop is closed. Events that happened before Run are not sent.
func (s *Sender) Run(h *Hub, endpoints []Endpoint, stop <-chan struct{}) {
    s.defaults()
    last := h.Last()
    replay, sub := h.Subscribe(last)
    queues := make([]chan Event, len(endpoints))
    for i, ep := range endpoints {
        queues[i] = make(chan Event, 512)
        go s.worker(ep, queues[i], stop)
    }
    push := func(ev Event) {
        last = ev.ID
        for _, q := range queues {
            select {
            case q <- ev:
            default:
                // the queue is full: drop the oldest event to make room
                select {
                case <-q: s.dropped.Add(1)
                default:
                }
                select {
                case q <- ev:
                default: s.dropped.Add(1)
                }
            }
        }
    }
    go func() {
        defer func() { sub.Close() }()
        for _, ev := range replay { push(ev) }
        for {
            select {
            case ev, ok := <-sub.C:
                if !ok {
                    // A burst filled the buffer of the subscription and the hub cut it off. Nothing is lost:
                    // take up again after the last event that was handed on.
                    replay, sub = h.Subscribe(last)
                    for _, ev := range replay { push(ev) }
                    continue
                }
                push(ev)
            case <-stop:
                return
            }
        }
    }()
}

func (s *Sender) worker(ep Endpoint, q <-chan Event, stop <-chan struct{}) {
    for {
        select {
        case ev := <-q:
            if err := s.deliver(ep, ev, stop); err != nil {
                s.failed.Add(1)
                s.Lg.Error().Err(err).Str("url", ep.URL).Int64("event", ev.ID).Str("type", ev.Type).Msg("webhook not delivered")
            } else {
                s.delivered.Add(1)
            }
        case <-stop:
            return
        }
    }
}

var errFinal = errors.New("the receiver refused the event")

func (s *Sender) deliver(ep Endpoint, ev Event, stop <-chan struct{}) error {
    body, _ := json.Marshal(ev)
    pause := s.Backoff
    var last error
    for attempt := 0; attempt <= s.Retries; attempt++ {
        if attempt > 0 {
            select {
            case <-time.After(pause):
            case <-stop:
                return errors.New("stopped")
            }
            pause *= 2
        }
        req, err := http.NewRequest(http.MethodPost, ep.URL, bytes.NewReader(body))
        if err != nil { return err }
        req.Header.Set("Content-Type", "application/json")
        req.Header.Set("X-FTVX-Event", ev.Type)
        req.Header.Set("X-FTVX-Delivery", fmt.Sprint(ev.ID))
        if ep.Secret != "" { req.Header.Set("X-FTVX-Signature", Sign(ep.Secret, body)) }
        resp, err := s.Client.Do(req)
        if err != nil { last = err; continue }
        _, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
        resp.Body.Close()
        switch {
        case resp.StatusCode >= 200 && resp.StatusCode < 300:
            return nil
        case resp.StatusCode == 408 || resp.StatusCode == 429 || resp.StatusCode >= 500:
            last = fmt.Errorf("status %d", resp.StatusCode)
        default:
            return fmt.Errorf("%w: status %d", errFinal, resp.StatusCode)
        }
    }
    return fmt.Errorf("gave up after %d attempts: %w", s.Retries+1, last)
}
