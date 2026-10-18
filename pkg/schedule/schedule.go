// Package schedule checks a schedule and edits it the way a broadcaster does on the air: cut an ad break into the
// running movie and let the movie go on afterwards, skip what is playing, hold the picture with a slate.
// It works on plain lists of entries, no database, so that every rule can be tested on its own.
package schedule

import (
    "fmt"
    "sort"
    "time"

    "ftvx/pkg/models"
)

type Entry = models.TimelineEntry

const (
    MaxWindow  = 12 * time.Hour
    MaxEntries = 20000
)

// Problem is something wrong with a request; Field names the part that is wrong.
type Problem struct{ Field, Msg string }

func (p *Problem) Error() string { return p.Field + ": " + p.Msg }

func bad(field, format string, a ...any) *Problem { return &Problem{Field: field, Msg: fmt.Sprintf(format, a...)} }

var validTypes = map[string]bool{"content": true, "bumper": true, "slate": true, "ad_break": true}

// Validate checks entries that are to replace the window [from,to) and returns them sorted.
// Times are whole seconds (every segment lasts one second), entries do not overlap and start inside the window.
func Validate(entries []Entry, from, to time.Time) ([]Entry, error) {
    if from.IsZero() || to.IsZero() || !to.After(from) { return nil, bad("window", "from must be before to") }
    if to.Sub(from) > MaxWindow { return nil, bad("window", "the window is longer than %v", MaxWindow) }
    if from.Nanosecond() != 0 || to.Nanosecond() != 0 { return nil, bad("window", "times must be whole seconds") }
    return CheckEntries(entries, from, to)
}

// CheckEntries is Validate without the limit on the length of the window: the entries produced by an edit of the schedule
// may reach far into the future.
func CheckEntries(entries []Entry, from, to time.Time) ([]Entry, error) {
    if len(entries) > MaxEntries { return nil, bad("entries", "more than %d entries", MaxEntries) }
    out := append([]Entry(nil), entries...)
    sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
    for i, e := range out {
        f := fmt.Sprintf("entries[%d]", i)
        if !validTypes[e.Type] { return nil, bad(f+".type", "must be content, bumper, slate or ad_break") }
        switch e.Type {
        case "content", "bumper":
            if e.AssetID == "" && e.URI == "" { return nil, bad(f, "a %s needs an asset_id or a uri", e.Type) }
        case "ad_break":
            if e.AssetID != "" || e.URI != "" { return nil, bad(f, "an ad_break has no asset: the ads service chooses what to play") }
        }
        if e.DurationS <= 0 { return nil, bad(f+".duration", "must be positive") }
        if e.OffsetS < 0 { return nil, bad(f+".offset_s", "must not be negative") }
        if e.Start.Nanosecond() != 0 { return nil, bad(f+".start", "must be a whole second") }
        if e.Start.Before(from) || !e.Start.Before(to) { return nil, bad(f+".start", "is outside the window") }
        if i > 0 {
            prevEnd := out[i-1].Start.Add(time.Duration(out[i-1].DurationS) * time.Second)
            if e.Start.Before(prevEnd) { return nil, bad(f, "overlaps the entry before it (it ends at %s)", prevEnd.Format(time.RFC3339)) }
        }
    }
    return out, nil
}

func endOf(e Entry) time.Time { return e.Start.Add(time.Duration(e.DurationS) * time.Second) }

// Edit says: replace everything that starts in [From,To) with Entries.
type Edit struct {
    From, To time.Time
    Entries  []Entry
}

// ErrNothing means that the command has nothing to act on.
type ErrNothing struct{ Msg string }

func (e ErrNothing) Error() string { return e.Msg }

// InsertShift puts ins on the air at at and shifts everything that follows later by its duration.
// The entry that is on the air at that moment is cut in two: the second part goes on from where the first stopped
// (OffsetS), so a movie resumes after the break at the same frame. existing must hold every entry that ends after at,
// sorted. An ad_break cannot be cut by another ad_break.
func InsertShift(existing []Entry, at time.Time, ins Entry) (Edit, error) {
    if at.Nanosecond() != 0 { return Edit{}, bad("at", "must be a whole second") }
    if ins.DurationS <= 0 { return Edit{}, bad("duration", "must be positive") }
    d := time.Duration(ins.DurationS) * time.Second
    ins.Start = at
    if ins.Type == "ad_break" {
        // a break that is on the air at that moment, or that starts at it, is the break the command asks for
        for _, e := range existing {
            if e.Type == "ad_break" && !e.Start.After(at) && endOf(e).After(at) { return Edit{}, ErrNothing{"an ad break is already on the air"} }
        }
    }
    ed := Edit{From: at}
    var after []Entry
    for _, e := range existing {
        end := endOf(e)
        if !end.After(at) { continue }
        switch {
        case e.Start.Before(at): // on the air at that moment: cut it
            ed.From = e.Start
            a := e
            a.DurationS = int(at.Sub(e.Start) / time.Second)
            ed.Entries = append(ed.Entries, a)
            b := e
            b.Start = at.Add(d)
            b.DurationS = int(end.Sub(at) / time.Second)
            b.OffsetS = e.OffsetS + a.DurationS
            after = append(after, b)
        default:
            e.Start = e.Start.Add(d)
            after = append(after, e)
        }
    }
    ed.Entries = append(ed.Entries, ins)
    ed.Entries = append(ed.Entries, after...)
    ed.To = endOf(ed.Entries[len(ed.Entries)-1])
    return ed, nil
}

// Skip ends the entry that is on the air at at, and pulls the entries that followed it back to back one after the
// other earlier, up to the first gap in the schedule.
func Skip(existing []Entry, at time.Time) (Edit, error) {
    if at.Nanosecond() != 0 { return Edit{}, bad("at", "must be a whole second") }
    idx := -1
    for i, e := range existing {
        if !e.Start.After(at) && endOf(e).After(at) { idx = i; break }
    }
    if idx < 0 { return Edit{}, ErrNothing{"nothing is scheduled at that moment"} }
    cur := existing[idx]
    pulled := endOf(cur).Sub(at)
    ed := Edit{From: cur.Start}
    if at.After(cur.Start) {
        a := cur
        a.DurationS = int(at.Sub(cur.Start) / time.Second)
        ed.Entries = append(ed.Entries, a)
    }
    prevEnd := endOf(cur)
    chain := true
    // the window must reach the OLD ends too: the entries that move earlier leave their old places inside it,
    // and whatever stays there would play a second time
    ed.To = endOf(cur)
    for _, e := range existing[idx+1:] {
        if oldEnd := endOf(e); oldEnd.After(ed.To) { ed.To = oldEnd }
        if chain && e.Start.Equal(prevEnd) {
            prevEnd = endOf(e)
            e.Start = e.Start.Add(-pulled)
        } else {
            chain = false
        }
        ed.Entries = append(ed.Entries, e)
    }
    if len(ed.Entries) == 0 { ed.To = endOf(cur) }
    return ed, nil
}
