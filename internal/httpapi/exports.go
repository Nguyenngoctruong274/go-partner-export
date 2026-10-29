package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
)

// Exports is what the /v1/exports endpoints need: the pipeline and the
// run history.
type Exports interface {
	Run(ctx context.Context, day export.Day) (export.Run, error)
	Repush(ctx context.Context, day export.Day) (export.Run, error)
	Get(ctx context.Context, day export.Day) (export.Run, error)
	List(ctx context.Context, state export.State, before export.Day, limit int) ([]export.Run, error)
}

// ExportOptions configures the export endpoints.
type ExportOptions struct {
	Exports Exports
	// Location is the zone the default day (yesterday) is computed in.
	Location *time.Location
	// RunTimeout bounds a run started over HTTP; 0 means 30 minutes.
	RunTimeout time.Duration
	Now        func() time.Time
}

// exportRunner starts runs in the background and keeps one per day in
// flight. Runs started over HTTP outlive the request, so shutdown waits
// for them through Drain.
type exportRunner struct {
	exports Exports
	loc     *time.Location
	timeout time.Duration
	now     func() time.Time
	logger  *slog.Logger

	mu      sync.Mutex
	running map[export.Day]bool
	// idle is closed while nothing is in flight and replaced when a run
	// starts, so Drain can wait on it without racing a new start.
	idle chan struct{}
}

func newExportRunner(logger *slog.Logger, opts ExportOptions) *exportRunner {
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = 30 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Location == nil {
		opts.Location = time.UTC
	}
	idle := make(chan struct{})
	close(idle)
	return &exportRunner{
		exports: opts.Exports, loc: opts.Location, timeout: opts.RunTimeout, now: opts.Now,
		logger: logger, running: map[export.Day]bool{}, idle: idle,
	}
}

// errBusy means a run for the day is already in flight.
var errBusy = errors.New("a run for this day is in progress")

// launch starts fn for day unless one is in flight.
func (r *exportRunner) launch(day export.Day, fn func(ctx context.Context) (export.Run, error)) error {
	r.mu.Lock()
	if r.running[day] {
		r.mu.Unlock()
		return errBusy
	}
	if len(r.running) == 0 {
		r.idle = make(chan struct{})
	}
	r.running[day] = true
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.running, day)
			if len(r.running) == 0 {
				close(r.idle)
			}
			r.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), r.timeout)
		defer cancel()
		if _, err := fn(ctx); err != nil {
			r.logger.Error("export started over http failed", slog.String("day", day.String()), slog.Any("error", err))
		}
	}()
	return nil
}

func (r *exportRunner) isRunning(day export.Day) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.running[day]
}

// Drain waits for in-flight runs or until ctx ends.
func (r *exportRunner) Drain(ctx context.Context) error {
	r.mu.Lock()
	idle := r.idle
	r.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a *api) routeExports(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/exports", a.startExport)
	mux.HandleFunc("GET /v1/exports", a.listExports)
	mux.HandleFunc("GET /v1/exports/{day}", a.getExport)
	mux.HandleFunc("POST /v1/exports/{day}/repush", a.repushExport)
}

// runJSON is the wire form of a run.
type runJSON struct {
	Day         string  `json:"day"`
	From        string  `json:"from"`
	To          string  `json:"to"`
	State       string  `json:"state"`
	Running     bool    `json:"running"`
	Stage       string  `json:"stage,omitempty"`
	Error       string  `json:"error,omitempty"`
	Rows        int     `json:"rows"`
	Bytes       int64   `json:"bytes"`
	FileName    string  `json:"file_name,omitempty"`
	BlobKey     string  `json:"blob_key,omitempty"`
	DownloadURL string  `json:"download_url,omitempty"`
	RemotePath  string  `json:"remote_path,omitempty"`
	Attempts    int     `json:"attempts"`
	StartedAt   string  `json:"started_at"`
	FinishedAt  *string `json:"finished_at"`
}

func (a *api) toJSON(r export.Run) runJSON {
	out := runJSON{
		Day: r.Day.String(), From: r.From.Format(time.RFC3339), To: r.To.Format(time.RFC3339),
		State: string(r.State), Running: a.exports.isRunning(r.Day), Stage: r.Stage, Error: r.Error,
		Rows: r.Rows, Bytes: r.Bytes, FileName: r.FileName, BlobKey: r.BlobKey, DownloadURL: r.DownloadURL,
		RemotePath: r.RemotePath, Attempts: r.Attempts, StartedAt: r.StartedAt.Format(time.RFC3339),
	}
	if !r.FinishedAt.IsZero() {
		s := r.FinishedAt.Format(time.RFC3339)
		out.FinishedAt = &s
	}
	return out
}

// startExport runs a day in the background. The body is optional:
// {"day":"YYYY-MM-DD"}; the default is yesterday in the report zone.
// 202 with the day; 409 while a run for it is in flight.
func (a *api) startExport(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Day string `json:"day"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<10)).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "body must be JSON like {\"day\":\"2026-10-28\"}")
			return
		}
	}
	day := export.ReportDay(a.exports.now(), a.exports.loc)
	if body.Day != "" {
		d, err := export.ParseDay(body.Day)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		day = d
	}
	if today := export.DayOf(a.exports.now(), a.exports.loc); !day.Before(today) {
		writeError(w, http.StatusUnprocessableEntity, "day must be before today in the report zone: it is not over yet")
		return
	}
	if err := a.exports.launch(day, func(ctx context.Context) (export.Run, error) {
		return a.exports.exports.Run(ctx, day)
	}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"day": day.String(), "state": string(export.StatePending)})
}

func (a *api) repushExport(w http.ResponseWriter, r *http.Request) {
	day, err := export.ParseDay(r.PathValue("day"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := a.exports.exports.Get(r.Context(), day)
	if errors.Is(err, postgres.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "no run for this day")
		return
	}
	if err != nil {
		a.logger.ErrorContext(r.Context(), "get run", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "cannot read run")
		return
	}
	if run.BlobKey == "" {
		writeError(w, http.StatusUnprocessableEntity, "day was never archived; start a run instead")
		return
	}
	if err := a.exports.launch(day, func(ctx context.Context) (export.Run, error) {
		return a.exports.exports.Repush(ctx, day)
	}); err != nil {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"day": day.String(), "state": string(run.State)})
}

func (a *api) getExport(w http.ResponseWriter, r *http.Request) {
	day, err := export.ParseDay(r.PathValue("day"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	run, err := a.exports.exports.Get(r.Context(), day)
	if errors.Is(err, postgres.ErrRunNotFound) {
		writeError(w, http.StatusNotFound, "no run for this day")
		return
	}
	if err != nil {
		a.logger.ErrorContext(r.Context(), "get run", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "cannot read run")
		return
	}
	writeJSON(w, http.StatusOK, a.toJSON(run))
}

// listExports returns runs newest first. ?state= filters, ?before=YYYY-MM-DD
// pages, ?limit= caps (default 50, max 500).
func (a *api) listExports(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := export.State(q.Get("state"))
	if state != "" && !state.Valid() {
		writeError(w, http.StatusBadRequest, "unknown state")
		return
	}
	var before export.Day
	if s := q.Get("before"); s != "" {
		d, err := export.ParseDay(s)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		before = d
	}
	limit := 50
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "limit must be 1-500")
			return
		}
		limit = n
	}
	runs, err := a.exports.exports.List(r.Context(), state, before, limit)
	if err != nil {
		a.logger.ErrorContext(r.Context(), "list runs", slog.Any("error", err))
		writeError(w, http.StatusInternalServerError, "cannot list runs")
		return
	}
	out := make([]runJSON, 0, len(runs))
	for _, run := range runs {
		out = append(out, a.toJSON(run))
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": out})
}
