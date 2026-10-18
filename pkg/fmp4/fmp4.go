// Package fmp4 reads the parts of fragmented MP4 (CMAF) that the packager needs and moves a segment onto
// the stream timeline, so that segments taken from different files play one after another.
package fmp4

import (
    "encoding/binary"
    "errors"
    "fmt"
    "math"
)

// Track describes one track of an initialization section.
type Track struct {
    ID                    uint32
    Kind                  string // handler type: "vide", "soun", ...
    Timescale             uint32
    DefaultSampleDuration uint32 // from mvex/trex, 0 when absent
}

// Init is the parsed initialization section (moov).
type Init struct {
    Tracks map[uint32]Track
}

// Fragment is one track fragment (traf) of a media segment.
type Fragment struct {
    Track    Track
    Tfdt     uint64 // baseMediaDecodeTime in track timescale ticks
    Duration uint64 // sum of the sample durations in ticks
}

var (
    ErrTfdtVersion = errors.New("tfdt version 0 is not supported, the source must use 64-bit decode times (tfdt version 1)")
    ErrNoDuration  = errors.New("cannot determine the duration of a fragment (no sample durations, no defaults)")
    // ErrOffGrid: the decode time of a segment is not where its position in the file says it should be.
    ErrOffGrid = errors.New("the segment is not on the grid of its file")
)

type box struct {
    typ   string
    start int // offset of the box header
    hdr   int // header size, 8 or 16
    size  int // whole box size
}

func (b box) bodyStart() int { return b.start + b.hdr }
func (b box) end() int       { return b.start + b.size }

// children lists the boxes inside b[start:end] and checks that their sizes fit.
func children(b []byte, start, end int) ([]box, error) {
    var out []box
    p := start
    for p < end {
        if end-p < 8 { return nil, fmt.Errorf("truncated box header at %d", p) }
        size := int(binary.BigEndian.Uint32(b[p : p+4]))
        typ := string(b[p+4 : p+8])
        hdr := 8
        if size == 1 {
            if end-p < 16 { return nil, fmt.Errorf("truncated large box header at %d", p) }
            big := binary.BigEndian.Uint64(b[p+8 : p+16])
            if big > math.MaxInt32 { return nil, fmt.Errorf("box %q is too large", typ) }
            size, hdr = int(big), 16
        } else if size == 0 {
            size = end - p
        }
        if size < hdr || p+size > end { return nil, fmt.Errorf("box %q at %d has a bad size %d", typ, p, size) }
        out = append(out, box{typ: typ, start: p, hdr: hdr, size: size})
        p += size
    }
    return out, nil
}

func find(bs []box, typ string) (box, bool) {
    for _, x := range bs { if x.typ == typ { return x, true } }
    return box{}, false
}

func u8(b []byte, p int) (byte, error) {
    if p < 0 || p >= len(b) { return 0, errors.New("short box") }
    return b[p], nil
}

func u32(b []byte, p int) (uint32, error) {
    if p < 0 || p+4 > len(b) { return 0, errors.New("short box") }
    return binary.BigEndian.Uint32(b[p : p+4]), nil
}

// ParseInit reads tracks (id, handler, timescale) and the default sample durations.
func ParseInit(b []byte) (Init, error) {
    in := Init{Tracks: map[uint32]Track{}}
    top, err := children(b, 0, len(b))
    if err != nil { return in, err }
    moov, ok := find(top, "moov")
    if !ok { return in, errors.New("no moov box") }
    kids, err := children(b, moov.bodyStart(), moov.end())
    if err != nil { return in, err }
    for _, k := range kids {
        if k.typ != "trak" { continue }
        t, err := parseTrak(b, k)
        if err != nil { return in, err }
        in.Tracks[t.ID] = t
    }
    if len(in.Tracks) == 0 { return in, errors.New("no tracks in moov") }
    if mvex, ok := find(kids, "mvex"); ok {
        trexes, err := children(b, mvex.bodyStart(), mvex.end())
        if err != nil { return in, err }
        for _, x := range trexes {
            if x.typ != "trex" { continue }
            id, e1 := u32(b, x.bodyStart()+4)
            dur, e2 := u32(b, x.bodyStart()+12)
            if e1 != nil || e2 != nil { return in, errors.New("short trex") }
            if t, ok := in.Tracks[id]; ok { t.DefaultSampleDuration = dur; in.Tracks[id] = t }
        }
    }
    return in, nil
}

func parseTrak(b []byte, trak box) (Track, error) {
    var t Track
    kids, err := children(b, trak.bodyStart(), trak.end())
    if err != nil { return t, err }
    tkhd, ok := find(kids, "tkhd")
    if !ok { return t, errors.New("trak without tkhd") }
    ver, err := u8(b, tkhd.bodyStart())
    if err != nil { return t, err }
    off := 12
    if ver == 1 { off = 20 }
    if t.ID, err = u32(b, tkhd.bodyStart()+off); err != nil { return t, err }
    mdia, ok := find(kids, "mdia")
    if !ok { return t, errors.New("trak without mdia") }
    mk, err := children(b, mdia.bodyStart(), mdia.end())
    if err != nil { return t, err }
    mdhd, ok := find(mk, "mdhd")
    if !ok { return t, errors.New("mdia without mdhd") }
    if ver, err = u8(b, mdhd.bodyStart()); err != nil { return t, err }
    off = 12
    if ver == 1 { off = 20 }
    if t.Timescale, err = u32(b, mdhd.bodyStart()+off); err != nil { return t, err }
    if t.Timescale == 0 { return t, errors.New("zero timescale") }
    if hdlr, ok := find(mk, "hdlr"); ok {
        p := hdlr.bodyStart() + 8
        if p+4 <= hdlr.end() { t.Kind = string(b[p : p+4]) }
    }
    return t, nil
}

// traf is a located track fragment.
type traf struct {
    track    Track
    tfdt     box
    tfdtBase int // offset of the tfdt body
    version  byte
    duration uint64
}

// walk finds the track fragments of a segment.
func walk(seg []byte, in Init) ([]traf, error) {
    top, err := children(seg, 0, len(seg))
    if err != nil { return nil, err }
    var out []traf
    for _, m := range top {
        if m.typ != "moof" { continue }
        mk, err := children(seg, m.bodyStart(), m.end())
        if err != nil { return nil, err }
        for _, t := range mk {
            if t.typ != "traf" { continue }
            tf, err := parseTraf(seg, t, in)
            if err != nil { return nil, err }
            out = append(out, tf)
        }
    }
    if len(out) == 0 { return nil, errors.New("segment has no moof/traf") }
    return out, nil
}

func parseTraf(b []byte, t box, in Init) (traf, error) {
    var tf traf
    kids, err := children(b, t.bodyStart(), t.end())
    if err != nil { return tf, err }
    tfhd, ok := find(kids, "tfhd")
    if !ok { return tf, errors.New("traf without tfhd") }
    flags, err := u32(b, tfhd.bodyStart())
    if err != nil { return tf, err }
    flags &= 0xffffff
    id, err := u32(b, tfhd.bodyStart()+4)
    if err != nil { return tf, err }
    track, ok := in.Tracks[id]
    if !ok { return tf, fmt.Errorf("segment has track %d that the init section does not describe", id) }
    p := tfhd.bodyStart() + 8
    if flags&0x1 != 0 { p += 8 } // base data offset
    if flags&0x2 != 0 { p += 4 } // sample description index
    defDur := track.DefaultSampleDuration
    if flags&0x8 != 0 {
        if defDur, err = u32(b, p); err != nil { return tf, err }
    }
    tf.track = track

    tfdt, ok := find(kids, "tfdt")
    if !ok { return tf, errors.New("traf without tfdt") }
    tf.tfdt, tf.tfdtBase = tfdt, tfdt.bodyStart()
    if tf.tfdtBase >= len(b) { return tf, errors.New("short tfdt") }
    tf.version = b[tf.tfdtBase]

    for _, k := range kids {
        if k.typ != "trun" { continue }
        d, err := trunDuration(b, k, defDur)
        if err != nil { return tf, err }
        tf.duration += d
    }
    return tf, nil
}

func trunDuration(b []byte, k box, defDur uint32) (uint64, error) {
    flags, err := u32(b, k.bodyStart())
    if err != nil { return 0, err }
    flags &= 0xffffff
    count, err := u32(b, k.bodyStart()+4)
    if err != nil { return 0, err }
    if flags&0x100 == 0 {
        if defDur == 0 { return 0, ErrNoDuration }
        return uint64(count) * uint64(defDur), nil
    }
    p := k.bodyStart() + 8
    if flags&0x1 != 0 { p += 4 }
    if flags&0x4 != 0 { p += 4 }
    per := 4
    for _, f := range []uint32{0x200, 0x400, 0x800} { if flags&f != 0 { per += 4 } }
    if p+int(count)*per > k.end() { return 0, errors.New("trun is shorter than its sample count") }
    var sum uint64
    for i := 0; i < int(count); i++ {
        d, _ := u32(b, p+i*per)
        sum += uint64(d)
    }
    return sum, nil
}

// Fragments reports the decode time and the duration of every track fragment of a segment.
func Fragments(seg []byte, in Init) ([]Fragment, error) {
    tfs, err := walk(seg, in)
    if err != nil { return nil, err }
    out := make([]Fragment, 0, len(tfs))
    for _, tf := range tfs {
        var v uint64
        if tf.version == 1 {
            if tf.tfdtBase+12 > len(seg) { return nil, errors.New("short tfdt") }
            v = binary.BigEndian.Uint64(seg[tf.tfdtBase+4 : tf.tfdtBase+12])
        } else {
            if tf.tfdtBase+8 > len(seg) { return nil, errors.New("short tfdt") }
            v = uint64(binary.BigEndian.Uint32(seg[tf.tfdtBase+4 : tf.tfdtBase+8]))
        }
        out = append(out, Fragment{Track: tf.track, Tfdt: v, Duration: tf.duration})
    }
    return out, nil
}

// Retime returns a copy of the segment moved onto the stream timeline. The segment is the nominalS-th second
// of its file (index times the segment length), and it is placed at anchorS seconds of the stream:
//
//     new decode time = anchorS + (old decode time - nominalS)     in the ticks of every track
//
// Because everything but a whole number of seconds is kept, the small offset between audio and video that the
// source has (the muxer cuts audio at a whole audio frame) stays what it was, and nothing accumulates: the
// timeline is a function of the clock, not of the history. The arithmetic is on integers, so it is exact.
// A track that is more than tolS away from nominalS makes the source unusable (ErrOffGrid).
// The media data is not touched and the source buffer is not modified.
func Retime(seg []byte, in Init, anchorS, nominalS int64, tolS float64) ([]byte, error) {
    tfs, err := walk(seg, in)
    if err != nil { return nil, err }
    out := append([]byte(nil), seg...)
    for _, tf := range tfs {
        if tf.version != 1 { return nil, ErrTfdtVersion }
        if tf.tfdtBase+12 > len(out) { return nil, errors.New("short tfdt") }
        scale := int64(tf.track.Timescale)
        have := int64(binary.BigEndian.Uint64(out[tf.tfdtBase+4 : tf.tfdtBase+12]))
        diff := have - nominalS*scale
        if math.Abs(float64(diff)) > tolS*float64(scale) {
            return nil, fmt.Errorf("%w: track %s starts %.3fs from second %d", ErrOffGrid, tf.track.Kind, float64(diff)/float64(scale), nominalS)
        }
        v := anchorS*scale + diff
        if v < 0 { return nil, errors.New("the stream position is before the start of the timeline") }
        binary.BigEndian.PutUint64(out[tf.tfdtBase+4:tf.tfdtBase+12], uint64(v))
    }
    return out, nil
}

// MediaData returns the bytes of all mdat boxes, which Retime never changes. Used to tell what a segment contains.
func MediaData(seg []byte) ([]byte, error) {
    top, err := children(seg, 0, len(seg))
    if err != nil { return nil, err }
    var out []byte
    for _, x := range top {
        if x.typ == "mdat" { out = append(out, seg[x.bodyStart():x.end()]...) }
    }
    return out, nil
}
