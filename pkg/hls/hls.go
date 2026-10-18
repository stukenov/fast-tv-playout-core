package hls

import (
    "bufio"
    "errors"
    "fmt"
    "io"
    "net/url"
    "strconv"
    "strings"
    "time"
)

// Segment is one media segment of a source playlist with an absolute URI.
type Segment struct {
    URI             string
    Duration        float64
    Discontinuity   bool      // an EXT-X-DISCONTINUITY tag precedes the segment
    ProgramDateTime time.Time // zero when the playlist has no EXT-X-PROGRAM-DATE-TIME for it
    MapURI          string    // the init section that applies to the segment, empty for formats without one
}

// Playlist is the part of a VOD media playlist the stitcher needs.
type Playlist struct {
    MapURI         string
    TargetDuration float64
    MediaSequence  int64
    DiscontinuitySequence int64
    Segments       []Segment
    Ended          bool
}

// Parse reads a media playlist and resolves every URI against base.
func Parse(r io.Reader, base *url.URL) (Playlist, error) {
    var pl Playlist
    sc := bufio.NewScanner(r)
    sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
    var dur float64 = -1
    var disc bool
    var pdt time.Time
    var curMap string
    first := true
    for sc.Scan() {
        line := strings.TrimSpace(sc.Text())
        if line == "" { continue }
        if first {
            if line != "#EXTM3U" { return pl, errors.New("not an m3u8 playlist") }
            first = false
            continue
        }
        switch {
        case strings.HasPrefix(line, "#EXT-X-STREAM-INF"):
            return pl, errors.New("master playlist given, media playlist expected")
        case strings.HasPrefix(line, "#EXT-X-TARGETDURATION:"):
            v, err := strconv.ParseFloat(strings.TrimPrefix(line, "#EXT-X-TARGETDURATION:"), 64)
            if err != nil { return pl, fmt.Errorf("bad target duration: %w", err) }
            pl.TargetDuration = v
        case strings.HasPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"):
            v, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-MEDIA-SEQUENCE:"), 10, 64)
            if err != nil { return pl, fmt.Errorf("bad media sequence: %w", err) }
            pl.MediaSequence = v
        case strings.HasPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
            v, err := strconv.ParseInt(strings.TrimPrefix(line, "#EXT-X-DISCONTINUITY-SEQUENCE:"), 10, 64)
            if err != nil { return pl, fmt.Errorf("bad discontinuity sequence: %w", err) }
            pl.DiscontinuitySequence = v
        case line == "#EXT-X-DISCONTINUITY":
            disc = true
        case strings.HasPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"):
            t, err := time.Parse(time.RFC3339Nano, strings.TrimPrefix(line, "#EXT-X-PROGRAM-DATE-TIME:"))
            if err != nil { return pl, fmt.Errorf("bad program date time: %w", err) }
            pdt = t
        case strings.HasPrefix(line, "#EXT-X-MAP:"):
            uri := Attr(line, "URI")
            if uri == "" { return pl, errors.New("EXT-X-MAP without URI") }
            abs, err := resolve(base, uri)
            if err != nil { return pl, err }
            pl.MapURI = abs
            curMap = abs
        case strings.HasPrefix(line, "#EXTINF:"):
            v := strings.TrimPrefix(line, "#EXTINF:")
            if i := strings.IndexByte(v, ','); i >= 0 { v = v[:i] }
            d, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
            if err != nil { return pl, fmt.Errorf("bad EXTINF: %w", err) }
            dur = d
        case strings.HasPrefix(line, "#EXT-X-ENDLIST"):
            pl.Ended = true
        case strings.HasPrefix(line, "#EXT-X-KEY"):
            if m := Attr(line, "METHOD"); m != "" && m != "NONE" { return pl, errors.New("encrypted sources are not supported") }
        case strings.HasPrefix(line, "#"):
            // other tags are not needed
        default:
            if dur < 0 { return pl, errors.New("segment URI without EXTINF") }
            abs, err := resolve(base, line)
            if err != nil { return pl, err }
            pl.Segments = append(pl.Segments, Segment{URI: abs, Duration: dur, Discontinuity: disc, ProgramDateTime: pdt, MapURI: curMap})
            dur, disc, pdt = -1, false, time.Time{}
        }
    }
    if err := sc.Err(); err != nil { return pl, err }
    if first { return pl, errors.New("empty playlist") }
    if len(pl.Segments) == 0 { return pl, errors.New("playlist has no segments") }
    return pl, nil
}

// TotalDuration is the summed duration of all segments.
func (p Playlist) TotalDuration() float64 {
    t := 0.0
    for _, s := range p.Segments { t += s.Duration }
    return t
}

func resolve(base *url.URL, ref string) (string, error) {
    u, err := url.Parse(ref)
    if err != nil { return "", err }
    if base == nil { return u.String(), nil }
    return base.ResolveReference(u).String(), nil
}

// Attr extracts a quoted or bare attribute value from a tag line.
func Attr(line, key string) string {
    i := strings.Index(line, key+"=")
    if i < 0 { return "" }
    v := line[i+len(key)+1:]
    if strings.HasPrefix(v, "\"") {
        v = v[1:]
        if j := strings.IndexByte(v, '"'); j >= 0 { return v[:j] }
        return v
    }
    if j := strings.IndexByte(v, ','); j >= 0 { return v[:j] }
    return v
}
