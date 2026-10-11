package engine

import (
	"context"
	"sync"
	"time"
)

// Limiter caps how fast bytes may be transferred.
//
// It is a token bucket: tokens accrue at the configured rate and a transfer
// takes what it needs, waiting when the bucket is empty. One limiter is meant
// to be shared by every transfer that should count against the same budget, so
// the number the user configures is a ceiling on the total rather than a
// per-connection or per-file figure — a "10 MB/s" setting that each of eight
// connections obeyed separately would allow 80.
//
// A nil *Limiter is valid and means "no limit", which keeps the call sites free
// of nil checks.
type Limiter struct {
	mu sync.Mutex

	// rate is the sustained allowance in bytes per second. Zero disables it.
	rate int64
	// capacity is the largest burst the bucket holds. Tokens are capped here so
	// an idle transfer cannot bank unlimited credit and then spend it all at
	// once.
	capacity float64

	tokens float64
	last   time.Time
}

// minimumBurst is the smallest bucket size. It has to be at least one read
// block, or a transfer waiting for a full block could never be satisfied and
// would block forever at low rate limits.
const minimumBurst = 256 << 10

// NewLimiter returns a limiter for the given rate, or nil for an unlimited one.
func NewLimiter(bytesPerSecond int64) *Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	capacity := float64(bytesPerSecond) / 2 // allow a half-second burst
	if capacity < minimumBurst {
		capacity = minimumBurst
	}
	return &Limiter{
		rate:     bytesPerSecond,
		capacity: capacity,
		// Start full so the first block is not delayed.
		tokens: capacity,
		last:   time.Now(),
	}
}

// Rate reports the configured ceiling in bytes per second, or 0 when unlimited.
func (l *Limiter) Rate() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.rate
}

// Wait blocks until n bytes may be transferred, or ctx is cancelled.
//
// It returns ctx.Err() in that case so a paused transfer stops promptly rather
// than finishing its allowance first.
func (l *Limiter) Wait(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}

	remaining := float64(n)
	for remaining > 0 {
		// Never ask for more than the bucket can hold, or a request larger than
		// the capacity could never be satisfied.
		take := remaining
		if take > l.capacity {
			take = l.capacity
		}

		wait, ok := l.reserve(take)
		if ok {
			remaining -= take
			continue
		}

		// Not enough credit yet: sleep for exactly as long as the shortfall
		// needs and try again.
		if wait <= 0 {
			wait = time.Millisecond
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return nil
}

// reserve takes n tokens if they are available, reporting how long to wait when
// they are not.
func (l *Limiter) reserve(n float64) (time.Duration, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
	l.last = now
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}

	if l.tokens >= n {
		l.tokens -= n
		return 0, true
	}

	shortfall := n - l.tokens
	return time.Duration(shortfall / float64(l.rate) * float64(time.Second)), false
}
