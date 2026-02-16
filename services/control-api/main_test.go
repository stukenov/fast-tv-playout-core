package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
)

func TestHandlePostChannel_AndScheduleValidation(t *testing.T) {
	// create channel
	chBody := bytes.NewBufferString(`{"id":"c1","name":"FAST Movies","segment_sec":2,"ladder":[1080]}`)
	req := httptest.NewRequest("POST", "/v1/channels", chBody)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handlePostChannel(rr, req)
	if rr.Code != http.StatusCreated {
		t.Fatalf("create channel status=%d body=%s", rr.Code, rr.Body.String())
	}

	// invalid schedule (non-quantized duration)
	schBody := bytes.NewBufferString(`{"window":{"from":"2025-01-01T00:00:00Z","to":"2025-01-01T01:00:00Z"},"entries":[{"type":"content","asset_id":"m1","start":"2025-01-01T00:00:00Z","duration":3}]}`)
	req2 := httptest.NewRequest("POST", "/v1/schedule/c1:replace", schBody)
	req2.Header.Set("Content-Type", "application/json")
	// inject chi URL param
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("channel_id", "c1")
	req2 = req2.WithContext(context.WithValue(req2.Context(), chi.RouteCtxKey, rctx))
	rr2 := httptest.NewRecorder()
	handleScheduleReplace(rr2, req2)
	if rr2.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-quantized, got %d", rr2.Code)
	}
}

// contextWithChi attaches chi route context
func contextWithChi(ctx context.Context, rctx *chi.Context) context.Context { return context.WithValue(ctx, chi.RouteCtxKey, rctx) }