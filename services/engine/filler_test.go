package main

import (
    "encoding/json"
    "fmt"
    "math/rand"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/logger"
    "ftvx/pkg/models"
)

func partner(id string, d int) creative { return creative{ID: id, Type: "partner", Category: []string{"any"}, Duration: d, URI: "http://ads/" + id, Active: true} }
func house(id string, d int) creative   { return creative{ID: id, Type: "house", Category: []string{"any"}, Duration: d, URI: "http://ads/" + id, Active: true} }

func ids(items []models.AdItem) string {
    var out []string
    for _, i := range items { out = append(out, fmt.Sprintf("%s(%d)", i.CreativeID, i.DurationS)) }
    return strings.Join(out, " ")
}

func total(items []models.AdItem) int { n := 0; for _, i := range items { n += i.DurationS }; return n }

func TestFillBreak_ExactCombinationOfDifferentAds(t *testing.T) {
    reg := []creative{partner("a30", 30), partner("b15", 15), partner("c10", 10), partner("d5", 5), house("h1", 1)}
    // 40 = 30 + 10, the fewest ads
    if got := ids(fillBreak(reg, 40, "midroll", "", 1)); got != "a30(30) c10(10)" { t.Fatalf("40 s: %s", got) }
    // 25 = 15 + 10
    if got := ids(fillBreak(reg, 25, "midroll", "", 1)); got != "b15(15) c10(10)" { t.Fatalf("25 s: %s", got) }
    // each ad once: 60 s cannot be 30+30, it is all the partner ads (60) exactly
    if got := total(fillBreak(reg, 60, "midroll", "", 1)); got != 60 { t.Fatalf("60 s: %d", got) }
}

func TestFillBreak_NeverTheSameAdTwice(t *testing.T) {
    items := fillBreak([]creative{partner("only10", 10), house("h2", 2)}, 30, "any", "", 1)
    count := map[string]int{}
    for _, i := range items { if i.Type == "partner" { count[i.CreativeID]++ } }
    if count["only10"] != 1 { t.Fatalf("a partner ad was used %d times: %s", count["only10"], ids(items)) }
    if total(items) < 30 { t.Fatalf("the break is not filled: %s", ids(items)) }
}

func TestFillBreak_HouseAdsFillWhatPartnersCannot(t *testing.T) {
    reg := []creative{partner("p10", 10), house("h4", 4), house("h1", 1)}
    got := fillBreak(reg, 17, "any", "", 1)
    // 10 from the partner, then house: 4 (longest first), then the shortest repeated to the end
    if total(got) < 17 || got[0].CreativeID != "p10" { t.Fatalf("%s", ids(got)) }
    if over := total(got) - 17; over < 0 || over >= 1 { t.Fatalf("the pad of 1 s must not overshoot: %s", ids(got)) }
    // no ads at all: nothing, the break becomes slate
    if got := fillBreak(nil, 17, "any", "", 1); len(got) != 0 { t.Fatalf("%s", ids(got)) }
    // only partner ads that do not fit and no house ad: the best the partners can do
    if got := total(fillBreak([]creative{partner("p10", 10)}, 7, "any", "", 1)); got != 0 { t.Fatalf("a 10 s ad cannot go into 7 s: %d", got) }
}

func TestFillBreak_FiltersTheRegistry(t *testing.T) {
    inactive := partner("off", 10)
    inactive.Active = false
    odd := partner("odd", 3) // not a whole number of 2 s segments
    kids := partner("kids", 10)
    kids.Category = []string{"kids"}
    reg := []creative{inactive, odd, kids, partner("ok", 10)}
    if got := ids(fillBreak(reg, 10, "midroll", "", 2)); got != "ok(10)" { t.Fatalf("midroll: %s", got) }
    if got := ids(fillBreak(reg, 10, "kids", "", 2)); got != "ok(10)" && got != "kids(10)" { t.Fatalf("kids: %s", got) }
    // the ad that ended the previous break does not start the next one
    if got := ids(fillBreak([]creative{partner("x", 10), partner("y", 10)}, 10, "any", "x", 1)); got != "y(10)" { t.Fatalf("back to back: %s", got) }
    // ... but with a single ad the rule gives way: an empty break is worse than the same ad again
    if got := ids(fillBreak([]creative{partner("x", 10)}, 10, "any", "x", 1)); got != "x(10)" { t.Fatalf("a single ad: %s", got) }
}

// Whatever the registry and the break, the result is deterministic, uses each partner ad at most once, and
// fills the break unless there is nothing to fill it with.
func TestFillBreak_Properties(t *testing.T) {
    for seed := int64(1); seed <= 400; seed++ {
        rnd := rand.New(rand.NewSource(seed))
        var reg []creative
        for i := 0; i < rnd.Intn(8); i++ {
            c := partner(fmt.Sprintf("p%d", i), 1+rnd.Intn(30))
            if rnd.Intn(4) == 0 { c = house(fmt.Sprintf("h%d", i), 1+rnd.Intn(8)) }
            c.Active = rnd.Intn(8) != 0
            reg = append(reg, c)
        }
        d := 1 + rnd.Intn(120)
        a, b := fillBreak(reg, d, "any", "", 1), fillBreak(reg, d, "any", "", 1)
        if ids(a) != ids(b) { t.Fatalf("seed %d: not deterministic", seed) }
        // the order of the registry file does not matter
        shuffled := append([]creative(nil), reg...)
        rnd.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
        if got := ids(fillBreak(shuffled, d, "any", "", 1)); got != ids(a) { t.Fatalf("seed %d: the order of the registry changed the pod: %s vs %s", seed, got, ids(a)) }
        used := map[string]bool{}
        for _, it := range a {
            if it.Type == "partner" {
                if used[it.CreativeID] { t.Fatalf("seed %d: partner ad %s twice: %s", seed, it.CreativeID, ids(a)) }
                used[it.CreativeID] = true
            }
        }
        haveHouse := false
        for _, c := range reg { if c.Active && c.Type == "house" { haveHouse = true } }
        if haveHouse && total(a) < d { t.Fatalf("seed %d: house ads exist but %d of %d s are filled: %s", seed, total(a), d, ids(a)) }
        if total(a) > d && !haveHouse { t.Fatalf("seed %d: partners only, yet overshoot %d > %d: %s", seed, total(a), d, ids(a)) }
        if haveHouse {
            shortest := 1 << 30
            for _, c := range reg { if c.Active && c.Type == "house" && c.Duration < shortest { shortest = c.Duration } }
            if total(a)-d >= shortest { t.Fatalf("seed %d: overshoot %d is not less than the shortest house ad %d", seed, total(a)-d, shortest) }
        }
    }
}

func TestStaticFiller_ReadsTheFileAndFollowsChanges(t *testing.T) {
    dir := t.TempDir()
    path := filepath.Join(dir, "registry.json")
    write := func(cs ...creative) {
        b, _ := json.Marshal(registryFile{Creatives: cs})
        if err := os.WriteFile(path, b, 0o644); err != nil { t.Fatal(err) }
        future := time.Now().Add(time.Duration(len(b)) * time.Second) // a different modification time for every write
        _ = os.Chtimes(path, future, future)
    }
    write(partner("one", 10))
    f := newStaticFiller(config.Config{AdsRegistryFile: path, SegmentSec: 1, ChannelID: "c1"}, logger.Setup())
    e1 := models.TimelineEntry{Type: "ad_break", Start: time.Unix(1000, 0), DurationS: 10, Category: "midroll"}
    items, st := f.pod(e1, false)
    if st != podReady || ids(items) != "one(10)" { t.Fatalf("%s %v", ids(items), st) }
    // the same break keeps its pod
    write(partner("two", 10), partner("three", 10))
    if again, _ := f.pod(e1, false); ids(again) != "one(10)" { t.Fatalf("a break changed its pod: %s", ids(again)) }
    // the next break uses the new file, and does not repeat the ad that ended the last one
    e2 := models.TimelineEntry{Type: "ad_break", Start: time.Unix(2000, 0), DurationS: 10, Category: "midroll"}
    items, _ = f.pod(e2, false)
    if ids(items) != "three(10)" && ids(items) != "two(10)" { t.Fatalf("after the file changed: %s", ids(items)) }
    // a missing file: the break is slate, not a failure
    _ = os.Remove(path)
    e3 := models.TimelineEntry{Type: "ad_break", Start: time.Unix(3000, 0), DurationS: 10}
    if items, st := f.pod(e3, false); st != podReady || len(items) != 0 { t.Fatalf("%s %v", ids(items), st) }
    // a broken file keeps the previous registry
    write(partner("four", 10))
    f.pod(models.TimelineEntry{Type: "ad_break", Start: time.Unix(4000, 0), DurationS: 10}, false)
    if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil { t.Fatal(err) }
    future := time.Now().Add(time.Hour)
    _ = os.Chtimes(path, future, future)
    items, _ = f.pod(models.TimelineEntry{Type: "ad_break", Start: time.Unix(5000, 0), DurationS: 10}, false)
    if len(items) == 0 { t.Fatal("a broken registry file must not empty the registry that worked") }
}

func TestNewPods_ChoosesTheSource(t *testing.T) {
    lg := logger.Setup()
    if _, ok := newPods(config.Config{AdsURL: "http://ads/pod"}, lg).(*adBroker); !ok { t.Error("ADS_URL means an ad server") }
    if _, ok := newPods(config.Config{AdsRegistryFile: "/x.json"}, lg).(*staticFiller); !ok { t.Error("a registry file means the built-in filler") }
    if _, ok := newPods(config.Config{}, lg).(noAds); !ok { t.Error("nothing means slate") }
    // the ad server wins when both are set
    if _, ok := newPods(config.Config{AdsURL: "http://ads/pod", AdsRegistryFile: "/x.json"}, lg).(*adBroker); !ok { t.Error("ADS_URL wins") }
}
