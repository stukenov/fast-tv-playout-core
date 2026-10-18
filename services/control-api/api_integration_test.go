//go:build integration

package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/http/httptest"
    "os"
    "strings"
    "sync"
    "sync/atomic"
    "testing"
    "time"

    "ftvx/pkg/auth"
    "ftvx/pkg/logger"
    "ftvx/pkg/scte35"
    "ftvx/pkg/storage"
)

// Run with: POSTGRES_DSN=postgres://... go test -tags integration ./services/control-api/

type env struct {
    t   *testing.T
    st  *storage.Postgres
    srv *httptest.Server
    s   *server
    now time.Time
}

var fixed = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

const (
    adminKey  = "admin-secret-1"
    opsKey    = "ops-secret-001" // schedule, command and read, only on channel api_ops
    viewerKey = "viewer-secret-1"
)

func newEnv(t *testing.T) *env {
    t.Helper()
    dsn := os.Getenv("POSTGRES_DSN")
    if dsn == "" { t.Skip("POSTGRES_DSN not set") }
    pg, err := storage.ConnectPostgres(context.Background(), dsn)
    if err != nil { t.Fatal(err) }
    t.Cleanup(pg.Close)
    keys, err := auth.Parse("admin="+adminKey+":*:*;ops="+opsKey+":schedule+command+read:api_ops;viewer="+viewerKey+":read:*", "")
    if err != nil { t.Fatal(err) }
    e := &env{t: t, st: pg, now: fixed}
    e.s = newServer(pg, keys, logger.Setup(), 3*time.Second)
    e.s.now = func() time.Time { return e.now }
    e.srv = httptest.NewServer(e.s.routes())
    t.Cleanup(e.srv.Close)
    return e
}

type reply struct {
    code int
    hdr  http.Header
    body []byte
}

func (r reply) json() map[string]any {
    var m map[string]any
    _ = json.Unmarshal(r.body, &m)
    return m
}

func (e *env) call(method, path, key string, body any, headers ...string) reply {
    e.t.Helper()
    var rd io.Reader
    switch b := body.(type) {
    case nil:
    case string: rd = strings.NewReader(b)
    default:
        raw, _ := json.Marshal(b)
        rd = bytes.NewReader(raw)
    }
    req, _ := http.NewRequest(method, e.srv.URL+path, rd)
    if key != "" { req.Header.Set("X-API-Key", key) }
    for i := 0; i+1 < len(headers); i += 2 { req.Header.Set(headers[i], headers[i+1]) }
    resp, err := http.DefaultClient.Do(req)
    if err != nil { e.t.Fatal(err) }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    return reply{resp.StatusCode, resp.Header, b}
}

func (e *env) channel(id string) {
    e.t.Helper()
    _ = e.st.DeleteChannel(context.Background(), id)
    if r := e.call("POST", "/v1/channels", adminKey, map[string]any{"id": id, "name": id}); r.code != 201 { e.t.Fatalf("create channel %s: %d %s", id, r.code, r.body) }
}

func (e *env) asset(id string, secs int) {
    e.t.Helper()
    _ = e.st.DeleteAsset(context.Background(), id)
    if r := e.call("POST", "/v1/assets", adminKey, map[string]any{"id": id, "uri": "http://origin/vod/" + id + "/index.m3u8", "duration_s": secs, "tags": []string{"api-test"}}); r.code != 201 { e.t.Fatalf("create asset %s: %d %s", id, r.code, r.body) }
}

func ts(n int) string { return fixed.Add(time.Duration(n) * time.Second).Format(time.RFC3339) }

func replaceBody(entries ...map[string]any) map[string]any {
    return map[string]any{"window": map[string]any{"from": ts(0), "to": ts(3600)}, "entries": entries}
}

func entry(typ, asset string, start, dur int) map[string]any {
    e := map[string]any{"type": typ, "start": ts(start), "duration": dur}
    if asset != "" { e["asset_id"] = asset }
    return e
}

// --- authorization ---

func TestAuth_EveryRouteChecksTheKey(t *testing.T) {
    e := newEnv(t)
    e.channel("api_ops")
    e.channel("api_other")
    e.asset("auth_a", 10)
    type route struct{ method, path, scope string; body any }
    routes := []route{
        {"GET", "/v1/channels", "read", nil},
        {"POST", "/v1/channels", "admin", map[string]any{"id": "auth_new", "name": "x"}},
        {"GET", "/v1/channels/CH", "read", nil},
        {"PATCH", "/v1/channels/CH", "admin", map[string]any{"name": "n"}},
        {"GET", "/v1/channels/CH/schedule", "read", nil},
        {"POST", "/v1/channels/CH/schedule:replace?dry_run=true", "schedule", replaceBody(entry("slate", "", 10, 5))},
        {"GET", "/v1/channels/CH/now-next", "read", nil},
        {"POST", "/v1/channels/CH/commands", "command", map[string]any{"type": "skip"}},
        {"POST", "/v1/channels/CH/scte35", "command", map[string]any{"message": "0x00"}},
        {"GET", "/v1/assets", "read", nil},
        {"POST", "/v1/assets", "admin", map[string]any{"id": "auth_b", "uri": "http://x/b.m3u8", "duration_s": 5}},
        {"GET", "/v1/assets/auth_a", "read", nil},
        {"DELETE", "/v1/assets/auth_zzz", "admin", nil},
        {"DELETE", "/v1/channels/CH", "admin", nil},
    }
    // who may do what: the scope is what the key can do, "channel" is where
    keys := map[string]map[string]bool{
        adminKey:  {"read": true, "schedule": true, "command": true, "admin": true},
        opsKey:    {"read": true, "schedule": true, "command": true},
        viewerKey: {"read": true},
    }
    for _, rt := range routes {
        for _, ch := range []string{"api_ops", "api_other"} {
            path := strings.ReplaceAll(rt.path, "CH", ch)
            if rt.method == "DELETE" && strings.Contains(path, "/channels/") { continue } // do not delete the channels the other cases need
            if rt.method == "POST" && rt.path == "/v1/channels" && ch == "api_other" { continue }
            // no key and a wrong key: 401 everywhere
            for _, bad := range []string{"", "nope-nope-nope"} {
                if r := e.call(rt.method, path, bad, rt.body); r.code != 401 { t.Errorf("%s %s without a valid key: %d, want 401", rt.method, path, r.code) }
            }
            for key, scopes := range keys {
                allowed := scopes[rt.scope]
                if key == opsKey && ch == "api_other" && strings.Contains(path, "/channels/") { allowed = false } // ops is limited to its own channel
                r := e.call(rt.method, path, key, rt.body)
                denied := r.code == 403
                if allowed && denied { t.Errorf("%s %s with %s: forbidden, should be allowed (%s)", rt.method, path, name(key), r.body) }
                if !allowed && !denied { t.Errorf("%s %s with %s: %d, want 403", rt.method, path, name(key), r.code) }
                if allowed && r.code == 401 { t.Errorf("%s %s with %s: 401", rt.method, path, name(key)) }
            }
        }
    }
    // the open endpoints need no key
    for _, p := range []string{"/healthz", "/readyz", "/metrics"} {
        if r := e.call("GET", p, "", nil); r.code != 200 { t.Errorf("%s: %d", p, r.code) }
    }
    // a Bearer token works as well
    if r := e.call("GET", "/v1/channels", "", nil, "Authorization", "Bearer "+viewerKey); r.code != 200 { t.Errorf("bearer: %d", r.code) }
}

func name(key string) string {
    switch key { case adminKey: return "admin"; case opsKey: return "ops"; default: return "viewer" }
}

func TestAuth_ChannelListShowsOnlyWhatTheKeyMayRead(t *testing.T) {
    e := newEnv(t)
    e.channel("api_ops")
    e.channel("api_other")
    r := e.call("GET", "/v1/channels?limit=200", opsKey, nil)
    var out struct{ Items []struct{ ID string `json:"id"` } `json:"items"` }
    _ = json.Unmarshal(r.body, &out)
    for _, c := range out.Items { if c.ID != "api_ops" { t.Fatalf("the ops key sees channel %s", c.ID) } }
    if len(out.Items) != 1 { t.Fatalf("expected its own channel, got %v", out.Items) }
}

// --- channels and assets ---

func TestChannels_Lifecycle(t *testing.T) {
    e := newEnv(t)
    for _, id := range []string{"lc_a", "lc_b", "lc_c"} { _ = e.st.DeleteChannel(context.Background(), id) }
    r := e.call("POST", "/v1/channels", adminKey, map[string]any{"id": "lc_a", "name": "A", "ladder": []any{"720p", 480}})
    if r.code != 201 || r.hdr.Get("Location") != "/v1/channels/lc_a" { t.Fatalf("create: %d %s %v", r.code, r.body, r.hdr) }
    if r := e.call("POST", "/v1/channels", adminKey, map[string]any{"id": "lc_a", "name": "A"}); r.code != 409 { t.Fatalf("duplicate: %d", r.code) }
    bad := map[string]any{
        "a bad id":          map[string]any{"id": "has space", "name": "x"},
        "no name":           map[string]any{"id": "lc_x"},
        "another segment":   map[string]any{"id": "lc_x", "name": "x", "segment_sec": 2},
        "an unknown field":  map[string]any{"id": "lc_x", "name": "x", "colour": "red"},
    }
    for name, body := range bad {
        if r := e.call("POST", "/v1/channels", adminKey, body); r.code != 400 { t.Errorf("%s: %d %s", name, r.code, r.body) }
    }
    _ = e.call("POST", "/v1/channels", adminKey, map[string]any{"id": "lc_b", "name": "B"})
    _ = e.call("POST", "/v1/channels", adminKey, map[string]any{"id": "lc_c", "name": "C"})
    // pages
    p1 := e.call("GET", "/v1/channels?limit=2&cursor=lc_", adminKey, nil).json()
    if len(p1["items"].([]any)) != 2 || p1["next_cursor"] != "lc_b" { t.Fatalf("page 1: %v", p1) }
    p2 := e.call("GET", "/v1/channels?limit=2&cursor=lc_b", adminKey, nil).json()
    if items := p2["items"].([]any); len(items) < 1 || items[0].(map[string]any)["id"] != "lc_c" { t.Fatalf("page 2: %v", p2) }
    if r := e.call("GET", "/v1/channels?limit=0", adminKey, nil); r.code != 400 { t.Errorf("limit 0: %d", r.code) }
    // update
    r = e.call("PATCH", "/v1/channels/lc_a", adminKey, map[string]any{"name": "Renamed"})
    if r.code != 200 || r.json()["name"] != "Renamed" { t.Fatalf("patch: %d %s", r.code, r.body) }
    if r := e.call("PATCH", "/v1/channels/lc_a", adminKey, map[string]any{"name": " "}); r.code != 400 { t.Errorf("empty name: %d", r.code) }
    if r := e.call("PATCH", "/v1/channels/nope", adminKey, map[string]any{"name": "x"}); r.code != 404 { t.Errorf("patch missing: %d", r.code) }
    // delete
    if r := e.call("DELETE", "/v1/channels/lc_a", adminKey, nil); r.code != 204 { t.Fatalf("delete: %d", r.code) }
    if r := e.call("GET", "/v1/channels/lc_a", adminKey, nil); r.code != 404 { t.Fatalf("after delete: %d", r.code) }
    if r := e.call("DELETE", "/v1/channels/lc_a", adminKey, nil); r.code != 404 { t.Fatalf("delete twice: %d", r.code) }
}

func TestAssets_Lifecycle(t *testing.T) {
    e := newEnv(t)
    e.channel("as_ch")
    e.asset("as_a", 30)
    if r := e.call("POST", "/v1/assets", adminKey, map[string]any{"id": "as_a", "uri": "http://x/other.m3u8", "duration_s": 5}); r.code != 409 { t.Fatalf("duplicate id: %d", r.code) }
    for name, body := range map[string]map[string]any{
        "no uri":        {"id": "as_x", "duration_s": 5},
        "no duration":   {"id": "as_x", "uri": "http://x/a.m3u8"},
        "a bad id":      {"id": "a/b", "uri": "http://x/a.m3u8", "duration_s": 5},
    } {
        if r := e.call("POST", "/v1/assets", adminKey, body); r.code != 400 { t.Errorf("%s: %d", name, r.code) }
    }
    r := e.call("GET", "/v1/assets?tag=api-test&limit=200", viewerKey, nil)
    if r.code != 200 || !strings.Contains(string(r.body), "as_a") { t.Fatalf("list by tag: %d %s", r.code, r.body) }
    if r := e.call("GET", "/v1/assets/as_a", viewerKey, nil); r.code != 200 { t.Fatalf("get: %d", r.code) }
    if r := e.call("GET", "/v1/assets/missing", viewerKey, nil); r.code != 404 { t.Fatalf("get missing: %d", r.code) }
    // an asset that a schedule uses cannot be deleted
    if r := e.call("POST", "/v1/channels/as_ch/schedule:replace", adminKey, replaceBody(entry("content", "as_a", 10, 5))); r.code != 200 { t.Fatalf("schedule: %d %s", r.code, r.body) }
    if r := e.call("DELETE", "/v1/assets/as_a", adminKey, nil); r.code != 409 { t.Fatalf("delete in use: %d", r.code) }
    e.call("POST", "/v1/channels/as_ch/schedule:replace", adminKey, replaceBody(entry("slate", "", 10, 5)))
    if r := e.call("DELETE", "/v1/assets/as_a", adminKey, nil); r.code != 204 { t.Fatalf("delete after: %d", r.code) }
}

// --- schedule ---

func TestSchedule_ReplaceAndRead(t *testing.T) {
    e := newEnv(t)
    e.channel("sc_a")
    e.asset("sc_m", 100)
    r := e.call("POST", "/v1/channels/sc_a/schedule:replace", opsKey, nil)
    _ = r
    ch := "sc_a"
    r = e.call("POST", "/v1/channels/"+ch+"/schedule:replace", adminKey, replaceBody(entry("content", "sc_m", 10, 60), entry("ad_break", "", 70, 10), entry("slate", "", 80, 5)))
    if r.code != 200 || r.json()["replaced"] != float64(3) || r.json()["version"] != float64(1) || r.hdr.Get("ETag") != `"1"` { t.Fatalf("replace: %d %s %v", r.code, r.body, r.hdr) }
    g := e.call("GET", "/v1/channels/"+ch+"/schedule?from="+ts(0)+"&to="+ts(100), viewerKey, nil)
    out := g.json()
    if g.code != 200 || out["version"] != float64(1) || len(out["entries"].([]any)) != 3 || g.hdr.Get("ETag") != `"1"` { t.Fatalf("get: %d %s", g.code, g.body) }
    first := out["entries"].([]any)[0].(map[string]any)
    if first["asset_id"] != "sc_m" || first["duration"] != float64(60) { t.Fatalf("entry: %v", first) }
    if r := e.call("GET", "/v1/channels/"+ch+"/schedule?from=garbage", viewerKey, nil); r.code != 400 { t.Errorf("bad from: %d", r.code) }
    if r := e.call("GET", "/v1/channels/"+ch+"/schedule?from="+ts(0)+"&to="+ts(100000), viewerKey, nil); r.code != 400 { t.Errorf("a window of more than 12 hours: %d", r.code) }
    if r := e.call("GET", "/v1/channels/missing/schedule", viewerKey, nil); r.code != 404 { t.Errorf("missing channel: %d", r.code) }
}

func TestSchedule_ValidationAndDryRun(t *testing.T) {
    e := newEnv(t)
    e.channel("sv_a")
    e.asset("sv_m", 100)
    path := "/v1/channels/sv_a/schedule:replace"
    cases := map[string]struct{ body any; code int; want string }{
        "not JSON":                {"{oops", 400, "invalid JSON"},
        "an unknown field":        {map[string]any{"window": map[string]any{"from": ts(0), "to": ts(100)}, "entries": []any{}, "extra": 1}, 400, "unknown field"},
        "a window backwards":      {map[string]any{"window": map[string]any{"from": ts(100), "to": ts(0)}, "entries": []any{}}, 400, "from must be before to"},
        "a window of 13 hours":    {map[string]any{"window": map[string]any{"from": ts(0), "to": ts(13 * 3600)}, "entries": []any{}}, 400, "longer than"},
        "an unknown type":         {replaceBody(entry("movie", "sv_m", 10, 5)), 400, "type"},
        "an ad_break with asset":  {replaceBody(entry("ad_break", "sv_m", 10, 5)), 400, "no asset"},
        "a zero duration":         {replaceBody(entry("slate", "", 10, 0)), 400, "duration"},
        "an overlap":              {replaceBody(entry("slate", "", 10, 20), entry("slate", "", 20, 5)), 400, "overlaps"},
        "outside the window":      {replaceBody(entry("slate", "", 5000, 5)), 400, "outside"},
        "an unknown asset":        {replaceBody(entry("content", "no_such_asset", 10, 5)), 422, "not registered"},
    }
    for name, c := range cases {
        r := e.call("POST", path, adminKey, c.body)
        if r.code != c.code || !strings.Contains(string(r.body), c.want) { t.Errorf("%s: %d %s (want %d with %q)", name, r.code, r.body, c.code, c.want) }
    }
    // dry run: the same checks, nothing written
    ok := replaceBody(entry("content", "sv_m", 10, 20))
    r := e.call("POST", path+"?dry_run=true", adminKey, ok)
    if r.code != 200 || r.json()["valid"] != true { t.Fatalf("dry run: %d %s", r.code, r.body) }
    if r := e.call("POST", path+"?dry_run=true", adminKey, replaceBody(entry("movie", "sv_m", 10, 5))); r.code != 400 { t.Errorf("a dry run reports errors too: %d", r.code) }
    ch := e.call("GET", "/v1/channels/sv_a", adminKey, nil).json()
    if ch["schedule_version"] != float64(0) { t.Fatalf("a dry run changed the schedule: version %v", ch["schedule_version"]) }
    if g := e.call("GET", "/v1/channels/sv_a/schedule?from="+ts(0)+"&to="+ts(100), adminKey, nil).json(); len(g["entries"].([]any)) != 0 { t.Fatalf("a dry run wrote entries: %v", g) }
}

func TestSchedule_IfMatch(t *testing.T) {
    e := newEnv(t)
    e.channel("im_a")
    path := "/v1/channels/im_a/schedule:replace"
    body := replaceBody(entry("slate", "", 10, 5))
    if r := e.call("POST", path, adminKey, body, "If-Match", `"0"`); r.code != 200 || r.hdr.Get("ETag") != `"1"` { t.Fatalf("matching: %d %s", r.code, r.body) }
    r := e.call("POST", path, adminKey, body, "If-Match", `"0"`)
    if r.code != 412 || r.hdr.Get("ETag") != `"1"` { t.Fatalf("a stale version must be 412 with the current ETag: %d %v", r.code, r.hdr) }
    if r := e.call("POST", path, adminKey, body, "If-Match", `"1"`); r.code != 200 { t.Fatalf("current version: %d", r.code) }
    if r := e.call("POST", path, adminKey, body, "If-Match", "banana"); r.code != 400 { t.Fatalf("garbage: %d", r.code) }
    if r := e.call("POST", path, adminKey, body, "If-Match", "*"); r.code != 200 { t.Fatalf("*: %d", r.code) }
}

func TestSchedule_IdempotencyKey(t *testing.T) {
    e := newEnv(t)
    e.channel("id_a")
    path := "/v1/channels/id_a/schedule:replace"
    body := replaceBody(entry("slate", "", 10, 5))
    first := e.call("POST", path, adminKey, body, "Idempotency-Key", "key-1")
    again := e.call("POST", path, adminKey, body, "Idempotency-Key", "key-1")
    if first.code != 200 || again.code != 200 || again.hdr.Get("Idempotent-Replay") != "true" || string(first.body) != strings.TrimSpace(string(again.body)) && first.json()["version"] != again.json()["version"] {
        t.Fatalf("replay: %d %s / %d %s %v", first.code, first.body, again.code, again.body, again.hdr)
    }
    if first.hdr.Get("Idempotent-Replay") != "" { t.Fatal("the first request is not a replay") }
    if v := e.call("GET", "/v1/channels/id_a", adminKey, nil).json()["schedule_version"]; v != float64(1) { t.Fatalf("a replay changed the schedule: version %v", v) }
    other := e.call("POST", path, adminKey, replaceBody(entry("slate", "", 10, 7)), "Idempotency-Key", "key-1")
    if other.code != 422 { t.Fatalf("the same key with another body: %d %s", other.code, other.body) }
    // a long key is refused
    if r := e.call("POST", path, adminKey, body, "Idempotency-Key", strings.Repeat("k", 200)); r.code != 400 { t.Fatalf("long key: %d", r.code) }
    // eight identical requests at once: one change
    e.channel("id_b")
    var wg sync.WaitGroup
    var replays atomic.Int32
    for i := 0; i < 8; i++ {
        wg.Add(1)
        go func() {
            defer wg.Done()
            r := e.call("POST", "/v1/channels/id_b/schedule:replace", adminKey, body, "Idempotency-Key", "race")
            if r.code != 200 { t.Errorf("race: %d %s", r.code, r.body); return }
            if r.hdr.Get("Idempotent-Replay") == "true" { replays.Add(1) }
        }()
    }
    wg.Wait()
    if replays.Load() != 7 { t.Fatalf("%d replays of 8: exactly one request must be applied", replays.Load()) }
    if v := e.call("GET", "/v1/channels/id_b", adminKey, nil).json()["schedule_version"]; v != float64(1) { t.Fatalf("the schedule changed %v times", v) }
}

func TestNowNext(t *testing.T) {
    e := newEnv(t)
    e.channel("nn_a")
    e.asset("nn_m", 100)
    e.call("POST", "/v1/channels/nn_a/schedule:replace", adminKey, replaceBody(entry("content", "nn_m", 0, 60), entry("ad_break", "", 60, 10)))
    e.now = fixed.Add(30 * time.Second)
    out := e.call("GET", "/v1/channels/nn_a/now-next", viewerKey, nil).json()
    now, next := out["now"].(map[string]any), out["next"].(map[string]any)
    if now["asset_id"] != "nn_m" || next["type"] != "ad_break" { t.Fatalf("%v", out) }
    e.now = fixed.Add(500 * time.Second)
    out = e.call("GET", "/v1/channels/nn_a/now-next", viewerKey, nil).json()
    if out["now"] != nil || out["next"] != nil { t.Fatalf("after the schedule: %v", out) }
}

// --- commands ---

func entriesOf(e *env, ch string, from, to int) []map[string]any {
    e.t.Helper()
    r := e.call("GET", "/v1/channels/"+ch+"/schedule?from="+ts(from)+"&to="+ts(to), adminKey, nil)
    var out []map[string]any
    for _, x := range r.json()["entries"].([]any) { out = append(out, x.(map[string]any)) }
    return out
}

func TestCommands_AdBreakNowCutsTheMovieAndItGoesOn(t *testing.T) {
    e := newEnv(t)
    e.channel("cm_a")
    e.asset("cm_m", 200)
    e.asset("cm_n", 50)
    e.call("POST", "/v1/channels/cm_a/schedule:replace", adminKey, replaceBody(entry("content", "cm_m", -10+10, 100), entry("content", "cm_n", 110, 50)))
    e.now = fixed.Add(20*time.Second + 400*time.Millisecond) // the movie has been on for 20 s
    r := e.call("POST", "/v1/channels/cm_a/commands", opsKeyFor(), map[string]any{"type": "ad_break", "duration": 30})
    if r.code != 403 { t.Fatalf("the ops key is limited to another channel: %d", r.code) }
    r = e.call("POST", "/v1/channels/cm_a/commands", adminKey, map[string]any{"type": "ad_break", "duration": 30})
    if r.code != 200 { t.Fatalf("command: %d %s", r.code, r.body) }
    // now is 20.4 s; the engine needs 3 s: the break airs at 24 s (the next whole second after 23.4)
    if got := r.json()["effective_at"]; got != ts(24) { t.Fatalf("effective_at %v, want %s", got, ts(24)) }
    got := entriesOf(e, "cm_a", 0, 400)
    if len(got) != 4 { t.Fatalf("%v", got) }
    check := func(i int, typ string, start, dur int, offset float64) {
        x := got[i]
        if x["type"] != typ || x["start"] != ts(start) || x["duration"] != float64(dur) || (x["offset_s"] == nil && offset != 0) || (x["offset_s"] != nil && x["offset_s"] != offset) {
            t.Errorf("entry %d is %v, want %s from %d for %d s at offset %v", i, x, typ, start, dur, offset)
        }
    }
    check(0, "content", 0, 24, 0)       // the movie up to the break
    check(1, "ad_break", 24, 30, 0)     // the break
    check(2, "content", 54, 76, 24)     // the movie goes on from second 24 of itself
    check(3, "content", 140, 50, 0)     // everything after is 30 s later
    // the same command again with the same key is not repeated
    key := "cue-1"
    first := e.call("POST", "/v1/channels/cm_a/commands", adminKey, map[string]any{"type": "slate", "duration": 5}, "Idempotency-Key", key)
    again := e.call("POST", "/v1/channels/cm_a/commands", adminKey, map[string]any{"type": "slate", "duration": 5}, "Idempotency-Key", key)
    if first.code != 200 || again.code != 200 || again.hdr.Get("Idempotent-Replay") != "true" { t.Fatalf("idempotent command: %d %d %v", first.code, again.code, again.hdr) }
    if n := len(entriesOf(e, "cm_a", 0, 400)); n != 5 { t.Fatalf("the slate was put in twice? %d entries (4 and one slate make 5)", n) }
}

func opsKeyFor() string { return opsKey }

func TestCommands_RulesAndErrors(t *testing.T) {
    e := newEnv(t)
    e.channel("api_ops")
    e.asset("cr_m", 100)
    e.call("POST", "/v1/channels/api_ops/schedule:replace", adminKey, replaceBody(entry("content", "cr_m", 0, 100)))
    e.now = fixed.Add(10 * time.Second)
    cmd := func(body map[string]any) reply { return e.call("POST", "/v1/channels/api_ops/commands", opsKey, body) }
    if r := cmd(map[string]any{"type": "dance"}); r.code != 400 { t.Errorf("unknown type: %d", r.code) }
    if r := cmd(map[string]any{"type": "ad_break"}); r.code != 400 { t.Errorf("no duration: %d", r.code) }
    if r := cmd(map[string]any{"type": "ad_break", "duration": 999999}); r.code != 400 { t.Errorf("too long: %d", r.code) }
    if r := cmd(map[string]any{"type": "ad_break", "duration": 10, "asset_id": "cr_m"}); r.code != 400 { t.Errorf("an ad break with an asset: %d", r.code) }
    if r := cmd(map[string]any{"type": "ad_break", "duration": 10, "at": ts(11)}); r.code != 400 { t.Errorf("too soon (the engine needs 3 s): %d %s", r.code, r.body) }
    if r := cmd(map[string]any{"type": "ad_break", "duration": 10, "at": "yesterday"}); r.code != 400 { t.Errorf("bad time: %d", r.code) }
    if r := cmd(map[string]any{"type": "insert", "duration": 10}); r.code != 400 { t.Errorf("insert without an asset: %d", r.code) }
    if r := cmd(map[string]any{"type": "insert", "asset_id": "ghost"}); r.code != 400 { t.Errorf("insert of an unknown asset: %d", r.code) }
    if r := cmd(map[string]any{"type": "ad_break", "duration": 10, "at": ts(30)}); r.code != 200 { t.Fatalf("a break in the future: %d %s", r.code, r.body) }
    // a break cannot be cut by a break
    e.now = fixed.Add(30 * time.Second)
    if r := cmd(map[string]any{"type": "ad_break", "duration": 10, "at": ts(35)}); r.code != 409 { t.Errorf("a break inside a break: %d %s", r.code, r.body) }
    // insert takes the length of the asset
    e.asset("cr_n", 7)
    e.now = fixed.Add(60 * time.Second)
    r := cmd(map[string]any{"type": "insert", "asset_id": "cr_n"})
    if r.code != 200 { t.Fatalf("insert: %d %s", r.code, r.body) }
    // skip, and nothing to skip
    e.now = fixed.Add(1000 * time.Second)
    if r := cmd(map[string]any{"type": "skip"}); r.code != 409 { t.Errorf("skip with nothing on the air: %d", r.code) }
    // a viewer may not command, a channel that does not exist is 404
    if r := e.call("POST", "/v1/channels/api_ops/commands", viewerKey, map[string]any{"type": "skip"}); r.code != 403 { t.Errorf("viewer: %d", r.code) }
    if r := e.call("POST", "/v1/channels/ghost/commands", adminKey, map[string]any{"type": "skip"}); r.code != 404 { t.Errorf("missing channel: %d", r.code) }
}

func TestCommands_SkipPullsTheChainBack(t *testing.T) {
    e := newEnv(t)
    e.channel("sk_a")
    e.asset("sk_m", 500)
    e.call("POST", "/v1/channels/sk_a/schedule:replace", adminKey, replaceBody(entry("content", "sk_m", 0, 100), entry("slate", "", 100, 20), entry("content", "sk_m", 300, 50)))
    e.now = fixed.Add(40 * time.Second)
    r := e.call("POST", "/v1/channels/sk_a/commands", adminKey, map[string]any{"type": "skip"})
    if r.code != 200 { t.Fatalf("skip: %d %s", r.code, r.body) }
    got := entriesOf(e, "sk_a", 0, 400)
    // the movie ends at 43, the slate that followed it moves up, the entry after the gap stays
    if len(got) != 3 || got[0]["duration"] != float64(43) || got[1]["start"] != ts(43) || got[2]["start"] != ts(300) { t.Fatalf("%v", got) }
}

func TestCommands_ConcurrentCommandsDoNotLoseEachOther(t *testing.T) {
    e := newEnv(t)
    e.channel("cc_a")
    e.asset("cc_m", 500)
    e.call("POST", "/v1/channels/cc_a/schedule:replace", adminKey, replaceBody(entry("content", "cc_m", 0, 400)))
    e.now = fixed.Add(10 * time.Second)
    var wg sync.WaitGroup
    var ok atomic.Int32
    for i := 0; i < 6; i++ {
        i := i
        wg.Add(1)
        go func() {
            defer wg.Done()
            r := e.call("POST", "/v1/channels/cc_a/commands", adminKey, map[string]any{"type": "slate", "duration": 5, "at": ts(100 + 20*i)})
            if r.code == 200 { ok.Add(1) } else { t.Errorf("command %d: %d %s", i, r.code, r.body) }
        }()
    }
    wg.Wait()
    got := entriesOf(e, "cc_a", 0, 1000)
    slates, total := 0, 0
    for i, x := range got {
        if x["type"] == "slate" { slates++ }
        total += int(x["duration"].(float64))
        if i > 0 {
            prevEnd := got[i-1]["start"].(string)
            _ = prevEnd
        }
    }
    // six slates of 5 s were cut into a movie of 400 s: the movie lasts 400 s in all, the slates 30 more
    if ok.Load() != 6 || slates != 6 || total != 430 { t.Fatalf("%d commands, %d slates, %d seconds in the schedule (want 6, 6, 430): %v", ok.Load(), slates, total, got) }
    // no overlaps
    var prevEnd time.Time
    for _, x := range got {
        start, _ := time.Parse(time.RFC3339, x["start"].(string))
        if start.Before(prevEnd) { t.Fatalf("entries overlap: %v", got) }
        prevEnd = start.Add(time.Duration(x["duration"].(float64)) * time.Second)
    }
    _ = fmt.Sprint
}

func TestSCTE35_OutStartsABreakAndInEndsIt(t *testing.T) {
    e := newEnv(t)
    e.channel("sc35_a")
    e.asset("sc35_m", 500)
    e.call("POST", "/v1/channels/sc35_a/schedule:replace", adminKey, replaceBody(entry("content", "sc35_m", 0, 300)))
    e.now = fixed.Add(10 * time.Second)

    out, _ := scte35.Out(777, 20)
    path := "/v1/channels/sc35_a/scte35"
    r := e.call("POST", path, opsKeyForChannel(), map[string]any{"message": scte35.Hex(out)})
    if r.code != 403 { t.Fatalf("the ops key may not command this channel: %d", r.code) }
    r = e.call("POST", path, adminKey, map[string]any{"message": scte35.Hex(out)})
    if r.code != 200 || r.json()["type"] != "ad_break" || r.json()["effective_at"] != ts(13) { t.Fatalf("out: %d %s", r.code, r.body) }
    got := entriesOf(e, "sc35_a", 0, 400)
    if len(got) != 3 || got[1]["type"] != "ad_break" || got[1]["duration"] != float64(20) || got[1]["scte35"] != scte35.Hex(out) { t.Fatalf("the break must carry the message that started it: %v", got) }
    if got[2]["offset_s"] != float64(13) { t.Fatalf("the movie must go on from where it stopped: %v", got[2]) }

    // the same message again with the same key is a repeat; without the key the break is already on the air
    r = e.call("POST", path, adminKey, map[string]any{"message": scte35.Hex(out)})
    if r.code != 409 { t.Fatalf("a break is already on the air: %d %s", r.code, r.body) }

    // the return message ends the break that is on the air: the break is cut short, the movie comes back sooner
    e.now = fixed.Add(20 * time.Second) // the break (13..33) is on the air
    in, _ := scte35.In(777)
    r = e.call("POST", path, adminKey, map[string]any{"message": scte35.Base64(in)})
    if r.code != 200 { t.Fatalf("in: %d %s", r.code, r.body) }
    got = entriesOf(e, "sc35_a", 0, 400)
    if got[1]["type"] != "ad_break" || got[1]["duration"] != float64(10) { t.Fatalf("the break must end at second 23 (it started at 13): %v", got) }
    // and with no break on the air there is nothing to end
    e.now = fixed.Add(100 * time.Second)
    if r := e.call("POST", path, adminKey, map[string]any{"message": scte35.Hex(in)}); r.code != 409 { t.Fatalf("return without a break: %d", r.code) }
}

func opsKeyForChannel() string { return opsKey }

func TestSCTE35_BadMessages(t *testing.T) {
    e := newEnv(t)
    e.channel("sc35_b")
    path := "/v1/channels/sc35_b/scte35"
    noDuration, _ := scte35.Encode(scte35.SpliceInsert{EventID: 1, Out: true, Immediate: true})
    cancel, _ := scte35.Encode(scte35.SpliceInsert{EventID: 1, Cancel: true})
    damaged, _ := scte35.Out(1, 10)
    damaged[len(damaged)-1] ^= 1
    cases := map[string]string{
        "not hex or base64":     "this is not a message!",
        "a damaged message":     scte35.Hex(damaged),
        "a heartbeat":           scte35.Hex(scte35.Null()),
        "a cancel":              scte35.Hex(cancel),
        "an out without length": scte35.Hex(noDuration),
        "empty":                 "",
    }
    for name, msg := range cases {
        if r := e.call("POST", path, adminKey, map[string]any{"message": msg}); r.code != 400 { t.Errorf("%s: %d %s", name, r.code, r.body) }
    }
    if r := e.call("POST", path, adminKey, map[string]any{"message": "0x00", "extra": 1}); r.code != 400 { t.Errorf("an unknown field: %d", r.code) }
}

// The scope that api/openapi.yaml names for every operation is the one the server enforces.
func TestOpenAPI_ScopesAreEnforcedAsDocumented(t *testing.T) {
    e := newEnv(t)
    e.channel("oa_ch")
    ops, err := specOperations(t, "control-api")
    if err != nil { t.Fatal(err) }
    // a key for each single scope, on every channel
    keys := map[string]string{}
    list := []string{}
    for _, sc := range []string{"read", "schedule", "command", "admin"} {
        keys[sc] = "key-for-" + sc + "-xx"
        list = append(list, sc+"="+keys[sc]+":"+sc+":*")
    }
    ks, err := auth.Parse(strings.Join(list, ";"), "")
    if err != nil { t.Fatal(err) }
    e.s.keys = ks
    e.srv.Close()
    e.srv = httptest.NewServer(e.s.routes()) // the router takes the keys when it is built
    t.Cleanup(e.srv.Close)
    checked := 0
    for op, scope := range ops {
        if scope == "" { continue }
        method, path, _ := strings.Cut(op, " ")
        path = strings.NewReplacer("{channel_id}", "oa_ch", "{asset_id}", "oa_asset").Replace(path)
        for sc, key := range keys {
            r := e.call(method, path, key, map[string]any{})
            allowed := sc == scope || sc == "admin"
            if allowed && (r.code == 401 || r.code == 403) { t.Errorf("%s with a %q key: %d, the spec says scope %q", op, sc, r.code, scope) }
            if !allowed && r.code != 403 { t.Errorf("%s with a %q key: %d, want 403 (scope %q)", op, sc, r.code, scope) }
        }
        checked++
    }
    if checked < 12 { t.Fatalf("checked %d operations", checked) }
}
