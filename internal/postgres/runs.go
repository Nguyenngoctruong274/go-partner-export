package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

// ErrRunNotFound is returned when no run exists for a day.
var ErrRunNotFound = errors.New("export run not found")

// RunStore keeps the run history.
type RunStore struct {
	db *sql.DB
}

// NewRunStore returns a store over db.
func NewRunStore(db *sql.DB) *RunStore {
	return &RunStore{db: db}
}

const runColumns = `report_day, from_time, to_time, state, stage, error, row_count, byte_count,
	file_name, blob_key, download_url, remote_path, attempts, started_at, finished_at, created_at, updated_at`

// Save writes the run for its day, replacing an earlier record of the
// same day. The pipeline calls it after every stage, so the row always
// shows how far the current attempt got.
func (s *RunStore) Save(ctx context.Context, r export.Run) error {
	if !r.State.Valid() {
		return fmt.Errorf("save run %s: invalid state %q", r.Day, r.State)
	}
	var finished sql.NullTime
	if !r.FinishedAt.IsZero() {
		finished = sql.NullTime{Time: r.FinishedAt, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO export_runs (`+runColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, now(), now())
		ON CONFLICT (report_day) DO UPDATE SET
			from_time = EXCLUDED.from_time, to_time = EXCLUDED.to_time,
			state = EXCLUDED.state, stage = EXCLUDED.stage, error = EXCLUDED.error,
			row_count = EXCLUDED.row_count, byte_count = EXCLUDED.byte_count,
			file_name = EXCLUDED.file_name, blob_key = EXCLUDED.blob_key,
			download_url = EXCLUDED.download_url, remote_path = EXCLUDED.remote_path,
			attempts = EXCLUDED.attempts, started_at = EXCLUDED.started_at,
			finished_at = EXCLUDED.finished_at, updated_at = now()`,
		dayValue(r.Day), r.From, r.To, string(r.State), r.Stage, r.Error, r.Rows, r.Bytes,
		r.FileName, r.BlobKey, r.DownloadURL, r.RemotePath, r.Attempts, r.StartedAt, finished)
	if err != nil {
		return fmt.Errorf("save run %s: %w", r.Day, err)
	}
	return nil
}

// Get returns the run of a day, or ErrRunNotFound.
func (s *RunStore) Get(ctx context.Context, day export.Day) (export.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM export_runs WHERE report_day = $1`, dayValue(day))
	r, err := scanRun(row)
	if errors.Is(err, sql.ErrNoRows) {
		return export.Run{}, ErrRunNotFound
	}
	if err != nil {
		return export.Run{}, fmt.Errorf("get run %s: %w", day, err)
	}
	return r, nil
}

// ListFilter narrows List. Zero values mean no filter.
type ListFilter struct {
	State export.State
	// Before returns only days earlier than this one, for paging.
	Before export.Day
	Limit  int
}

// List returns runs newest day first.
func (s *RunStore) List(ctx context.Context, f ListFilter) ([]export.Run, error) {
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	var before any
	if !f.Before.IsZero() {
		before = dayValue(f.Before)
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+runColumns+` FROM export_runs
		WHERE ($1 = '' OR state = $1) AND ($2::date IS NULL OR report_day < $2)
		ORDER BY report_day DESC
		LIMIT $3`, string(f.State), before, f.Limit)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	var out []export.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("list runs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	return out, nil
}

type scanner interface {
	Scan(dest ...any) error
}

func scanRun(sc scanner) (export.Run, error) {
	var (
		r        export.Run
		day      time.Time
		state    string
		finished sql.NullTime
	)
	if err := sc.Scan(&day, &r.From, &r.To, &state, &r.Stage, &r.Error, &r.Rows, &r.Bytes,
		&r.FileName, &r.BlobKey, &r.DownloadURL, &r.RemotePath, &r.Attempts, &r.StartedAt, &finished,
		&r.CreatedAt, &r.UpdatedAt); err != nil {
		return export.Run{}, err
	}
	r.Day = export.DayOf(day, time.UTC)
	r.State = export.State(state)
	if finished.Valid {
		r.FinishedAt = finished.Time
	}
	return r, nil
}

// dayValue passes a Day as a DATE. lib/pq sends time.Time as a timestamp
// with zone; a UTC midnight maps onto the same calendar date.
func dayValue(d export.Day) time.Time {
	return time.Date(d.Year, d.Month, d.Date, 0, 0, 0, 0, time.UTC)
}
