package main

import (
    "bytes"
    "context"
    "crypto/sha256"
    "encoding/hex"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "net/http"
    "regexp"
    "strconv"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"
    "github.com/rs/zerolog"

    "ftvx/pkg/auth"
    "ftvx/pkg/config"
    "ftvx/pkg/httpx"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
    "ftvx/pkg/schedule"
    "ftvx/pkg/storage"
)

type server struct {
    st      *storage.Postgres
    keys    *auth.Keys
    lg      zerolog.Logger
    cueLead time.Duration    // the earliest a command can reach the air: the engine needs this long
    now     func() time.Time // replaced in tests
}

func newServer(st *storage.Postgres, keys *auth.Keys, lg zerolog.Logger, cueLead time.Duration) *server {
    return &server{st: st, keys: keys, lg: lg, cueLead: cueLead, now: func() time.Time { return time.Now().UTC() }}
}

var validID = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

func channelParam(r *http.Request) string { return chi.URLParam(r, "channel_id") }

func (s *server) routes() http.Handler {
    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer, middleware.Timeout(30*time.Second))
    r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
    r.Get("/readyz", s.readyz)
    r.Method("GET", "/metrics", metrics.Handler())

    read := s.keys.Require(auth.Read, channelParam)
    readAll := s.keys.Require(auth.Read, nil)
    sched := s.keys.Require(auth.Schedule, channelParam)
    command := s.keys.Require(auth.Command, channelParam)
    admin := s.keys.Require(auth.Admin, nil)

    r.Route("/v1", func(r chi.Router) {
        r.With(readAll).Get("/channels", s.listChannels)
        r.With(admin).Post("/channels", s.createChannel)
        r.With(read).Get("/channels/{channel_id}", s.getChannel)
        r.With(admin).Patch("/channels/{channel_id}", s.updateChannel)
        r.With(admin).Delete("/channels/{channel_id}", s.deleteChannel)
        r.With(read).Get("/channels/{channel_id}/schedule", s.getSchedule)
        r.With(sched).Post("/channels/{channel_id}/schedule:replace", s.replaceSchedule)
        r.With(read).Get("/channels/{channel_id}/now-next", s.nowNext)
        r.With(command).Post("/channels/{channel_id}/commands", s.command)
        r.With(command).Post("/channels/{channel_id}/scte35", s.scte35)

        r.With(readAll).Get("/assets", s.listAssets)
        r.With(admin).Post("/assets", s.createAsset)
        r.With(readAll).Get("/assets/{asset_id}", s.getAsset)
        r.With(admin).Delete("/assets/{asset_id}", s.deleteAsset)
    })
    return r
}

func (s *server) readyz(w http.ResponseWriter, r *http.Request) {
    ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
    defer cancel()
    if err := s.st.Ping(ctx); err != nil { httpx.WriteProblem(w, 503, "postgres not ready", err.Error()); return }
    w.WriteHeader(200)
}

// --- helpers ---

func (s *server) fail(w http.ResponseWriter, err error) {
    var p *schedule.Problem
    var vm storage.VersionMismatch
    var nothing schedule.ErrNothing
    switch {
    case errors.As(err, &p): httpx.WriteProblem(w, 400, "Validation error", p.Error())
    case errors.As(err, &vm):
        w.Header().Set("ETag", etag(vm.Current))
        httpx.WriteProblem(w, 412, "Precondition failed", fmt.Sprintf("the schedule is at version %d, not the one in If-Match", vm.Current))
    case errors.As(err, &nothing): httpx.WriteProblem(w, 409, "Nothing to do", nothing.Msg)
    case errors.Is(err, storage.ErrNotFound): httpx.WriteProblem(w, 404, "Not Found", "no such resource")
    case errors.Is(err, storage.ErrConflict): httpx.WriteProblem(w, 409, "Conflict", "it already exists")
    case errors.Is(err, storage.ErrAssetInUse): httpx.WriteProblem(w, 409, "Conflict", "the asset is used by a schedule; replace those entries first")
    case errors.Is(err, storage.ErrUnknownReference): httpx.WriteProblem(w, 422, "Unprocessable", "an entry refers to an asset that does not exist")
    case errors.Is(err, storage.ErrIdempotencyConflict): httpx.WriteProblem(w, 422, "Idempotency-Key reused", "this key was used for a different request")
    default:
        s.lg.Error().Err(err).Msg("request failed")
        httpx.WriteProblem(w, 500, "Internal error", "the request could not be completed")
    }
}

func etag(v int64) string { return `"` + strconv.FormatInt(v, 10) + `"` }

func parseIfMatch(r *http.Request) (*int64, error) {
    h := strings.TrimSpace(r.Header.Get("If-Match"))
    if h == "" || h == "*" { return nil, nil }
    v, err := strconv.ParseInt(strings.Trim(h, `"`), 10, 64)
    if err != nil || v < 0 { return nil, &schedule.Problem{Field: "If-Match", Msg: "must be a schedule version such as \"3\""} }
    return &v, nil
}

func queryInt(r *http.Request, name string, def, max int) (int, error) {
    v := r.URL.Query().Get(name)
    if v == "" { return def, nil }
    n, err := strconv.Atoi(v)
    if err != nil || n < 1 || n > max { return 0, &schedule.Problem{Field: name, Msg: fmt.Sprintf("must be between 1 and %d", max)} }
    return n, nil
}

func readBody(r *http.Request) ([]byte, error) {
    defer r.Body.Close()
    b, err := io.ReadAll(io.LimitReader(r.Body, 10<<20+1))
    if err != nil { return nil, err }
    if len(b) > 10<<20 { return nil, &schedule.Problem{Field: "body", Msg: "larger than 10 MB"} }
    return b, nil
}

func decodeStrict(b []byte, dst any) error {
    dec := json.NewDecoder(bytes.NewReader(b))
    dec.DisallowUnknownFields()
    if err := dec.Decode(dst); err != nil { return &schedule.Problem{Field: "body", Msg: "invalid JSON: " + err.Error()} }
    if dec.More() { return &schedule.Problem{Field: "body", Msg: "unexpected data after the JSON document"} }
    return nil
}

func idemKey(r *http.Request) (string, error) {
    k := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
    if len(k) > 128 { return "", &schedule.Problem{Field: "Idempotency-Key", Msg: "longer than 128 characters"} }
    return k, nil
}

func bodyHash(channel, op string, body []byte) string {
    h := sha256.New()
    h.Write([]byte(channel + "\n" + op + "\n"))
    h.Write(body)
    return hex.EncodeToString(h.Sum(nil))
}

// --- channels ---

func (s *server) listChannels(w http.ResponseWriter, r *http.Request) {
    limit, err := queryInt(r, "limit", 50, 200)
    if err != nil { s.fail(w, err); return }
    items, err := s.st.ListChannelsPage(r.Context(), r.URL.Query().Get("cursor"), limit+1)
    if err != nil { s.fail(w, err); return }
    next := ""
    if len(items) > limit { items = items[:limit]; next = items[limit-1].ID }
    // a key limited to some channels sees only those
    if k := auth.From(r.Context()); k != nil {
        kept := items[:0]
        for _, c := range items { if k.Allows(auth.Read, c.ID) { kept = append(kept, c) } }
        items = kept
    }
    httpx.WriteJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (s *server) createChannel(w http.ResponseWriter, r *http.Request) {
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in struct {
        ID         string     `json:"id"`
        Name       string     `json:"name"`
        SegmentSec int        `json:"segment_sec"`
        Ladder     ladderList `json:"ladder"`
    }
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    if !validID.MatchString(in.ID) { s.fail(w, &schedule.Problem{Field: "id", Msg: "may contain letters, digits, '_', '-', '.' (max 64)"}); return }
    if strings.TrimSpace(in.Name) == "" { s.fail(w, &schedule.Problem{Field: "name", Msg: "is required"}); return }
    if in.SegmentSec == 0 { in.SegmentSec = config.SegmentSec }
    if in.SegmentSec != config.SegmentSec {
        s.fail(w, &schedule.Problem{Field: "segment_sec", Msg: fmt.Sprintf("must be %d: every segment of the stream lasts exactly that long", config.SegmentSec)})
        return
    }
    c := models.Channel{ID: in.ID, Name: in.Name, SegmentSec: in.SegmentSec, Ladder: []string(in.Ladder), CreatedAt: s.now()}
    if err := s.st.InsertChannelChecked(r.Context(), c); err != nil { s.fail(w, err); return }
    got, err := s.st.GetChannelChecked(r.Context(), c.ID)
    if err != nil { s.fail(w, err); return }
    w.Header().Set("Location", "/v1/channels/"+c.ID)
    httpx.WriteJSON(w, 201, got)
}

func (s *server) getChannel(w http.ResponseWriter, r *http.Request) {
    c, err := s.st.GetChannelChecked(r.Context(), channelParam(r))
    if err != nil { s.fail(w, err); return }
    w.Header().Set("ETag", etag(c.ScheduleVersion))
    httpx.WriteJSON(w, 200, c)
}

func (s *server) updateChannel(w http.ResponseWriter, r *http.Request) {
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in struct {
        Name   *string    `json:"name"`
        Ladder ladderList `json:"ladder"`
    }
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    if in.Name != nil && strings.TrimSpace(*in.Name) == "" { s.fail(w, &schedule.Problem{Field: "name", Msg: "must not be empty"}); return }
    c, err := s.st.UpdateChannel(r.Context(), channelParam(r), in.Name, []string(in.Ladder))
    if err != nil { s.fail(w, err); return }
    httpx.WriteJSON(w, 200, c)
}

func (s *server) deleteChannel(w http.ResponseWriter, r *http.Request) {
    if err := s.st.DeleteChannel(r.Context(), channelParam(r)); err != nil { s.fail(w, err); return }
    w.WriteHeader(204)
}

// --- assets ---

func (s *server) listAssets(w http.ResponseWriter, r *http.Request) {
    limit, err := queryInt(r, "limit", 50, 200)
    if err != nil { s.fail(w, err); return }
    items, err := s.st.ListAssets(r.Context(), r.URL.Query().Get("tag"), r.URL.Query().Get("cursor"), limit+1)
    if err != nil { s.fail(w, err); return }
    next := ""
    if len(items) > limit { items = items[:limit]; next = items[limit-1].ID }
    httpx.WriteJSON(w, 200, map[string]any{"items": items, "next_cursor": next})
}

func (s *server) createAsset(w http.ResponseWriter, r *http.Request) {
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in struct {
        ID        string         `json:"id"`
        URI       string         `json:"uri"`
        DurationS int            `json:"duration_s"`
        Tags      []string       `json:"tags"`
        Meta      map[string]any `json:"meta"`
    }
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    if len(in.URI) < 5 { s.fail(w, &schedule.Problem{Field: "uri", Msg: "is required"}); return }
    if in.DurationS < 1 { s.fail(w, &schedule.Problem{Field: "duration_s", Msg: "must be at least 1"}); return }
    id := in.ID
    if id == "" {
        id = "m_" + s.now().Format("20060102T150405.000000000")
    } else if !validID.MatchString(id) {
        s.fail(w, &schedule.Problem{Field: "id", Msg: "may contain letters, digits, '_', '-', '.' (max 64)"})
        return
    }
    a := models.Asset{ID: id, URI: in.URI, DurationS: in.DurationS, Tags: in.Tags, Meta: in.Meta, CreatedAt: s.now()}
    if err := s.st.InsertAssetChecked(r.Context(), a); err != nil { s.fail(w, err); return }
    w.Header().Set("Location", "/v1/assets/"+id)
    httpx.WriteJSON(w, 201, map[string]string{"asset_id": id})
}

func (s *server) getAsset(w http.ResponseWriter, r *http.Request) {
    a, err := s.st.GetAssetChecked(r.Context(), chi.URLParam(r, "asset_id"))
    if err != nil { s.fail(w, err); return }
    httpx.WriteJSON(w, 200, a)
}

func (s *server) deleteAsset(w http.ResponseWriter, r *http.Request) {
    if err := s.st.DeleteAsset(r.Context(), chi.URLParam(r, "asset_id")); err != nil { s.fail(w, err); return }
    w.WriteHeader(204)
}

// --- schedule ---

type entryIn struct {
    Type     string    `json:"type"`
    AssetID  string    `json:"asset_id"`
    URI      string    `json:"uri"`
    Start    time.Time `json:"start"`
    Duration int       `json:"duration"`
    OffsetS  int       `json:"offset_s"`
    Category string    `json:"category"`
    SCTE35   string    `json:"scte35"`
}

func (e entryIn) model() models.TimelineEntry {
    return models.TimelineEntry{Type: e.Type, AssetID: e.AssetID, URI: e.URI, Start: e.Start.UTC(), DurationS: e.Duration, OffsetS: e.OffsetS, Category: e.Category, SCTE35: e.SCTE35}
}

func (s *server) getSchedule(w http.ResponseWriter, r *http.Request) {
    ch, err := s.st.GetChannelChecked(r.Context(), channelParam(r))
    if err != nil { s.fail(w, err); return }
    q := r.URL.Query()
    from := s.now().Truncate(time.Second)
    if v := q.Get("from"); v != "" {
        if from, err = time.Parse(time.RFC3339, v); err != nil { s.fail(w, &schedule.Problem{Field: "from", Msg: "must be an RFC 3339 time"}); return }
    }
    to := from.Add(6 * time.Hour)
    if v := q.Get("to"); v != "" {
        if to, err = time.Parse(time.RFC3339, v); err != nil { s.fail(w, &schedule.Problem{Field: "to", Msg: "must be an RFC 3339 time"}); return }
    }
    if !to.After(from) || to.Sub(from) > schedule.MaxWindow { s.fail(w, &schedule.Problem{Field: "to", Msg: "must be after from, at most 12 hours"}); return }
    items, err := s.st.GetTimelineRange(r.Context(), ch.ID, from.UTC(), to.UTC())
    if err != nil { s.fail(w, err); return }
    w.Header().Set("ETag", etag(ch.ScheduleVersion))
    httpx.WriteJSON(w, 200, map[string]any{"channel_id": ch.ID, "from": from.UTC(), "to": to.UTC(), "version": ch.ScheduleVersion, "entries": items})
}

func (s *server) replaceSchedule(w http.ResponseWriter, r *http.Request) {
    ch, err := s.st.GetChannelChecked(r.Context(), channelParam(r))
    if err != nil { s.fail(w, err); return }
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in struct {
        Window struct {
            From time.Time `json:"from"`
            To   time.Time `json:"to"`
        } `json:"window"`
        Entries []entryIn `json:"entries"`
    }
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    ifMatch, err := parseIfMatch(r)
    if err != nil { s.fail(w, err); return }
    key, err := idemKey(r)
    if err != nil { s.fail(w, err); return }

    entries := make([]models.TimelineEntry, len(in.Entries))
    for i, e := range in.Entries { entries[i] = e.model() }
    entries, err = schedule.Validate(entries, in.Window.From.UTC(), in.Window.To.UTC())
    if err != nil { s.fail(w, err); return }
    if bad, err := s.unknownAssets(r.Context(), entries); err != nil {
        s.fail(w, err); return
    } else if len(bad) > 0 {
        httpx.WriteProblem(w, 422, "Unprocessable", "these assets are not registered: "+strings.Join(bad, ", "))
        return
    }
    if r.URL.Query().Get("dry_run") == "true" {
        httpx.WriteJSON(w, 200, map[string]any{"valid": true, "entries": len(entries), "version": ch.ScheduleVersion, "dry_run": true})
        return
    }
    res, err := s.st.ApplySchedule(r.Context(), storage.ApplyRequest{
        ChannelID: ch.ID, From: in.Window.From.UTC(), To: in.Window.To.UTC(), Entries: entries,
        IfMatch: ifMatch, IdemKey: key, BodyHash: bodyHash(ch.ID, "replace", body),
        Response: func(v int64) []byte { b, _ := json.Marshal(map[string]any{"replaced": len(entries), "version": v, "warnings": []string{}}); return b },
    })
    if err != nil { s.fail(w, err); return }
    s.answerApply(w, res, len(entries))
}

func (s *server) answerApply(w http.ResponseWriter, res storage.ApplyResult, n int) {
    w.Header().Set("ETag", etag(res.Version))
    if res.Replayed {
        w.Header().Set("Idempotent-Replay", "true")
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(200)
        _, _ = w.Write(res.Response)
        return
    }
    httpx.WriteJSON(w, 200, map[string]any{"replaced": n, "version": res.Version, "warnings": []string{}})
}

func (s *server) unknownAssets(ctx context.Context, entries []models.TimelineEntry) ([]string, error) {
    seen := map[string]bool{}
    var ids []string
    for _, e := range entries { if e.AssetID != "" && !seen[e.AssetID] { seen[e.AssetID] = true; ids = append(ids, e.AssetID) } }
    return s.st.MissingAssets(ctx, ids)
}

func (s *server) nowNext(w http.ResponseWriter, r *http.Request) {
    ch, err := s.st.GetChannelChecked(r.Context(), channelParam(r))
    if err != nil { s.fail(w, err); return }
    now := s.now().Truncate(time.Second)
    items, err := s.st.GetTimelineRange(r.Context(), ch.ID, now, now.Add(12*time.Hour))
    if err != nil { s.fail(w, err); return }
    type item struct {
        Type    string    `json:"type"`
        AssetID string    `json:"asset_id,omitempty"`
        URI     string    `json:"uri,omitempty"`
        Start   time.Time `json:"start"`
        End     time.Time `json:"end"`
    }
    conv := func(e models.TimelineEntry) *item {
        return &item{Type: e.Type, AssetID: e.AssetID, URI: e.URI, Start: e.Start, End: e.Start.Add(time.Duration(e.DurationS) * time.Second)}
    }
    var cur, next *item
    for _, e := range items {
        switch {
        case !e.Start.After(now) && cur == nil: cur = conv(e)
        case e.Start.After(now) && next == nil: next = conv(e)
        }
    }
    httpx.WriteJSON(w, 200, map[string]any{"channel_id": ch.ID, "server_time": now, "now": cur, "next": next})
}

// ladderList accepts ["1080p","720p"] and the older numeric form [1080,720].
type ladderList []string

func (l *ladderList) UnmarshalJSON(b []byte) error {
    var raw []json.RawMessage
    if err := json.Unmarshal(b, &raw); err != nil { return err }
    out := make([]string, 0, len(raw))
    for _, r := range raw {
        var s string
        if err := json.Unmarshal(r, &s); err == nil { out = append(out, s); continue }
        var n int
        if err := json.Unmarshal(r, &n); err != nil { return errors.New("ladder items must be strings or numbers") }
        out = append(out, strconv.Itoa(n))
    }
    *l = out
    return nil
}
