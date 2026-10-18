package main

import (
    "encoding/json"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/rs/zerolog"

    "ftvx/pkg/config"
    "ftvx/pkg/models"
)

// buildPlan lays out the clips of the next seconds. Ad pods come from the pods source, which never waits for the network:
// a break whose pod is not known yet holds the plan back until it is about to be released, and then it is filled with slate.
func buildPlan(now time.Time, cfg config.Config, entries []models.TimelineEntry, pods podSource, lg zerolog.Logger) []models.ClipItem {
    seg := time.Duration(cfg.SegmentSec) * time.Second
    ahead := cfg.PublishAheadS
    if ahead <= 0 { ahead = cfg.PreloadWindowS }
    end := now.Add(time.Duration(ahead) * time.Second)
    var out []models.ClipItem
    // helper to find entry covering a time
    findEntry := func(ts time.Time) (models.TimelineEntry, bool) {
        for _, e := range entries {
            st := e.Start
            en := e.Start.Add(time.Duration(e.DurationS) * time.Second)
            if !ts.Before(st) && ts.Before(en) { return e, true }
        }
        return models.TimelineEntry{}, false
    }
    prevType := ""
    for ts := now; !ts.After(end.Add(-seg)); ts = ts.Add(seg) {
        seq := quantSeq(ts, cfg.SegmentSec)
        e, ok := findEntry(ts)
        var item models.ClipItem
        item.Seq = seq
        item.StartAt = ts
        item.DurationS = cfg.SegmentSec
        if !ok {
            // fallback slate
            item.Type = "slate"
            item.URI = cfg.SlateURI
        } else {
            switch e.Type {
            case "content", "bumper":
                item.Type = e.Type
                item.URI = e.URI
                item.Title, item.EntryStart = e.AssetID, e.Start
                if item.Title == "" { item.Title = e.URI }
                item.OffsetS = int(ts.Sub(e.Start)/time.Second) + e.OffsetS
            case "slate":
                item.Type = "slate"
                item.URI = cfg.SlateURI
            case "ad_break":
                due := !ts.After(now.Add(time.Duration(cfg.LiveLeadS) * time.Second)) // this clip is released now
                pod, state := pods.pod(e, due)
                if state != podReady {
                    if !due { return out } // the answer is on its way: plan the rest on a later tick
                    lg.Warn().Str("channel", cfg.ChannelID).Time("break", e.Start).Msg("no ad pod in time, the break is filled with slate")
                    pods.giveUp(e.Start)
                    pod = nil
                }
                breakItems := buildAdBreakQuanta(e, pod, cfg)
                idx := int(ts.Sub(e.Start) / seg)
                if idx >= 0 && idx < len(breakItems) {
                    item = breakItems[idx]
                    item.Seq = seq
                    item.StartAt = ts
                    item.DurationS = cfg.SegmentSec
                    item.EntryStart, item.BreakStart, item.BreakDurS, item.SCTE35 = e.Start, e.Start, e.DurationS, e.SCTE35
                } else {
                    item.Type = "slate"
                    item.URI = cfg.SlateURI
                }
            default:
                item.Type = "slate"
                item.URI = cfg.SlateURI
            }
        }
        // a slate loops: its offset just keeps growing and the packager wraps it
        if item.Type == "slate" { item.OffsetS = int(ts.Unix()); item.Title = "slate" }
        // discontinuity markers at ad boundaries
        if item.Type == "ad" && prevType != "ad" { item.Discontinuity = true }
        if item.Type != "ad" && prevType == "ad" { item.Discontinuity = true }
        out = append(out, item)
        prevType = item.Type
    }
    return out
}

func buildAdBreakQuanta(br models.TimelineEntry, items []models.AdItem, cfg config.Config) []models.ClipItem {
    seg := cfg.SegmentSec
    quantaTotal := br.DurationS / seg
    var out []models.ClipItem
    // Expand ad items into quanta
    for _, it := range items {
        n := it.DurationS / seg
        for i := 0; i < n; i++ {
            out = append(out, models.ClipItem{URI: it.URI, DurationS: seg, OffsetS: i * seg, Type: "ad", Title: it.CreativeID})
        }
    }
    // pad with slate if not enough
    for len(out) < quantaTotal {
        out = append(out, models.ClipItem{URI: cfg.SlateURI, DurationS: seg, Type: "slate"})
    }
    // trim if too many
    if len(out) > quantaTotal {
        out = out[:quantaTotal]
    }
    // mark discontinuity on first item of break; exit will be marked by caller on next clip
    if len(out) > 0 {
        out[0].Discontinuity = true
    }
    return out
}

func quantSeq(ts time.Time, segSec int) int64 {
    return ts.Unix() / int64(segSec)
}

func callAds(url, channelID string, br models.TimelineEntry, apiKey string) (models.AdPodResponse, error) {
    cli := http.Client{Timeout: 2 * time.Second}
    reqBody := models.AdPodRequest{ChannelID: channelID, Break: models.AdBreak{Start: br.Start, Duration: br.DurationS, Category: br.Category}}
    bs, _ := json.Marshal(reqBody)
    req, _ := http.NewRequest("POST", url, strings.NewReader(string(bs)))
    req.Header.Set("Content-Type", "application/json")
    if apiKey != "" { req.Header.Set("X-API-Key", apiKey) }
    resp, err := cli.Do(req)
    if err != nil { return models.AdPodResponse{}, err }
    defer resp.Body.Close()
    if resp.StatusCode == 204 { return models.AdPodResponse{}, nil } // no creatives: the break becomes slate
    if resp.StatusCode != 200 {
        return models.AdPodResponse{}, fmt.Errorf("ads status %d", resp.StatusCode)
    }
    var out models.AdPodResponse
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil { return models.AdPodResponse{}, err }
    // normalize URIs if needed
    for i := range out.Items {
        out.Items[i].URI = normalizeURI(out.Items[i].URI)
    }
    return out, nil
}

func normalizeURI(u string) string {
    // allow plain s3:// or http(s) URLs; for relative paths, prepend s3://bucket placeholder
    if strings.HasPrefix(u, "s3://") || strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") { return u }
    return "s3://" + strings.TrimPrefix(u, "/")
}


