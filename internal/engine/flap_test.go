package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vkmdownload/testsrv"
)

// A CDN whose links "flap" answers every request with 404 for a while and then
// serves normally again. The engine used to treat the first 404 as final, so
// one red window killed a task permanently even though the link was fine.

// flapSize is larger than testSize: the outage has to land while the transfer
// is already streaming, which needs enough requests in flight for bytes to
// have been committed by the time the window opens.
const flapSize = 12 << 20

// startFlapServer launches a testsrv of the given size with a 404 window.
func startFlapServer(t *testing.T, size int64, tweak func(*testsrv.Config)) (*testsrv.Server, string) {
	t.Helper()

	cfg := testsrv.DefaultConfig(size)
	cfg.Seed = testSeed
	if tweak != nil {
		tweak(&cfg)
	}

	srv := testsrv.New(cfg)
	base, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start testsrv: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, base + "/file"
}

// TestDownloadRidesOutFlapWindowAfterProgress is the end-to-end shape of the
// bug: the transfer is well under way, the server starts answering 404, and it
// must still finish with the right bytes.
//
// The window opens long after the probe (requests 1 and 2 are the HEAD and the
// ranged probe), so every 404 in it arrives with bytes already on disk — the
// only situation the recovery budget is meant to absorb.
func TestDownloadRidesOutFlapWindowAfterProgress(t *testing.T) {
	srv, url := startFlapServer(t, flapSize, func(c *testsrv.Config) {
		c.FlapAfter = 20
		c.FlapNotFound = 3
	})

	dest := filepath.Join(t.TempDir(), "flappy.bin")

	// Several connections so workers are in flight when the window opens,
	// which is what a real segmented transfer looks like.
	d, err := New(testOptions(8))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Size != flapSize {
		t.Errorf("Result.Size = %d, want %d", res.Size, int64(flapSize))
	}
	if got := srv.NotFound(); got != 3 {
		t.Errorf("the flap window answered %d requests with 404, want 3", got)
	}

	// Byte-for-byte equality, not just the right length: a session that
	// resumed at the wrong offset produces exactly the right size with the
	// wrong contents.
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read %s: %v", dest, err)
	}
	if want := testsrv.Expected(testSeed, flapSize); !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the served stream (%d bytes read, %d expected)", dest, len(got), len(want))
	}
}

// TestDownloadFlapPeriodDoesNotRetryForever covers a window that keeps
// reopening. The engine must run out of its post-progress budget and hand the
// failure up — as a 404 the manager can act on — instead of waiting forever.
//
// This is also the case that pins the counter being independent of the attempt
// budget: the errSegmentIncomplete branch resets `attempt` on every successful
// block, so an oscillating server can never exhaust it. If the 404 budget were
// reset the same way, this download would keep going until the whole resource
// arrived and never return an error.
func TestDownloadFlapPeriodDoesNotRetryForever(t *testing.T) {
	const size = 4 << 20

	srv, url := startFlapServer(t, size, func(c *testsrv.Config) {
		// Requests 1 and 2 are the probe; 3..8 succeed and commit bytes; 9..14
		// are 404; then the cycle repeats.
		c.FlapAfter = 8
		c.FlapNotFound = 6
		c.FlapPeriod = 16
	})

	dest := filepath.Join(t.TempDir(), "oscillating.bin")
	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
		done <- outcome{res, err}
	}()

	var out outcome
	select {
	case out = <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("Download did not return against a permanently flapping server: the 404 budget did not bound the retries")
	}

	if out.err == nil {
		t.Fatalf("Download succeeded (%d bytes) against a window that never closes", out.res.Size)
	}
	if !IsNotFound(out.err) {
		t.Errorf("Download error = %v, want a 404 the manager can recover from", out.err)
	}
	if out.res != nil {
		t.Errorf("a failed Download returned a Result: %+v", out.res)
	}
	if n := len(srv.Requests()); n > 200 {
		t.Errorf("the engine made %d requests before giving up, want a bounded handful", n)
	}
}

// TestFetchSegmentNotFoundBudgetNotResetBySuccess drives fetchSegment directly
// with a server that alternates between serving a block and 404ing, so
// successful blocks land between the 404s.
//
// MaxTries is deliberately huge: the only thing that may stop this loop is the
// post-progress 404 budget. A handful of requests proves the budget counted
// across the successful blocks; a reset would run until the whole segment was
// written and then report success.
func TestFetchSegmentNotFoundBudgetNotResetBySuccess(t *testing.T) {
	const size = 4 << 20

	srv, url := startFlapServer(t, size, func(c *testsrv.Config) {
		// One block served, then 404s, then blocks again: 1,2 ok; 3..6 404;
		// 7,8 ok; 9,10 404.
		c.FlapAfter = 2
		c.FlapNotFound = 4
		c.FlapPeriod = 6
	})

	opt := testOptions(1)
	opt.Backoff.MaxTries = 1000
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	dir := t.TempDir()
	target := filepath.Join(dir, "segment.bin")
	part := target + PartSuffix
	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	s := &session{
		d:      d,
		req:    Request{URL: url},
		probe:  &ProbeResult{FinalURL: url, Size: size, RangeSupported: true},
		target: target,
		part:   part,
		state:  target + StateSuffix,
		file:   f,
		mgr:    newSegmentManager([]*Segment{{Start: 0, End: size - 1}}),
	}
	s.newTracker(0, size)

	err = s.fetchSegment(context.Background(), s.mgr.segs[0])
	if err == nil {
		t.Fatal("fetchSegment succeeded; the 404 budget never bound the retries")
	}
	if !isPostProgressNotFound(err) {
		t.Errorf("fetchSegment error = %v, want a post-progress 404", err)
	}
	if !s.progressed() {
		t.Error("the session reports no progress, but the window only opened after blocks landed")
	}

	// The budget is 5, so with a block served before and between the outages
	// the count is a handful — nowhere near the 1000 attempts allowed.
	if n := len(srv.Requests()); n < 6 || n > 40 {
		t.Errorf("fetchSegment made %d requests, want a bounded handful (the 404 budget, not MaxTries, must stop it)", n)
	}
}

// TestRecoverableNotFoundRequiresEvidence pins the boundary the manager's
// recovery cycle turns on: a 404 is only worth replaying when something proves
// the link worked before.
func TestRecoverableNotFoundRequiresEvidence(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "x.bin")

	bare := fmt.Errorf("probe failed: %w", &statusError{Code: http.StatusNotFound, Detail: "HEAD"})
	if !IsNotFound(bare) {
		t.Fatal("IsNotFound missed a wrapped 404")
	}
	if RecoverableNotFound(bare, target) {
		t.Error("a first-request 404 with no sidecar was treated as recoverable")
	}

	after := fmt.Errorf("segmented download failed: %w",
		&statusError{Code: http.StatusNotFound, Detail: "range", PostProgress: true})
	if !RecoverableNotFound(after, target) {
		t.Error("a 404 after progress was not recoverable")
	}

	// A sidecar holding bytes is the other form of evidence: the link worked
	// long enough to store something, even if this attempt 404'd at the probe.
	// The evidence now also requires the partial file the sidecar describes to
	// actually exist, so plant both.
	part := target + PartSuffix
	if err := os.WriteFile(part, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	st := &State{
		Version:    StateVersion,
		URL:        "http://example.com/f",
		FinalURL:   "http://example.com/f",
		Size:       1 << 20,
		Segments:   []SegState{{Start: 0, End: (1 << 20) - 1, Written: 4096}},
		TempPath:   part,
		TargetPath: target,
	}
	if err := saveState(target+StateSuffix, st); err != nil {
		t.Fatal(err)
	}
	if !RecoverableNotFound(bare, target) {
		t.Error("a 404 with a sidecar holding bytes was not recoverable")
	}
	if !ResumableProgress(target) {
		t.Error("ResumableProgress missed a sidecar with written bytes")
	}
	if ResumableProgress(filepath.Join(dir, "absent.bin")) {
		t.Error("ResumableProgress reported progress for a missing sidecar")
	}

	// A sidecar whose partial file is gone describes a download that no longer
	// exists; it must not be treated as progress.
	if err := os.Remove(part); err != nil {
		t.Fatal(err)
	}
	if ResumableProgress(target) {
		t.Error("ResumableProgress accepted a sidecar with no partial file behind it")
	}

	// A sidecar that names a different target is not this download's progress.
	if err := os.WriteFile(part, make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	st.TargetPath = filepath.Join(dir, "someone-else.bin")
	if err := saveState(target+StateSuffix, st); err != nil {
		t.Fatal(err)
	}
	if ResumableProgress(target) {
		t.Error("ResumableProgress accepted a sidecar naming a different target")
	}

	// A 5xx is never a "the link is gone" case, recoverable or not.
	boom := fmt.Errorf("x: %w", &statusError{Code: http.StatusServiceUnavailable, Detail: "boom"})
	if IsNotFound(boom) {
		t.Error("IsNotFound matched a 503")
	}
	if RecoverableNotFound(boom, target) {
		t.Error("a 503 was treated as a recoverable 404")
	}
}
