package models

import (
    "time"
)

type Asset struct {
    ID         string            `json:"id"`
    URI        string            `json:"uri"`
    DurationS  int               `json:"duration_s"`
    Tags       []string          `json:"tags,omitempty"`
    Meta       map[string]any    `json:"meta,omitempty"`
    CreatedAt  time.Time         `json:"created_at"`
}

type Channel struct {
    ID          string         `json:"id"`
    Name        string         `json:"name"`
    SegmentSec  int            `json:"segment_sec"`
    Ladder      []string       `json:"ladder"`
    CreatedAt   time.Time      `json:"created_at"`
    ScheduleVersion int64      `json:"schedule_version"` // grows with every change of the schedule; the ETag of the schedule
}

type TimelineEntry struct {
    Type       string    `json:"type"`
    AssetID    string    `json:"asset_id,omitempty"`
    URI        string    `json:"uri,omitempty"`
    Start      time.Time `json:"start"`
    DurationS  int       `json:"duration"`
    OffsetS    int       `json:"offset_s,omitempty"` // where in the source the entry starts: a movie cut by an ad break goes on from here
    Category   string    `json:"category,omitempty"`
    SCTE35     string    `json:"scte35,omitempty"`
}

// Ads orchestrator DTOs
type AdBreak struct {
    Start    time.Time `json:"start"`
    Duration int       `json:"duration"`
    Category string    `json:"category"`
}

// AdPodRequest is what the engine sends to an ad server (ADS_URL) for every break: see x-ads-webhook in api/openapi.yaml.
type AdPodRequest struct {
    ChannelID string  `json:"channel_id"`
    Break     AdBreak `json:"break"`
}

type AdItem struct {
    URI       string `json:"uri"`
    DurationS int    `json:"duration_s"`
    CreativeID string `json:"creative_id,omitempty"`
    Type       string `json:"type,omitempty"` // partner|house
}

// AdPodResponse is the answer: the creatives to play, in order. Their lengths should add up to the break.
type AdPodResponse struct {
    Items []AdItem `json:"items"`
}

// ClipPlan item published by playout for consumption by packager
type ClipItem struct {
    Seq           int64     `json:"seq"`
    URI           string    `json:"uri"`
    StartAt       time.Time `json:"start_at"`
    DurationS     int       `json:"duration_s"`
    OffsetS       int       `json:"offset_s"` // seconds from the start of the source this quantum plays
    Discontinuity bool      `json:"discontinuity"`
    Type          string    `json:"type"` // content|ad|bumper|slate
    Title         string    `json:"title,omitempty"`        // what it is: the asset (or uri) of a program, the creative of an ad
    EntryStart    time.Time `json:"entry_start,omitempty"`  // the schedule entry it belongs to
    BreakStart    time.Time `json:"break_start,omitempty"`  // for ads: the ad break
    BreakDurS     int       `json:"break_duration,omitempty"`
    SCTE35        string    `json:"scte35,omitempty"` // hex of the SCTE-35 message the schedule gave for the break
}

// Channel status DTO for playout status endpoint
type ChannelStatus struct {
    NowSeq         int64     `json:"now_seq"`
    WindowLen      int       `json:"window_len"`
    LastTimelineAt time.Time `json:"last_timeline_at"`
    AdState        string    `json:"ad_state"`
    Gaps           int       `json:"gaps"`
    Segments       int       `json:"segments"`         // segments in the live playlist
    LastPlaylistAt time.Time `json:"last_playlist_at"` // when the playlist was written last
}


