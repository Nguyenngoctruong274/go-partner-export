// Package pipeline runs one export of one day: select the rows, build the
// file, archive a copy, sign a link to it, push the file to the partner
// and record every step. It depends on small interfaces so that each
// stage is replaceable in tests and the real wiring lives in exportd.
package pipeline

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/blob"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/postgres"
	"github.com/Nguyenngoctruong274/go-partner-export/internal/retry"
)

// Trips supplies the rows to export.
type Trips interface {
	ListEnded(ctx context.Context, from, to time.Time) ([]export.Trip, error)
}

// Runs keeps the run history.
type Runs interface {
	Save(ctx context.Context, r export.Run) error
	Get(ctx context.Context, day export.Day) (export.Run, error)
}

// Uploader delivers a file to the partner.
type Uploader interface {
	Upload(ctx context.Context, remoteDir, name string, r io.Reader, size int64) (string, error)
}

// Options configures a Pipeline.
type Options struct {
	Naming   export.Naming
	Format   export.Format
	Location *time.Location
	// BlobPrefix is the folder in the archive; RemoteDir the folder on the
	// partner's server.
	BlobPrefix   string
	RemoteDir    string
	SignedURLTTL time.Duration
	// Retry bounds the archive and push stages, which talk to the network.
	Retry retry.Policy
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Observer, when set, is told about each stage; metrics hook in here.
	Observer Observer
}

// Observer sees stage outcomes.
type Observer interface {
	StageDone(stage string, d time.Duration, err error)
	RunDone(r export.Run)
}

// Pipeline exports days.
type Pipeline struct {
	trips    Trips
	runs     Runs
	store    blob.Store
	signer   blob.Signer
	uploader Uploader
	logger   *slog.Logger
	opts     Options
}

// New wires a pipeline.
func New(trips Trips, runs Runs, store blob.Store, signer blob.Signer, uploader Uploader, logger *slog.Logger, opts Options) (*Pipeline, error) {
	if trips == nil || runs == nil || store == nil || signer == nil || uploader == nil {
		return nil, errors.New("pipeline: every dependency is required")
	}
	if err := opts.Naming.Validate(); err != nil {
		return nil, err
	}
	if opts.Location == nil {
		return nil, errors.New("pipeline: location is required")
	}
	if opts.Format.Location == nil {
		opts.Format.Location = opts.Location
	}
	if err := opts.Format.Validate(); err != nil {
		return nil, err
	}
	if opts.SignedURLTTL <= 0 {
		opts.SignedURLTTL = 24 * time.Hour
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Observer == nil {
		opts.Observer = nopObserver{}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Pipeline{trips: trips, runs: runs, store: store, signer: signer, uploader: uploader, logger: logger, opts: opts}, nil
}

// StageError says which stage failed.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return "pipeline: " + e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// Run exports the day. Running a day again starts over with Attempts
// incremented; the archived object and the remote file are replaced, so
// the result converges on the latest data however many times it runs.
func (p *Pipeline) Run(ctx context.Context, day export.Day) (export.Run, error) {
	from, to := day.Window(p.opts.Location)
	run := export.Run{Day: day, From: from, To: to, State: export.StatePending, StartedAt: p.opts.Now(), Attempts: 1}
	if prev, err := p.runs.Get(ctx, day); err == nil {
		run.Attempts = prev.Attempts + 1
	} else if !errors.Is(err, postgres.ErrRunNotFound) {
		return p.fail(ctx, run, export.StageRecord, err)
	}
	run.FileName = p.opts.Naming.FileName(day, 1)
	if err := p.runs.Save(ctx, run); err != nil {
		return run, &StageError{Stage: export.StageRecord, Err: err}
	}
	log := p.logger.With(slog.String("day", day.String()), slog.Int("attempt", run.Attempts), slog.String("file", run.FileName))
	log.Info("export started")

	// select
	trips, err := stage(ctx, p, export.StageSelect, func(ctx context.Context) ([]export.Trip, error) {
		return p.trips.ListEnded(ctx, from, to)
	})
	if err != nil {
		return p.fail(ctx, run, export.StageSelect, err)
	}
	run.Rows = len(trips)

	// build
	var buf bytes.Buffer
	n, err := stage(ctx, p, export.StageBuild, func(context.Context) (int64, error) {
		return p.opts.Format.Write(&buf, trips)
	})
	if err != nil {
		return p.fail(ctx, run, export.StageBuild, err)
	}
	run.Bytes = n
	run.State = export.StateBuilt
	run.Stage = export.StageBuild
	if err := p.runs.Save(ctx, run); err != nil {
		return run, &StageError{Stage: export.StageRecord, Err: err}
	}

	// archive
	key, err := blob.Key(p.opts.BlobPrefix, run.FileName)
	if err != nil {
		return p.fail(ctx, run, export.StageArchive, err)
	}
	data := buf.Bytes()
	_, err = stage(ctx, p, export.StageArchive, func(ctx context.Context) (struct{}, error) {
		return struct{}{}, retry.Do(ctx, p.opts.Retry, func(ctx context.Context) error {
			return p.store.Put(ctx, key, blob.ContentType(run.FileName), bytes.NewReader(data), int64(len(data)))
		})
	})
	if err != nil {
		return p.fail(ctx, run, export.StageArchive, err)
	}
	run.BlobKey = key

	// sign
	link, err := stage(ctx, p, export.StageSign, func(context.Context) (string, error) {
		return p.signer.SignedURL(key, p.opts.SignedURLTTL)
	})
	if err != nil {
		return p.fail(ctx, run, export.StageSign, err)
	}
	run.DownloadURL = link
	run.State = export.StateArchived
	run.Stage = export.StageSign
	if err := p.runs.Save(ctx, run); err != nil {
		return run, &StageError{Stage: export.StageRecord, Err: err}
	}

	// push
	remote, err := p.push(ctx, run.FileName, data)
	if err != nil {
		return p.fail(ctx, run, export.StagePush, err)
	}
	run.RemotePath = remote
	run.State = export.StatePushed
	run.Stage = export.StagePush
	run.Error = ""
	run.FinishedAt = p.opts.Now()
	if err := p.runs.Save(ctx, run); err != nil {
		return run, &StageError{Stage: export.StageRecord, Err: err}
	}
	p.opts.Observer.RunDone(run)
	log.Info("export pushed", slog.Int("rows", run.Rows), slog.Int64("bytes", run.Bytes),
		slog.String("remote", remote), slog.Duration("duration", run.FinishedAt.Sub(run.StartedAt)))
	return run, nil
}

// Repush sends the archived copy of a day again, without rebuilding it.
// It is for the case where the partner lost or rejected the file and the
// data must stay exactly what was archived.
func (p *Pipeline) Repush(ctx context.Context, day export.Day) (export.Run, error) {
	run, err := p.runs.Get(ctx, day)
	if err != nil {
		return export.Run{}, &StageError{Stage: export.StageRecord, Err: err}
	}
	if run.BlobKey == "" {
		return run, &StageError{Stage: export.StagePush, Err: errors.New("day was never archived; run it instead")}
	}
	rc, err := p.store.Open(ctx, run.BlobKey)
	if err != nil {
		return p.fail(ctx, run, export.StageArchive, err)
	}
	data, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		return p.fail(ctx, run, export.StageArchive, err)
	}
	run.Attempts++
	run.StartedAt = p.opts.Now()
	run.FinishedAt = time.Time{}
	remote, err := p.push(ctx, run.FileName, data)
	if err != nil {
		return p.fail(ctx, run, export.StagePush, err)
	}
	run.RemotePath = remote
	run.State = export.StatePushed
	run.Stage = export.StagePush
	run.Error = ""
	run.FinishedAt = p.opts.Now()
	if err := p.runs.Save(ctx, run); err != nil {
		return run, &StageError{Stage: export.StageRecord, Err: err}
	}
	p.opts.Observer.RunDone(run)
	p.logger.Info("export re-pushed", slog.String("day", day.String()), slog.String("remote", remote))
	return run, nil
}

func (p *Pipeline) push(ctx context.Context, name string, data []byte) (string, error) {
	return stage(ctx, p, export.StagePush, func(ctx context.Context) (string, error) {
		var remote string
		err := retry.Do(ctx, p.opts.Retry, func(ctx context.Context) error {
			var err error
			remote, err = p.uploader.Upload(ctx, p.opts.RemoteDir, name, bytes.NewReader(data), int64(len(data)))
			return err
		})
		return remote, err
	})
}

// stage times fn and reports it to the observer.
func stage[T any](ctx context.Context, p *Pipeline, name string, fn func(context.Context) (T, error)) (T, error) {
	start := p.opts.Now()
	v, err := fn(ctx)
	p.opts.Observer.StageDone(name, p.opts.Now().Sub(start), err)
	return v, err
}

// fail records the failure and returns it as a StageError. The record is
// written with a fresh context, so a cancelled run is still written down.
func (p *Pipeline) fail(ctx context.Context, run export.Run, stage string, err error) (export.Run, error) {
	run.State = export.StateFailed
	run.Stage = stage
	run.Error = err.Error()
	run.FinishedAt = p.opts.Now()
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	if saveErr := p.runs.Save(saveCtx, run); saveErr != nil {
		p.logger.Error("record failed run", slog.String("day", run.Day.String()), slog.Any("error", saveErr))
	}
	p.opts.Observer.RunDone(run)
	p.logger.Error("export failed", slog.String("day", run.Day.String()), slog.String("stage", stage), slog.Any("error", err))
	return run, &StageError{Stage: stage, Err: err}
}

type nopObserver struct{}

func (nopObserver) StageDone(string, time.Duration, error) {}
func (nopObserver) RunDone(export.Run)                     {}
