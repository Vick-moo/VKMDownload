package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestLimiterPacesTransfers checks the core promise: bytes come out at roughly
// the configured rate once the initial burst is spent.
func TestLimiterPacesTransfers(t *testing.T) {
	const rate = 200 << 10 // 200 KiB/s

	lim := NewLimiter(rate)
	if lim == nil {
		t.Fatal("NewLimiter returned nil for a positive rate")
	}

	ctx := context.Background()

	// The bucket starts full, so this is free and establishes a zero balance.
	if err := lim.Wait(ctx, int(minimumBurst)); err != nil {
		t.Fatalf("draining the burst: %v", err)
	}

	start := time.Now()
	if err := lim.Wait(ctx, rate); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	elapsed := time.Since(start)

	// One second's worth of tokens has to be earned. Allow generous slack: the
	// point is that it waits, not that it waits to the millisecond.
	if elapsed < 700*time.Millisecond {
		t.Errorf("waiting for %d bytes at %d B/s took %v; it did not throttle", rate, rate, elapsed)
	}
	if elapsed > 3*time.Second {
		t.Errorf("waiting for one second of credit took %v, far too long", elapsed)
	}
}

// TestLimiterWaitHonoursContext checks that a throttled transfer stops when it
// is cancelled rather than serving out its allowance first.
func TestLimiterWaitHonoursContext(t *testing.T) {
	lim := NewLimiter(1 << 10) // 1 KiB/s: any real request takes a long time
	ctx := context.Background()

	if err := lim.Wait(ctx, int(minimumBurst)); err != nil {
		t.Fatalf("draining the burst: %v", err)
	}

	timed, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := lim.Wait(timed, 100<<10)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Wait error = %v, want DeadlineExceeded", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("Wait kept waiting after the context expired")
	}
}

// TestLimiterHandlesRequestsLargerThanTheBucket guards the batching in Wait: a
// request bigger than the bucket must still complete rather than deadlock.
func TestLimiterHandlesRequestsLargerThanTheBucket(t *testing.T) {
	lim := NewLimiter(64 << 20) // high rate so the test stays quick
	ctx := context.Background()

	// Four times the bucket.
	n := int(lim.capacity * 4)

	done := make(chan error, 1)
	go func() { done <- lim.Wait(ctx, n) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Wait: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Wait deadlocked on a request larger than the bucket")
	}
}

// TestNilLimiterIsUnlimited pins the nil-means-no-limit contract the callers
// rely on, which is what keeps them free of nil checks.
func TestNilLimiterIsUnlimited(t *testing.T) {
	var lim *Limiter

	start := time.Now()
	if err := lim.Wait(context.Background(), 1<<30); err != nil {
		t.Errorf("nil limiter returned %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("nil limiter blocked for %v", elapsed)
	}
	if got := lim.Rate(); got != 0 {
		t.Errorf("Rate() on a nil limiter = %d, want 0", got)
	}
	if NewLimiter(0) != nil {
		t.Error("NewLimiter(0) should mean unlimited, so it must return nil")
	}
	if NewLimiter(-1) != nil {
		t.Error("NewLimiter(-1) should mean unlimited, so it must return nil")
	}
}

// TestLimiterSetRateAppliesLive pins in-place retuning: running transfers
// share the instance, so a new ceiling must reach them without a restart.
func TestLimiterSetRateAppliesLive(t *testing.T) {
	lim := NewLimiter(1 << 10) // 1 KiB/s: anything real takes ages
	if lim == nil {
		t.Fatal("NewLimiter returned nil for a positive rate")
	}
	ctx := context.Background()

	if err := lim.Wait(ctx, int(minimumBurst)); err != nil {
		t.Fatalf("draining the burst: %v", err)
	}

	// Open the tap wide: the same Wait that would have taken minutes must
	// now finish at once, on the same instance.
	lim.SetRate(100 << 20)
	if got := lim.Rate(); got != 100<<20 {
		t.Errorf("Rate = %d, want %d", got, 100<<20)
	}
	start := time.Now()
	if err := lim.Wait(ctx, 1<<20); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait after SetRate took %v; the new rate did not apply", elapsed)
	}

	// Zero means unlimited, also live and on the same instance.
	lim.SetRate(0)
	if got := lim.Rate(); got != 0 {
		t.Errorf("Rate = %d, want 0", got)
	}
	if err := lim.Wait(ctx, 10<<20); err != nil {
		t.Fatalf("unlimited Wait: %v", err)
	}

	var nilLim *Limiter
	nilLim.SetRate(1 << 10) // must not panic
	if err := nilLim.Wait(ctx, 1); err != nil {
		t.Errorf("nil limiter Wait: %v", err)
	}
}
