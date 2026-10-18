package fmp4

import (
    "bytes"
    "errors"
    "math"
    "testing"

    "ftvx/pkg/fmp4/fmp4test"
)

var (
    video = fmp4test.TrackSpec{ID: 1, Kind: "vide", Timescale: 12800}
    audio = fmp4test.TrackSpec{ID: 2, Kind: "soun", Timescale: 48000}
)

// audioStart is where audio of the k-th 1 second segment starts in a real file: the muxer cuts at a whole
// AAC frame (1024 samples), so a second holds 46.875 frames and the start is off the grid by up to 21 ms.
func audioStart(k int) uint64 { return uint64(math.Round(float64(k)*46.875)) * 1024 }

// seg is the k-th 1 second segment of a source: 25 video samples of 512 ticks, and 46 or 47 audio frames.
func seg(payload string, k int, mode fmp4test.DurationMode) []byte {
    frames := int(audioStart(k+1)-audioStart(k)) / 1024
    return fmp4test.Segment([]byte(payload),
        fmp4test.FragSpec{TrackID: 1, Tfdt: uint64(k) * 12800, Samples: 25, SampleDur: 512, Mode: mode},
        fmp4test.FragSpec{TrackID: 2, Tfdt: audioStart(k), Samples: frames, SampleDur: 1024, Mode: mode})
}

func TestParseInit(t *testing.T) {
    in, err := ParseInit(fmp4test.Init(video, audio))
    if err != nil { t.Fatal(err) }
    if len(in.Tracks) != 2 || in.Tracks[1].Kind != "vide" || in.Tracks[1].Timescale != 12800 || in.Tracks[2].Kind != "soun" || in.Tracks[2].Timescale != 48000 {
        t.Fatalf("tracks: %+v", in.Tracks)
    }
}

func TestFragments_DurationFromTrunTfhdAndTrex(t *testing.T) {
    v := video; v.DefaultDur = 512
    a := audio; a.DefaultDur = 1024
    in, _ := ParseInit(fmp4test.Init(v, a))
    for name, mode := range map[string]fmp4test.DurationMode{"trun": fmp4test.InTrun, "tfhd": fmp4test.InTfhd, "trex": fmp4test.InTrex} {
        fr, err := Fragments(seg("x", 10, mode), in)
        if err != nil { t.Fatalf("%s: %v", name, err) }
        if len(fr) != 2 || fr[0].Duration != 25*512 || fr[1].Duration != (audioStart(11)-audioStart(10)) || fr[0].Tfdt != 128000 || fr[1].Tfdt != audioStart(10) {
            t.Fatalf("%s: %+v", name, fr)
        }
    }
}

func TestRetime_PlacesTheSegmentOnTheClock(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video, audio))
    // the 7th second of a file is put at second 1000 of the stream
    out, err := Retime(seg("x", 7, fmp4test.InTrun), in, 1000, 7, 0.25)
    if err != nil { t.Fatal(err) }
    fr, _ := Fragments(out, in)
    if fr[0].Tfdt != 1000*12800 { t.Fatalf("video must start exactly at the anchor, got %d", fr[0].Tfdt) }
    // audio keeps its own offset from the whole second: it is 0.4 ms later here, as in the source
    wantAudio := 1000*48000 + int64(audioStart(7)) - 7*48000
    if int64(fr[1].Tfdt) != wantAudio { t.Fatalf("audio %d, want %d (the offset of the source is kept)", fr[1].Tfdt, wantAudio) }
}

func TestRetime_StreamStaysContinuousInsideASourceForHours(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video, audio))
    var prev []Fragment
    for k := 0; k < 10800; k++ { // three hours of 1 second segments of one file, on the clock from second 5000
        out, err := Retime(seg("x", k, fmp4test.InTrun), in, int64(5000+k), int64(k), 0.25)
        if err != nil { t.Fatal(err) }
        fr, err := Fragments(out, in)
        if err != nil { t.Fatal(err) }
        for i := range prev {
            if fr[i].Tfdt != prev[i].Tfdt+prev[i].Duration {
                t.Fatalf("second %d track %s: starts at %d, the previous segment ended at %d", k, fr[i].Track.Kind, fr[i].Tfdt, prev[i].Tfdt+prev[i].Duration)
            }
        }
        prev = fr
    }
}

// Two files cut at different places: the audio offset of each is its own, so audio and video never drift apart.
func TestRetime_AudioAndVideoStayInSyncAcrossFiles(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video, audio))
    for _, k := range []int{0, 1, 5, 99, 1234} {
        out, err := Retime(seg("x", k, fmp4test.InTrun), in, 4_000_000, int64(k), 0.25)
        if err != nil { t.Fatal(err) }
        fr, _ := Fragments(out, in)
        src, _ := Fragments(seg("x", k, fmp4test.InTrun), in)
        got := int64(fr[1].Tfdt)*12800 - int64(fr[0].Tfdt)*48000
        want := int64(src[1].Tfdt)*12800 - int64(src[0].Tfdt)*48000
        if got != want { t.Fatalf("k=%d: A/V offset %d, the source has %d", k, got, want) }
    }
}

func TestRetime_LeavesTheMediaDataAlone(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video, audio))
    src := seg("picture bytes", 3, fmp4test.InTrun)
    keep := append([]byte(nil), src...)
    out, err := Retime(src, in, 77, 3, 0.25)
    if err != nil { t.Fatal(err) }
    a, _ := MediaData(src)
    b, _ := MediaData(out)
    if !bytes.Equal(a, b) || string(a) != "picture bytes" { t.Fatalf("media data changed: %q vs %q", a, b) }
    if bytes.Equal(src, out) { t.Fatal("decode times must have changed") }
    if len(src) != len(out) { t.Fatal("size must not change") }
    if !bytes.Equal(src, keep) { t.Fatal("the source buffer was modified") }
}

func TestRetime_RefusesASegmentThatIsNotOnTheGrid(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video, audio))
    // the file says this is its 5th second, but the decode time is 12 s: not a normalized source
    _, err := Retime(seg("x", 12, fmp4test.InTrun), in, 100, 5, 0.25)
    if !errors.Is(err, ErrOffGrid) { t.Fatalf("want ErrOffGrid, got %v", err) }
    // a file whose video lasts 1.001 s per segment drifts off the grid after a while
    drift := fmp4test.Segment([]byte("x"), fmp4test.FragSpec{TrackID: 1, Tfdt: uint64(float64(3000) * 12812.8), Samples: 25, SampleDur: 512})
    if _, err := Retime(drift, in, 100, 3000, 0.25); !errors.Is(err, ErrOffGrid) { t.Fatalf("29.97 fps style drift must be refused, got %v", err) }
}

func TestRetime_RejectsTfdtVersion0(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video))
    s := fmp4test.Segment([]byte("x"), fmp4test.FragSpec{TrackID: 1, Tfdt: 0, Samples: 1, SampleDur: 256, TfdtV0: true})
    if _, err := Retime(s, in, 0, 0, 0.25); err != ErrTfdtVersion { t.Fatalf("want ErrTfdtVersion, got %v", err) }
}

func TestRetime_RefusesToStartBeforeTheEpoch(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video))
    s := fmp4test.Segment([]byte("x"), fmp4test.FragSpec{TrackID: 1, Tfdt: 0, Samples: 1, SampleDur: 256})
    if _, err := Retime(s, in, -5, 0, 0.25); err == nil { t.Fatal("a negative decode time cannot be written") }
}

func TestMalformedInputNeverPanics(t *testing.T) {
    initB := fmp4test.Init(video, audio)
    segB := seg("payload", 4, fmp4test.InTrun)
    in, _ := ParseInit(initB)
    // every truncation and a few corruptions of real files must give an error or a result, never a panic
    for n := 0; n < len(initB); n++ { _, _ = ParseInit(initB[:n]) }
    for n := 0; n < len(segB); n++ {
        _, _ = Fragments(segB[:n], in)
        _, _ = Retime(segB[:n], in, 10, 4, 0.25)
    }
    for i := 0; i < len(segB); i++ {
        c := append([]byte(nil), segB...)
        c[i] ^= 0xff
        _, _ = Fragments(c, in)
        _, _ = Retime(c, in, 10, 4, 0.25)
    }
    for i := 0; i < len(initB); i++ {
        c := append([]byte(nil), initB...)
        c[i] ^= 0xff
        _, _ = ParseInit(c)
    }
    if _, err := ParseInit([]byte("not an mp4 at all")); err == nil { t.Fatal("garbage must be rejected") }
    if _, err := Fragments([]byte{}, in); err == nil { t.Fatal("empty segment must be rejected") }
}

func TestSegmentOfAnUnknownTrackIsRejected(t *testing.T) {
    in, _ := ParseInit(fmp4test.Init(video))
    s := fmp4test.Segment([]byte("x"), fmp4test.FragSpec{TrackID: 9, Tfdt: 0, Samples: 1, SampleDur: 256})
    if _, err := Fragments(s, in); err == nil { t.Fatal("track 9 is not in the init section") }
}
