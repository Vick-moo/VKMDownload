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

// minimumBurst caps the scaled floor below, not the floor itself: a tiny rate
// must not bank a quarter-megabyte burst that makes the limit meaningless. The
// effective floor is burstFloor(rate), which shrinks with the rate.
const minimumBurst = 256 << 10

// burstFloor scales the minimum bucket with the rate: a 1 KiB/s limit with a
// 256 KiB bucket would transfer for minutes before throttling ever engaged.
// The 8 KiB lower bound keeps at least one small block satisfiable so a
// transfer can always make progress.
func burstFloor(rate int64) float64 {
	lo := float64(rate) / 10
	if lo < 8<<10 {
		lo = 8 << 10
	}
	if lo > minimumBurst {
		lo = minimumBurst
	}
	return lo
}

// bucketCapacity is the burst allowance for rate: half a second of traffic,
// but never below the scaled floor.
func bucketCapacity(rate int64) float64 {
	c := float64(rate) / 2
	if f := burstFloor(rate); c < f {
		c = f
	}
	return c
}

// NewLimiter returns a limiter for the given rate, or nil for an unlimited one.
func NewLimiter(bytesPerSecond int64) *Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	capacity := bucketCapacity(bytesPerSecond)
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

// SetRate changes the ceiling live, without replacing the limiter. Running
// transfers share the instance, so they pick the new rate up on their next
// Wait instead of needing a stop/start cycle. A non-positive rate means
// unlimited. The token balance is kept (clamped to the new capacity) rather
// than refilled, so flipping the setting cannot bank a burst.
func (l *Limiter) SetRate(bytesPerSecond int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	l.rate = bytesPerSecond
	l.capacity = bucketCapacity(bytesPerSecond)
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}
}

// cap returns the bucket size under lock: Wait reads it on every block, so a
// SetRate racing a transfer must not tear it.
func (l *Limiter) cap() float64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.capacity
}

// Wait blocks until n bytes may be transferred, or ctx is cancelled.
//
// n is int64 rather than int on purpose: it carries a byte count, and int is
// 32 bits on 32-bit Windows builds, where a single block over 2 GiB (or any
// value that overflowed on the way in) would silently become a small or
// negative request and leave the transfer unthrottled.
//
// It returns ctx.Err() in that case so a paused transfer stops promptly rather
// than finishing its allowance first.
func (l *Limiter) Wait(ctx context.Context, n int64) error {
	if l == nil || n <= 0 {
		return nil
	}

	remaining := float64(n)
	for remaining > 0 {
		// Capping and spending happen under one lock (see takeCapped): reading
		// the capacity and then reserving in two steps would let a concurrent
		// SetRate change the bucket between them.
		take, wait, ok := l.takeCapped(remaining)
		if ok {
			remaining -= take
			continue
		}

		// Not enough credit yet: sleep for exactly as long as the shortfall
		// needs and try again. NewTimer (not time.After) so a cancelled
		// context does not leave the timer hanging until it fires.
		if wait <= 0 {
			wait = time.Millisecond
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return ctx.Err()
		case <-t.C:
		}
	}
	return nil
}

// takeCapped grants up to need tokens under a single lock, capping the grant
// at the bucket size so a request larger than the bucket still completes in
// pieces instead of deadlocking. It reports how long to wait when credit is
// short. Merging the cap read and the spend avoids a SetRate racing a transfer
// between the two.
func (l *Limiter) takeCapped(need float64) (take float64, wait time.Duration, ok bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	// A non-positive rate (see SetRate) means unlimited: grant everything.
	if l.rate <= 0 {
		return need, 0, true
	}

	now := time.Now()
	l.tokens += now.Sub(l.last).Seconds() * float64(l.rate)
	l.last = now
	if l.tokens > l.capacity {
		l.tokens = l.capacity
	}

	take = need
	if take > l.capacity {
		take = l.capacity
	}
	if l.tokens >= take {
		l.tokens -= take
		return take, 0, true
	}

	shortfall := take - l.tokens
	return 0, time.Duration(shortfall / float64(l.rate) * float64(time.Second)), false
}
