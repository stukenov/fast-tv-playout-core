package scte35

import (
    "bytes"
    "encoding/hex"
    "strings"
    "testing"
)

// A splice_insert from the SCTE-35 specification: out of the network at splice time 0x7369C02E, auto return after 5426421 ticks (60.29 s).
const specVector = "/DAvAAAAAAAA///wFAVIAACPf+/+c2nALv4AUsz1AAAAAAAKAAhDVUVJAAABNWLbowo="

func TestDecode_ASpecificationExample(t *testing.T) {
    raw, err := Parse(specVector)
    if err != nil { t.Fatal(err)  }
    s, err := Decode(raw)
    if err != nil { t.Fatalf("a valid message from the specification was refused: %v", err) }
    in := s.Insert
    if s.CommandType != CmdSpliceInsert || in == nil { t.Fatalf("%+v", s) }
    if in.EventID != 0x4800008F || !in.Out || in.Cancel || !in.AutoReturn || in.Immediate || in.DurationTicks != 5426421 {
        t.Fatalf("%+v", *in)
    }
    if d := in.DurationSeconds(); d < 60.29 || d > 60.30 { t.Fatalf("duration %v s", d) }
    if in.PTS == nil || *in.PTS != 0x7369C02E { t.Fatalf("splice time %v", in.PTS) }
    // encoding what was decoded gives the same bytes back: the encoder writes the format as the specification has it
    again, err := Encode(*in)
    if err != nil { t.Fatal(err) }
    again2, _ := Decode(again)
    if *again2.Insert.PTS != *in.PTS || again2.Insert.DurationTicks != in.DurationTicks || again2.Insert.EventID != in.EventID { t.Fatalf("%+v", again2.Insert) }
}

func TestRoundTrip(t *testing.T) {
    pts := uint64(8_100_000_000) // 90000 s
    cases := []SpliceInsert{
        {EventID: 1, Out: true, Immediate: true, DurationTicks: 30 * TickRate, AutoReturn: true, AvailsExpected: 1},
        {EventID: 0xFFFFFFFF, Out: false, Immediate: true},
        {EventID: 7, Out: true, PTS: &pts, DurationTicks: 1, UniqueProgramID: 0xBEEF, AvailNum: 3, AvailsExpected: 4},
        {EventID: 9, Cancel: true},
        {EventID: 12, Out: true, Immediate: true}, // no duration
    }
    for i, c := range cases {
        raw, err := Encode(c)
        if err != nil { t.Fatalf("case %d: %v", i, err) }
        back, err := Decode(raw)
        if err != nil { t.Fatalf("case %d: %v", i, err) }
        got := *back.Insert
        if c.PTS != nil { if got.PTS == nil || *got.PTS != *c.PTS { t.Fatalf("case %d: pts %v", i, got.PTS) }; got.PTS, c.PTS = nil, nil }
        if got != c { t.Fatalf("case %d: %+v != %+v", i, got, c) }
    }
}

func TestDecode_RefusesDamagedMessages(t *testing.T) {
    raw, _ := Out(5, 30)
    // every byte changed, one at a time: the CRC (or the structure) must catch it
    for i := range raw {
        c := append([]byte(nil), raw...)
        c[i] ^= 0x01
        if _, err := Decode(c); err == nil { t.Fatalf("a change in byte %d was not noticed", i) }
    }
    // every truncation: an error, never a panic
    for n := 0; n < len(raw); n++ {
        if _, err := Decode(raw[:n]); err == nil { t.Fatalf("a message cut to %d bytes was accepted", n) }
    }
    if _, err := Decode(append(raw, 0)); err == nil { t.Fatal("trailing garbage was accepted") }
}

func TestEncode_RefusesWhatDoesNotFit(t *testing.T) {
    if _, err := Encode(SpliceInsert{Out: true, Immediate: true, DurationTicks: 1 << 33}); err == nil { t.Error("a duration of 2^33 ticks") }
    big := uint64(1 << 33)
    if _, err := Encode(SpliceInsert{Out: true, PTS: &big}); err == nil { t.Error("a splice time of 2^33 ticks") }
    if _, err := Encode(SpliceInsert{Out: true}); err == nil { t.Error("not immediate and no time") }
}

func TestParse(t *testing.T) {
    raw, _ := Out(1, 15)
    for name, s := range map[string]string{"hex": strings.ToUpper(hex.EncodeToString(raw)), "0x hex": Hex(raw), "lower hex": hex.EncodeToString(raw)} {
        got, err := Parse(s)
        if err != nil || !bytes.Equal(got, raw) { t.Errorf("%s: %v", name, err) }
    }
    if got, err := Parse("/DAvAAAAAAAA///wFAVIAACPf+/+c2nALv4AUsz1AAAAAAAKAAhDVUVJAAABNWLbowo"); err != nil || len(got) == 0 { t.Errorf("base64 without padding: %v", err) }
    if _, err := Parse("this is not it!"); err == nil { t.Error("garbage") }
    if _, err := Parse(""); err == nil { t.Error("empty") }
}

func TestOutAndIn(t *testing.T) {
    out, _ := Out(0x1234, 90)
    in, _ := In(0x1234)
    o, i := mustInsert(t, out), mustInsert(t, in)
    if !o.Out || o.EventID != 0x1234 || o.DurationSeconds() != 90 || !o.Immediate || !o.AutoReturn { t.Fatalf("%+v", o) }
    if i.Out || i.EventID != 0x1234 || i.DurationTicks != 0 || !i.Immediate { t.Fatalf("%+v", i) }
    if !strings.HasPrefix(Hex(out), "0xFC30") { t.Fatalf("%s", Hex(out)) }
}

func mustInsert(t *testing.T, raw []byte) SpliceInsert {
    t.Helper()
    s, err := Decode(raw)
    if err != nil || s.Insert == nil { t.Fatalf("%v %+v", err, s) }
    return *s.Insert
}

func TestNullSection(t *testing.T) {
    n := Null()
    s, err := Decode(n)
    if err != nil || s.CommandType != CmdNull || s.Insert != nil { t.Fatalf("%v %+v", err, s) }
}
