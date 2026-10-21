package main

import (
	"sync"
	"testing"
	"time"
)

// TestRaiseWindowWithoutATarget covers the "nothing to raise" contract: the
// test binary owns no visible top-level window, so raiseWindow must answer
// false rather than panic or invent a handle. Off Windows the platform stub
// answers false for the same reason, which is why this test carries no build
// tag.
func TestRaiseWindowWithoutATarget(t *testing.T) {
	// Warm the path twice: the first call creates the EnumWindows callback on
	// Windows, and a crash there would take the app down on its first download.
	for i := 0; i < 2; i++ {
		if got := raiseWindow(); got {
			t.Fatalf("raiseWindow() = true, want false: the test process owns no visible top-level window")
		}
	}
}

// TestHoldTopmostReleasesAndRespectsANewerHold covers the pin bookkeeping
// behind BringToFront: a single call pins now and releases after the hold, and
// two overlapping calls produce pin, pin, unpin — not pin, pin, unpin, unpin,
// which would drop the window behind the browser while the second dialog is
// still open.
func TestHoldTopmostReleasesAndRespectsANewerHold(t *testing.T) {
	var mu sync.Mutex
	var events []bool
	record := func(onTop bool) {
		mu.Lock()
		events = append(events, onTop)
		mu.Unlock()
	}
	snapshot := func() []bool {
		mu.Lock()
		defer mu.Unlock()
		return append([]bool(nil), events...)
	}

	// One call: pinned synchronously, released once the hold is over.
	holdTopmost(20*time.Millisecond, record)
	if got := snapshot(); len(got) != 1 || !got[0] {
		t.Fatalf("immediately after holdTopmost: events = %v, want [true]", got)
	}
	time.Sleep(80 * time.Millisecond)
	if got := snapshot(); len(got) != 2 || got[1] {
		t.Fatalf("after the hold: events = %v, want [true false]", got)
	}

	// Two overlapping calls: the older timer must be skipped, so the pin the
	// newer call took survives until the newer hold expires.
	mu.Lock()
	events = nil
	mu.Unlock()
	holdTopmost(50*time.Millisecond, record)
	time.Sleep(10 * time.Millisecond)
	holdTopmost(150*time.Millisecond, record)
	// Wait past both timers: the old one would have appended a second release.
	time.Sleep(300 * time.Millisecond)
	if got := snapshot(); len(got) != 3 || !got[0] || !got[1] || got[2] {
		t.Fatalf("overlapping holds: events = %v, want [true true false]", got)
	}
}
