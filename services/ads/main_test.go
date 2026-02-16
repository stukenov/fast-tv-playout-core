package main

import (
    "encoding/json"
    "net/http/httptest"
    "os"
    "path/filepath"
    "testing"

    "github.com/go-chi/chi/v5"
    "github.com/go-chi/chi/v5/middleware"

    "ftvx/pkg/models"
)

func TestReadyzDependsOnRegistry(t *testing.T) {
    // prepare temp registry file
    dir := t.TempDir()
    regPath := filepath.Join(dir, "_registry.json")
    os.WriteFile(regPath, []byte(`{"version":1,"updated_at":"2025-01-01T00:00:00Z","creatives":[{"id":"house_pad_2","type":"house","category":["any"],"duration":2,"uri":"s3://bucket/ads/house/pad_2/index.m3u8","active":true}]}`), 0o644)

    os.Setenv("ADS_REGISTRY_FILE", regPath)
    os.Setenv("API_KEY", "dev")
    st := &state{}
    // minimalize router
    r := chi.NewRouter()
    r.Use(middleware.RequestID, middleware.RealIP, middleware.Recoverer)
    st.loadRegistry()

    // inline readiness check
    if !st.isReady() {
        t.Fatalf("expected ready after registry load")
    }
}

func TestSelectionPadsToExact(t *testing.T) {
    st := &state{segSec: 2, selectOrder: []int{30,15,10,5,2}}
    st.reg = &registry{Version: 1, Creatives: []creative{
        {ID: "adv_30_a", Type: "partner", Category: []string{"midroll"}, Duration: 30, URI: "s3://bucket/ads/adv_30_a/index.m3u8", Active: true},
        {ID: "house_pad_2", Type: "house", Category: []string{"any"}, Duration: 2, URI: "s3://bucket/ads/house/pad_2/index.m3u8", Active: true},
    }}
    st.ready = true

    req := models.AdPodRequest{ChannelID: "c1", Break: models.AdBreak{Duration: 34, Category: "midroll"}, Constraints: &models.AdConstraints{AllowHouse: true, NoBackToBack: true}}
    cands := st.selectCandidates(req, false)
    items, filled, _ := st.fillPod(req, cands)
    if filled != 34 { t.Fatalf("filled=%d, want 34", filled) }
    got := 0
    for _, it := range items { got += it.DurationS }
    if got != 34 { t.Fatalf("sum=%d, want 34", got) }
    _ = json.NewEncoder(httptest.NewRecorder()).Encode(items)
}

