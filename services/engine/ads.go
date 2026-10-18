package main

import (
    "sync"
    "time"

    "github.com/rs/zerolog"

    "ftvx/pkg/config"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
)

type podState int

const (
    podReady   podState = iota // the pod is known (it may be empty: the break is filled with slate)
    podPending                 // the request is on its way
    podFailed                  // the last request failed, another one follows
)

// podSource gives the planner the ad pod of a break without ever making it wait for the network.
type podSource interface {
    // pod returns the pod of the break. due says that the break is about to be released: then the call may
    // wait a moment (dueWait) for an answer that is on its way.
    pod(e models.TimelineEntry, due bool) ([]models.AdItem, podState)
    // giveUp decides, once, that the break is filled with slate: a pod that arrives later is not used in the middle of it.
    giveUp(start time.Time)
    // evict forgets breaks that are long over.
    evict(now time.Time)
}

const (
    adRetryEvery = 1 * time.Second
    dueWait      = 300 * time.Millisecond
)

type podEntry struct {
    items    []models.AdItem
    state    podState
    done     chan struct{} // closed when the request in flight is over
    failedAt time.Time
}

// adBroker asks the ads service for pods in the background, so that a slow or dead ads service never stops the stream.
type adBroker struct {
    cfg  config.Config
    lg   zerolog.Logger
    mu   sync.Mutex
    pods map[time.Time]*podEntry
}

func newAdBroker(cfg config.Config, lg zerolog.Logger) *adBroker {
    return &adBroker{cfg: cfg, lg: lg, pods: map[time.Time]*podEntry{}}
}

// seed sets a ready pod (tests, and breaks whose pod is known from elsewhere).
func (b *adBroker) seed(start time.Time, items []models.AdItem) {
    b.mu.Lock()
    defer b.mu.Unlock()
    done := make(chan struct{})
    close(done)
    b.pods[start] = &podEntry{items: items, state: podReady, done: done}
}

func (b *adBroker) pod(e models.TimelineEntry, due bool) ([]models.AdItem, podState) {
    b.mu.Lock()
    en := b.pods[e.Start]
    if en == nil {
        en = &podEntry{state: podPending, done: make(chan struct{})}
        b.pods[e.Start] = en
        b.fetch(e, en)
    } else if en.state == podFailed && time.Since(en.failedAt) >= adRetryEvery {
        en.state, en.done = podPending, make(chan struct{})
        b.fetch(e, en)
    }
    done := en.done
    b.mu.Unlock()
    if due {
        select {
        case <-done:
        case <-time.After(dueWait):
        }
    }
    b.mu.Lock()
    defer b.mu.Unlock()
    return en.items, en.state
}

// fetch starts the request; the caller holds the lock.
func (b *adBroker) fetch(e models.TimelineEntry, en *podEntry) {
    done := en.done
    go func() {
        metrics.PlayoutAdRequestsTotal.WithLabelValues(b.cfg.ChannelID).Inc()
        resp, err := callAds(b.cfg.AdsURL, b.cfg.ChannelID, e, b.cfg.APIKey)
        b.mu.Lock()
        if err != nil {
            metrics.PlayoutAdErrorsTotal.WithLabelValues(b.cfg.ChannelID, "request_error").Inc()
            b.lg.Warn().Err(err).Str("channel", b.cfg.ChannelID).Time("break", e.Start).Msg("ads request failed")
            if en.state == podPending { en.state, en.failedAt = podFailed, time.Now() }
        } else if en.state == podPending {
            en.items, en.state = resp.Items, podReady
        }
        b.mu.Unlock()
        close(done)
    }()
}

func (b *adBroker) giveUp(start time.Time) {
    b.mu.Lock()
    defer b.mu.Unlock()
    en := b.pods[start]
    if en == nil { en = &podEntry{done: make(chan struct{})}; close(en.done); b.pods[start] = en }
    en.items, en.state = nil, podReady
}

// evict forgets breaks that are long over.
func (b *adBroker) evict(now time.Time) {
    cutoff := now.Add(-time.Hour)
    b.mu.Lock()
    defer b.mu.Unlock()
    for k := range b.pods { if k.Before(cutoff) { delete(b.pods, k) } }
}
