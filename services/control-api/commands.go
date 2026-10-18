package main

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/go-chi/chi/v5"

    "ftvx/pkg/httpx"
    "ftvx/pkg/models"
    "ftvx/pkg/schedule"
    "ftvx/pkg/scte35"
    "ftvx/pkg/storage"
)

type commandIn struct {
    Type     string `json:"type"`     // ad_break, insert, skip or slate
    At       string `json:"at"`       // empty or "now": as soon as possible; or an RFC 3339 time
    Duration int    `json:"duration"` // seconds; for insert it defaults to the duration of the asset
    AssetID  string `json:"asset_id"`
    URI      string `json:"uri"`
    Category string `json:"category"`
    SCTE35   string `json:"scte35"` // hex of a splice_info_section for an ad break

    onlyBreak bool // skip only if an ad break is on the air (the return message of SCTE-35)
}

const maxCommandSeconds = 4 * 3600

// command acts on the air: it edits the schedule so that the change reaches the stream as soon as the engine allows.
func (s *server) command(w http.ResponseWriter, r *http.Request) {
    ch, err := s.st.GetChannelChecked(r.Context(), chi.URLParam(r, "channel_id"))
    if err != nil { s.fail(w, err); return }
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in commandIn
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    key, err := idemKey(r)
    if err != nil { s.fail(w, err); return }
    res, err := s.runCommand(r.Context(), ch, in, key, bodyHash(ch.ID, "command", body))
    if err != nil { s.fail(w, err); return }
    if res.replayed {
        w.Header().Set("ETag", etag(res.version))
        w.Header().Set("Idempotent-Replay", "true")
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(200)
        _, _ = w.Write(res.stored)
        return
    }
    w.Header().Set("ETag", etag(res.version))
    httpx.WriteJSON(w, 200, res.answer())
}

type commandResult struct {
    Type        string    `json:"type"`
    EffectiveAt time.Time `json:"effective_at"` // when the change reaches the air
    Version     int64     `json:"version"`
    Entries     int       `json:"entries"` // entries written
    Warnings    []string  `json:"warnings,omitempty"`
    version     int64
    replayed    bool
    stored      []byte
}

func (c commandResult) answer() commandResult { c.version = 0; return c }

// earliest is the first second at which a command sent now can air.
func (s *server) earliest() time.Time {
    t := s.now().Add(s.cueLead)
    if t.Nanosecond() != 0 { t = t.Truncate(time.Second).Add(time.Second) }
    return t
}

func (s *server) runCommand(ctx context.Context, ch models.Channel, in commandIn, key, hash string) (commandResult, error) {
    switch in.Type {
    case "ad_break", "slate", "insert", "skip":
    default:
        return commandResult{}, &schedule.Problem{Field: "type", Msg: "must be ad_break, insert, skip or slate"}
    }
    at := s.earliest()
    if in.At != "" && in.At != "now" {
        t, err := time.Parse(time.RFC3339, in.At)
        if err != nil { return commandResult{}, &schedule.Problem{Field: "at", Msg: `must be "now" or an RFC 3339 time`} }
        if t.Nanosecond() != 0 { return commandResult{}, &schedule.Problem{Field: "at", Msg: "must be a whole second"} }
        if t.Before(at) { return commandResult{}, &schedule.Problem{Field: "at", Msg: fmt.Sprintf("is too soon, the earliest moment is %s", at.Format(time.RFC3339))} }
        at = t.UTC()
    }
    ins, err := s.inserted(ctx, in)
    if err != nil { return commandResult{}, err }

    var out commandResult
    res, err := s.st.EditSchedule(ctx, ch.ID, nil, key, hash, func(rd storage.Reader) (storage.Change, error) {
        end, err := rd.Horizon()
        if err != nil { return storage.Change{}, err }
        if end.Before(at) { end = at }
        existing, err := rd.Range(at, end.Add(time.Second))
        if err != nil { return storage.Change{}, err }
        if len(existing) > schedule.MaxEntries { return storage.Change{}, &schedule.Problem{Field: "schedule", Msg: "too long to edit in one step"} }
        var ed schedule.Edit
        if in.Type == "skip" {
            if in.onlyBreak {
                onAir := false
                for _, e := range existing { if e.Type == "ad_break" && !e.Start.After(at) && e.Start.Add(time.Duration(e.DurationS)*time.Second).After(at) { onAir = true } }
                if !onAir { return storage.Change{}, schedule.ErrNothing{Msg: "no ad break is on the air at that moment"} }
            }
            ed, err = schedule.Skip(existing, at)
        } else {
            ed, err = schedule.InsertShift(existing, at, ins)
        }
        if err != nil { return storage.Change{}, err }
        if _, err := schedule.CheckEntries(ed.Entries, ed.From, ed.To); err != nil { return storage.Change{}, err }
        out = commandResult{Type: in.Type, EffectiveAt: at, Entries: len(ed.Entries)}
        return storage.Change{From: ed.From, To: ed.To, Entries: ed.Entries,
            Response: func(v int64) []byte { o := out; o.Version = v; b, _ := json.Marshal(o); return b }}, nil
    })
    if err != nil { return commandResult{}, err }
    out.Version, out.version = res.Version, res.Version
    out.replayed, out.stored = res.Replayed, res.Response
    return out, nil
}

// inserted builds the entry that a command puts on the air (nothing for skip).
func (s *server) inserted(ctx context.Context, in commandIn) (models.TimelineEntry, error) {
    var e models.TimelineEntry
    switch in.Type {
    case "skip":
        return e, nil
    case "ad_break":
        e = models.TimelineEntry{Type: "ad_break", DurationS: in.Duration, Category: in.Category, SCTE35: in.SCTE35}
        if in.AssetID != "" || in.URI != "" { return e, &schedule.Problem{Field: "asset_id", Msg: "an ad_break has no asset: the ads service chooses what to play"} }
        if e.Category == "" { e.Category = "midroll" }
    case "slate":
        e = models.TimelineEntry{Type: "slate", DurationS: in.Duration}
    case "insert":
        e = models.TimelineEntry{Type: "content", AssetID: in.AssetID, URI: in.URI, DurationS: in.Duration}
        if e.AssetID == "" && e.URI == "" { return e, &schedule.Problem{Field: "asset_id", Msg: "insert needs an asset_id or a uri"} }
        if e.AssetID != "" {
            a, err := s.st.GetAssetChecked(ctx, e.AssetID)
            if errors.Is(err, storage.ErrNotFound) { return e, &schedule.Problem{Field: "asset_id", Msg: "this asset is not registered"} }
            if err != nil { return e, err }
            if e.DurationS == 0 { e.DurationS = a.DurationS }
        }
    }
    if e.DurationS < 1 || e.DurationS > maxCommandSeconds {
        return e, &schedule.Problem{Field: "duration", Msg: fmt.Sprintf("is required, between 1 and %d seconds", maxCommandSeconds)}
    }
    if strings.TrimSpace(e.Category) != e.Category { return e, &schedule.Problem{Field: "category", Msg: "has spaces around it"} }
    return e, nil
}

// scte35 takes a SCTE-35 message from an upstream system: "out of the network" starts an ad break of the length it
// carries, "return to the network" ends the break that is on the air.
func (s *server) scte35(w http.ResponseWriter, r *http.Request) {
    ch, err := s.st.GetChannelChecked(r.Context(), chi.URLParam(r, "channel_id"))
    if err != nil { s.fail(w, err); return }
    body, err := readBody(r)
    if err != nil { s.fail(w, err); return }
    var in struct {
        Message string `json:"message"` // the splice_info_section, hex (with or without 0x) or base64
        At      string `json:"at"`      // optional: when to apply it (default: as soon as possible)
    }
    if err := decodeStrict(body, &in); err != nil { s.fail(w, err); return }
    key, err := idemKey(r)
    if err != nil { s.fail(w, err); return }
    raw, err := scte35.Parse(in.Message)
    if err != nil { s.fail(w, &schedule.Problem{Field: "message", Msg: err.Error()}); return }
    sec, err := scte35.Decode(raw)
    if err != nil { s.fail(w, &schedule.Problem{Field: "message", Msg: err.Error()}); return }
    if sec.Insert == nil { s.fail(w, &schedule.Problem{Field: "message", Msg: "only splice_insert is understood"}); return }
    ins := sec.Insert
    if ins.Cancel { s.fail(w, &schedule.Problem{Field: "message", Msg: "splice_event_cancel_indicator is not supported"}); return }

    cmd := commandIn{At: in.At}
    var warnings []string
    if ins.Out {
        secs := int((ins.DurationTicks + scte35.TickRate - 1) / scte35.TickRate)
        if secs < 1 { s.fail(w, &schedule.Problem{Field: "message", Msg: "a message that starts a break must carry its break_duration"}); return }
        cmd.Type, cmd.Duration, cmd.SCTE35 = "ad_break", secs, scte35.Hex(raw)
        if !ins.Immediate { warnings = append(warnings, "splice_time is not used: the break starts at effective_at") }
    } else {
        cmd.Type, cmd.onlyBreak = "skip", true
    }
    res, err := s.runCommand(r.Context(), ch, cmd, key, bodyHash(ch.ID, "scte35", body))
    if err != nil { s.fail(w, err); return }
    if res.replayed {
        w.Header().Set("ETag", etag(res.version))
        w.Header().Set("Idempotent-Replay", "true")
        w.Header().Set("Content-Type", "application/json")
        w.WriteHeader(200)
        _, _ = w.Write(res.stored)
        return
    }
    res.Warnings = warnings
    w.Header().Set("ETag", etag(res.version))
    httpx.WriteJSON(w, 200, res)
}
