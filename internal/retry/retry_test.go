package retry

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type tempErr struct{ temp bool }

func (e tempErr) Error() string   { return "temp" }
func (e tempErr) Temporary() bool { return e.temp }

func TestDoSucceedsAfterFailures(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Policy{Attempts: 3, Backoff: time.Millisecond}, func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("flaky")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestDoGivesUpAfterAttempts(t *testing.T) {
	calls := 0
	base := errors.New("down")
	err := Do(context.Background(), Policy{Attempts: 3, Backoff: time.Millisecond}, func(context.Context) error {
		calls++
		return base
	})
	if calls != 3 {
		t.Fatalf("calls = %d", calls)
	}
	if !errors.Is(err, base) || !strings.Contains(err.Error(), "after 3 attempts") {
		t.Fatalf("err = %v", err)
	}
}

func TestDoSingleAttemptReturnsBareError(t *testing.T) {
	base := errors.New("down")
	err := Do(context.Background(), Policy{Attempts: 0}, func(context.Context) error { return base })
	if err != base { //nolint:errorlint // the bare error is the contract
		t.Fatalf("err = %v", err)
	}
}

func TestDoStopsOnPermanent(t *testing.T) {
	calls := 0
	base := errors.New("bad request")
	err := Do(context.Background(), Policy{Attempts: 5, Backoff: time.Millisecond}, func(context.Context) error {
		calls++
		return Stop(base)
	})
	if calls != 1 || err != base { //nolint:errorlint // unwrapped on purpose
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if Stop(nil) != nil {
		t.Error("Stop(nil) should be nil")
	}
}

func TestDoHonoursTemporary(t *testing.T) {
	calls := 0
	err := Do(context.Background(), Policy{Attempts: 5, Backoff: time.Millisecond}, func(context.Context) error {
		calls++
		return tempErr{temp: false}
	})
	if calls != 1 || !errors.Is(err, tempErr{temp: false}) {
		t.Fatalf("non-temporary: calls=%d err=%v", calls, err)
	}
	calls = 0
	err = Do(context.Background(), Policy{Attempts: 2, Backoff: time.Millisecond}, func(context.Context) error {
		calls++
		return tempErr{temp: true}
	})
	if calls != 2 || err == nil {
		t.Fatalf("temporary: calls=%d err=%v", calls, err)
	}
}

func TestDoStopsWhenContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	err := Do(ctx, Policy{Attempts: 10, Backoff: time.Hour}, func(context.Context) error {
		calls++
		cancel()
		return errors.New("flaky")
	})
	if calls != 1 || !errors.Is(err, context.Canceled) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
	if !strings.Contains(err.Error(), "flaky") {
		t.Errorf("last error not reported: %v", err)
	}

	cancelled, cancel2 := context.WithCancel(context.Background())
	cancel2()
	calls = 0
	err = Do(cancelled, Policy{Attempts: 3}, func(context.Context) error { calls++; return nil })
	if calls != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("already cancelled: calls=%d err=%v", calls, err)
	}
}

func TestBackoffDoublesAndCaps(t *testing.T) {
	var gaps []time.Duration
	var last time.Time
	_ = Do(context.Background(), Policy{Attempts: 5, Backoff: 10 * time.Millisecond, MaxBackoff: 25 * time.Millisecond}, func(context.Context) error {
		now := time.Now()
		if !last.IsZero() {
			gaps = append(gaps, now.Sub(last))
		}
		last = now
		return errors.New("x")
	})
	want := []time.Duration{10, 20, 25, 25}
	if len(gaps) != len(want) {
		t.Fatalf("gaps = %v", gaps)
	}
	for i, g := range gaps {
		lo := want[i] * time.Millisecond
		if g < lo || g > lo*3 {
			t.Errorf("gap %d = %v, want about %v", i, g, lo)
		}
	}
}

func TestJitter(t *testing.T) {
	for i := 0; i < 100; i++ {
		d := jitter(100*time.Millisecond, 0.5)
		if d < 100*time.Millisecond || d > 150*time.Millisecond {
			t.Fatalf("jitter out of range: %v", d)
		}
	}
	if jitter(0, 0.5) != 0 || jitter(time.Second, 0) != time.Second {
		t.Error("jitter with zero inputs")
	}
}
