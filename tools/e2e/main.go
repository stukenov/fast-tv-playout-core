// Command e2e runs the whole system against real sources and checks what a viewer and an integrator would get.
//
// Phase 1 schedules "movie 1, ad break (16 s), movie 2, a short tail" and lets the stream pass it.
// Phase 2 puts a movie on the air and cuts an ad break into it with the "ad break now" command.
// Then it verifies:
//   1. every 1 s slot carries the picture of the right source segment, with a discontinuity only where the source changes;
//   2. every URL of the playlist can be downloaded, none was late;
//   3. the decode times of the segments follow each other without a gap or a step back;
//   4. ffmpeg decodes the whole stream: no errors, exactly 25 pictures per segment, none lost;
//   5. the picture of every segment has the colour of its source;
//   6. the command reached the air at the time the API promised, and the movie went on where it was cut;
//   7. SCTE-35 markers (EXT-X-DATERANGE and EXT-X-CUE) in the playlist decode to the right breaks;
//   8. events come over SSE and over signed webhooks, and both carry the same events;
//   9. the API refuses what it should (no key, a key without the scope or for another channel), dry runs write nothing,
//      a repeated Idempotency-Key does not repeat the change, a stale If-Match is refused.
package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "os/exec"
    "path/filepath"
    "sort"
    "strings"
    "sync"
    "time"

    "ftvx/pkg/events"
    "ftvx/pkg/fmp4"
    "ftvx/pkg/hls"
    "ftvx/pkg/scte35"
)

const (
    slot      = time.Second // every segment lasts exactly one second
    framesPer = 25          // frames of a segment (25 fps)
    m1Slots   = 20          // movie 1: its first 20 seconds
    ad1Slots  = 10          // ad break of 16 s: a 10 s creative ...
    ad2Slots  = 6           // ... and a 6 s one
    adSlots   = ad1Slots + ad2Slots
    m2Slots   = 20          // movie 2: its first 20 seconds
    tailSlots = 6           // a 5.6 s file made a whole 6 s by the fade-out and the black fill
    total     = m1Slots + adSlots + m2Slots + tailSlots // first slot after the schedule of phase 1

    movieSlots = 30 // phase 2: the movie on the air (the whole of m2)
    cmdBreak   = 10 // phase 2: length of the break that is cut in
    phase2Len  = movieSlots + cmdBreak

    mediaDir   = "/media"
    adminKey   = "dev-key"
    viewerKey  = "e2e-viewer-secret-0001"
    opsKey     = "e2e-ops-secret-0000002" // may command channel "other" only
    hookSecret = "e2e-webhook-secret"
)

var (
    apiBase    = getenv("API_BASE", "http://control-api:8081")
    originBase = getenv("ORIGIN_BASE", "http://origin:8080")
    engineBase = getenv("ENGINE_BASE", "http://engine:8085")
    outDir     = getenv("OUT_DIR", "/out")
    channel    = getenv("CHANNEL", "c1")
)

var (
    profile   = getenv("E2E_PROFILE", "default") // "tight": the playlist holds two segments and a segment is released one second before it airs
    startLead = getenv("E2E_START_LEAD_S", "10")
    player    *livePlayer
    failures  []string
    report    strings.Builder
)

// timeline of the run: where each phase is, so that any second can be mapped to the source it must carry.
var (
    s1 time.Time // phase 1 starts
    t2 time.Time // phase 2 starts
    k2 int       // second of phase 2 at which the break starts (known after the command answered)
)

func getenv(k, d string) string {
    if v := os.Getenv(k); v != "" { return v }
    return d
}

func logf(format string, a ...any) {
    line := fmt.Sprintf(format, a...)
    fmt.Println(line)
    report.WriteString(line + "\n")
}

func fail(format string, a ...any) {
    msg := fmt.Sprintf(format, a...)
    failures = append(failures, msg)
    logf("FAIL: %s", msg)
}

func main() {
    _ = os.MkdirAll(outDir, 0o755)
    err := run()
    if err != nil { fail("%v", err) }
    _ = os.WriteFile(filepath.Join(outDir, "report.txt"), []byte(report.String()), 0o644)
    if len(failures) > 0 {
        fmt.Printf("\nE2E FAILED: %d problem(s)\n", len(failures))
        os.Exit(1)
    }
    fmt.Println("\nE2E OK")
}

func run() error {
    for _, u := range []string{apiBase + "/healthz", engineBase + "/healthz", originBase + "/healthz"} {
        if err := waitHTTP(u, 120*time.Second); err != nil { return err }
    }
    logf("services are up")

    stopHook := startWebhookReceiver()
    defer stopHook()
    sse := startSSE()

    if err := register(); err != nil { return err }
    checkAPI()

    // ---- phase 1: a schedule written ahead of time
    lead := 10
    fmt.Sscanf(startLead, "%d", &lead)
    s1 = time.Now().UTC().Add(time.Duration(lead) * time.Second).Truncate(slot)
    playCtx, stopPlayer := context.WithCancel(context.Background())
    defer stopPlayer()
    player = newLivePlayer(playlistURL())
    go player.run(playCtx)
    ffDone := make(chan liveFfmpegResult, 1)
    go func() { time.Sleep(time.Until(s1)); ffDone <- runLiveFfmpeg(45 * time.Second) }()
    if err := schedule(s1); err != nil { return err }
    end := s1.Add(time.Duration(total) * slot)
    logf("phase 1: %s .. %s", s1.Format("15:04:05"), end.Format("15:04:05"))

    time.Sleep(time.Until(s1.Add(6 * time.Second)))
    checkNowNext("m1", "ad_break")

    want := end.Add(3 * slot)
    deadline := time.Now().Add(time.Until(want) + 90*time.Second)
    for player.lastSegmentTime().Before(want) {
        if time.Now().After(deadline) { return fmt.Errorf("the player never reached %s (it is at %v)", want.Format("15:04:05"), player.lastSegmentTime()) }
        time.Sleep(500 * time.Millisecond)
    }

    // ---- phase 2: "ad break now" in the middle of a movie
    cmd, err := phase2()
    if err != nil { return err }
    for player.lastSegmentTime().Before(t2.Add(time.Duration(phase2Len+3) * slot)) {
        if time.Now().After(cmd.sentAt.Add(120 * time.Second)) { return fmt.Errorf("the player never reached the end of phase 2") }
        time.Sleep(500 * time.Millisecond)
    }
    stopPlayer()
    pl := player.stream()
    logf("the live player collected %d segments (profile %q)", len(pl.Segments), profile)
    saveFile("v720.m3u8", []byte(player.lastRaw))
    saveFile("master.m3u8", httpGet(originBase+"/hls/"+channel+"/master.m3u8"))

    checkMaster()
    saveStream(pl)
    snapshot = filepath.Join(outDir, "stream", "local.m3u8")
    checkStructure(pl)
    checkPlayer()
    checkTimeline(pl)
    checkDecode(pl)
    checkPictures(pl)
    checkMetrics()
    checkCommand(cmd)
    checkMarkers()
    checkEvents(sse)
    select {
    case r := <-ffDone:
        checkLiveFfmpeg(r)
    case <-time.After(90 * time.Second):
        fail("the live ffmpeg run did not finish")
    }
    return nil
}

// --- what a second of the stream must carry ---

// expectedAt returns the source segment file and the colour for the second that starts at t.
func expectedAt(t time.Time) (path, color string) {
    seg := func(dir string, i int) string { return fmt.Sprintf("%s/%s/seg_%d.m4s", mediaDir, dir, i) }
    if n := int(t.Unix() - s1.Unix()); !s1.IsZero() && n >= 0 && n < total {
        switch {
        case n < m1Slots:
            return seg("m1", n), "blue"
        case n < m1Slots+ad1Slots:
            return seg("ads/ad1", n-m1Slots), "red"
        case n < m1Slots+adSlots:
            return seg("ads/ad2", n-m1Slots-ad1Slots), "red"
        case n < m1Slots+adSlots+m2Slots:
            return seg("m2", n-m1Slots-adSlots), "green"
        case n < total-1:
            return seg("m3", n-m1Slots-adSlots-m2Slots), "orange"
        }
        // the last second of a 5.6 s file: the picture has faded to almost black (it lasts only 0.6 s)
        return seg("m3", n-m1Slots-adSlots-m2Slots), "dark"
    }
    if j := int(t.Unix() - t2.Unix()); !t2.IsZero() && k2 > 0 && j >= 0 && j < phase2Len {
        switch {
        case j < k2:
            return seg("m2", j), "green"
        case j < k2+cmdBreak:
            return seg("ads/ad1", j-k2), "red"
        }
        return seg("m2", j-cmdBreak), "green" // the movie goes on where the break cut it
    }
    return seg("slate", int(t.Unix()%10)), "gray" // the slate loops by the clock
}

// --- setup ---

func register() error {
    status, body, err := call("POST", "/v1/channels", adminKey, nil, map[string]any{"id": channel, "name": "E2E channel", "segment_sec": 1, "ladder": []string{"720p"}})
    if err != nil { return err }
    if status != 201 { return fmt.Errorf("create channel: %d %s", status, body) }
    for _, a := range []struct{ id, uri string; dur int }{
        {"m1", "file://" + mediaDir + "/m1/index.m3u8", 30},
        {"m3", "file://" + mediaDir + "/m3/index.m3u8", tailSlots},
        // one source over http, to cover that way of reading as well (the origin serves the media directory)
        {"m2", "http://origin:8080/vod/m2/index.m3u8", 30},
    } {
        status, body, err = call("POST", "/v1/assets", adminKey, nil, map[string]any{"id": a.id, "uri": a.uri, "duration_s": a.dur, "tags": []string{"e2e"}})
        if err != nil { return err }
        if status != 201 { return fmt.Errorf("create asset %s: %d %s", a.id, status, body) }
    }
    logf("channel and assets registered")
    return nil
}

func schedule(start time.Time) error {
    t := func(n int) string { return start.Add(time.Duration(n) * slot).Format(time.RFC3339) }
    status, resp, err := call("POST", "/v1/channels/"+channel+"/schedule:replace", adminKey, nil, map[string]any{
        "window": map[string]string{"from": t(0), "to": t(total)},
        "entries": []map[string]any{
            {"type": "content", "asset_id": "m1", "start": t(0), "duration": m1Slots},
            {"type": "ad_break", "start": t(m1Slots), "duration": adSlots, "category": "midroll"},
            {"type": "content", "asset_id": "m2", "start": t(m1Slots + adSlots), "duration": m2Slots},
            {"type": "content", "asset_id": "m3", "start": t(m1Slots + adSlots + m2Slots), "duration": tailSlots},
        },
    })
    if err != nil { return err }
    if status != 200 { return fmt.Errorf("schedule: %d %s", status, resp) }
    return nil
}

// --- phase 2: the command ---

type cmdRun struct {
    sentAt      time.Time
    answeredAt  time.Time
    effectiveAt time.Time
}

// phase2 puts movie 2 on the air and, six seconds into it, sends "ad break now".
func phase2() (cmdRun, error) {
    var c cmdRun
    cueLead := 3 // the control API's CUE_LEAD_S: how far ahead a command airs
    fmt.Sscanf(getenv("CUE_LEAD_S", "3"), "%d", &cueLead)
    t2 = time.Now().UTC().Add(time.Duration(cueLead+4) * time.Second).Truncate(slot)
    t := func(n int) string { return t2.Add(time.Duration(n) * slot).Format(time.RFC3339) }
    status, resp, err := call("POST", "/v1/channels/"+channel+"/schedule:replace", adminKey, nil, map[string]any{
        "window":  map[string]string{"from": t(0), "to": t(movieSlots)},
        "entries": []map[string]any{{"type": "content", "asset_id": "m2", "start": t(0), "duration": movieSlots}},
    })
    if err != nil { return c, err }
    if status != 200 { return c, fmt.Errorf("phase 2 schedule: %d %s", status, resp) }

    time.Sleep(time.Until(t2.Add(6 * time.Second)))
    c.sentAt = time.Now()
    status, resp, err = call("POST", "/v1/channels/"+channel+"/commands", adminKey, nil, map[string]any{"type": "ad_break", "duration": cmdBreak, "category": "midroll"})
    c.answeredAt = time.Now()
    if err != nil { return c, err }
    if status != 200 { return c, fmt.Errorf("command: %d %s", status, resp) }
    var ans struct{ EffectiveAt time.Time `json:"effective_at"`; Warnings []string `json:"warnings"` }
    if err := json.Unmarshal(resp, &ans); err != nil { return c, err }
    c.effectiveAt = ans.EffectiveAt
    k2 = int(c.effectiveAt.Unix() - t2.Unix())
    if k2 < 6 || k2 >= movieSlots-1 { return c, fmt.Errorf("the command airs at second %d of the movie (%s): out of the expected range", k2, resp) }
    logf("phase 2: movie 2 from %s; command sent at second %.1f, answer after %v, airs at second %d (%.1f s after the command)",
        t2.Format("15:04:05"), c.sentAt.Sub(t2).Seconds(), c.answeredAt.Sub(c.sentAt).Round(time.Millisecond), k2, c.effectiveAt.Sub(c.sentAt).Seconds())
    return c, nil
}

func checkCommand(c cmdRun) {
    player.mu.Lock()
    byTime := map[int64]*collected{}
    for _, s := range player.segs { byTime[s.seg.ProgramDateTime.Unix()] = s }
    player.mu.Unlock()
    first := byTime[c.effectiveAt.Unix()]
    if first == nil { fail("command: the player has no segment for the first second of the break"); return }
    visible := first.seenAt.Sub(c.sentAt)
    slack := first.seg.ProgramDateTime.Sub(first.doneAt)
    if slack <= 0 { fail("command: the first segment of the break was downloaded %v after its air time", -slack); return }
    logf("command ok: the break was in the playlist %.1f s after the command and aired %.1f s after it (%.1f s of margin for a player); the movie goes on at second %d where it was cut",
        visible.Seconds(), first.seg.ProgramDateTime.Sub(c.sentAt).Seconds(), slack.Seconds(), k2)
}

func checkNowNext(wantNow, wantNextType string) {
    status, b, err := call("GET", "/v1/channels/"+channel+"/now-next", viewerKey, nil, nil)
    if err != nil { fail("now-next: %v", err); return }
    var nn struct {
        Now  struct{ AssetID string `json:"asset_id"` } `json:"now"`
        Next struct{ Type string `json:"type"` }        `json:"next"`
    }
    if status != 200 || json.Unmarshal(b, &nn) != nil { fail("now-next: status %d body %s", status, b); return }
    if nn.Now.AssetID != wantNow || nn.Next.Type != wantNextType { fail("now-next: now=%q next=%q, want %q and %q (%s)", nn.Now.AssetID, nn.Next.Type, wantNow, wantNextType, b); return }
    logf("now-next ok: %s on the air, then an %s", wantNow, wantNextType)
}

// --- the API on the real stack ---

func checkAPI() {
    before := len(failures)
    expect := func(what string, want int, method, path, key string, hdr map[string]string, body any) []byte {
        status, b, err := call(method, path, key, hdr, body)
        if err != nil { fail("api %s: %v", what, err); return nil }
        if status != want { fail("api %s: status %d, want %d (%s)", what, status, want, b) }
        return b
    }
    expect("no key", 401, "GET", "/v1/channels", "", nil, nil)
    expect("wrong key", 401, "GET", "/v1/channels", "nope-nope-nope-nope", nil, nil)
    expect("read key reads", 200, "GET", "/v1/channels/"+channel, viewerKey, nil, nil)
    cmd := map[string]any{"type": "slate", "duration": 5}
    expect("read key cannot command", 403, "POST", "/v1/channels/"+channel+"/commands", viewerKey, nil, cmd)
    expect("key of another channel cannot command", 403, "POST", "/v1/channels/"+channel+"/commands", opsKey, nil, cmd)

    version := func() float64 {
        _, b, _ := call("GET", "/v1/channels/"+channel, adminKey, nil, nil)
        var ch struct{ V float64 `json:"schedule_version"` }
        _ = json.Unmarshal(b, &ch)
        return ch.V
    }
    far := time.Now().UTC().Add(3 * time.Hour).Truncate(slot)
    body := func() map[string]any {
        return map[string]any{
            "window":  map[string]string{"from": far.Format(time.RFC3339), "to": far.Add(5 * time.Second).Format(time.RFC3339)},
            "entries": []map[string]any{{"type": "content", "asset_id": "m1", "start": far.Format(time.RFC3339), "duration": 5}},
        }
    }
    path := "/v1/channels/" + channel + "/schedule:replace"
    v0 := version()
    expect("dry run", 200, "POST", path+"?dry_run=true", adminKey, nil, body())
    if v := version(); v != v0 { fail("api: a dry run changed the schedule (version %v -> %v)", v0, v) }

    hdr := map[string]string{"Idempotency-Key": "e2e-key-1"}
    expect("idempotent first", 200, "POST", path, adminKey, hdr, body())
    status, _, _ := call("POST", path, adminKey, hdr, body())
    if status != 200 { fail("api: the replay answered %d", status) }
    if v := version(); v != v0+1 { fail("api: a repeated Idempotency-Key changed the schedule more than once (version %v -> %v)", v0, v) }
    expect("stale If-Match", 412, "POST", path, adminKey, map[string]string{"If-Match": fmt.Sprintf("\"%d\"", int(v0))}, body())
    expect("unknown asset", 422, "POST", path, adminKey, nil, map[string]any{
        "window":  map[string]string{"from": far.Format(time.RFC3339), "to": far.Add(5 * time.Second).Format(time.RFC3339)},
        "entries": []map[string]any{{"type": "content", "asset_id": "nothing", "start": far.Format(time.RFC3339), "duration": 5}},
    })
    // leave the schedule as it was found
    expect("clear the test entry", 200, "POST", path, adminKey, nil, map[string]any{
        "window": map[string]string{"from": far.Format(time.RFC3339), "to": far.Add(5 * time.Second).Format(time.RFC3339)}, "entries": []any{},
    })
    if len(failures) == before { logf("api ok: 401/403 where they belong, dry run writes nothing, a repeated Idempotency-Key changes the schedule once, a stale If-Match is refused") }
}

// --- events: SSE and webhooks ---

type eventRec struct {
    ID   int64
    Type string
}

var (
    hookMu   sync.Mutex
    hookGot  = map[int64]string{}
    hookBad  int
    hookSrv  *http.Server
)

func startWebhookReceiver() func() {
    mux := http.NewServeMux()
    mux.HandleFunc("/hook", func(w http.ResponseWriter, r *http.Request) {
        b, _ := io.ReadAll(r.Body)
        var ev struct{ ID int64 `json:"id"`; Type string `json:"type"` }
        ok := r.Header.Get("X-FTVX-Signature") == events.Sign(hookSecret, b) && json.Unmarshal(b, &ev) == nil
        hookMu.Lock()
        if ok { hookGot[ev.ID] = ev.Type } else { hookBad++ }
        hookMu.Unlock()
        w.WriteHeader(200)
    })
    hookSrv = &http.Server{Addr: ":9099", Handler: mux}
    go func() { _ = hookSrv.ListenAndServe() }()
    return func() { _ = hookSrv.Close() }
}

type sseCollector struct {
    mu  sync.Mutex
    got []eventRec
}

// startSSE follows the engine's event stream the way an integrator would, and reconnects with Last-Event-ID.
func startSSE() *sseCollector {
    c := &sseCollector{}
    go func() {
        var last int64
        for {
            req, _ := http.NewRequest("GET", engineBase+"/v1/channels/"+channel+"/events", nil)
            req.Header.Set("X-API-Key", viewerKey)
            if last > 0 { req.Header.Set("Last-Event-ID", fmt.Sprint(last)) }
            resp, err := http.DefaultClient.Do(req)
            if err != nil || resp.StatusCode != 200 {
                if resp != nil { resp.Body.Close() }
                time.Sleep(time.Second)
                continue
            }
            var id int64
            var typ string
            buf := make([]byte, 0, 4096)
            tmp := make([]byte, 1024)
            for {
                n, err := resp.Body.Read(tmp)
                buf = append(buf, tmp[:n]...)
                for {
                    i := bytes.Index(buf, []byte("\n"))
                    if i < 0 { break }
                    line := string(buf[:i])
                    buf = buf[i+1:]
                    switch {
                    case strings.HasPrefix(line, "id: "): fmt.Sscanf(line[4:], "%d", &id)
                    case strings.HasPrefix(line, "event: "): typ = line[7:]
                    case line == "" && typ != "":
                        c.mu.Lock()
                        c.got = append(c.got, eventRec{id, typ})
                        c.mu.Unlock()
                        last = id
                        typ = ""
                    }
                }
                if err != nil { break }
            }
            resp.Body.Close()
            time.Sleep(time.Second)
        }
    }()
    return c
}

func checkEvents(c *sseCollector) {
    before := len(failures)
    time.Sleep(3 * time.Second) // webhooks are delivered a little behind
    c.mu.Lock()
    got := append([]eventRec(nil), c.got...)
    c.mu.Unlock()
    count := map[string]int{}
    seen := map[int64]bool{}
    for _, e := range got {
        if seen[e.ID] { fail("events: event %d arrived twice over SSE", e.ID) }
        seen[e.ID] = true
        count[e.Type]++
    }
    // phase 1 has one break, phase 2 one more; programs: m1, m2, m3, then m2 cut in two pieces
    if count["engine.started"] < 1 { fail("events: no engine.started") }
    if count["break.started"] < 2 || count["break.ended"] < 2 { fail("events: %d break.started and %d break.ended, want at least 2 each", count["break.started"], count["break.ended"]) }
    if count["program.started"] < 5 { fail("events: %d program.started, want at least 5", count["program.started"]) }
    if count["slate.started"] < 1 { fail("events: no slate.started") }

    hookMu.Lock()
    defer hookMu.Unlock()
    if hookBad > 0 { fail("events: %d webhook calls had a wrong signature or body", hookBad) }
    var missing []string
    for _, e := range got {
        if e.Type == "engine.started" { continue } // sent before this checker was reachable, as the stack came up
        if t, ok := hookGot[e.ID]; !ok || t != e.Type { missing = append(missing, fmt.Sprint(e.ID)) }
    }
    if len(missing) > 0 { fail("events: %d of %d events never reached the webhook, for example %s", len(missing), len(got), strings.Join(missing[:min(3, len(missing))], ",")) }
    if len(failures) == before {
        types := make([]string, 0, len(count))
        for t, n := range count { types = append(types, fmt.Sprintf("%s x%d", t, n)) }
        sort.Strings(types)
        logf("events ok: %d events over SSE, all of them also at the signed webhook (%s)", len(got), strings.Join(types, ", "))
    }
}

// --- SCTE-35 markers in the playlist ---

func checkMarkers() {
    before := len(failures)
    player.mu.Lock()
    var lines []string
    for l := range player.tags { lines = append(lines, l) }
    player.mu.Unlock()
    outs, ins, cueOut, cueIn := map[int]bool{}, 0, map[int]bool{}, 0
    for _, l := range lines {
        switch {
        case strings.HasPrefix(l, "#EXT-X-DATERANGE:") && strings.Contains(l, "SCTE35-OUT="):
            ins2, ok := decodeSplice(l, "SCTE35-OUT=")
            if !ok || !ins2.Out { fail("markers: SCTE35-OUT does not decode to a break start: %s", l); continue }
            outs[int(ins2.DurationSeconds()+0.5)] = true
        case strings.HasPrefix(l, "#EXT-X-DATERANGE:") && strings.Contains(l, "SCTE35-IN="):
            ins2, ok := decodeSplice(l, "SCTE35-IN=")
            if !ok || ins2.Out { fail("markers: SCTE35-IN does not decode to a return: %s", l); continue }
            ins++
        case strings.HasPrefix(l, "#EXT-X-CUE-OUT:"):
            var n int
            fmt.Sscanf(l, "#EXT-X-CUE-OUT:%d", &n)
            cueOut[n] = true
        case l == "#EXT-X-CUE-IN":
            cueIn++
        }
    }
    for _, secs := range []int{adSlots, cmdBreak} {
        if !outs[secs] { fail("markers: no SCTE35-OUT for a %d s break", secs) }
        if !cueOut[secs] { fail("markers: no EXT-X-CUE-OUT:%d", secs) }
    }
    if ins < 2 { fail("markers: %d SCTE35-IN, want 2 (one per break)", ins) }
    if cueIn < 1 { fail("markers: no EXT-X-CUE-IN") }
    if len(failures) == before { logf("markers ok: both breaks (16 s and 10 s) have SCTE35-OUT/IN that decode, and CUE-OUT/IN") }
}

func decodeSplice(line, key string) (*scte35.SpliceInsert, bool) {
    i := strings.Index(line, key)
    if i < 0 { return nil, false }
    v := line[i+len(key):]
    if j := strings.IndexAny(v, ", "); j >= 0 { v = v[:j] }
    raw, err := scte35.Parse(v)
    if err != nil { return nil, false }
    sec, err := scte35.Decode(raw)
    if err != nil || sec.Insert == nil { return nil, false }
    return sec.Insert, true
}

// --- checks ---

func checkMaster() {
    b := httpGet(originBase + "/hls/" + channel + "/master.m3u8")
    s := string(b)
    for _, want := range []string{"#EXTM3U", "#EXT-X-STREAM-INF:", "v720.m3u8", "CODECS="} {
        if !strings.Contains(s, want) { fail("master playlist lacks %q", want) }
    }
    logf("master playlist ok")
}

func checkStructure(pl hls.Playlist) {
    // time grid: one segment every second, EXTINF exactly 1 s
    for i := 1; i < len(pl.Segments); i++ {
        if d := pl.Segments[i].ProgramDateTime.Sub(pl.Segments[i-1].ProgramDateTime); d != slot {
            fail("segments %d and %d are %v apart in program date time, want %v", i-1, i, d, slot)
            break
        }
    }
    for i, s := range pl.Segments {
        if s.Duration != 1.0 { fail("segment %d lasts %.3fs, every segment must last exactly 1 s", i, s.Duration); break }
    }
    // Every slot carries the picture of the right source segment. The segment is a copy with other decode
    // times, so the pictures (mdat) are compared.
    byTime := map[int64]hls.Segment{}
    for _, s := range pl.Segments { byTime[s.ProgramDateTime.Unix()] = s }
    disc := map[int64]bool{}
    for _, n := range []int{0, m1Slots, m1Slots + ad1Slots, m1Slots + adSlots, m1Slots + adSlots + m2Slots, total} { disc[s1.Unix()+int64(n)] = true }
    for _, n := range []int{0, k2, k2 + cmdBreak, phase2Len} { disc[t2.Unix()+int64(n)] = true }
    var slots []int64
    for n := 0; n <= total; n++ { slots = append(slots, s1.Unix()+int64(n)) }
    for n := 0; n <= phase2Len; n++ { slots = append(slots, t2.Unix()+int64(n)) }
    bad := 0
    for _, unix := range slots {
        at := time.Unix(unix, 0).UTC()
        seg, ok := byTime[unix]
        if !ok { fail("slot %s is missing from the playlist", at.Format("15:04:05")); bad++; continue }
        want, _ := expectedAt(at)
        got := mediaOf(seg.URI)
        if got == nil || !bytes.Equal(got, mediaOf(want)) { fail("slot %s: the picture is not the one of %s", at.Format("15:04:05"), want); bad++ }
        if seg.Discontinuity != disc[unix] { fail("slot %s: discontinuity=%v, want %v", at.Format("15:04:05"), seg.Discontinuity, disc[unix]); bad++ }
    }
    if bad == 0 { logf("structure ok: %d checked slots of both phases carry the right source segments, discontinuities only at source changes", len(slots)) }
}

// mediaOf returns the picture data (mdat) of a segment given by URL or by a file under /media, nil on failure.
func mediaOf(u string) []byte {
    var b []byte
    var err error
    if strings.HasPrefix(u, "/") { b, err = os.ReadFile(u) } else { b, err = fetchBytes(u) }
    if err != nil { return nil }
    m, err := fmp4.MediaData(b)
    if err != nil { return nil }
    return m
}

// checkTimeline verifies that, per track, every segment starts where the previous one ended: this is what lets any
// player (and ffmpeg) play the stream across ads and movies without losing a segment.
func checkTimeline(pl hls.Playlist) {
    type end struct{ ticks uint64; scale uint32 }
    prev := map[string]end{}
    inits := map[string]fmp4.Init{}
    bad := 0
    for i, sg := range pl.Segments {
        in, ok := inits[sg.MapURI]
        if !ok {
            raw, err := fetchBytes(sg.MapURI)
            if err == nil { in, err = fmp4.ParseInit(raw) }
            if err != nil { fail("init section %s: %v", sg.MapURI, err); return }
            inits[sg.MapURI] = in
        }
        raw, err := fetchBytes(sg.URI)
        if err != nil { fail("segment %d: %v", i, err); return }
        frags, err := fmp4.Fragments(raw, in)
        if err != nil { fail("segment %d (%s): %v", i, sg.URI, err); return }
        for _, f := range frags {
            if p, ok := prev[f.Track.Kind]; ok && p.scale == f.Track.Timescale {
                gap := int64(f.Tfdt) - int64(p.ticks)
                // inside a file it is exact. Where the source changes the audio of the two files was cut at different
                // places, so it may differ by a few audio frames (observed up to 62 ms; 0.1 s is allowed); the picture never does
                limit := int64(0)
                if sg.Discontinuity && f.Track.Kind == "soun" { limit = int64(f.Track.Timescale) / 10 } // 0.1 s
                if gap > limit || gap < -limit {
                    fail("segment %d (%s) track %s starts at %d, the previous one ended at %d (off by %d ticks)", i, sg.URI, f.Track.Kind, f.Tfdt, p.ticks, gap)
                    bad++
                }
            }
            prev[f.Track.Kind] = end{f.Tfdt + f.Duration, f.Track.Timescale}
        }
        if bad >= 5 { return }
    }
    if bad == 0 { logf("timeline ok: decode times of all %d segments follow each other, across every source change", len(pl.Segments)) }
}

// saveStream keeps everything the player would download, plus a playlist that points at the saved files, so that a failed
// run can be examined with ffmpeg afterwards without bringing the stack up again.
func saveStream(pl hls.Playlist) {
    dir := filepath.Join(outDir, "stream")
    _ = os.MkdirAll(dir, 0o755)
    var b strings.Builder
    b.WriteString("#EXTM3U\n#EXT-X-VERSION:7\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:0\n")
    cur := ""
    for _, sg := range pl.Segments {
        if sg.Discontinuity { b.WriteString("#EXT-X-DISCONTINUITY\n") }
        if sg.MapURI != "" && (sg.MapURI != cur || sg.Discontinuity) {
            fmt.Fprintf(&b, "#EXT-X-MAP:URI=\"%s\"\n", filepath.Base(sg.MapURI))
            cur = sg.MapURI
        }
        fmt.Fprintf(&b, "#EXT-X-PROGRAM-DATE-TIME:%s\n#EXTINF:%.3f,\n%s\n", sg.ProgramDateTime.UTC().Format("2006-01-02T15:04:05.000Z"), sg.Duration, filepath.Base(sg.URI))
        for _, u := range []string{sg.URI, sg.MapURI} {
            if u == "" { continue }
            if data, err := fetchBytes(u); err == nil { _ = os.WriteFile(filepath.Join(dir, filepath.Base(u)), data, 0o644) }
        }
    }
    b.WriteString("#EXT-X-ENDLIST\n")
    _ = os.WriteFile(filepath.Join(dir, "local.m3u8"), []byte(b.String()), 0o644)
}

// checkPlayer judges what the live player went through: nothing missing, nothing late, the window as configured.
func checkPlayer() {
    p := player
    p.mu.Lock()
    fetchErrs, pollErrs, pollMsg, gaps, maxW, minW, polls := p.fetchErrs, p.pollErrs, p.pollErrMsg, p.seqGaps, p.maxWindow, p.minWindow, p.polls
    p.mu.Unlock()
    t := p.timings()
    wantWindow, minSlack, maxGap := 180, 3*time.Second, 2500*time.Millisecond
    if profile == "tight" { wantWindow, minSlack, maxGap = 2, 300*time.Millisecond, 1600*time.Millisecond }
    bad := 0
    if len(fetchErrs) > 0 { fail("the player could not download %d files, for example %s", len(fetchErrs), fetchErrs[0]); bad++ }
    if pollErrs > 0 { fail("%d of %d playlist reads failed or did not parse (last: %s)", pollErrs, polls, pollMsg); bad++ }
    if gaps > 0 { fail("the playlist moved past segments the player had not seen, %d times", gaps); bad++ }
    if maxW > wantWindow { fail("the playlist held %d segments, the window is %d", maxW, wantWindow); bad++ }
    if t.minSlack < minSlack { fail("a segment was downloaded only %v before its air time, at least %v is required", t.minSlack, minSlack); bad++ }
    if t.maxChangeGap > maxGap { fail("the playlist did not change for %v", t.maxChangeGap); bad++ }
    logf("player timings: %d segments, window %d..%d, %d polls; a segment was ready %v before its air time at the least (median %v); longest time without a new playlist %v",
        t.segments, minW, maxW, polls, t.minSlack.Round(time.Millisecond), t.medSlack.Round(time.Millisecond), t.maxChangeGap.Round(time.Millisecond))
    if bad == 0 { logf("player ok: no download failed, no segment was missed, none was late") }
}

type liveFfmpegResult struct {
    frames  int
    errs    string
    err     error
    seconds float64
}

// runLiveFfmpeg plays the live stream with ffmpeg for a while, the way a real client would, and counts the pictures.
func runLiveFfmpeg(d time.Duration) liveFfmpegResult {
    started := time.Now()
    cmd := ffmpegCmd("-nostdin", "-v", "error", "-progress", "pipe:1", "-i", playlistURL(), "-t", fmt.Sprintf("%d", int(d.Seconds())), "-f", "null", "-")
    var stderr strings.Builder
    cmd.Stderr = &stderr
    out, err := cmd.Output()
    res := liveFfmpegResult{err: err, errs: strings.TrimSpace(stderr.String()), seconds: time.Since(started).Seconds()}
    for _, l := range strings.Split(string(out), "\n") {
        if strings.HasPrefix(l, "frame=") { fmt.Sscanf(strings.TrimPrefix(l, "frame="), "%d", &res.frames) }
    }
    return res
}

func checkLiveFfmpeg(r liveFfmpegResult) {
    if r.err != nil { fail("ffmpeg could not follow the live playlist: %v", r.err); return }
    if strings.Contains(r.errs, "error while decoding") || strings.Contains(r.errs, "Invalid data") {
        fail("ffmpeg reported errors while following the live playlist:\n%.600s", r.errs)
        return
    }
    // 45 seconds of the stream at 25 fps; ffmpeg needs a few seconds to start
    if want := 25 * 30; r.frames < want { fail("ffmpeg followed the live playlist for %.0f s and got only %d pictures (at least %d expected)", r.seconds, r.frames, want); return }
    if r.errs != "" { logf("ffmpeg warnings while following the live playlist: %.300s", r.errs) }
    logf("live ffmpeg ok: followed the live playlist for %.0f s, %d pictures, no decoding errors", r.seconds, r.frames)
}

func playlistURL() string { return originBase + "/hls/" + channel + "/v720.m3u8" }

// snapshot is a playlist of the whole collected stream on disk, with an end: ffmpeg would follow a live one forever.
var snapshot string

// ffmpegCmd never lets ffmpeg run for more than three minutes.
func ffmpegCmd(args ...string) *exec.Cmd {
    ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
    _ = cancel // released when the process ends or the timeout fires
    return exec.CommandContext(ctx, "ffmpeg", args...)
}

var ffmpegIn = []string{"-protocol_whitelist", "file,http,https,tcp,tls,crypto"}

func checkDecode(pl hls.Playlist) {
    cmd := ffmpegCmd(append(append([]string{"-nostdin", "-v", "error"}, ffmpegIn...), "-i", snapshot, "-f", "null", "-")...)
    var stderr bytes.Buffer
    cmd.Stderr = &stderr
    started := time.Now()
    if err := cmd.Run(); err != nil { fail("ffmpeg could not decode the stream: %v\n%s", err, stderr.String()); return }
    if s := strings.TrimSpace(stderr.String()); s != "" { fail("ffmpeg reported errors while decoding:\n%s", s); return }

    // Every segment must give all its pictures: ffmpeg drops frames when decode times step back.
    probe := exec.Command("ffprobe", append(append([]string{"-v", "error"}, ffmpegIn...), "-select_streams", "v", "-count_frames",
        "-show_entries", "stream=nb_read_frames", "-of", "csv=p=0", snapshot)...)
    out, err := probe.Output()
    if err != nil { fail("ffprobe: %v", err); return }
    frames := 0
    fmt.Sscanf(strings.TrimSpace(strings.Split(string(out), "\n")[0]), "%d", &frames)
    if want := framesPer * len(pl.Segments); frames != want { fail("ffmpeg decoded %d pictures, %d segments should give %d", frames, len(pl.Segments), want); return }
    logf("decode ok: ffmpeg played the whole stream in %.1fs without errors, %d pictures = %d per segment, none lost", time.Since(started).Seconds(), frames, framesPer)
}

var colors = map[string]func(r, g, b int) bool{
    "blue":  func(r, g, b int) bool { return b > r+80 && b > g+80 },
    "red":   func(r, g, b int) bool { return r > g+80 && r > b+80 },
    "green": func(r, g, b int) bool { return g > r+60 && g > b+60 },
    "gray":  func(r, g, b int) bool { return abs(r-g) < 30 && abs(g-b) < 30 && r > 60 && r < 200 },
    "orange": func(r, g, b int) bool { return r > g+50 && g > b+80 },
    // the last second of the 5.6 s file: the picture fades out and then it is black, so it is dark (not gray, not bright)
    "dark": func(r, g, b int) bool { return r+g+b < 200 },
}

func abs(a int) int { if a < 0 { return -a }; return a }

// checkPictures decodes the whole stream and takes the middle frame of every segment (25 frames at 25 fps).
func checkPictures(pl hls.Playlist) {
    cmd := ffmpegCmd(append(append([]string{"-nostdin", "-v", "error"}, ffmpegIn...), "-i", snapshot,
        "-vf", `select=eq(mod(n\,25)\,12),scale=1:1,format=rgb24`, "-vsync", "0", "-f", "rawvideo", "-")...)
    var stderr bytes.Buffer
    cmd.Stderr = &stderr
    out, err := cmd.Output()
    if err != nil { fail("pixel extraction: %v %s", err, stderr.String()); return }
    if frames := len(out) / 3; frames != len(pl.Segments) { fail("decoded %d pictures for %d segments", frames, len(pl.Segments)); return }
    bad := 0
    for i, sg := range pl.Segments {
        n := int(sg.ProgramDateTime.Unix() - s1.Unix())
        _, color := expectedAt(sg.ProgramDateTime)
        red, green, blue := int(out[3*i]), int(out[3*i+1]), int(out[3*i+2])
        if !colors[color](red, green, blue) {
            fail("segment %d (slot %d): picture is rgb(%d,%d,%d), expected %s", i, n, red, green, blue, color)
            if bad++; bad >= 5 { break }
        }
    }
    if bad == 0 { logf("pictures ok: all %d segments have the colour of their source (blue movie, red ad, green movie, orange tail that fades to black, gray slate; the break cut into movie 2 is red)", len(pl.Segments)) }

    sheet := ffmpegCmd(append(append([]string{"-nostdin", "-v", "error", "-y"}, ffmpegIn...), "-i", snapshot,
        "-vf", `select=eq(mod(n\,50)\,12),scale=200:-1,tile=10x8`, "-frames:v", "1", "-vsync", "0", filepath.Join(outDir, "contact.png"))...)
    if b, err := sheet.CombinedOutput(); err != nil { logf("contact sheet not written: %v %s", err, b) }
}

func checkMetrics() {
    before := len(failures)
    m := string(httpGet(engineBase + "/metrics"))
    for _, name := range []string{"packager_clips_dropped_total", "packager_stitch_errors_total", "packager_init_changes_total"} {
        for _, line := range strings.Split(m, "\n") {
            if strings.HasPrefix(line, name+"{") && !strings.HasSuffix(line, " 0") { fail("packager metric is not zero: %s", line) }
        }
    }
    if !strings.Contains(m, "packager_segments_in_window") { fail("packager does not export packager_segments_in_window") }
    p := string(httpGet(engineBase + "/metrics"))
    for _, line := range strings.Split(p, "\n") {
        if strings.HasPrefix(line, "playout_ad_errors_total{") && !strings.HasSuffix(line, " 0") { fail("playout metric is not zero: %s", line) }
    }
    if len(failures) == before { logf("metrics ok: nothing dropped, no stitch errors, no ad errors") }
}

// --- helpers ---

func fetchPlaylist() (hls.Playlist, error) {
    b, err := tryGet(playlistURL())
    if err != nil { return hls.Playlist{}, err }
    base, _ := url.Parse(playlistURL())
    return hls.Parse(bytes.NewReader(b), base)
}

// call sends a request to the control API (or any URL starting with /) with an API key and returns status and body.
func call(method, path, key string, hdr map[string]string, body any) (int, []byte, error) {
    var rd io.Reader
    if body != nil { bs, _ := json.Marshal(body); rd = bytes.NewReader(bs) }
    req, err := http.NewRequestWithContext(context.Background(), method, apiBase+path, rd)
    if err != nil { return 0, nil, err }
    if body != nil { req.Header.Set("Content-Type", "application/json") }
    if key != "" { req.Header.Set("X-API-Key", key) }
    for k, v := range hdr { req.Header.Set(k, v) }
    resp, err := http.DefaultClient.Do(req)
    if err != nil { return 0, nil, err }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    return resp.StatusCode, b, nil
}

// fetchBytes serves a file of the stream from what the live player downloaded, and from the network otherwise.
func fetchBytes(u string) ([]byte, error) {
    if b, ok := player.bytes(u); ok { return b, nil }
    return tryGet(u)
}

func tryGet(u string) ([]byte, error) {
    resp, err := http.Get(u)
    if err != nil { return nil, err }
    defer resp.Body.Close()
    b, _ := io.ReadAll(resp.Body)
    if resp.StatusCode != 200 { return nil, fmt.Errorf("GET %s: %d", u, resp.StatusCode) }
    return b, nil
}

func httpGet(u string) []byte {
    b, err := tryGet(u)
    if err != nil { fail("%v", err) }
    return b
}

func waitHTTP(u string, d time.Duration) error {
    deadline := time.Now().Add(d)
    var last error
    for time.Now().Before(deadline) {
        resp, err := http.Get(u)
        if err == nil {
            resp.Body.Close()
            if resp.StatusCode == 200 { return nil }
            last = fmt.Errorf("status %d", resp.StatusCode)
        } else {
            last = err
        }
        time.Sleep(time.Second)
    }
    return fmt.Errorf("%s did not become ready: %v", u, last)
}

func saveFile(name string, b []byte) { _ = os.WriteFile(filepath.Join(outDir, name), b, 0o644) }
