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
	"sync"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
)

var hcm = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		panic(err)
	}
	return loc
}()

// fakeExports records calls and lets a test hold a run open.
type fakeExports struct {
	mu       sync.Mutex
	runs     map[export.Day]export.Run
	started  []export.Day
	repushed []export.Day
	block    chan struct{} // Run blocks until closed, when set
	listErr  error
}

func newFakeExports() *fakeExports { return &fakeExports{runs: map[export.Day]export.Run{}} }

func (f *fakeExports) Run(ctx context.Context, day export.Day) (export.Run, error) {
	f.mu.Lock()
	f.started = append(f.started, day)
	block := f.block
	f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return export.Run{}, ctx.Err()
		}
	}
	r := export.Run{Day: day, State: export.StatePushed, Rows: 2, Attempts: 1, StartedAt: time.Now(), FinishedAt: time.Now(), BlobKey: "k"}
	f.mu.Lock()
	f.runs[day] = r
	f.mu.Unlock()
	return r, nil
}

func (f *fakeExports) Repush(_ context.Context, day export.Day) (export.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.repushed = append(f.repushed, day)
	return f.runs[day], nil
}

func (f *fakeExports) Get(_ context.Context, day export.Day) (export.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.runs[day]
	if !ok {
		return export.Run{}, postgres.ErrRunNotFound
	}
	return r, nil
}

func (f *fakeExports) List(_ context.Context, state export.State, before export.Day, limit int) ([]export.Run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []export.Run
	for _, r := range f.runs {
		if (state == "" || r.State == state) && (before.IsZero() || r.Day.Before(before)) {
			out = append(out, r)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeExports) startedDays() []export.Day {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]export.Day(nil), f.started...)
}

var now = time.Date(2026, 10, 29, 1, 0, 0, 0, hcm)

func newExportsHandler(t *testing.T, fx *fakeExports) *Handler {
	t.Helper()
	return NewHandler(slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Exports: &ExportOptions{Exports: fx, Location: hcm, Now: func() time.Time { return now }, RunTimeout: 5 * time.Second},
	})
}

func do(t *testing.T, h http.Handler, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, r)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	if rec.Body.Len() > 0 {
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
	}
	return rec, out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

func TestStartExportDefaultsToYesterday(t *testing.T) {
	fx := newFakeExports()
	h := newExportsHandler(t, fx)
	rec, body := do(t, h, http.MethodPost, "/v1/exports", "")
	if rec.Code != http.StatusAccepted || body["day"] != "2026-10-28" || body["state"] != "pending" {
		t.Fatalf("status %d body %v", rec.Code, body)
	}
	waitFor(t, func() bool { return len(fx.startedDays()) == 1 })
	if fx.startedDays()[0] != (export.Day{Year: 2026, Month: time.October, Date: 28}) {
		t.Errorf("started %v", fx.startedDays())
	}
	if err := h.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, body = do(t, h, http.MethodGet, "/v1/exports/2026-10-28", "")
	if rec.Code != http.StatusOK || body["state"] != "pushed" || body["rows"] != float64(2) || body["running"] != false || body["finished_at"] == nil {
		t.Fatalf("get: status %d body %v", rec.Code, body)
	}
}

func TestStartExportWithDayAndRejections(t *testing.T) {
	fx := newFakeExports()
	h := newExportsHandler(t, fx)
	rec, body := do(t, h, http.MethodPost, "/v1/exports", `{"day":"2026-10-01"}`)
	if rec.Code != http.StatusAccepted || body["day"] != "2026-10-01" {
		t.Fatalf("status %d body %v", rec.Code, body)
	}
	_ = h.Drain(context.Background())

	tests := []struct {
		name string
		body string
		want int
	}{
		{"bad json", `{`, http.StatusBadRequest},
		{"bad day", `{"day":"01/10/2026"}`, http.StatusBadRequest},
		{"today is not over", `{"day":"2026-10-29"}`, http.StatusUnprocessableEntity},
		{"future", `{"day":"2027-01-01"}`, http.StatusUnprocessableEntity},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, _ := do(t, h, http.MethodPost, "/v1/exports", tt.body)
			if rec.Code != tt.want {
				t.Fatalf("status %d, want %d", rec.Code, tt.want)
			}
		})
	}
}

func TestOneRunPerDayInFlight(t *testing.T) {
	fx := newFakeExports()
	fx.block = make(chan struct{})
	h := newExportsHandler(t, fx)
	rec, _ := do(t, h, http.MethodPost, "/v1/exports", "")
	if rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code)
	}
	waitFor(t, func() bool { return len(fx.startedDays()) == 1 })
	rec, _ = do(t, h, http.MethodPost, "/v1/exports", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second start: %d", rec.Code)
	}
	// Another day is fine.
	rec, _ = do(t, h, http.MethodPost, "/v1/exports", `{"day":"2026-10-27"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("other day: %d", rec.Code)
	}
	// Drain times out while the run is held open, then finishes.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := h.Drain(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Drain with a blocked run = %v", err)
	}
	close(fx.block)
	if err := h.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
	rec, _ = do(t, h, http.MethodPost, "/v1/exports", "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("after drain: %d", rec.Code)
	}
	_ = h.Drain(context.Background())
}

func TestGetAndListExports(t *testing.T) {
	fx := newFakeExports()
	fx.runs[export.Day{Year: 2026, Month: time.October, Date: 27}] = export.Run{Day: export.Day{Year: 2026, Month: time.October, Date: 27}, State: export.StateFailed, Stage: "push", Error: "down", StartedAt: now}
	fx.runs[export.Day{Year: 2026, Month: time.October, Date: 28}] = export.Run{Day: export.Day{Year: 2026, Month: time.October, Date: 28}, State: export.StatePushed, StartedAt: now, FinishedAt: now}
	h := newExportsHandler(t, fx)

	rec, body := do(t, h, http.MethodGet, "/v1/exports/2026-10-27", "")
	if rec.Code != http.StatusOK || body["state"] != "failed" || body["stage"] != "push" || body["error"] != "down" || body["finished_at"] != nil {
		t.Errorf("get failed run: %d %v", rec.Code, body)
	}
	rec, _ = do(t, h, http.MethodGet, "/v1/exports/2026-10-01", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("get missing: %d", rec.Code)
	}
	rec, _ = do(t, h, http.MethodGet, "/v1/exports/yesterday", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("get bad day: %d", rec.Code)
	}

	rec, body = do(t, h, http.MethodGet, "/v1/exports", "")
	if rec.Code != http.StatusOK || len(body["runs"].([]any)) != 2 {
		t.Errorf("list all: %d %v", rec.Code, body)
	}
	rec, body = do(t, h, http.MethodGet, "/v1/exports?state=failed", "")
	if rec.Code != http.StatusOK || len(body["runs"].([]any)) != 1 {
		t.Errorf("list failed: %d %v", rec.Code, body)
	}
	rec, body = do(t, h, http.MethodGet, "/v1/exports?before=2026-10-28&limit=1", "")
	if rec.Code != http.StatusOK || len(body["runs"].([]any)) != 1 {
		t.Errorf("list page: %d %v", rec.Code, body)
	}
	for _, q := range []string{"state=weird", "before=x", "limit=0", "limit=501", "limit=ten"} {
		rec, _ = do(t, h, http.MethodGet, "/v1/exports?"+q, "")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("list ?%s: %d", q, rec.Code)
		}
	}
	fx.listErr = errors.New("db down")
	rec, _ = do(t, h, http.MethodGet, "/v1/exports", "")
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("list with db down: %d", rec.Code)
	}
}

func TestRepushExport(t *testing.T) {
	fx := newFakeExports()
	day := export.Day{Year: 2026, Month: time.October, Date: 28}
	fx.runs[day] = export.Run{Day: day, State: export.StateFailed, Stage: "push", BlobKey: "partner-export/x.csv", StartedAt: now}
	fx.runs[day.Previous()] = export.Run{Day: day.Previous(), State: export.StateFailed, Stage: "select", StartedAt: now}
	h := newExportsHandler(t, fx)

	rec, body := do(t, h, http.MethodPost, "/v1/exports/2026-10-28/repush", "")
	if rec.Code != http.StatusAccepted || body["day"] != "2026-10-28" {
		t.Fatalf("repush: %d %v", rec.Code, body)
	}
	_ = h.Drain(context.Background())
	if len(fx.repushed) != 1 || fx.repushed[0] != day {
		t.Errorf("repushed = %v", fx.repushed)
	}
	rec, _ = do(t, h, http.MethodPost, "/v1/exports/2026-10-27/repush", "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("repush without archive: %d", rec.Code)
	}
	rec, _ = do(t, h, http.MethodPost, "/v1/exports/2026-10-01/repush", "")
	if rec.Code != http.StatusNotFound {
		t.Errorf("repush unknown: %d", rec.Code)
	}
	rec, _ = do(t, h, http.MethodPost, "/v1/exports/x/repush", "")
	if rec.Code != http.StatusBadRequest {
		t.Errorf("repush bad day: %d", rec.Code)
	}
}

func TestExportRoutesAbsentWithoutExports(t *testing.T) {
	h := newTestHandler(nil)
	rec, _ := do(t, h, http.MethodGet, "/v1/exports", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	if err := h.Drain(context.Background()); err != nil {
		t.Fatal(err)
	}
}
