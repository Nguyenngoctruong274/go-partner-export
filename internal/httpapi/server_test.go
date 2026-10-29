package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestHandler(ready func(context.Context) error) http.Handler {
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Ready: ready})
}

func get(t *testing.T, h http.Handler, path string, headers map[string]string) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]string
	if ct := rec.Header().Get("Content-Type"); ct == "application/json" {
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
	}
	return rec, body
}

func TestHealthzAlwaysOK(t *testing.T) {
	// Liveness never looks at dependencies, even when readiness fails.
	h := newTestHandler(func(context.Context) error { return errors.New("db down") })
	rec, body := get(t, h, "/healthz", nil)
	if rec.Code != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("got %d %v", rec.Code, body)
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name       string
		ready      func(context.Context) error
		wantCode   int
		wantStatus string
	}{
		{"no check means ready", nil, http.StatusOK, "ready"},
		{"check passes", func(context.Context) error { return nil }, http.StatusOK, "ready"},
		{"check fails", func(context.Context) error { return errors.New("draining") }, http.StatusServiceUnavailable, "unavailable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, body := get(t, newTestHandler(tt.ready), "/readyz", nil)
			if rec.Code != tt.wantCode || body["status"] != tt.wantStatus {
				t.Fatalf("got %d %v, want %d %q", rec.Code, body, tt.wantCode, tt.wantStatus)
			}
		})
	}
}

func TestReadyzCheckGetsADeadline(t *testing.T) {
	h := newTestHandler(func(ctx context.Context) error {
		if _, ok := ctx.Deadline(); !ok {
			t.Error("readiness check ran without a deadline")
		}
		return nil
	})
	get(t, h, "/readyz", nil)
}

func TestRequestID(t *testing.T) {
	h := newTestHandler(nil)

	rec, _ := get(t, h, "/healthz", nil)
	if id := rec.Header().Get(RequestIDHeader); len(id) != 24 {
		t.Errorf("generated id %q, want 24 hex chars", id)
	}

	rec, _ = get(t, h, "/healthz", map[string]string{RequestIDHeader: "abc-123"})
	if id := rec.Header().Get(RequestIDHeader); id != "abc-123" {
		t.Errorf("caller id not echoed: got %q", id)
	}

	long := strings.Repeat("x", 129)
	rec, _ = get(t, h, "/healthz", map[string]string{RequestIDHeader: long})
	if id := rec.Header().Get(RequestIDHeader); id == long {
		t.Error("oversized caller id was accepted")
	}
}

func TestRequestIDFromContext(t *testing.T) {
	if got := RequestID(context.Background()); got != "" {
		t.Fatalf("RequestID on empty context = %q", got)
	}
	var seen string
	h := NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Ready: func(ctx context.Context) error { seen = RequestID(ctx); return nil },
	})
	get(t, h, "/readyz", map[string]string{RequestIDHeader: "req-9"})
	if seen != "req-9" {
		t.Fatalf("handler saw request id %q", seen)
	}
}

func TestUnknownRouteAndMethod(t *testing.T) {
	h := newTestHandler(nil)
	rec, _ := get(t, h, "/nope", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown path: got %d", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/healthz", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /healthz: got %d", rec.Code)
	}
}
