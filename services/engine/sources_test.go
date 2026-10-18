package main

import (
    "context"
    "fmt"
    "net/http"
    "net/http/httptest"
    "os"
    "path/filepath"
    "strings"
    "testing"
    "time"

    "ftvx/pkg/config"
    "ftvx/pkg/fmp4"
    "ftvx/pkg/models"
)

// writeSource writes source s as files in dir: index.m3u8, init.mp4 and the segments.
func writeSource(t *testing.T, dir string, s source) {
    t.Helper()
    if err := os.MkdirAll(dir, 0o755); err != nil { t.Fatal(err) }
    must := func(name string, b []byte) { if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil { t.Fatal(err) } }
    must("index.m3u8", []byte(s.playlist()))
    must("init.mp4", initBytes[s.name])
    for i := 0; i < s.segs; i++ { must(fmt.Sprintf("seg_%d.m4s", i), s.segment(i)) }
}

func stitcherWith(t *testing.T, tweak func(*config.Config)) (*stitcher, config.Config) {
    t.Helper()
    srv := sourceServer(t)
    cfg := testConfig(srv)
    cfg.OriginRoot = t.TempDir()
    if tweak != nil { tweak(&cfg) }
    return newStitcher(cfg, newResolver(cfg), filepath.Join(cfg.OriginRoot, "hls", "c1", "v720")), cfg
}

func clipOf(uri string, n int) models.ClipItem {
    return models.ClipItem{Seq: t0.Unix() + int64(n), URI: uri, StartAt: t0.Add(time.Duration(n) * time.Second), DurationS: 1, OffsetS: n, Type: "content"}
}

func pictureOf(t *testing.T, st *stitcher, i int) string {
    t.Helper()
    pic, _ := fmp4.MediaData(readSeg(t, st, i))
    return string(pic)
}

// refused is true when the source was not played: the clip was dropped, or the slate took its place.
func refused(t *testing.T, st *stitcher) bool {
    t.Helper()
    if len(st.win.Items) == 0 { return true }
    return strings.HasPrefix(pictureOf(t, st, 0), "slate_")
}

func TestFileSources_PlayFromADirectory(t *testing.T) {
    root := t.TempDir()
    writeSource(t, filepath.Join(root, "movies", "a"), sources[0])
    st, _ := stitcherWith(t, func(c *config.Config) { c.SourceFileRoot = root })
    if !st.add(context.Background(), clipOf("file://"+filepath.Join(root, "movies", "a", "index.m3u8"), 3)) { t.Fatal("not added") }
    if got := pictureOf(t, st, 0); got != "a_3" { t.Fatalf("got %s: the file source was not played, the slate was", got) }
}

func TestFileSources_AreOffUnlessARootIsSet(t *testing.T) {
    root := t.TempDir()
    writeSource(t, filepath.Join(root, "a"), sources[0])
    st, _ := stitcherWith(t, nil)
    st.add(context.Background(), clipOf("file://"+filepath.Join(root, "a", "index.m3u8"), 3))
    if got := pictureOf(t, st, 0); !strings.HasPrefix(got, "slate_") { t.Fatalf("file sources must be refused without SOURCE_FILE_ROOT, got %s", got) }
}

func TestFileSources_CannotLeaveTheRoot(t *testing.T) {
    base := t.TempDir()
    root := filepath.Join(base, "root")
    outside := filepath.Join(base, "outside")
    writeSource(t, filepath.Join(root, "ok"), sources[0])
    writeSource(t, outside, sources[0])
    // 1. a playlist outside the root
    st, _ := stitcherWith(t, func(c *config.Config) { c.SourceFileRoot = root })
    st.add(context.Background(), clipOf("file://"+filepath.Join(outside, "index.m3u8"), 1))
    if !refused(t, st) { t.Fatalf("a playlist outside the root was played: %s", pictureOf(t, st, 0)) }
    // 2. a playlist inside the root that points at segments outside it
    evil := filepath.Join(root, "evil")
    if err := os.MkdirAll(evil, 0o755); err != nil { t.Fatal(err) }
    pl := "#EXTM3U\n#EXT-X-MAP:URI=\"../../outside/init.mp4\"\n#EXTINF:1.000,\n../../outside/seg_1.m4s\n#EXT-X-ENDLIST\n"
    _ = os.WriteFile(filepath.Join(evil, "index.m3u8"), []byte(pl), 0o644)
    st2, _ := stitcherWith(t, func(c *config.Config) { c.SourceFileRoot = root })
    st2.add(context.Background(), clipOf("file://"+filepath.Join(evil, "index.m3u8"), 0))
    if got := pictureOf(t, st2, 0); !strings.HasPrefix(got, "slate_") { t.Fatalf("a segment outside the root was played: %s", got) }
    // 3. a symbolic link inside the root that leads outside
    if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil { t.Fatal(err) }
    st3, _ := stitcherWith(t, func(c *config.Config) { c.SourceFileRoot = root })
    st3.add(context.Background(), clipOf("file://"+filepath.Join(root, "link", "index.m3u8"), 1))
    if got := pictureOf(t, st3, 0); !strings.HasPrefix(got, "slate_") { t.Fatalf("a link out of the root was followed: %s", got) }
    // and what is inside still works
    st4, _ := stitcherWith(t, func(c *config.Config) { c.SourceFileRoot = root })
    st4.add(context.Background(), clipOf("file://"+filepath.Join(root, "ok", "index.m3u8"), 2))
    if got := pictureOf(t, st4, 0); got != "a_2" { t.Fatalf("a file inside the root: %s", got) }
}

func TestHostAllowList(t *testing.T) {
    srv, _ := sourceServerCtl(t)
    host := strings.TrimPrefix(srv.URL, "http://")
    uri := srv.URL + "/vod/a/index.m3u8"
    open, _ := stitcherWith(t, nil)
    open.add(context.Background(), clipOf(uri, 2))
    if got := pictureOf(t, open, 0); got != "a_2" { t.Fatalf("without a list any host is fine: %s", got) }

    listed, _ := stitcherWith(t, func(c *config.Config) { c.SourceAllowHosts = "somewhere.else, " + host })
    listed.add(context.Background(), clipOf(uri, 2))
    if got := pictureOf(t, listed, 0); got != "a_2" { t.Fatalf("a listed host: %s", got) }

    notListed, _ := stitcherWith(t, func(c *config.Config) { c.SourceAllowHosts = "somewhere.else:8080" })
    notListed.add(context.Background(), clipOf(uri, 2))
    if !refused(t, notListed) { t.Fatalf("a host that is not listed was played: %s", pictureOf(t, notListed, 0)) }
}

// A listed host that redirects to one that is not listed must not be followed.
func TestHostAllowList_RedirectsAreChecked(t *testing.T) {
    inner, _ := sourceServerCtl(t)
    var redirectorHits int
    redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        redirectorHits++
        http.Redirect(w, r, inner.URL+r.URL.Path, http.StatusFound)
    }))
    t.Cleanup(redirector.Close)
    st, _ := stitcherWith(t, func(c *config.Config) { c.SourceAllowHosts = strings.TrimPrefix(redirector.URL, "http://") })
    st.add(context.Background(), clipOf(redirector.URL+"/vod/a/index.m3u8", 2))
    if redirectorHits == 0 { t.Fatal("the redirector was never asked: the test proves nothing") }
    if !refused(t, st) { t.Fatalf("a redirect to a host that is not listed was followed: %s", pictureOf(t, st, 0)) }
}

func TestUnknownSchemesAreRefused(t *testing.T) {
    st, _ := stitcherWith(t, nil)
    for _, uri := range []string{"ftp://x/y.m3u8", "gopher://x", "data:text/plain,hi", "/etc/passwd", "../x.m3u8"} {
        st2, _ := stitcherWith(t, nil)
        st2.add(context.Background(), clipOf(uri, 1))
        if !refused(t, st2) { t.Errorf("%s was played: %s", uri, pictureOf(t, st2, 0)) }
    }
    _ = st
}
