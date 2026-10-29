// Package retry runs an operation again after transient failures, with
// exponential backoff and a little jitter so parallel runs do not retry
// in lockstep.
package retry

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"
)

// Policy bounds the attempts.
type Policy struct {
	// Attempts is the total number of tries, at least 1.
	Attempts int
	// Backoff is the delay before the second attempt; it doubles after
	// each failure and is capped at MaxBackoff.
	Backoff    time.Duration
	MaxBackoff time.Duration
	// Jitter adds up to this fraction of the delay (0.2 = up to 20%).
	Jitter float64
}

// Permanent wraps an error that must not be retried.
type Permanent struct{ Err error }

func (p Permanent) Error() string { return p.Err.Error() }
func (p Permanent) Unwrap() error { return p.Err }

// Stop marks err as not worth retrying.
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return Permanent{Err: err}
}

// temporary is implemented by errors that say whether a retry may help.
type temporary interface{ Temporary() bool }

// Do calls fn until it succeeds, the attempts are used up, it returns a
// Permanent error, or ctx ends. The returned error is the last one, with
// the attempt count in its message when all attempts failed.
func Do(ctx context.Context, p Policy, fn func(ctx context.Context) error) error {
	if p.Attempts < 1 {
		p.Attempts = 1
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = time.Minute
	}
	delay := p.Backoff
	var last error
	for attempt := 1; attempt <= p.Attempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn(ctx)
		if err == nil {
			return nil
		}
		last = err
		var perm Permanent
		if errors.As(err, &perm) {
			return perm.Err
		}
		var tmp temporary
		if errors.As(err, &tmp) && !tmp.Temporary() {
			return err
		}
		if attempt == p.Attempts {
			break
		}
		if err := sleep(ctx, jitter(delay, p.Jitter)); err != nil {
			return errors.Join(err, fmt.Errorf("after %d attempts, last error: %w", attempt, last))
		}
		delay = min(delay*2, p.MaxBackoff)
	}
	if p.Attempts == 1 {
		return last
	}
	return fmt.Errorf("after %d attempts: %w", p.Attempts, last)
}

func jitter(d time.Duration, frac float64) time.Duration {
	if d <= 0 || frac <= 0 {
		return d
	}
	return d + time.Duration(rand.Float64()*frac*float64(d)) // #nosec G404 -- jitter, not security
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
