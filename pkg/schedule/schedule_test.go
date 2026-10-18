package schedule

import (
    "errors"
    "fmt"
    "math/rand"
    "sort"
    "strings"
    "testing"
    "time"
)

var t0 = time.Date(2025, 1, 1, 12, 0, 0, 0, time.UTC)

func at(n int) time.Time { return t0.Add(time.Duration(n) * time.Second) }

func content(asset string, start, secs, offset int) Entry {
    return Entry{Type: "content", AssetID: asset, Start: at(start), DurationS: secs, OffsetS: offset}
}

func TestValidate(t *testing.T) {
    from, to := at(0), at(100)
    ok := []Entry{content("a", 10, 5, 0), {Type: "ad_break", Start: at(15), DurationS: 3}, {Type: "slate", Start: at(18), DurationS: 2}}
    sorted, err := Validate([]Entry{ok[2], ok[0], ok[1]}, from, to)
    if err != nil || sorted[0].Type != "content" || sorted[2].Type != "slate" { t.Fatalf("valid entries, unsorted: %v %+v", err, sorted) }

    cases := map[string]struct{ e []Entry; want string }{
        "unknown type":           {[]Entry{{Type: "movie", Start: at(1), DurationS: 1, AssetID: "a"}}, "type"},
        "content without asset":  {[]Entry{{Type: "content", Start: at(1), DurationS: 1}}, "needs an asset_id or a uri"},
        "ad_break with an asset": {[]Entry{{Type: "ad_break", Start: at(1), DurationS: 1, AssetID: "a"}}, "no asset"},
        "zero duration":          {[]Entry{content("a", 1, 0, 0)}, "duration"},
        "negative offset":        {[]Entry{content("a", 1, 5, -1)}, "offset_s"},
        "fractional start":       {[]Entry{{Type: "slate", Start: at(1).Add(500 * time.Millisecond), DurationS: 1}}, "whole second"},
        "before the window":      {[]Entry{{Type: "slate", Start: at(-1), DurationS: 1}}, "outside"},
        "at the end of window":   {[]Entry{{Type: "slate", Start: at(100), DurationS: 1}}, "outside"},
        "overlap":                {[]Entry{content("a", 1, 10, 0), content("b", 5, 3, 0)}, "overlaps"},
    }
    for name, c := range cases {
        _, err := Validate(c.e, from, to)
        var p *Problem
        if !errors.As(err, &p) || !strings.Contains(p.Error(), c.want) { t.Errorf("%s: got %v, want something about %q", name, err, c.want) }
    }
    if _, err := Validate(ok, to, from); err == nil { t.Error("a window that ends before it starts") }
    if _, err := Validate(ok, from, from.Add(13*time.Hour)); err == nil { t.Error("a window of 13 hours") }
    // two entries that touch are fine
    if _, err := Validate([]Entry{content("a", 1, 4, 0), content("b", 5, 4, 0)}, from, to); err != nil { t.Errorf("back to back: %v", err) }
}

func TestInsertShift_CutsTheMovieAndResumesIt(t *testing.T) {
    movie := content("m", 0, 100, 0)
    next := content("n", 100, 50, 0)
    ed, err := InsertShift([]Entry{movie, next}, at(30), Entry{Type: "ad_break", DurationS: 20})
    if err != nil { t.Fatal(err) }
    want := []Entry{
        content("m", 0, 30, 0),
        {Type: "ad_break", Start: at(30), DurationS: 20},
        content("m", 50, 70, 30), // the movie goes on from second 30 of itself
        content("n", 120, 50, 0), // everything after is 20 s later
    }
    if len(ed.Entries) != len(want) { t.Fatalf("%+v", ed.Entries) }
    for i := range want { if ed.Entries[i] != want[i] { t.Fatalf("entry %d: %+v, want %+v", i, ed.Entries[i], want[i]) } }
    if !ed.From.Equal(at(0)) || !ed.To.Equal(at(170)) { t.Fatalf("the edit covers %v .. %v", ed.From, ed.To) }
}

func TestInsertShift_OnABoundaryAndInAGap(t *testing.T) {
    // exactly where an entry starts: nothing is cut, it just moves
    ed, err := InsertShift([]Entry{content("a", 10, 10, 0)}, at(10), Entry{Type: "slate", DurationS: 5})
    if err != nil || len(ed.Entries) != 2 || !ed.Entries[1].Start.Equal(at(15)) || ed.Entries[1].OffsetS != 0 { t.Fatalf("%v %+v", err, ed.Entries) }
    // in a gap before the next entry: the break is inserted and the entry moves
    ed, err = InsertShift([]Entry{content("a", 50, 10, 0)}, at(20), Entry{Type: "ad_break", DurationS: 5})
    if err != nil || len(ed.Entries) != 2 || !ed.From.Equal(at(20)) || !ed.Entries[1].Start.Equal(at(55)) { t.Fatalf("%v %+v", err, ed.Entries) }
    // an empty schedule: just the inserted entry
    ed, err = InsertShift(nil, at(20), Entry{Type: "ad_break", DurationS: 5})
    if err != nil || len(ed.Entries) != 1 || !ed.To.Equal(at(25)) { t.Fatalf("%v %+v", err, ed) }
    // entries that ended before are left alone
    ed, _ = InsertShift([]Entry{content("old", 0, 5, 0)}, at(20), Entry{Type: "ad_break", DurationS: 5})
    if len(ed.Entries) != 1 { t.Fatalf("%+v", ed.Entries) }
}

func TestInsertShift_AnAdBreakCannotBeCutByAnotherAdBreak(t *testing.T) {
    _, err := InsertShift([]Entry{{Type: "ad_break", Start: at(10), DurationS: 30}}, at(20), Entry{Type: "ad_break", DurationS: 10})
    var n ErrNothing
    if !errors.As(err, &n) { t.Fatalf("got %v", err) }
    // nor by one that starts at the very second where it starts: the same command twice is not two breaks
    if _, err := InsertShift([]Entry{{Type: "ad_break", Start: at(20), DurationS: 30}}, at(20), Entry{Type: "ad_break", DurationS: 10}); !errors.As(err, &n) { t.Fatalf("got %v", err) }
    // but a slate may be put into a break, and a break into a slate
    if _, err := InsertShift([]Entry{{Type: "ad_break", Start: at(10), DurationS: 30}}, at(20), Entry{Type: "slate", DurationS: 10}); err != nil { t.Fatal(err) }
}

func TestSkip(t *testing.T) {
    ed, err := Skip([]Entry{content("a", 0, 60, 0), content("b", 60, 30, 0), content("c", 90, 30, 0), content("d", 200, 30, 0)}, at(20))
    if err != nil { t.Fatal(err) }
    // a ends at 20, b and c (back to back with it) move 40 s earlier, d (after a gap) stays
    want := []Entry{content("a", 0, 20, 0), content("b", 20, 30, 0), content("c", 50, 30, 0), content("d", 200, 30, 0)}
    if len(ed.Entries) != len(want) { t.Fatalf("%+v", ed.Entries) }
    for i := range want { if ed.Entries[i] != want[i] { t.Fatalf("entry %d: %+v, want %+v", i, ed.Entries[i], want[i]) } }
    // skipping right at the start removes the entry
    ed, _ = Skip([]Entry{content("a", 0, 60, 0), content("b", 60, 30, 0)}, at(0))
    if len(ed.Entries) != 1 || ed.Entries[0].AssetID != "b" || !ed.Entries[0].Start.Equal(at(0)) { t.Fatalf("%+v", ed.Entries) }
    // a chain that is not followed by anything: the old copies of its entries must go with the edit
    old := []Entry{content("a", 0, 60, 0), content("b", 60, 30, 0), content("c", 90, 30, 0)}
    ed, _ = Skip(old, at(20))
    got := merge(old, ed)
    if len(got) != 3 || got[1].AssetID != "b" || !got[1].Start.Equal(at(20)) || got[2].AssetID != "c" || !got[2].Start.Equal(at(50)) { t.Fatalf("an old copy was left behind: %+v", got) }
    // nothing on the air
    if _, err := Skip([]Entry{content("a", 50, 10, 0)}, at(20)); !errors.As(err, new(ErrNothing)) { t.Fatalf("got %v", err) }
}

// --- properties on random schedules ---

// pos tells what is on the air at second n and where in its source: the picture of the stream at that moment.
func pos(entries []Entry, n int) string {
	for _, e := range entries {
        if !e.Start.After(at(n)) && endOf(e).After(at(n)) {
            return fmt.Sprintf("%s/%s/%s@%d", e.Type, e.AssetID, e.URI, e.OffsetS+n-int(e.Start.Sub(t0)/time.Second))
        }
    }
    return "-"
}

func randomSchedule(rnd *rand.Rand) []Entry {
    var out []Entry
    cur := rnd.Intn(5)
    for i := 0; i < 2+rnd.Intn(8); i++ {
        d := 1 + rnd.Intn(40)
        e := content(fmt.Sprintf("a%d", i), cur, d, rnd.Intn(3)*10)
        switch rnd.Intn(5) {
        case 0: e = Entry{Type: "ad_break", Start: at(cur), DurationS: d}
        case 1: e = Entry{Type: "slate", Start: at(cur), DurationS: d}
        }
        out = append(out, e)
        cur += d
        if rnd.Intn(4) == 0 { cur += 1 + rnd.Intn(10) } // a gap
    }
    return out
}

func sortedOK(t *testing.T, entries []Entry, ctx string) {
    t.Helper()
    if !sort.SliceIsSorted(entries, func(i, j int) bool { return entries[i].Start.Before(entries[j].Start) }) { t.Fatalf("%s: not sorted: %+v", ctx, entries) }
    for i := 1; i < len(entries); i++ {
        if entries[i].Start.Before(endOf(entries[i-1])) { t.Fatalf("%s: entries overlap: %+v and %+v", ctx, entries[i-1], entries[i]) }
    }
    for _, e := range entries { if e.DurationS <= 0 { t.Fatalf("%s: empty entry %+v", ctx, e) } }
}

// merge applies an edit to the schedule the way the database does: entries that start in [From,To) are replaced.
func merge(old []Entry, ed Edit) []Entry {
    var out []Entry
    for _, e := range old { if e.Start.Before(ed.From) || !e.Start.Before(ed.To) { out = append(out, e) } }
    out = append(out, ed.Entries...)
    sort.SliceStable(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
    return out
}

func TestProperty_InsertShift(t *testing.T) {
    for seed := int64(1); seed <= 300; seed++ {
        rnd := rand.New(rand.NewSource(seed))
        old := randomSchedule(rnd)
        now := rnd.Intn(150)
        d := 1 + rnd.Intn(30)
        ins := Entry{Type: "ad_break", DurationS: d}
        ed, err := InsertShift(old, at(now), ins)
        if err != nil {
            if !errors.As(err, new(ErrNothing)) { t.Fatalf("seed %d: %v", seed, err) }
            continue
        }
        got := merge(old, ed)
        ctx := fmt.Sprintf("seed %d (at %d, %d s)", seed, now, d)
        sortedOK(t, got, ctx)
        for n := 0; n < 260; n++ {
            switch {
            case n < now:
                if pos(got, n) != pos(old, n) { t.Fatalf("%s: second %d was on the air before the command and changed: %s -> %s", ctx, n, pos(old, n), pos(got, n)) }
            case n < now+d:
                if !strings.HasPrefix(pos(got, n), "ad_break") { t.Fatalf("%s: second %d should be the break, is %s", ctx, n, pos(got, n)) }
            default:
                // what would have aired at n-d airs at n, from the same place of the same source
                if pos(got, n) != pos(old, n-d) { t.Fatalf("%s: second %d shows %s, the schedule before the command had %s at second %d", ctx, n, pos(got, n), pos(old, n-d), n-d) }
            }
        }
    }
}

func TestProperty_Skip(t *testing.T) {
    for seed := int64(1); seed <= 300; seed++ {
        rnd := rand.New(rand.NewSource(seed))
        old := randomSchedule(rnd)
        now := rnd.Intn(150)
        ed, err := Skip(old, at(now))
        if err != nil {
            if !errors.As(err, new(ErrNothing)) { t.Fatalf("seed %d: %v", seed, err) }
            continue
        }
        got := merge(old, ed)
        ctx := fmt.Sprintf("seed %d (at %d)", seed, now)
        sortedOK(t, got, ctx)
        // find the entry that was skipped and how far the chain moved
        var cur Entry
        for _, e := range old { if !e.Start.After(at(now)) && endOf(e).After(at(now)) { cur = e } }
        pulled := int(endOf(cur).Sub(at(now)) / time.Second)
        // the chain: entries back to back with the skipped one
        chainEnd := endOf(cur)
        for _, e := range old { if e.Start.Equal(chainEnd) { chainEnd = endOf(e) } }
        for n := 0; n < 260; n++ {
            switch {
            case n < now:
                if pos(got, n) != pos(old, n) { t.Fatalf("%s: second %d changed before the command: %s -> %s", ctx, n, pos(old, n), pos(got, n)) }
            case at(n).Before(chainEnd.Add(-time.Duration(pulled) * time.Second)):
                if pos(got, n) != pos(old, n+pulled) { t.Fatalf("%s: second %d shows %s, expected what was %d s later: %s", ctx, n, pos(got, n), pulled, pos(old, n+pulled)) }
            case !at(n).Before(chainEnd):
                if pos(got, n) != pos(old, n) { t.Fatalf("%s: second %d is after the chain and must not change: %s -> %s", ctx, n, pos(old, n), pos(got, n)) }
            default:
                // the time that the pulled entries left behind is empty: nothing of the old chain may remain there
                if pos(got, n) != "-" { t.Fatalf("%s: second %d is where the chain used to be and must be empty, but shows %s", ctx, n, pos(got, n)) }
            }
        }
    }
}
