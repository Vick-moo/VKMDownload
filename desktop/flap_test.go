package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vkmdownload/internal/engine"
	"vkmdownload/testsrv"
)

// The manager's session-level recovery: a 404 that arrives after the engine
// already wrote bytes is ridden out by replaying the whole session, not by
// failing the task. These tests drive that loop against the fault-injecting
// test server.

// flapServer starts a testsrv of size bytes with a 404 window.
func flapServer(t *testing.T, size int64, tweak func(*testsrv.Config)) (*testsrv.Server, string) {
	t.Helper()

	cfg := testsrv.DefaultConfig(size)
	cfg.Seed = 11
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

// withRecoveryDelays drives the session-level retry delays at test speed. The
// package does not run tests in parallel, so replacing the package variable is
// safe as long as it is restored.
func withRecoveryDelays(t *testing.T, delays ...time.Duration) {
	t.Helper()
	old := sessionRetryDelays
	sessionRetryDelays = delays
	t.Cleanup(func() { sessionRetryDelays = old })
}

// plantSidecarWithProgress writes a resume sidecar that records bytes already
// written but no longer matches the resource (its length is wrong), so the
// engine ignores it while the manager's recovery check still treats it as
// evidence that the link once worked. The partial file must exist too: the
// engine's ResumableProgress evidence now requires sidecar identity and a
// non-empty .part, mirroring what a real interrupted download leaves behind.
func plantSidecarWithProgress(t *testing.T, target string, written int64) {
	t.Helper()

	if err := os.WriteFile(target+engine.PartSuffix, make([]byte, written), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(&engine.State{
		Version:    engine.StateVersion,
		URL:        "http://example.com/f",
		FinalURL:   "http://example.com/f",
		Size:       1 << 40,
		TargetPath: target,
		TempPath:   target + engine.PartSuffix,
		Segments:   []engine.SegState{{Start: 0, End: written - 1, Written: written}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+engine.StateSuffix, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// startFlapTask adds a named task into dir and starts it. A caller-supplied
// filename keeps the add-time probe out of the picture (its own HEAD would
// otherwise land inside the request-count windows these tests rely on), and
// makes Dest a real directory, so partialPaths resolves to dir/name exactly as
// the engine does.
func startFlapTask(t *testing.T, app *App, url, dir, name string) string {
	t.Helper()

	task, err := app.mgr.Start(StartRequest{
		URL: url, Dest: dir, Filename: name, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return task.ID
}

// flapDest returns a fresh directory and the path a download named `name` will
// be published to inside it.
func flapDest(t *testing.T, name string) (dir, target string) {
	t.Helper()
	dir = t.TempDir()
	return dir, filepath.Join(dir, name)
}

// TestSessionRecoveryReplaysAFlappingDownload is the happy path: the first two
// requests (the probe of the first two sessions) 404, the manager replays the
// session, and the third attempt downloads the whole file byte for byte.
func TestSessionRecoveryReplaysAFlappingDownload(t *testing.T) {
	const size = 512 << 10

	srv, url := flapServer(t, size, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 2
	})
	withRecoveryDelays(t, 10*time.Millisecond, 10*time.Millisecond, 10*time.Millisecond)

	dir, dest := flapDest(t, "out.bin")
	// A link that worked before: partial data proves this is an outage, not a
	// mistyped address.
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "out.bin")

	waitFor(t, "task to finish", 60*time.Second, func() bool {
		v, ok := find(app.mgr, id)
		return ok && (v.Status == StatusCompleted || v.Status == StatusFailed)
	})

	v, _ := find(app.mgr, id)
	if v.Status != StatusCompleted {
		t.Fatalf("task ended as %s: %s", v.Status, v.Error)
	}
	if v.Notice != "" {
		t.Errorf("notice survived completion: %q", v.Notice)
	}
	if v.Error != "" {
		t.Errorf("completed task carries an error: %q", v.Error)
	}
	if srv.NotFound() == 0 {
		t.Error("no 404 was served; the recovery path was never exercised")
	}
	assertFileEquals(t, dest, testsrv.Expected(11, size))
}

// TestMidTransferFlapRecoversAndFinishes covers the real shape of the bug: the
// transfer is streaming, the red window opens once the segment retry budget is
// already past, the engine gives up, and the manager's session replay picks it
// up and finishes.
//
// The resource is deliberately larger than one reserve block per connection
// (the engine claims up to 16 MiB at a time), so requests 3 and 4 land bytes
// and requests 5 and 6 are the first to 404 — every 404 therefore has progress
// behind it.
func TestMidTransferFlapRecoversAndFinishes(t *testing.T) {
	const size = 34 << 20

	srv, url := flapServer(t, size, func(c *testsrv.Config) {
		// Requests 1-2 are the probe, 3-4 are the first 16 MiB of each worker,
		// and everything from 5 on is a permanently open window. It is healed
		// below, once the manager has taken over, so the replay can finish.
		c.FlapAfter = 4
		c.FlapNotFound = 1 << 30
	})
	// Long enough to heal the server before the replay's probe goes out.
	withRecoveryDelays(t, 2*time.Second)

	dir, dest := flapDest(t, "flap.bin")
	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "flap.bin")

	// The engine's segment budget has to run out first, which costs several
	// backoff sleeps. Throughout it the task must stay "downloading": flipping
	// it to failed would flash an error, end a running plan early and drop the
	// goroutine whose context makes Pause instant.
	waitFor(t, "the session-level recovery to be announced", 90*time.Second, func() bool {
		v, ok := find(app.mgr, id)
		if ok && v.Status != StatusDownloading {
			t.Fatalf("task left the downloading state during recovery: %s (%s)", v.Status, v.Error)
		}
		return ok && v.Notice != ""
	})
	if got := srv.NotFound(); got < 6 {
		t.Fatalf("only %d requests were 404'd; the window never exhausted the segment budget", got)
	}

	// Heal the server: the replay's probe and transfer are now normal.
	srv.SetFlap(0, 0, 0)

	waitFor(t, "task to finish", 90*time.Second, func() bool {
		v, ok := find(app.mgr, id)
		return ok && (v.Status == StatusCompleted || v.Status == StatusFailed)
	})

	v, _ := find(app.mgr, id)
	if v.Status != StatusCompleted {
		t.Fatalf("task ended as %s: %s", v.Status, v.Error)
	}
	if v.Notice != "" {
		t.Errorf("notice survived completion: %q", v.Notice)
	}
	assertFileEquals(t, dest, testsrv.Expected(11, size))
}

// TestRecoveryExhaustionReportsAStalledServer pins the terminal message: after
// the whole replay cycle the link is still 404ing, and the user is told what to
// do rather than shown the generic "the link does not exist".
func TestRecoveryExhaustionReportsAStalledServer(t *testing.T) {
	srv, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	withRecoveryDelays(t, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)

	dir, dest := flapDest(t, "stalled.bin")
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "stalled.bin")

	waitFor(t, "task to fail", 30*time.Second, func() bool {
		v, ok := find(app.mgr, id)
		return ok && v.Status == StatusFailed
	})

	v, _ := find(app.mgr, id)
	if !strings.Contains(v.Error, serverStalledMessage) {
		t.Errorf("error = %q, want %q", v.Error, serverStalledMessage)
	}
	if strings.Contains(v.Error, "链接不存在") {
		t.Errorf("error = %q, want the stalled-server wording, not the dead-link wording", v.Error)
	}
	if v.Notice != "" {
		t.Errorf("notice survived failure: %q", v.Notice)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a failed download published a file")
	}
	if srv.NotFound() == 0 {
		t.Error("no 404 was served")
	}
}

// TestFirstRequestNotFoundFailsFast is the other side of the boundary: a link
// that 404s before anything was ever written is a mistyped address (or a spent
// single-use token), and must fail at once instead of waiting out a recovery
// cycle.
func TestFirstRequestNotFoundFailsFast(t *testing.T) {
	srv, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	// A recovery cycle, if it were entered, would stall for this long.
	withRecoveryDelays(t, 10*time.Second)

	dir, dest := flapDest(t, "missing.bin")
	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "missing.bin")

	start := time.Now()
	waitFor(t, "task to fail", 20*time.Second, func() bool {
		v, ok := find(app.mgr, id)
		return ok && v.Status == StatusFailed
	})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a first-request 404 took %s to fail; it entered the recovery cycle", elapsed)
	}

	v, _ := find(app.mgr, id)
	if !strings.Contains(v.Error, "链接不存在") {
		t.Errorf("error = %q, want the dead-link wording", v.Error)
	}
	if strings.Contains(v.Error, serverStalledMessage) {
		t.Errorf("error = %q, want a plain 404, not the stalled-server wording", v.Error)
	}
	if n := len(srv.Requests()); n > 4 {
		t.Errorf("made %d requests before failing, want only the probe", n)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a failed download published a file")
	}
}

// TestPauseDuringRecoveryWaitStopsImmediately checks that the wait between
// sessions is interruptible: Pause must not have to wait out the delay, and no
// request may go out afterwards.
func TestPauseDuringRecoveryWaitStopsImmediately(t *testing.T) {
	srv, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	withRecoveryDelays(t, time.Hour)

	dir, dest := flapDest(t, "pause.bin")
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "pause.bin")

	waitFor(t, "the recovery wait to be announced", 30*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusDownloading && v.Notice != ""
	})
	before := len(srv.Requests())

	if err := app.mgr.Pause(id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	// The notice is cleared by the run goroutine on its way out, so waiting
	// for that proves the hour-long sleep was interrupted rather than timed
	// out.
	waitFor(t, "the paused task to settle", 10*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusPaused && v.Notice == ""
	})

	time.Sleep(300 * time.Millisecond)
	if n := len(srv.Requests()); n != before {
		t.Errorf("the server saw %d more requests after the pause", n-before)
	}
}

// TestCancelDuringRecoveryWaitDiscardsPartials covers Cancel during the same
// wait: the task ends canceled and the working set is removed.
func TestCancelDuringRecoveryWaitDiscardsPartials(t *testing.T) {
	_, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	withRecoveryDelays(t, time.Hour)

	dir, dest := flapDest(t, "cancel.bin")
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "cancel.bin")

	waitFor(t, "the recovery wait to be announced", 30*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusDownloading && v.Notice != ""
	})

	if err := app.mgr.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, "the canceled task to settle", 10*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusCanceled && v.Notice == ""
	})
	waitFor(t, "the partial data to be discarded", 10*time.Second, func() bool {
		for _, suffix := range []string{engine.PartSuffix, engine.StateSuffix} {
			if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
				return false
			}
		}
		return true
	})
}

// TestUpdateFreezeDuringRecoveryWaitDoesNotRestart pins the update path: while
// an update is being applied, the end of a recovery wait must not start
// another transfer. The task parks as paused, exactly as it would if the update
// had paused it before the swap.
func TestUpdateFreezeDuringRecoveryWaitDoesNotRestart(t *testing.T) {
	srv, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	withRecoveryDelays(t, 400*time.Millisecond)

	dir, dest := flapDest(t, "freeze.bin")
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "freeze.bin")

	waitFor(t, "the recovery wait to be announced", 30*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusDownloading && v.Notice != ""
	})

	app.mgr.mu.Lock()
	app.mgr.updating = true
	app.mgr.mu.Unlock()
	t.Cleanup(func() {
		app.mgr.mu.Lock()
		app.mgr.updating = false
		app.mgr.mu.Unlock()
	})
	before := len(srv.Requests())

	waitFor(t, "the frozen task to park as paused", 10*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusPaused && v.Notice == ""
	})

	time.Sleep(300 * time.Millisecond)
	if n := len(srv.Requests()); n != before {
		t.Errorf("the frozen task restarted a transfer (%d new requests)", n-before)
	}
}
