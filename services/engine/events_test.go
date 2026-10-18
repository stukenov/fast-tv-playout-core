package main

import (
    "bufio"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "testing"
    "time"

    "github.com/go-chi/chi/v5"
    "gopkg.in/yaml.v3"

    "ftvx/pkg/auth"
    "ftvx/pkg/events"
)

// published returns the events so far as "type" or "type:detail" strings, and the events themselves.
func published(r *rig) ([]string, []events.Event) {
    replay, sub := r.e.hub.Subscribe(0)
    sub.Close()
    var names []string
    for _, ev := range replay { names = append(names, ev.Type) }
    return names, replay
}

func TestEvents_AProgramBreakAndSlateAreAnnouncedWhenTheyAir(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 5), r.adBreak(5, 3), r.content("a", 8, 4)) // then nothing: the slate
    r.ticks(0, 6) // released up to second 12, but only second 6 is on the air
    names, evs := published(r)
    // the engine started, the movie began at second 0, the break at second 5: nothing that is still in the future
    if strings.Join(names, " ") != "engine.started program.started break.started" {
        t.Fatalf("at second 6: %v", names)
    }
    if got := evs[1].Data["title"]; got != nil && got != r.srv.URL+"/vod/a/index.m3u8" { t.Fatalf("title %v", got) }
    if fmt.Sprint(evs[2].Data["duration"]) != "3" { t.Fatalf("the break lasts 3 s: %v", evs[2].Data) }
    r.ticks(7, 16)
    names, evs = published(r)
    want := "engine.started program.started break.started break.ended program.started slate.started"
    if strings.Join(names, " ") != want { t.Fatalf("at second 16:\n got %v\nwant %s", names, want) }
    // the time of the event is the time of the segment, not of the tick that wrote it
    at := func(i int) string { b, _ := json.Marshal(evs[i].Data["at"]); return strings.Trim(string(b), `"`) }
    if at(1) != r.at(0).Format(time.RFC3339) || at(2) != r.at(5).Format(time.RFC3339) || at(3) != r.at(8).Format(time.RFC3339) || at(4) != r.at(8).Format(time.RFC3339) || at(5) != r.at(12).Format(time.RFC3339) {
        t.Fatalf("times: %s %s %s %s %s", at(1), at(2), at(3), at(4), at(5))
    }
    // the slate ends when something airs again (what was released already, up to second 22, cannot change: schedule beyond it)
    r.sched.set(r.content("a", 0, 5), r.adBreak(5, 3), r.content("a", 8, 4), r.content("a", 26, 4))
    r.ticks(17, 26)
    names, _ = published(r)
    if tail := strings.Join(names[len(names)-2:], " "); tail != "slate.ended program.started" { t.Fatalf("tail: %v", names) }
}

func TestEvents_ASourceThatFailsIsReportedOnceAndTheSlateStarts(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 5), r.content("nope", 5, 20))
    count := func(typ string) int {
        _, evs := published(r)
        n := 0
        for _, ev := range evs { if ev.Type == typ { n++ } }
        return n
    }
    r.ticks(0, 6) // the clips of seconds 5..12 are tried and fail
    if got := count("source.error"); got != 1 { names, _ := published(r); t.Fatalf("%d source.error events (one per ten seconds of clips): %v", got, names) }
    names, evs := published(r)
    // the failure is announced as soon as it is found (seconds before the slate airs); the program and the slate at their air time
    if strings.Join(names, " ") != "engine.started source.error program.started slate.started" {
        t.Fatalf("the slate that airs in place of the source must be announced as a slate, not as the program: %v", names)
    }
    if evs[1].Data["reason"] != "fetch" { t.Fatalf("%v", evs[1].Data) }
    // ten seconds later the source is still down: a second report
    r.ticks(7, 24)
    if got := count("source.error"); got != 2 { t.Fatalf("%d source.error events after the second ten seconds, want 2", got) }
}

func TestEvents_ScheduleStaleIsReportedOncePerHalfMinute(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 3)
    r.sched.fail(errors.New("db down"))
    r.ticks(4, 25)
    names, _ := published(r)
    n := 0
    for _, x := range names { if x == "schedule.stale" { n++ } }
    if n != 1 { t.Fatalf("%d schedule.stale events in 22 seconds: %v", n, names) }
    r.ticks(26, 40)
    names, _ = published(r)
    n = 0
    for _, x := range names { if x == "schedule.stale" { n++ } }
    if n != 2 { t.Fatalf("%d after 36 seconds, want 2", n) }
}

func TestEvents_ABreakWithoutAPodIsReported(t *testing.T) {
    r := newRig(t)
    r.ads.set(0, true) // the ad server is down
    r.sched.set(r.content("a", 0, 5), r.adBreak(5, 3), r.content("a", 8, 10))
    r.ticks(0, 10)
    names, _ := published(r)
    if !strings.Contains(strings.Join(names, " "), "ads.fallback") { t.Fatalf("%v", names) }
}

func TestEvents_ARestartDoesNotAnnounceWhatAlreadyAired(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 10) // the playlist holds up to second 16
    again := newEngine(r.e.cfg, r.e.lg, r.sched, newResolver(r.e.cfg))
    if err := again.prepare(context.Background()); err != nil { t.Fatal(err) }
    r.e = again
    r.ticks(11, 15)
    names, _ := published(r)
    // seconds 11..16 were already in the playlist: they are not announced again
    if strings.Join(names, " ") != "engine.started" { t.Fatalf("%v", names) }
    r.ticks(16, 18)
    names, _ = published(r)
    // the first segment after the restart airs: the engine does not know what came before, so it announces the program
    if strings.Join(names, " ") != "engine.started program.started" { t.Fatalf("%v", names) }
}

// --- the HTTP interface of the engine ---

func engineServer(t *testing.T, r *rig) (*httptest.Server, *auth.Keys) {
    t.Helper()
    keys, err := auth.Parse("viewer=viewer-secret:read:*;other=other-secret:read:c9", "")
    if err != nil { t.Fatal(err) }
    srv := httptest.NewServer(r.e.routes(keys, func(context.Context) error { return nil }))
    t.Cleanup(srv.Close)
    return srv, keys
}

func get(t *testing.T, url, key string) *http.Response {
    t.Helper()
    req, _ := http.NewRequest("GET", url, nil)
    if key != "" { req.Header.Set("X-API-Key", key) }
    resp, err := http.DefaultClient.Do(req)
    if err != nil { t.Fatal(err) }
    return resp
}

func TestEngineHTTP_StatusAndEventsNeedAKeyForTheChannel(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 3)
    srv, _ := engineServer(t, r)
    for _, path := range []string{"/v1/channels/c1/status", "/v1/channels/c1/events"} {
        if resp := get(t, srv.URL+path, ""); resp.StatusCode != 401 { t.Errorf("%s without a key: %d", path, resp.StatusCode) }
        if resp := get(t, srv.URL+path, "wrong-secret"); resp.StatusCode != 401 { t.Errorf("%s with a wrong key: %d", path, resp.StatusCode) }
        if resp := get(t, srv.URL+path, "other-secret"); resp.StatusCode != 403 { t.Errorf("%s with a key for another channel: %d", path, resp.StatusCode) }
    }
    if resp := get(t, srv.URL+"/v1/channels/c9/status", "viewer-secret"); resp.StatusCode != 404 { t.Errorf("a channel this engine does not play: %d", resp.StatusCode) }
    resp := get(t, srv.URL+"/v1/channels/c1/status", "viewer-secret")
    var st statusReply
    _ = json.NewDecoder(resp.Body).Decode(&st)
    if resp.StatusCode != 200 || st.Segments == 0 || st.LastPlaylistAt.IsZero() { t.Fatalf("status: %d %+v", resp.StatusCode, st) }
    for _, p := range []string{"/healthz", "/metrics"} {
        if resp := get(t, srv.URL+p, ""); resp.StatusCode != 200 { t.Errorf("%s: %d", p, resp.StatusCode) }
    }
    if resp := get(t, srv.URL+"/readyz", ""); resp.StatusCode != 200 { t.Errorf("readyz: %d", resp.StatusCode) }
}

func TestEngineHTTP_EventsStreamWithResume(t *testing.T) {
    r := newRig(t)
    r.sched.set(r.content("a", 0, 25))
    r.ticks(0, 2) // engine.started and program.started
    srv, _ := engineServer(t, r)
    resp := get(t, srv.URL+"/v1/channels/c1/events", "viewer-secret")
    defer resp.Body.Close()
    if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" { t.Fatalf("%d %v", resp.StatusCode, resp.Header) }
    sc := bufio.NewScanner(resp.Body)
    var seen []string
    for sc.Scan() && len(seen) < 2 {
        if line := sc.Text(); strings.HasPrefix(line, "event: ") { seen = append(seen, strings.TrimPrefix(line, "event: ")) }
    }
    if strings.Join(seen, " ") != "engine.started program.started" { t.Fatalf("%v", seen) }
}

// The engine documents its own routes in api/openapi.yaml; the two must agree.
func TestOpenAPI_DescribesExactlyTheRoutesOfTheEngine(t *testing.T) {
    r := newRig(t)
    keys, _ := auth.Parse("", "some-key-1234")
    h := r.e.routes(keys, func(context.Context) error { return nil })
    routed := map[string]bool{}
    if err := chi.Walk(h.(chi.Routes), func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
        routed[method+" "+strings.TrimSuffix(route, "/")] = true
        return nil
    }); err != nil { t.Fatal(err) }
    raw, err := os.ReadFile("../../api/openapi.yaml")
    if err != nil { t.Fatal(err) }
    var doc struct{ Paths map[string]map[string]yaml.Node `yaml:"paths"` }
    if err := yaml.Unmarshal(raw, &doc); err != nil { t.Fatal(err) }
    documented := map[string]bool{}
    for path, item := range doc.Paths {
        for method, node := range item {
            var op struct{ Service []string `yaml:"x-service"` }
            if node.Decode(&op) != nil { continue }
            for _, sv := range op.Service { if sv == "engine" { documented[strings.ToUpper(method)+" "+path] = true } }
        }
    }
    for op := range routed { if !documented[op] { t.Errorf("the engine routes %s but api/openapi.yaml does not describe it", op) } }
    for op := range documented { if !routed[op] { t.Errorf("api/openapi.yaml describes %s but the engine does not have it", op) } }
    if len(routed) < 5 { t.Fatalf("only %d routes: the test proves nothing", len(routed)) }
}
