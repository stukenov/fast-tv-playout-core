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
}

type TimelineEntry struct {
    Type       string    `json:"type"`
    AssetID    string    `json:"asset_id,omitempty"`
    URI        string    `json:"uri,omitempty"`
    Start      time.Time `json:"start"`
    DurationS  int       `json:"duration"`
    Category   string    `json:"category,omitempty"`
    SCTE35     string    `json:"scte35,omitempty"`
}

type ScheduleRequest struct {
    From    time.Time       `json:"from"`
    To      time.Time       `json:"to"`
    Entries []TimelineEntry `json:"entries,omitempty"`
    Rules   *ScheduleRules  `json:"rules,omitempty"`
    Mode    string          `json:"mode,omitempty"`
}

type NowNext struct {
    ChannelID string        `json:"channel_id"`
    Now       NowNextItem   `json:"now"`
    Next      NowNextItem   `json:"next"`
}

type NowNextItem struct {
    Title     string    `json:"title"`
    StartsAt  time.Time `json:"started_at,omitempty"`
    StartsAt2 time.Time `json:"starts_at,omitempty"`
    EndsAt    time.Time `json:"ends_at"`
}

// Scheduler rules DTO (see spec)
type ScheduleRules struct {
    SegmentSec int `json:"segment_sec"`
    AdBreaks   struct {
        DefaultDurationS int    `json:"default_duration_s"`
        AdsEvery         string `json:"ads_every"`
    } `json:"ad_breaks"`
    Blocks []struct {
        Range   string `json:"range"`
        Library string `json:"library"`
    } `json:"blocks"`
    SlateURI       *string `json:"slate_uri,omitempty"`
    BumperStartURI *string `json:"bumper_start_uri,omitempty"`
}

type ScheduleResponse struct {
    ChannelID           string    `json:"channel_id"`
    WrittenFrom         time.Time `json:"written_from"`
    WrittenTo           time.Time `json:"written_to"`
    EntriesCount        int       `json:"entries_count"`
    HorizonCoveredUntil time.Time `json:"horizon_covered_until"`
    NowNext             any       `json:"now_next,omitempty"`
}

// Ads orchestrator DTOs
type AdBreak struct {
    Start    time.Time `json:"start"`
    Duration int       `json:"duration"`
    Category string    `json:"category"`
}

type AdPodRequest struct {
    ChannelID string  `json:"channel_id"`
    Break     AdBreak `json:"break"`
    VastTags  []string `json:"vast_tags,omitempty"`
    Constraints *AdConstraints `json:"constraints,omitempty"`
}

type AdItem struct {
    URI       string `json:"uri"`
    DurationS int    `json:"duration_s"`
    CreativeID string `json:"creative_id,omitempty"`
    Type       string `json:"type,omitempty"` // partner|house
}

type AdPodResponse struct {
    Items []AdItem `json:"items"`
    FilledDuration int     `json:"filled_duration,omitempty"`
    FillRatio      float64 `json:"fill_ratio,omitempty"`
    Strategy       string  `json:"strategy,omitempty"`
    Vast           *VastStatus `json:"vast,omitempty"`
}

type AdConstraints struct {
    NoBackToBack bool `json:"no_back_to_back"`
    AllowHouse   bool `json:"allow_house"`
}

type VastStatus struct {
    Status      string `json:"status"` // ok|error|timeout|skipped
    CheckedTags int    `json:"checked_tags"`
}

// Segment descriptor passed from playout to packager
type SegmentDescriptor struct {
    ChannelID       string    `json:"channel_id"`
    Sequence        int64     `json:"sequence"`
    ProgramDateTime time.Time `json:"program_date_time"`
    DurationS       int       `json:"duration_s"`
    Filename        string    `json:"filename"`
}

// ClipPlan item published by playout for consumption by packager
type ClipItem struct {
    Seq           int64     `json:"seq"`
    URI           string    `json:"uri"`
    StartAt       time.Time `json:"start_at"`
    DurationS     int       `json:"duration_s"`
    Discontinuity bool      `json:"discontinuity"`
    Type          string    `json:"type"` // content|ad|bumper|slate
}

// Channel status DTO for playout status endpoint
type ChannelStatus struct {
    NowSeq         int64     `json:"now_seq"`
    WindowLen      int       `json:"window_len"`
    LastTimelineAt time.Time `json:"last_timeline_at"`
    AdState        string    `json:"ad_state"`
    Gaps           int       `json:"gaps"`
}


