package hls

import (
    "net/url"
    "strings"
    "testing"
)

const vod = `#EXTM3U
#EXT-X-VERSION:7
#EXT-X-TARGETDURATION:2
#EXT-X-MEDIA-SEQUENCE:7
#EXT-X-DISCONTINUITY-SEQUENCE:3
#EXT-X-MAP:URI="init.mp4"
#EXT-X-PROGRAM-DATE-TIME:2025-01-01T00:00:00.000Z
#EXTINF:2.000,
seg_0.m4s
#EXT-X-DISCONTINUITY
#EXTINF:2.000,
seg_1.m4s
#EXTINF:1.500,
../shared/seg_2.m4s
#EXT-X-ENDLIST
`

func TestParseResolvesURIsAndDurations(t *testing.T) {
    base, _ := url.Parse("https://cdn.example.com/content/m1/index.m3u8")
    pl, err := Parse(strings.NewReader(vod), base)
    if err != nil { t.Fatal(err) }
    if pl.MapURI != "https://cdn.example.com/content/m1/init.mp4" { t.Fatalf("map: %s", pl.MapURI) }
    if len(pl.Segments) != 3 || !pl.Ended { t.Fatalf("segments=%d ended=%v", len(pl.Segments), pl.Ended) }
    if pl.Segments[2].URI != "https://cdn.example.com/content/shared/seg_2.m4s" { t.Fatalf("uri: %s", pl.Segments[2].URI) }
    if pl.TotalDuration() != 5.5 { t.Fatalf("total: %v", pl.TotalDuration()) }
    if pl.MediaSequence != 7 || pl.DiscontinuitySequence != 3 { t.Fatalf("sequences: %d %d", pl.MediaSequence, pl.DiscontinuitySequence) }
    if pl.Segments[1].MapURI != pl.MapURI { t.Fatalf("segment map: %s", pl.Segments[1].MapURI) }
    if pl.Segments[0].Discontinuity || !pl.Segments[1].Discontinuity { t.Fatal("discontinuity must belong to the segment after the tag") }
    if pl.Segments[0].ProgramDateTime.Unix() != 1735689600 || !pl.Segments[1].ProgramDateTime.IsZero() { t.Fatalf("pdt: %v %v", pl.Segments[0].ProgramDateTime, pl.Segments[1].ProgramDateTime) }
}

func TestParseRejectsBadInput(t *testing.T) {
    cases := map[string]string{
        "not m3u8":    "hello",
        "master":      "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nv.m3u8\n",
        "no segments": "#EXTM3U\n#EXT-X-TARGETDURATION:2\n",
        "no extinf":   "#EXTM3U\nseg.m4s\n",
        "encrypted":   "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"k\"\n#EXTINF:2,\ns.m4s\n",
    }
    for name, body := range cases {
        if _, err := Parse(strings.NewReader(body), nil); err == nil { t.Errorf("%s: expected error", name) }
    }
}
