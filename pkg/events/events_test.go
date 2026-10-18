package events

import (
    "bufio"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/http/httptest"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "github.com/rs/zerolog"
)

func TestHub_ReplayAndLiveWithoutGapsOrRepeats(t *testing.T) {
    h := NewHub(100)
    for i := 0; i < 5; i++ { h.Publish("a", "c1", map[string]any{"n": i}) }
    replay, sub := h.Subscribe(2) // saw 1 and 2
    defer sub.Close()
    if len(replay) != 3 || replay[0].ID != 3 || replay[2].ID != 5 { t.Fatalf("replay: %+v", replay) }
    h.Publish("b", "c1", nil)
    select {
    case e := <-sub.C:
        if e.ID != 6 || e.Type != "b" { t.Fatalf("live: %+v", e) }
    case <-time.After(time.Second): t.Fatal("no live event")
    }
    if h.Last() != 6 { t.Fatalf("last %d", h.Last()) }
}

func TestHub_RingKeepsOnlyTheLast(t *testing.T) {
    h := NewHub(3)
    for i := 0; i < 10; i++ { h.Publish("x", "c", nil) }
    replay, sub := h.Subscribe(0)
    defer sub.Close()
    if len(replay) != 3 || replay[0].ID != 8 { t.Fatalf("%+v", replay) }
}

// Publishing never waits for a subscriber.
func TestHub_ASlowSubscriberIsCutOffAndOthersAreNotHeldBack(t *testing.T) {
    h := NewHub(1000)
    _, slow := h.Subscribe(0) // nobody reads from it
    _, fast := h.Subscribe(0)
    defer fast.Close()
    var got atomic.Int64
    go func() { for range fast.C { got.Add(1) } }()
    started := time.Now()
    for i := 0; i < 600; i++ {
        h.Publish("x", "c", nil)
        // a reader that keeps up: it is given the time to take what is there before the next burst of 100
        if i%100 == 99 { for deadline := time.Now().Add(time.Second); got.Load() < int64(i+1) && time.Now().Before(deadline); { time.Sleep(time.Millisecond) } }
    }
    if time.Since(started) > 2*time.Second { t.Fatal("publishing was held back by a subscriber") }
    // the slow one's channel is closed after its buffer filled
    n := 0
    for range slow.C { n++ }
    if n != 256 { t.Fatalf("the slow subscriber got %d events before it was cut off (buffer 256)", n) }
    time.Sleep(50 * time.Millisecond)
    if got.Load() != 600 { t.Fatalf("the fast subscriber got %d of 600", got.Load()) }
}

func readSSE(t *testing.T, r io.Reader, n int) []Event {
    t.Helper()
    sc := bufio.NewScanner(r)
    var out []Event
    var id, typ, data string
    for sc.Scan() {
        line := sc.Text()
        switch {
        case strings.HasPrefix(line, "id: "): id = strings.TrimPrefix(line, "id: ")
        case strings.HasPrefix(line, "event: "): typ = strings.TrimPrefix(line, "event: ")
        case strings.HasPrefix(line, "data: "): data = strings.TrimPrefix(line, "data: ")
        case line == "":
            if data != "" {
                var e Event
                if err := json.Unmarshal([]byte(data), &e); err != nil { t.Fatalf("bad data %q: %v", data, err) }
                if fmt.Sprint(e.ID) != id || e.Type != typ { t.Fatalf("id/event lines do not match the data: %s %s %+v", id, typ, e) }
                out = append(out, e)
                if len(out) == n { return out }
            }
            id, typ, data = "", "", ""
        }
    }
    return out
}

func TestSSE_ReplayLiveAndResume(t *testing.T) {
    h := NewHub(100)
    h.Publish("old", "c1", nil)
    h.Publish("other", "c2", nil) // another channel: never shown
    h.Publish("old2", "c1", nil)
    srv := httptest.NewServer(h.ServeSSE("c1"))
    defer srv.Close()

    resp, err := http.Get(srv.URL)
    if err != nil { t.Fatal(err) }
    if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" { t.Fatalf("content type %q", ct) }
    go func() { time.Sleep(100 * time.Millisecond); h.Publish("live", "c1", map[string]any{"k": "v"}) }()
    got := readSSE(t, resp.Body, 3)
    resp.Body.Close()
    if len(got) != 3 || got[0].Type != "old" || got[1].Type != "old2" || got[2].Type != "live" || got[2].Data["k"] != "v" { t.Fatalf("%+v", got) }

    // a client that reconnects with Last-Event-ID gets exactly what it missed
    h.Publish("missed", "c1", nil)
    req, _ := http.NewRequest("GET", srv.URL, nil)
    req.Header.Set("Last-Event-ID", fmt.Sprint(got[2].ID))
    resp2, err := http.DefaultClient.Do(req)
    if err != nil { t.Fatal(err) }
    defer resp2.Body.Close()
    again := readSSE(t, resp2.Body, 1)
    if len(again) != 1 || again[0].Type != "missed" { t.Fatalf("%+v", again) }

    // from=now: nothing old
    go func() { time.Sleep(100 * time.Millisecond); h.Publish("fresh", "c1", nil) }()
    resp3, err := http.Get(srv.URL + "?from=now")
    if err != nil { t.Fatal(err) }
    defer resp3.Body.Close()
    if now := readSSE(t, resp3.Body, 1); len(now) != 1 || now[0].Type != "fresh" { t.Fatalf("%+v", now) }
}

// --- webhooks ---

type receiver struct {
    srv   *httptest.Server
    mu    sync.Mutex
    got   []Event
    sigs  []string
    fail  atomic.Int32 // fail this many requests with 503 before accepting
    code  atomic.Int32 // answer this code instead (when not 0)
    calls atomic.Int32
}

func newReceiver(t *testing.T) *receiver {
    r := &receiver{}
    r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
        r.calls.Add(1)
        body, _ := io.ReadAll(req.Body)
        if r.fail.Add(-1) >= 0 { w.WriteHeader(503); return }
        if c := r.code.Load(); c != 0 { w.WriteHeader(int(c)); return }
        var e Event
        _ = json.Unmarshal(body, &e)
        r.mu.Lock()
        r.got = append(r.got, e)
        r.sigs = append(r.sigs, req.Header.Get("X-FTVX-Signature")+"|"+Sign("topsecret", body))
        r.mu.Unlock()
        w.WriteHeader(204)
    }))
    t.Cleanup(r.srv.Close)
    return r
}

func (r *receiver) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.got) }

func waitFor(t *testing.T, what string, cond func() bool) {
    t.Helper()
    deadline := time.Now().Add(5 * time.Second)
    for time.Now().Before(deadline) {
        if cond() { return }
        time.Sleep(10 * time.Millisecond)
    }
    t.Fatalf("timeout waiting for %s", what)
}

func fastSender() *Sender { return &Sender{Retries: 4, Backoff: 5 * time.Millisecond, Timeout: time.Second, Lg: zerolog.Nop()} }

func TestWebhook_DeliversInOrderAndSigned(t *testing.T) {
    h := NewHub(100)
    rc := newReceiver(t)
    s := fastSender()
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: rc.srv.URL, Secret: "topsecret"}}, stop)
    for i := 0; i < 20; i++ { h.Publish("tick", "c1", map[string]any{"n": i}) }
    waitFor(t, "20 events", func() bool { return rc.count() == 20 })
    rc.mu.Lock()
    defer rc.mu.Unlock()
    for i, e := range rc.got {
        if e.Data["n"] != float64(i) { t.Fatalf("event %d arrived as n=%v: the order was not kept", i, e.Data["n"]) }
    }
    for _, pair := range rc.sigs {
        a, b, _ := strings.Cut(pair, "|")
        if a != b || !strings.HasPrefix(a, "sha256=") { t.Fatalf("signature %q does not match the body", a) }
    }
}

func TestWebhook_RetriesServerErrorsAndKeepsTheOrder(t *testing.T) {
    h := NewHub(100)
    rc := newReceiver(t)
    rc.fail.Store(3) // the first three requests get a 503
    s := fastSender()
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: rc.srv.URL, Secret: "topsecret"}}, stop)
    h.Publish("first", "c1", nil)
    h.Publish("second", "c1", nil)
    waitFor(t, "both events", func() bool { return rc.count() == 2 })
    rc.mu.Lock()
    defer rc.mu.Unlock()
    if rc.got[0].Type != "first" || rc.got[1].Type != "second" { t.Fatalf("order after a retry: %+v", rc.got) }
    if s.Failed() != 0 || s.Delivered() != 2 { t.Fatalf("delivered %d failed %d", s.Delivered(), s.Failed()) }
}

func TestWebhook_ARefusalIsFinalAndAnEndpointThatIsDownGivesUp(t *testing.T) {
    h := NewHub(100)
    refusing := newReceiver(t)
    refusing.code.Store(400)
    down := newReceiver(t)
    down.srv.Close() // nobody listens
    s := fastSender()
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: refusing.srv.URL}, {URL: down.srv.URL}}, stop)
    h.Publish("x", "c1", nil)
    waitFor(t, "both to fail", func() bool { return s.Failed() == 2 })
    if refusing.calls.Load() != 1 { t.Fatalf("a 400 is not retried, got %d calls", refusing.calls.Load()) }
}

func TestWebhook_OneBadEndpointDoesNotHoldTheOthers(t *testing.T) {
    h := NewHub(100)
    good := newReceiver(t)
    bad := newReceiver(t)
    bad.fail.Store(1000)
    s := &Sender{Retries: 20, Backoff: 200 * time.Millisecond, Timeout: time.Second, Lg: zerolog.Nop()}
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: bad.srv.URL}, {URL: good.srv.URL, Secret: "topsecret"}}, stop)
    for i := 0; i < 30; i++ { h.Publish("x", "c1", nil) }
    waitFor(t, "the good endpoint to get everything while the bad one is still retrying", func() bool { return good.count() == 30 })
}

func TestWebhook_AFullQueueDropsTheOldestAndKeepsTheNewest(t *testing.T) {
    h := NewHub(5000)
    stuck := newReceiver(t)
    stuck.fail.Store(1 << 30)
    s := &Sender{Retries: 100000, Backoff: 20 * time.Millisecond, Timeout: time.Second, Lg: zerolog.Nop()}
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: stuck.srv.URL}}, stop)
    for i := 0; i < 2000; i++ { h.Publish("x", "c1", nil) }
    waitFor(t, "events to be dropped", func() bool { return s.Dropped() > 0 })
    stuck.fail.Store(0) // the receiver is back
    waitFor(t, "the newest event to arrive", func() bool {
        stuck.mu.Lock()
        defer stuck.mu.Unlock()
        return len(stuck.got) > 0 && stuck.got[len(stuck.got)-1].ID == 2000
    })
    stuck.mu.Lock()
    defer stuck.mu.Unlock()
    if n := len(stuck.got); n > 520 { t.Fatalf("%d events arrived: the queue holds 512 at most", n) }
    if stuck.got[1].ID < 1400 { t.Fatalf("after the first (in flight) event the oldest ones must have been dropped, but event %d came next", stuck.got[1].ID) }
}

func TestParseEndpoints(t *testing.T) {
    eps, err := ParseEndpoints("https://a.example/hook|s3cret; http://b.example/x ;")
    if err != nil || len(eps) != 2 || eps[0].Secret != "s3cret" || eps[1].URL != "http://b.example/x" || eps[1].Secret != "" { t.Fatalf("%+v %v", eps, err) }
    if _, err := ParseEndpoints("ftp://nope"); err == nil { t.Fatal("only http and https") }
    if eps, _ := ParseEndpoints(""); len(eps) != 0 { t.Fatal("empty means none") }
    _ = context.Background
}

// A burst larger than the buffer of a subscription cuts the sender off; it must take up again where it was and lose nothing.
func TestWebhook_ABurstLosesNothing(t *testing.T) {
    h := NewHub(5000)
    r := newReceiver(t)
    s := fastSender()
    stop := make(chan struct{})
    defer close(stop)
    s.Run(h, []Endpoint{{URL: r.srv.URL}}, stop)
    for i := 0; i < 400; i++ { h.Publish("x", "c1", nil) }
    waitFor(t, "all 400 events", func() bool { return r.count() == 400 })
    if s.Dropped() != 0 { t.Fatalf("%d events dropped", s.Dropped()) }
}
