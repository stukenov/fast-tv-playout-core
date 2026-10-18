// Package events tells the outside world what is on the air: a bus that keeps the last events, a Server-Sent
// Events endpoint that resumes after a reconnect, and signed webhooks with retries.
package events

import (
    "encoding/json"
    "fmt"
    "net/http"
    "strconv"
    "sync"
    "time"
)

// Event is something that happened to a channel.
type Event struct {
    ID      int64          `json:"id"`
    Type    string         `json:"type"`
    Time    time.Time      `json:"time"`
    Channel string         `json:"channel"`
    Data    map[string]any `json:"data,omitempty"`
}

// Hub keeps the last events and hands new ones to subscribers. Publishing never waits for a subscriber:
// one that cannot keep up is cut off and picks up again from the last event it saw (it reconnects with Last-Event-ID).
type Hub struct {
    mu     sync.Mutex
    ring   []Event
    cap    int
    nextID int64
    subs   map[*Subscription]struct{}
    now    func() time.Time
}

func NewHub(capacity int) *Hub {
    if capacity < 1 { capacity = 1000 }
    return &Hub{cap: capacity, nextID: 1, subs: map[*Subscription]struct{}{}, now: func() time.Time { return time.Now().UTC() }}
}

// Subscription is a stream of events. C is closed when the subscription ends or when the subscriber was too slow.
type Subscription struct {
    C      chan Event
    hub    *Hub
    closed bool
}

// Publish adds an event and returns it.
func (h *Hub) Publish(typ, channel string, data map[string]any) Event {
    h.mu.Lock()
    defer h.mu.Unlock()
    ev := Event{ID: h.nextID, Type: typ, Time: h.now(), Channel: channel, Data: data}
    h.nextID++
    h.ring = append(h.ring, ev)
    if len(h.ring) > h.cap { h.ring = h.ring[len(h.ring)-h.cap:] }
    for s := range h.subs {
        select {
        case s.C <- ev:
        default: // too slow: cut it off, it can resume from the ring
            s.closeLocked()
        }
    }
    return ev
}

func (s *Subscription) closeLocked() {
    if s.closed { return }
    s.closed = true
    delete(s.hub.subs, s)
    close(s.C)
}

// Close ends the subscription.
func (s *Subscription) Close() {
    s.hub.mu.Lock()
    defer s.hub.mu.Unlock()
    s.closeLocked()
}

// Subscribe returns the events with an id greater than after (those still remembered) and then a live stream.
// The replay and the live stream do not miss or repeat an event.
func (h *Hub) Subscribe(after int64) ([]Event, *Subscription) {
    h.mu.Lock()
    defer h.mu.Unlock()
    var replay []Event
    for _, e := range h.ring { if e.ID > after { replay = append(replay, e) } }
    s := &Subscription{C: make(chan Event, 256), hub: h}
    h.subs[s] = struct{}{}
    return replay, s
}

// Last is the id of the newest event (0 when nothing happened yet).
func (h *Hub) Last() int64 {
    h.mu.Lock()
    defer h.mu.Unlock()
    return h.nextID - 1
}

// ServeSSE streams the events of a channel as Server-Sent Events. A client that reconnects sends Last-Event-ID
// (browsers do it by themselves) and gets what it missed.
func (h *Hub) ServeSSE(channel string) http.HandlerFunc {
    return func(w http.ResponseWriter, r *http.Request) {
        fl, ok := w.(http.Flusher)
        if !ok { http.Error(w, "streaming is not supported", http.StatusInternalServerError); return }
        after := int64(0)
        if v := r.Header.Get("Last-Event-ID"); v != "" {
            after, _ = strconv.ParseInt(v, 10, 64)
        } else if v := r.URL.Query().Get("last_event_id"); v != "" {
            after, _ = strconv.ParseInt(v, 10, 64)
        } else if r.URL.Query().Get("from") == "now" {
            after = h.Last() // only what happens from now on
        }
        w.Header().Set("Content-Type", "text/event-stream")
        w.Header().Set("Cache-Control", "no-cache")
        w.Header().Set("X-Accel-Buffering", "no")
        w.WriteHeader(200)
        replay, sub := h.Subscribe(after)
        defer sub.Close()
        write := func(e Event) bool {
            if e.Channel != channel { return true }
            b, _ := json.Marshal(e)
            _, err := fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", e.ID, e.Type, b)
            fl.Flush()
            return err == nil
        }
        fmt.Fprint(w, ": connected\n\n")
        fl.Flush()
        for _, e := range replay { if !write(e) { return } }
        keepalive := time.NewTicker(15 * time.Second)
        defer keepalive.Stop()
        for {
            select {
            case e, open := <-sub.C:
                if !open { return } // cut off as too slow: the client reconnects and resumes
                if !write(e) { return }
            case <-keepalive.C:
                if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil { return }
                fl.Flush()
            case <-r.Context().Done():
                return
            }
        }
    }
}
