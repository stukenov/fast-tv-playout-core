package main

import (
    "time"

    "ftvx/pkg/events"
    "ftvx/pkg/metrics"
    "ftvx/pkg/models"
)

// Events the engine publishes, to SSE (GET /v1/channels/{id}/events) and to webhooks (WEBHOOK_URLS):
//
//	engine.started    the process started
//	program.started   a program (content or bumper) goes on the air
//	break.started     an ad break goes on the air
//	break.ended       the break is over
//	slate.started     nothing is scheduled, or a source failed: the slate is on the air
//	slate.ended       the slate is over
//	source.error      a source could not be used (at most once in 10 s per source and reason)
//	schedule.stale    the schedule store does not answer; the channel airs the last schedule it knows
//	ads.fallback      no ad pod in time: the break is filled with slate
//
// "On the air" means the moment of the segment, not the moment it was written into the playlist.

// onAir is what the last announced segment was.
type onAir struct {
    kind, title string
    entry, brk  time.Time
}

func (a onAir) sameProgram(b onAir) bool { return a.kind == b.kind && a.title == b.title && a.entry.Equal(b.entry) }

func (e *engine) publish(typ string, data map[string]any) {
    metrics.EventsPublishedTotal.WithLabelValues(e.cfg.ChannelID, typ).Inc()
    e.hub.Publish(typ, e.cfg.ChannelID, data)
}

// announce publishes what has gone on the air since the last call.
func (e *engine) announce(now time.Time) {
    for _, it := range e.st.win.Items {
        if it.ProgramDateTime.After(now) { break }
        if !it.ProgramDateTime.After(e.announcedTo) { continue }
        e.announcedTo = it.ProgramDateTime
        if it.Kind == "" { continue } // restored from the playlist: its kind is not known
        e.transition(it)
    }
}

func (e *engine) transition(it SegmentMeta) {
    cur := onAir{kind: it.Kind, title: it.Title, entry: it.EntryStart, brk: it.BreakStart}
    prev := e.onAir
    if cur.kind == "ad" {
        if prev.kind == "ad" && prev.brk.Equal(cur.brk) { return } // the same break goes on
    } else if cur.sameProgram(prev) {
        return
    }
    at := it.ProgramDateTime
    if prev.kind == "ad" && (cur.kind != "ad" || !prev.brk.Equal(cur.brk)) {
        e.publish("break.ended", map[string]any{"at": at, "break_start": prev.brk})
    }
    if prev.kind == "slate" && cur.kind != "slate" { e.publish("slate.ended", map[string]any{"at": at}) }
    switch cur.kind {
    case "ad":
        e.publish("break.started", map[string]any{"at": at, "break_start": cur.brk, "duration": it.BreakDurS})
    case "slate":
        if prev.kind != "slate" { e.publish("slate.started", map[string]any{"at": at}) }
    default:
        e.publish("program.started", map[string]any{"at": at, "kind": cur.kind, "title": cur.title, "entry_start": cur.entry})
    }
    e.onAir = cur
}

// sourceFailed is told by the stitcher when a source could not be used. A source that is down for a minute would
// otherwise make sixty events a minute.
func (e *engine) sourceFailed(reason, uri string, c models.ClipItem, err error) {
    key := reason + "|" + uri
    if last, ok := e.failedAt[key]; ok && c.StartAt.Sub(last) < 10*time.Second { return }
    if len(e.failedAt) > 1000 { e.failedAt = map[string]time.Time{} }
    e.failedAt[key] = c.StartAt
    msg := ""
    if err != nil { msg = err.Error() }
    e.publish("source.error", map[string]any{"reason": reason, "uri": uri, "at": c.StartAt, "error": msg})
}

// notifyingPods tells the engine when a break had to be filled with slate.
type notifyingPods struct {
    podSource
    onGiveUp func(start time.Time)
}

func (n notifyingPods) giveUp(start time.Time) {
    n.podSource.giveUp(start)
    if n.onGiveUp != nil { n.onGiveUp(start) }
}

var _ = events.Event{}
