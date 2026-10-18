// Package fmp4test builds small but structurally real fragmented MP4 files for tests.
package fmp4test

import "encoding/binary"

type TrackSpec struct {
    ID         uint32
    Kind       string // "vide" or "soun"
    Timescale  uint32
    DefaultDur uint32 // trex default_sample_duration, 0 for none
}

// DurationMode says where the sample durations of a fragment are written.
type DurationMode int

const (
    InTrun DurationMode = iota // every sample has its own duration
    InTfhd                     // default_sample_duration in tfhd
    InTrex                     // nothing in the segment, the init section has the default
)

type FragSpec struct {
    TrackID   uint32
    Tfdt      uint64
    Samples   int
    SampleDur uint32
    Mode      DurationMode
    TfdtV0    bool // write tfdt version 0 (32-bit)
}

func box(typ string, body ...[]byte) []byte {
    n := 8
    for _, b := range body { n += len(b) }
    out := make([]byte, 8, n)
    binary.BigEndian.PutUint32(out, uint32(n))
    copy(out[4:], typ)
    for _, b := range body { out = append(out, b...) }
    return out
}

func u32(v uint32) []byte { b := make([]byte, 4); binary.BigEndian.PutUint32(b, v); return b }
func u64(v uint64) []byte { b := make([]byte, 8); binary.BigEndian.PutUint64(b, v); return b }

// Init builds an initialization section (ftyp + moov) for the tracks.
func Init(tracks ...TrackSpec) []byte {
    moov := [][]byte{}
    mvex := [][]byte{}
    for _, t := range tracks {
        tkhd := box("tkhd", append(append(u32(0), make([]byte, 8)...), append(u32(t.ID), make([]byte, 68)...)...))
        mdhd := box("mdhd", append(append(u32(0), make([]byte, 8)...), append(u32(t.Timescale), make([]byte, 8)...)...))
        hdlr := box("hdlr", append(append(u32(0), u32(0)...), append([]byte(t.Kind), make([]byte, 13)...)...))
        moov = append(moov, box("trak", tkhd, box("mdia", mdhd, hdlr)))
        mvex = append(mvex, box("trex", u32(0), u32(t.ID), u32(1), u32(t.DefaultDur), u32(0), u32(0)))
    }
    moov = append(moov, box("mvex", mvex...))
    return append(box("ftyp", []byte("isom"), u32(0), []byte("isomiso6")), box("moov", moov...)...)
}

// Segment builds a media segment: one moof per fragment followed by one mdat that holds payload.
func Segment(payload []byte, frags ...FragSpec) []byte {
    var out []byte
    for _, f := range frags {
        var tfhdFlags uint32 = 0x020000 // default-base-is-moof
        tfhdBody := []byte{}
        if f.Mode == InTfhd {
            tfhdFlags |= 0x8
            tfhdBody = u32(f.SampleDur)
        }
        tfhd := box("tfhd", append(append(u32(tfhdFlags), u32(f.TrackID)...), tfhdBody...))
        var tfdt []byte
        if f.TfdtV0 {
            tfdt = box("tfdt", append(u32(0), u32(uint32(f.Tfdt))...))
        } else {
            tfdt = box("tfdt", append(u32(1<<24), u64(f.Tfdt)...))
        }
        var trunFlags uint32 = 0x1 // data offset
        samples := []byte{}
        if f.Mode == InTrun {
            trunFlags |= 0x100 | 0x200
            for i := 0; i < f.Samples; i++ { samples = append(samples, append(u32(f.SampleDur), u32(100)...)...) }
        }
        trun := box("trun", append(append(append(u32(trunFlags), u32(uint32(f.Samples))...), u32(0)...), samples...))
        out = append(out, box("moof", box("mfhd", u32(0), u32(1)), box("traf", tfhd, tfdt, trun))...)
    }
    return append(out, box("mdat", payload)...)
}
