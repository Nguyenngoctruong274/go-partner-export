package scheduler

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

type recorder struct {
	mu   sync.Mutex
	days []export.Day
	errs int
	fail bool
	done chan struct{}
}

func (r *recorder) Run(ctx context.Context, day export.Day) (export.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.days = append(r.days, day)
	if _, ok := ctx.Deadline(); !ok {
		panic("run without a deadline")
	}
	select {
	case r.done <- struct{}{}:
	default:
	}
	if r.fail {
		r.errs++
		return export.Run{}, errors.New("boom")
	}
	return export.Run{Day: day, State: export.StatePushed}, nil
}

var hcm = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Ho_Chi_Minh")
	if err != nil {
		panic(err)
	}
	return loc
}()

func TestSchedulerFiresAndKeepsGoingAfterFailures(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 10), fail: true}
	// Pretend it is 00:30 on 2026-10-29 in Ho Chi Minh; the trigger comes
	// 5ms later each time.
	now := time.Date(2026, 10, 29, 0, 30, 0, 0, hcm)
	s, err := New(rec, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{
		Location: hcm, Hour: 1, Minute: 0,
		Now:  func() time.Time { return now },
		Next: func(now time.Time) time.Time { return now.Add(5 * time.Millisecond) },
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()

	for i := 0; i < 3; i++ {
		select {
		case <-rec.done:
		case <-time.After(5 * time.Second):
			t.Fatalf("trigger %d did not fire", i+1)
		}
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run returned %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.days) < 3 || rec.errs < 3 {
		t.Fatalf("days=%v errs=%d", rec.days, rec.errs)
	}
	for _, d := range rec.days {
		if d != (export.Day{Year: 2026, Month: time.October, Date: 28}) {
			t.Errorf("exported %v, want the previous local day", d)
		}
	}
}

func TestSchedulerStopsWhileWaiting(t *testing.T) {
	rec := &recorder{done: make(chan struct{}, 1)}
	s, err := New(rec, nil, Options{Location: hcm, Hour: 1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() { errc <- s.Run(ctx) }()
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
	if len(rec.days) != 0 {
		t.Errorf("ran %v before the trigger", rec.days)
	}
}

func TestNewValidatesAndDefaults(t *testing.T) {
	rec := &recorder{}
	if _, err := New(nil, nil, Options{Location: hcm}); err == nil {
		t.Error("nil runner accepted")
	}
	if _, err := New(rec, nil, Options{}); err == nil {
		t.Error("nil location accepted")
	}
	if _, err := New(rec, nil, Options{Location: hcm, Hour: 24}); err == nil {
		t.Error("hour 24 accepted")
	}
	if _, err := New(rec, nil, Options{Location: hcm, Minute: 60}); err == nil {
		t.Error("minute 60 accepted")
	}
	s, err := New(rec, nil, Options{Location: hcm, Hour: 1, Minute: 30})
	if err != nil {
		t.Fatal(err)
	}
	if s.opts.RunTimeout != 30*time.Minute || s.opts.Now == nil {
		t.Errorf("defaults = %+v", s.opts)
	}
	at := time.Date(2026, 10, 29, 0, 0, 0, 0, hcm)
	if next := s.opts.Next(at); !next.Equal(time.Date(2026, 10, 29, 1, 30, 0, 0, hcm)) {
		t.Errorf("Next = %v", next)
	}
}
