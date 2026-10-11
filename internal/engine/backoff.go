package engine

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff describes exponential backoff with jitter for retryable failures.
//
// Jitter matters more than it looks: when a server drops every connection at
// the same moment, all workers fail together, and without jitter they would
// also retry together forever, in lockstep. Spreading the retries out lets the
// server recover between attempts.
type Backoff struct {
	// Base is the delay before the first retry.
	Base time.Duration
	// Max caps the delay.
	Max time.Duration
	// Factor multiplies the delay after each attempt. Values below 1 are
	// treated as 2.
	Factor float64
	// Jitter is the symmetric fraction applied to the nominal delay, 0..1.
	Jitter float64
	// MaxTries bounds attempts per segment.
	MaxTries int
}

// DefaultBackoff returns 500ms doubling to 30s, with 25% jitter and 10 tries.
func DefaultBackoff() Backoff {
	return Backoff{
		Base:     500 * time.Millisecond,
		Max:      30 * time.Second,
		Factor:   2,
		Jitter:   0.25,
		MaxTries: 10,
	}
}

// normalized returns b with invalid fields replaced by defaults.
func (b Backoff) normalized() Backoff {
	if b.Base <= 0 {
		b.Base = 500 * time.Millisecond
	}
	if b.Max <= 0 {
		b.Max = 30 * time.Second
	}
	if b.Factor < 1 {
		b.Factor = 2
	}
	if b.Jitter < 0 {
		b.Jitter = 0
	}
	if b.Jitter > 1 {
		b.Jitter = 1
	}
	if b.MaxTries <= 0 {
		b.MaxTries = 10
	}
	return b
}

// Delay returns how long to wait before the given zero-based retry attempt.
func (b Backoff) Delay(attempt int) time.Duration {
	b = b.normalized()
	if attempt < 0 {
		attempt = 0
	}

	d := float64(b.Base)
	for i := 0; i < attempt; i++ {
		d *= b.Factor
		if d >= float64(b.Max) {
			d = float64(b.Max)
			break
		}
	}
	if d > float64(b.Max) {
		d = float64(b.Max)
	}

	// Symmetric jitter keeps the average delay equal to the nominal delay,
	// so jitter cannot accidentally turn into unbounded retry storms.
	if b.Jitter > 0 {
		d *= 1 - b.Jitter + rand.Float64()*2*b.Jitter
	}
	if d < 0 {
		d = 0
	}
	return time.Duration(d)
}

// Sleep waits for the delay before the given attempt, or until ctx is done.
func (b Backoff) Sleep(ctx context.Context, attempt int) error {
	d := b.Delay(attempt)
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
