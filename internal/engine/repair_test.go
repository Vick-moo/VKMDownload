package engine

// Regression tests for the repair batch: accounting, unknown-length resume,
// mandatory final size check, write-error classification, sidecar watermark,
// limiter floor, debug handle, chunked reads and the NoRange guard.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A partial block commit must leave the rest of the claim outstanding, which in
// turn must keep the dynamic splitter away from the segment. Settling the whole
// reservation on the first write (the old bug) zeroes the claim mid-block and
// lets a split cut across an in-flight fetch.
func TestCommitPartialKeepsClaimOutstanding(t *testing.T) {
	m := newSegmentManager([]*Segment{{Start: 0, End: (1 << 20) - 1}})
	seg := m.segs[0]

	off, n, ok := m.reserve(seg, 64<<10)
	if !ok || off != 0 || n != 64<<10 {
		t.Fatalf("reserve = (%d,%d,%v), want (0,65536,true)", off, n, ok)
	}

	// First write of the block: only half of it reaches the disk.
	const wn = int64(32 << 10)
	m.commit(seg, wn, off+wn)

	if got := seg.outstanding(); got != n-wn {
		t.Fatalf("outstanding = %d, want %d after a partial commit", got, n-wn)
	}
	// The claim must still fence the split: the tail may only take bytes past
	// the claim's end, never the claimed-but-unwritten run itself.
	if !m.splitLargest(1 << 10) {
		t.Fatal("splitLargest refused to split past a partially-settled claim")
	}
	if tail := m.segs[1]; tail.Start < n {
		t.Errorf("tail starts at %d, which overlaps the outstanding claim [%d,%d)",
			tail.Start, seg.Next(), n)
	}
}

// Unknown lengths must never enter the segmented path: an empty plan
// completes vacuously and would publish a false success.
func TestRunSegmentedRejectsUnknownLength(t *testing.T) {
	d, err := New(testOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dir := t.TempDir()
	mkSession := func(size int64) *session {
		target := filepath.Join(dir, "out.bin")
		return &session{
			d:      d,
			req:    Request{URL: "http://example.com/f"},
			probe:  &ProbeResult{Size: size, RangeSupported: true, FinalURL: "http://example.com/f"},
			target: target,
			part:   target + PartSuffix,
			state:  target + StateSuffix,
		}
	}

	if _, err := mkSession(-1).runSegmented(context.Background()); err == nil {
		t.Error("runSegmented succeeded with Size<0; want an error")
	} else if got := err.Error(); !contains(got, "known length") {
		t.Errorf("runSegmented error = %q, want it to mention the known length", got)
	}

	// The only empty plan that may succeed is the empty resource.
	empty := mkSession(0)
	res, err := empty.runSegmented(context.Background())
	if err != nil {
		t.Fatalf("runSegmented with Size==0: %v", err)
	}
	if empty.file != nil {
		// runSegmented leaves the partial file open for publish; the test
		// owns cleanup here (Windows cannot delete an open file).
		_ = empty.file.Close()
		empty.file = nil
	}
	if res.Size != 0 {
		t.Errorf("Result.Size = %d, want 0", res.Size)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

// The finished length is checked against the announced size even when
// VerifySize is off; the flag only gates stronger checks.
func TestPublishEnforcesKnownSizeWithoutVerify(t *testing.T) {
	opt := testOptions(1)
	opt.VerifySize = false
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	mkSession := func(t *testing.T, body []byte) *session {
		t.Helper()
		target := filepath.Join(t.TempDir(), "out.bin")
		part := target + PartSuffix
		if err := os.WriteFile(part, body, 0o644); err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(part, os.O_RDWR, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return &session{
			d:      d,
			probe:  &ProbeResult{Size: 100},
			target: target,
			part:   part,
			state:  target + StateSuffix,
			file:   f,
			mgr:    newSegmentManager([]*Segment{{Start: 0, End: 99}}),
		}
	}

	short := mkSession(t, make([]byte, 5))
	if _, err := short.publish(&Result{}); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("publish with 5/100 bytes, VerifySize=false: err = %v, want ErrSizeMismatch", err)
	}

	full := mkSession(t, make([]byte, 100))
	if _, err := full.publish(&Result{}); err != nil {
		t.Errorf("publish with 100/100 bytes: %v", err)
	}
}

// Local write failures must not be retried: a full disk will still be full in
// thirty seconds.
func TestIsRetryableRejectsWriteErrors(t *testing.T) {
	pathErr := &os.PathError{Op: "write", Path: "x", Err: errors.New("denied")}
	sysErr := &os.SyscallError{Syscall: "write", Err: errors.New("no space left")}
	short := fmt.Errorf("short write at offset 0: %w", io.ErrShortWrite)

	for name, err := range map[string]error{
		"PathError":         pathErr,
		"wrapped PathError": fmt.Errorf("segment failed: %w", pathErr),
		"SyscallError":      sysErr,
		"wrapped Syscall":   fmt.Errorf("segment failed: %w", sysErr),
		"short write":       short,
		"real OS PathError": realPathError(t),
	} {
		if isRetryable(err) {
			t.Errorf("isRetryable(%s) = true, want false", name)
		}
	}

	// Controls: transient faults still retry.
	if !isRetryable(&statusError{Code: http.StatusServiceUnavailable}) {
		t.Error("isRetryable(503) = false, want true")
	}
	if !isRetryable(io.ErrUnexpectedEOF) {
		t.Error("isRetryable(UnexpectedEOF) = false, want true")
	}
}

// realPathError produces a genuine *os.PathError from the OS (a missing
// parent directory), standing in for "disk full" without needing a full disk.
func realPathError(t *testing.T) error {
	t.Helper()
	_, err := os.OpenFile(filepath.Join(t.TempDir(), "no-such-dir", "f"), os.O_CREATE|os.O_WRONLY, 0o644)
	if err == nil {
		t.Fatal("write into a missing directory unexpectedly succeeded")
	}
	return err
}

// Only a successful sidecar flush may advance the save watermark; failures are
// counted and logged instead of being mistaken for durability.
func TestSidecarWatermarkAdvancesOnlyOnSaveSuccess(t *testing.T) {
	iv := atomic.Int64{}
	iv.Store(int64(time.Hour))
	calls := 0
	p := &progressTracker{
		total:        1000,
		interval:     &iv,
		step:         1,
		saveInterval: time.Hour, // only the byte trigger fires
		saveStep:     10,
		start:        time.Now(),
		lastEmit:     time.Now(),
		lastSave:     time.Now(),
		report:       func(Progress) {},
		save: func() error {
			calls++
			if calls == 1 {
				return errors.New("disk full")
			}
			return nil
		},
	}

	p.add(10) // first flush fails
	p.mu.Lock()
	mark, fails := p.lastSaveMark, p.saveFails
	p.mu.Unlock()
	if mark != 0 {
		t.Errorf("lastSaveMark = %d after a failed save, want 0", mark)
	}
	if fails != 1 {
		t.Errorf("saveFails = %d, want 1", fails)
	}

	p.add(10) // second flush succeeds
	p.mu.Lock()
	mark, fails = p.lastSaveMark, p.saveFails
	p.mu.Unlock()
	if mark != 20 {
		t.Errorf("lastSaveMark = %d after a successful save, want 20", mark)
	}
	if fails != 1 {
		t.Errorf("saveFails = %d, want it to stay 1", fails)
	}
}

// A tiny rate must still throttle: the bucket floor scales with the rate
// instead of letting every small limit start with a 256 KiB free burst.
func TestLimiterSmallRateBucketScaled(t *testing.T) {
	small := NewLimiter(4 << 10)
	if small == nil {
		t.Fatal("NewLimiter returned nil for a positive rate")
	}
	if got := small.cap(); got != 8<<10 {
		t.Errorf("capacity at 4 KiB/s = %v, want 8 KiB", got)
	}

	big := NewLimiter(2 << 20)
	if got := big.cap(); got != 1<<20 {
		t.Errorf("capacity at 2 MiB/s = %v, want 1 MiB (half-second burst)", got)
	}

	// Prove the small bucket actually bites: after draining it, one second of
	// traffic must be earned.
	ctx := context.Background()
	if err := small.Wait(ctx, int(small.cap())); err != nil {
		t.Fatalf("draining the burst: %v", err)
	}
	start := time.Now()
	if err := small.Wait(ctx, 4<<10); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Errorf("4 KiB at 4 KiB/s took %v; the small limit did not throttle", elapsed)
	} else if elapsed > 10*time.Second {
		t.Errorf("4 KiB at 4 KiB/s took %v, far too long", elapsed)
	}

	// A retuned small rate rescales the bucket as well.
	small.SetRate(8 << 10)
	if got := small.cap(); got != 8<<10 {
		t.Errorf("capacity after SetRate(8 KiB/s) = %v, want 8 KiB", got)
	}
}

// Concurrent SetRate and Wait must not race (run under -race): the cap read
// and the token spend happen under one lock.
func TestLimiterConcurrentSetRateAndWait(t *testing.T) {
	lim := NewLimiter(1 << 20)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for ctx.Err() == nil {
				lim.SetRate(int64(1+i) << 20)
				_ = lim.Wait(ctx, 1024)
			}
		}(i)
	}
	wg.Wait()
}

// Close must release the shared debug handle so open/close cycles do not leak
// descriptors; the next debugf reopens it on demand.
func TestCloseReleasesDebugFile(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "dbg.log")
	t.Setenv("VKMDOWNLOAD_DEBUG", dest)

	debugf("hello %d", 1)
	debugMu.Lock()
	open := debugFile != nil
	debugMu.Unlock()
	if !open {
		t.Fatal("debugf did not open the debug file")
	}

	d, err := New(Options{}.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	d.Close()

	debugMu.Lock()
	stillOpen := debugFile != nil
	debugMu.Unlock()
	if stillOpen {
		t.Error("Close did not release the debug handle")
	}

	debugf("after close")
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	if len(got) == 0 {
		t.Error("debug log is empty; the reopen after Close did not work")
	}
	// The reopen above leaves the handle open; release it so TempDir cleanup
	// (which cannot delete an open file on Windows) succeeds.
	d.Close()
}

// A small ChunkSize must still produce exact bytes: the segmented reader
// slices its takes by min(ChunkSize, buffer, remaining).
func TestSmallChunkSizeDownloadIsByteExact(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	opt := testOptions(4)
	opt.ChunkSize = 4 << 10 // far below the 64 KiB reserve: many reads per block
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// A non-range segment must never be validated by the segmented fetcher; the
// old dead branch silently accepted it.
func TestValidateRangeResponseRejectsNoRange(t *testing.T) {
	s := &session{}
	seg := &Segment{Start: 0, End: 99, NoRange: true}
	resp := &http.Response{StatusCode: http.StatusOK}
	if err := s.validateRangeResponse(resp, seg, 0, 100); err == nil {
		t.Error("validateRangeResponse accepted a non-range segment in the segmented fetcher")
	}
}
