package main

import (
    "context"
    "fmt"
    "regexp"
    "strings"
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/hls"
    "ftvx/pkg/scte35"
)

func withTags(mode string) func(*config.Config) { return func(c *config.Config) { c.SCTE35Tags = mode } }

// lines of the playlist that mark an ad break, each with the picture of the segment that follows it
type marker struct{ line, next string }

func markers(t *testing.T, r *rig) []marker {
    t.Helper()
    var out []marker
    var pending []string
    for _, l := range strings.Split(r.playlist(t), "\n") {
        switch {
        case strings.HasPrefix(l, "#EXT-X-DATERANGE") || strings.HasPrefix(l, "#EXT-X-CUE-"):
            pending = append(pending, l)
        case l != "" && !strings.HasPrefix(l, "#"):
            for _, p := range pending { out = append(out, marker{line: p, next: segmentPicture(t, r, l)}) }
            pending = nil
        }
    }
    return out
}

func segmentPicture(t *testing.T, r *rig, uri string) string {
    t.Helper()
    toks := tokens(t, r.e.st, "#EXT-X-MAP:URI=\""+r.e.st.win.Items[0].MapURI+"\"\n"+uri+"\n")
    return strings.TrimPrefix(toks[len(toks)-1], "S:")
}

var scteRe = regexp.MustCompile(`SCTE35-(OUT|IN)=(0x[0-9A-F]+)`)

func decodeMarker(t *testing.T, line string) (string, *scte35.SpliceInsert) {
    t.Helper()
    m := scteRe.FindStringSubmatch(line)
    if m == nil { t.Fatalf("no SCTE-35 in %q", line) }
    raw, err := scte35.Parse(m[2])
    if err != nil { t.Fatal(err) }
    s, err := scte35.Decode(raw)
    if err != nil || s.Insert == nil { t.Fatalf("the message in %q is not a valid splice_insert: %v", line, err) }
    return m[1], s.Insert
}

func TestMarkers_ABreakIsMarkedOutAndIn(t *testing.T) {
    r := newRigWith(t, withTags("both"))
    r.sched.set(r.content("a", 0, 5), r.adBreak(5, 4), r.content("a", 9, 5))
    r.ticks(0, 9)
    got := markers(t, r)
    if len(got) != 4 { t.Fatalf("expected DATERANGE and CUE for the start and for the end, got %d: %+v", len(got), got) }
    id := uint32(r.at(5).Unix())
    // the start is before the first second of the break
    if !strings.HasPrefix(got[0].next, "ad_") || !strings.HasPrefix(got[1].next, "ad_") { t.Fatalf("the start markers must precede the first ad segment: %+v", got[:2]) }
    if want := fmt.Sprintf(`#EXT-X-DATERANGE:ID="break-%d",START-DATE="%s",PLANNED-DURATION=4.000,SCTE35-OUT=`, id, r.at(5).Format(dateFormat)); !strings.HasPrefix(got[0].line, want) { t.Fatalf("%s\nwant a line that starts with\n%s", got[0].line, want) }
    if got[1].line != "#EXT-X-CUE-OUT:4" { t.Fatalf("%s", got[1].line) }
    kind, out := decodeMarker(t, got[0].line)
    if kind != "OUT" || !out.Out || out.EventID != id || out.DurationSeconds() != 4 || !out.AutoReturn { t.Fatalf("%+v", out) }
    // the end is before the first segment after the break
    if !strings.HasPrefix(got[2].next, "a_") { t.Fatalf("the end markers must precede the first segment after the break: %+v", got[2]) }
    if want := fmt.Sprintf(`#EXT-X-DATERANGE:ID="break-%d",START-DATE="%s",DURATION=4.000,SCTE35-IN=`, id, r.at(5).Format(dateFormat)); !strings.HasPrefix(got[2].line, want) { t.Fatalf("%s", got[2].line) }
    if got[3].line != "#EXT-X-CUE-IN" { t.Fatalf("%s", got[3].line) }
    kind, in := decodeMarker(t, got[2].line)
    if kind != "IN" || in.Out || in.EventID != id { t.Fatalf("%+v", in) }
    // the playlist is still a playlist for the players that know nothing of this
    if _, err := hls.Parse(strings.NewReader(r.playlist(t)), nil); err != nil { t.Fatal(err) }
}

func TestMarkers_Modes(t *testing.T) {
    sched := func(r *rig) { r.sched.set(r.content("a", 0, 4), r.adBreak(4, 3), r.content("a", 7, 5)); r.ticks(0, 7) }
    count := func(r *rig, prefix string) int {
        n := 0
        for _, m := range markers(t, r) { if strings.HasPrefix(m.line, prefix) { n++ } }
        return n
    }
    for mode, want := range map[string][3]int{ // DATERANGE, CUE-OUT, CUE-IN
        "daterange": {2, 0, 0}, "cue": {0, 1, 1}, "both": {2, 1, 1}, "off": {0, 0, 0}, "": {0, 0, 0},
    } {
        r := newRigWith(t, withTags(mode))
        sched(r)
        if got := [3]int{count(r, "#EXT-X-DATERANGE"), count(r, "#EXT-X-CUE-OUT"), count(r, "#EXT-X-CUE-IN")}; got != want { t.Errorf("mode %q: %v, want %v", mode, got, want) }
    }
}

func TestMarkers_AValidMessageFromTheScheduleIsUsedAsItIs(t *testing.T) {
    r := newRigWith(t, withTags("daterange"))
    given, _ := scte35.Encode(scte35.SpliceInsert{EventID: 4242, Out: true, Immediate: true, DurationTicks: 3 * scte35.TickRate, UniqueProgramID: 77, AvailsExpected: 1})
    e := r.adBreak(4, 3)
    e.SCTE35 = scte35.Hex(given)
    r.sched.set(r.content("a", 0, 4), e, r.content("a", 7, 5))
    r.ticks(0, 7)
    kind, out := decodeMarker(t, markers(t, r)[0].line)
    if kind != "OUT" || out.EventID != 4242 || out.UniqueProgramID != 77 { t.Fatalf("the message of the schedule was not used: %+v", out) }
    // a message that is not valid is not passed on: the engine makes its own
    r2 := newRigWith(t, withTags("daterange"))
    bad := r2.adBreak(4, 3)
    bad.SCTE35 = "0xFC30DEADBEEF"
    r2.sched.set(r2.content("a", 0, 4), bad, r2.content("a", 7, 5))
    r2.ticks(0, 7)
    _, out = decodeMarker(t, markers(t, r2)[0].line)
    if out.EventID != uint32(r2.at(4).Unix()) || out.DurationSeconds() != 3 { t.Fatalf("%+v", out) }
}

// A break that is filled with slate (no ad server) is still an avail for whoever listens to the markers.
func TestMarkers_ABreakFilledWithSlateIsStillMarked(t *testing.T) {
    r := newRigWith(t, withTags("both"))
    r.ads.set(0, true)
    r.sched.set(r.content("a", 0, 4), r.adBreak(4, 3), r.content("a", 7, 5))
    r.ticks(0, 7)
    got := markers(t, r)
    if len(got) != 4 || !strings.HasPrefix(got[0].next, "slate_") || !strings.HasPrefix(got[2].next, "a_") { t.Fatalf("%+v", got) }
}

func TestMarkers_ABreakIsNotStartedTwiceByARestartInTheMiddleOfIt(t *testing.T) {
    r := newRigWith(t, withTags("both"))
    r.sched.set(r.content("a", 0, 4), r.adBreak(4, 8), r.content("a", 12, 5))
    r.ticks(0, 6) // the break (4..11) is on the air, the playlist holds up to second 12
    before := markers(t, r)
    again := newEngine(r.e.cfg, r.e.lg, r.sched, newResolver(r.e.cfg))
    if err := again.prepare(context.Background()); err != nil { t.Fatal(err) }
    r.e = again
    // the tags of the segments that were written survive the restart
    if got := markers(t, r); fmt.Sprint(got) != fmt.Sprint(before) { t.Fatalf("the restored playlist lost its markers:\n%v\n%v", got, before) }
    r.ticks(7, 12)
    out, in := 0, 0
    for _, m := range markers(t, r) {
        if strings.HasPrefix(m.line, "#EXT-X-CUE-OUT") { out++ }
        if strings.HasPrefix(m.line, "#EXT-X-CUE-IN") { in++ }
    }
    if out != 1 { t.Fatalf("%d CUE-OUT lines: a restart in the middle of a break is not a new break", out) }
    // the break that was started before the restart ends after it: the end is marked
    if in != 1 { t.Fatalf("%d CUE-IN lines", in) }
}

func TestMarkers_TheTightPlaylistKeepsThemWhileTheSegmentsAreThere(t *testing.T) {
    r := newRigWith(t, func(c *config.Config) { tight(c); c.SCTE35Tags = "daterange" })
    r.sched.set(r.content("a", 0, 5), r.adBreak(5, 2), r.content("a", 7, 8))
    w := newWatch(t, r)
    sawOut, sawIn := false, false
    for n := 0; n <= 10; n++ {
        w.tickAndPoll(r.at(n))
        raw := r.playlist(t)
        if strings.Contains(raw, "SCTE35-OUT") { sawOut = true }
        if strings.Contains(raw, "SCTE35-IN") { sawIn = true }
        // a tag belongs to a segment that is in the playlist: never a marker without the segment
        lines := strings.Split(strings.TrimSpace(raw), "\n")
        if strings.HasPrefix(lines[len(lines)-1], "#EXT-X-DATERANGE") { t.Fatalf("a marker at the end, without its segment:\n%s", raw) }
    }
    if !sawOut || !sawIn { t.Fatalf("out %v in %v", sawOut, sawIn) }
    _ = time.Second
}
