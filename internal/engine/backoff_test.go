package engine

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBackoffDelayIsCapped pins the growth curve and its ceiling: the delay
// doubles from Base, never exceeds Max, and keeps working for attempt numbers
// far beyond any real retry budget.
func TestBackoffDelayIsCapped(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Factor: 2}
	want := []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, time.Second}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i, got, w)
		}
	}
	if got := b.Delay(10_000); got != time.Second {
		t.Errorf("Delay(10000) = %v, want the %v cap", got, time.Second)
	}
	if got := b.Delay(-3); got != want[0] {
		t.Errorf("Delay(-3) = %v, want the first attempt's %v", got, want[0])
	}
}

// TestBackoffDefaults pins the documented default schedule, which the audit
// used to compute the old worst-case wait times.
func TestBackoffDefaults(t *testing.T) {
	d := DefaultBackoff()
	if d.Base != 500*time.Millisecond || d.Max != 30*time.Second || d.Factor != 2 || d.Jitter != 0.25 || d.MaxTries != 10 {
		t.Errorf("DefaultBackoff = %+v", d)
	}
}

// TestBackoffNormalizesInvalid pins that junk values are replaced rather than
// trusted: a zero Base or Max would otherwise make every retry immediate.
func TestBackoffNormalizesInvalid(t *testing.T) {
	b := Backoff{Base: 0, Max: -1, Factor: 0.5, Jitter: 2, MaxTries: 0}.normalized()
	if b.Base != 500*time.Millisecond || b.Max != 30*time.Second || b.Factor != 2 || b.Jitter != 1 || b.MaxTries != 10 {
		t.Errorf("normalized = %+v", b)
	}
}

// TestBackoffJitterStaysInBand keeps the jitter symmetric and bounded: with a
// 50% jitter every delay must fall within half to one-and-a-half of nominal,
// so a retry storm cannot be created by a bad jitter value.
func TestBackoffJitterStaysInBand(t *testing.T) {
	b := Backoff{Base: time.Second, Max: time.Second, Factor: 2, Jitter: 0.5}
	for i := 0; i < 200; i++ {
		d := b.Delay(0)
		if d < 500*time.Millisecond || d > 1500*time.Millisecond {
			t.Fatalf("Delay(0) = %v, outside 0.5s..1.5s", d)
		}
	}
}

// TestBackoffSleepHonoursContext proves a cancelled context cuts the sleep
// short; without it a pause would wait out the full backoff.
func TestBackoffSleepHonoursContext(t *testing.T) {
	b := Backoff{Base: 10 * time.Second, Max: 10 * time.Second, Factor: 2}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := b.Sleep(ctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep error = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Sleep waited %v after cancellation", elapsed)
	}
}

// TestBackoffSleepCancelledWithZeroDelay documents the zero-delay path: it
// reports the context state rather than pretending to have slept.
func TestBackoffSleepCancelledWithZeroDelay(t *testing.T) {
	b := Backoff{Base: time.Nanosecond, Max: time.Nanosecond, Factor: 2, Jitter: 1}
	// A jittered nanosecond delay truncates to zero, which is the only way into
	// the no-sleep branch; a live context must then report nil, a cancelled one
	// its error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Sleep(ctx, 0); err == nil {
		t.Fatal("Sleep with a cancelled context and no delay returned nil")
	}
}
