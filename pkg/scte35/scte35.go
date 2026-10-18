// Package scte35 encodes and decodes SCTE-35 splice_info_section, the message that marks the start and the end of an
// ad break in a broadcast. Only what a playout needs is understood: splice_insert (out of the network and back),
// and the rest is recognised and checked (CRC) but not interpreted.
package scte35

import (
    "encoding/base64"
    "encoding/hex"
    "errors"
    "fmt"
    "strings"
)

const (
    TickRate = 90000 // splice times and durations are in ticks of a 90 kHz clock

    CmdNull         = 0x00
    CmdSpliceInsert = 0x05
    CmdTimeSignal   = 0x06
)

// SpliceInsert is a splice_insert command.
type SpliceInsert struct {
    EventID         uint32
    Cancel          bool
    Out             bool    // out_of_network_indicator: true leaves the network (the break starts), false returns to it
    Immediate       bool    // splice_immediate_flag: at once, no time is given
    PTS             *uint64 // splice_time in 90 kHz ticks, when not immediate
    DurationTicks   uint64  // break_duration; 0 means none is given
    AutoReturn      bool
    UniqueProgramID uint16
    AvailNum        uint8
    AvailsExpected  uint8
}

// Duration is the length of the break in seconds (0 when none is given).
func (s SpliceInsert) DurationSeconds() float64 { return float64(s.DurationTicks) / TickRate }

// Section is a decoded splice_info_section.
type Section struct {
    PTSAdjustment uint64
    CommandType   byte
    Insert        *SpliceInsert // set for splice_insert
}

// crc32mpeg is CRC-32/MPEG-2: polynomial 0x04C11DB7, initial value 0xFFFFFFFF, no reflection, no final xor.
func crc32mpeg(b []byte) uint32 {
    crc := uint32(0xFFFFFFFF)
    for _, x := range b {
        crc ^= uint32(x) << 24
        for i := 0; i < 8; i++ {
            if crc&0x80000000 != 0 { crc = crc<<1 ^ 0x04C11DB7 } else { crc <<= 1 }
        }
    }
    return crc
}

// Encode builds a splice_info_section around a splice_insert.
func Encode(s SpliceInsert) ([]byte, error) {
    if s.DurationTicks >= 1<<33 { return nil, errors.New("the duration does not fit into 33 bits") }
    if s.PTS != nil && *s.PTS >= 1<<33 { return nil, errors.New("the splice time does not fit into 33 bits") }
    var cmd []byte
    cmd = append(cmd, byte(s.EventID>>24), byte(s.EventID>>16), byte(s.EventID>>8), byte(s.EventID))
    if s.Cancel {
        cmd = append(cmd, 0xFF) // splice_event_cancel_indicator = 1, reserved = 1111111
    } else {
        cmd = append(cmd, 0x7F) // cancel = 0
        flags := byte(0x0F)     // reserved bits
        if s.Out { flags |= 0x80 }
        programSplice := true
        flags |= 0x40 // program_splice_flag: the splice applies to the whole program
        if s.DurationTicks > 0 { flags |= 0x20 }
        if s.Immediate { flags |= 0x10 }
        cmd = append(cmd, flags)
        if programSplice && !s.Immediate {
            if s.PTS == nil { return nil, errors.New("a splice that is not immediate needs a time") }
            p := *s.PTS
            cmd = append(cmd, 0xFE|byte(p>>32&1), byte(p>>24), byte(p>>16), byte(p>>8), byte(p)) // time_specified_flag = 1, reserved 111111
        }
        if s.DurationTicks > 0 {
            d := s.DurationTicks
            ar := byte(0x7E) // reserved
            if s.AutoReturn { ar |= 0x80 }
            cmd = append(cmd, ar|byte(d>>32&1), byte(d>>24), byte(d>>16), byte(d>>8), byte(d))
        }
        cmd = append(cmd, byte(s.UniqueProgramID>>8), byte(s.UniqueProgramID), s.AvailNum, s.AvailsExpected)
    }

    var body []byte
    body = append(body, 0x00)                       // protocol_version
    body = append(body, 0x00, 0x00, 0x00, 0x00, 0x00) // encrypted_packet 0, algorithm 0, pts_adjustment 0 (33 bits)
    body = append(body, 0xFF)                       // cw_index
    body = append(body, 0xFF, 0xF0|byte(len(cmd)>>8&0x0F), byte(len(cmd))) // tier 0xFFF, splice_command_length (12 bits)
    body = append(body, CmdSpliceInsert)
    body = append(body, cmd...)
    body = append(body, 0x00, 0x00) // descriptor_loop_length

    length := len(body) + 4 // + CRC
    out := []byte{0xFC, 0x30 | byte(length>>8&0x0F), byte(length)} // table_id, syntax 0, private 0, sap 3, section_length
    out = append(out, body...)
    crc := crc32mpeg(out)
    return append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc)), nil
}

// Decode parses and checks a splice_info_section.
func Decode(b []byte) (*Section, error) {
    if len(b) < 18 { return nil, errors.New("too short for a splice_info_section") }
    if b[0] != 0xFC { return nil, fmt.Errorf("table_id is 0x%02X, not 0xFC", b[0]) }
    length := int(b[1]&0x0F)<<8 | int(b[2])
    if length+3 != len(b) { return nil, fmt.Errorf("section_length says %d bytes, there are %d", length+3, len(b)) }
    want := uint32(b[len(b)-4])<<24 | uint32(b[len(b)-3])<<16 | uint32(b[len(b)-2])<<8 | uint32(b[len(b)-1])
    if got := crc32mpeg(b[:len(b)-4]); got != want { return nil, errors.New("CRC_32 does not match") }
    if b[3] != 0 { return nil, fmt.Errorf("protocol_version %d is not supported", b[3]) }
    if b[4]&0x80 != 0 { return nil, errors.New("encrypted sections are not supported") }
    s := &Section{PTSAdjustment: uint64(b[4]&1)<<32 | uint64(b[5])<<24 | uint64(b[6])<<16 | uint64(b[7])<<8 | uint64(b[8])}
    cmdLen := int(b[11]&0x0F)<<8 | int(b[12])
    s.CommandType = b[13]
    if cmdLen == 0xFFF { return nil, errors.New("splice_command_length 0xFFF is not supported") }
    if 14+cmdLen > len(b)-4-2 { return nil, errors.New("the command runs past the end of the section") }
    cmd := b[14 : 14+cmdLen]
    if s.CommandType != CmdSpliceInsert { return s, nil }
    in, err := decodeInsert(cmd)
    if err != nil { return nil, err }
    s.Insert = in
    return s, nil
}

func decodeInsert(c []byte) (*SpliceInsert, error) {
    short := errors.New("splice_insert is cut short")
    if len(c) < 5 { return nil, short }
    s := &SpliceInsert{EventID: uint32(c[0])<<24 | uint32(c[1])<<16 | uint32(c[2])<<8 | uint32(c[3])}
    s.Cancel = c[4]&0x80 != 0
    if s.Cancel { return s, nil }
    if len(c) < 6 { return nil, short }
    f := c[5]
    s.Out, s.Immediate = f&0x80 != 0, f&0x10 != 0
    programSplice, hasDuration := f&0x40 != 0, f&0x20 != 0
    p := 6
    if !programSplice { return nil, errors.New("component splices are not supported") }
    if !s.Immediate {
        if len(c) < p+1 { return nil, short }
        if c[p]&0x80 != 0 { // time_specified_flag
            if len(c) < p+5 { return nil, short }
            pts := uint64(c[p]&1)<<32 | uint64(c[p+1])<<24 | uint64(c[p+2])<<16 | uint64(c[p+3])<<8 | uint64(c[p+4])
            s.PTS = &pts
            p += 5
        } else {
            p++
        }
    }
    if hasDuration {
        if len(c) < p+5 { return nil, short }
        s.AutoReturn = c[p]&0x80 != 0
        s.DurationTicks = uint64(c[p]&1)<<32 | uint64(c[p+1])<<24 | uint64(c[p+2])<<16 | uint64(c[p+3])<<8 | uint64(c[p+4])
        p += 5
    }
    if len(c) < p+4 { return nil, short }
    s.UniqueProgramID, s.AvailNum, s.AvailsExpected = uint16(c[p])<<8|uint16(c[p+1]), c[p+2], c[p+3]
    return s, nil
}

// Parse accepts the section as hex (with or without 0x) or as base64, the two ways it is written in practice.
func Parse(s string) ([]byte, error) {
    s = strings.TrimSpace(s)
    h := strings.TrimPrefix(strings.TrimPrefix(s, "0x"), "0X")
    if b, err := hex.DecodeString(h); err == nil && len(b) > 0 { return b, nil }
    if b, err := base64.StdEncoding.DecodeString(s); err == nil && len(b) > 0 { return b, nil }
    if b, err := base64.RawStdEncoding.DecodeString(s); err == nil && len(b) > 0 { return b, nil }
    return nil, errors.New("not hex and not base64")
}

// Hex writes the section the way EXT-X-DATERANGE wants it: 0x followed by hex digits.
func Hex(b []byte) string { return "0x" + strings.ToUpper(hex.EncodeToString(b)) }

// Out builds the message that starts an ad break of the given length. It is immediate: it applies from the segment it is attached to.
func Out(eventID uint32, seconds int) ([]byte, error) {
    return Encode(SpliceInsert{EventID: eventID, Out: true, Immediate: true, DurationTicks: uint64(seconds) * TickRate, AutoReturn: true, AvailsExpected: 1})
}

// In builds the message that ends the ad break with that event id.
func In(eventID uint32) ([]byte, error) {
    return Encode(SpliceInsert{EventID: eventID, Out: false, Immediate: true})
}

// Null builds a splice_null section, the heartbeat that says nothing is spliced.
func Null() []byte {
    body := []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xFF, 0xFF, 0xF0, 0x00, CmdNull, 0x00, 0x00}
    length := len(body) + 4
    out := append([]byte{0xFC, 0x30 | byte(length>>8&0x0F), byte(length)}, body...)
    crc := crc32mpeg(out)
    return append(out, byte(crc>>24), byte(crc>>16), byte(crc>>8), byte(crc))
}

// Base64 writes the section in base64, the other form in which it is exchanged.
func Base64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
