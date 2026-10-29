// Package scheduler triggers the daily export at a local wall-clock time.
// It is deliberately small: a Kubernetes CronJob with `exportd --once` is
// the other way to run the pipeline, and both call the same Runner.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

// Runner exports one day.
type Runner interface {
	Run(ctx context.Context, day export.Day) (export.Run, error)
}

// Options configures a Scheduler.
type Options struct {
	// Location, Hour and Minute define the daily trigger in local time.
	Location     *time.Location
	Hour, Minute int
	// RunTimeout bounds one export; 0 means 30 minutes.
	RunTimeout time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Next overrides the trigger computation (tests). nil derives it from
	// Location, Hour and Minute.
	Next func(now time.Time) time.Time
}

// Scheduler runs the Runner for the previous local day at each trigger.
type Scheduler struct {
	runner Runner
	logger *slog.Logger
	opts   Options
}

// New validates opts.
func New(runner Runner, logger *slog.Logger, opts Options) (*Scheduler, error) {
	if runner == nil {
		return nil, errors.New("scheduler: runner is required")
	}
	if opts.Location == nil {
		return nil, errors.New("scheduler: location is required")
	}
	if opts.Hour < 0 || opts.Hour > 23 || opts.Minute < 0 || opts.Minute > 59 {
		return nil, errors.New("scheduler: hour must be 0-23 and minute 0-59")
	}
	if opts.RunTimeout <= 0 {
		opts.RunTimeout = 30 * time.Minute
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Next == nil {
		loc, h, m := opts.Location, opts.Hour, opts.Minute
		opts.Next = func(now time.Time) time.Time { return export.NextRunAt(now, loc, h, m) }
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Scheduler{runner: runner, logger: logger, opts: opts}, nil
}

// Run blocks until ctx ends, exporting the previous local day at every
// trigger. A failed export is logged and the next trigger still fires: the
// run history holds the failure and an operator can re-run the day.
func (s *Scheduler) Run(ctx context.Context) error {
	for {
		now := s.opts.Now()
		next := s.opts.Next(now)
		s.logger.Info("next export scheduled", slog.Time("at", next), slog.Duration("in", next.Sub(now)))
		timer := time.NewTimer(next.Sub(now))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		s.runOnce(ctx)
	}
}

func (s *Scheduler) runOnce(ctx context.Context) {
	day := export.ReportDay(s.opts.Now(), s.opts.Location)
	runCtx, cancel := context.WithTimeout(ctx, s.opts.RunTimeout)
	defer cancel()
	if _, err := s.runner.Run(runCtx, day); err != nil {
		s.logger.Error("scheduled export failed", slog.String("day", day.String()), slog.Any("error", err))
	}
}
