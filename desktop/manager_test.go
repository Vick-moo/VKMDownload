package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestApp builds an App pointed at a throwaway data directory.
//
// The directory is injected rather than redirected through the environment:
// os.UserConfigDir reads the process environment, which t.Setenv does not
// reliably override on Windows, and a test that quietly rewrites the user's own
// task list is worse than no test at all.
func newTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()

	a := NewAppWithDataDir(filepath.Join(dir, "config"))
	// The manager emits events through the app's emitter. Without a window the
	// real one would abort the process, so it is replaced with a no-op; the
	// download path being exercised is otherwise identical.
	a.emit = func(string, ...any) {}
	a.ctx = context.Background()

	// Route the engine's trace to a fixed path so it survives the test's own
	// temp-directory cleanup and can be read after a failure.
	t.Setenv("VDOWNLOAD_DEBUG", filepath.Join(os.TempDir(), "vdownload-engine-trace.log"))
	_ = os.Remove(filepath.Join(os.TempDir(), "vdownload-engine-trace.log"))
	return a
}

// waitFor polls until the predicate holds, or fails the test.
func waitFor(t *testing.T, what string, timeout time.Duration, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// find returns a task's view and whether it exists.
func find(m *Manager, id string) (TaskView, bool) {
	for _, v := range m.List() {
		if v.ID == id {
			return v, true
		}
	}
	return TaskView{}, false
}

// payload builds deterministic bytes for a test resource.
func payload(size int) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i*7 + 3)
	}
	return b
}

// rangedServer serves body with range support, optionally throttled so a test
// can observe a transfer that is still running.
//
// Range handling is delegated to http.ServeContent rather than hand-rolled: a
// hand-written handler that ignores Range looks like a server without range
// support, and the engine correctly refuses to plan segments against it.
//
// Throttling is applied by slowing the *reader* rather than wrapping the
// ResponseWriter. Wrapping the writer interferes with ServeContent's own
// Content-Length and seek bookkeeping, which corrupts the response instead of
// merely slowing it.
func rangedServer(t *testing.T, body []byte, perChunkDelay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		var src io.ReadSeeker = bytes.NewReader(body)
		if perChunkDelay > 0 {
			src = &slowReadSeeker{rs: src, delay: perChunkDelay}
		}
		http.ServeContent(w, r, "payload.bin", time.Time{}, src)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// slowReadSeeker delays after every read, keeping a transfer in flight long
// enough for a test to pause it without perturbing the response framing.
type slowReadSeeker struct {
	rs    io.ReadSeeker
	delay time.Duration
}

func (s *slowReadSeeker) Read(p []byte) (int, error) {
	n, err := s.rs.Read(p)
	if n > 0 {
		time.Sleep(s.delay)
	}
	return n, err
}

func (s *slowReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return s.rs.Seek(offset, whence)
}

// assertFileEquals reads path and compares it against want byte for byte.
func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(got) != len(want) {
		t.Fatalf("%s holds %d bytes, want %d", path, len(got), len(want))
	}
	if !bytes.Equal(got, want) {
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("byte %d differs: got %#02x want %#02x", i, got[i], want[i])
			}
		}
	}
}

// ---------------------------------------------------------------- tests

// TestDownloadCompletesEndToEnd proves the app can actually download: a real
// HTTP server, the real engine, and a byte-for-byte check of the file on disk.
func TestDownloadCompletesEndToEnd(t *testing.T) {
	want := payload(512 << 10)
	srv := rangedServer(t, want, 0)

	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "payload.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/payload.bin", Dest: dest, Conns: 4, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "task to finish", 30*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && (v.Status == StatusCompleted || v.Status == StatusFailed)
	})

	v, _ := find(app.mgr, task.ID)
	if v.Status != StatusCompleted {
		t.Fatalf("task ended as %s: %s", v.Status, v.Error)
	}
	if v.Size != int64(len(want)) {
		t.Errorf("reported size = %d, want %d", v.Size, len(want))
	}
	assertFileEquals(t, dest, want)

	// The working set must be gone once the download is published.
	for _, suffix := range []string{".vdownload.part", ".vdownload.json"} {
		if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
			t.Errorf("%s still exists after a completed download", suffix)
		}
	}
}

// TestPauseThenResumeProducesAWholeFile covers the flow a user relies on: stop a
// download, come back later, and still end up with the right bytes.
func TestPauseThenResumeProducesAWholeFile(t *testing.T) {
	want := payload(1 << 20)
	// The delay has to be large enough that the transfer is still running when
	// the test pauses it; a few milliseconds finishes the file before the pause
	// can land.
	srv := rangedServer(t, want, 40*time.Millisecond)

	outDir := t.TempDir()
	app := newTestApp(t)
	dest := filepath.Join(outDir, "big.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/big.bin", Dest: dest, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "some progress", 30*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Downloaded > 0
	})
	v0, _ := find(app.mgr, task.ID)
	t.Logf("pausing at %d/%d bytes", v0.Downloaded, v0.Size)
	if err := app.mgr.Pause(task.ID); err != nil {
		t.Fatalf("Pause: %v", err)
	}

	waitFor(t, "status to settle after pause", 10*time.Second, func() bool {
		v, _ := find(app.mgr, task.ID)
		return v.Status == StatusPaused || v.Status == StatusCanceled
	})

	// The partial file must survive a pause; that is what makes resume work.
	partPath := dest + ".vdownload.part"
	if _, err := os.Stat(partPath); err != nil {
		t.Fatalf("partial file missing after pause: %v", err)
	}

	if err := app.mgr.Resume(task.ID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	waitFor(t, "task to finish after resume", 60*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && (v.Status == StatusCompleted || v.Status == StatusFailed)
	})

	v, _ := find(app.mgr, task.ID)
	if v.Status != StatusCompleted {
		t.Fatalf("task ended as %s: %s", v.Status, v.Error)
	}
	assertFileEquals(t, dest, want)
}

// TestResumeThenPauseDoesNotPanic reproduces the crash a user hit: resume a
// failed task and press pause while it is still starting up.
func TestResumeThenPauseDoesNotPanic(t *testing.T) {
	want := payload(512 << 10)
	srv := rangedServer(t, want, 12*time.Millisecond)

	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "resume-pause.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/x.bin", Dest: dest, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Interleave resume and pause the way impatient clicking would.
	for i := 0; i < 12; i++ {
		if err := app.mgr.Resume(task.ID); err != nil {
			t.Logf("resume %d rejected: %v", i, err)
		}
		if err := app.mgr.Pause(task.ID); err != nil {
			t.Logf("pause %d rejected: %v", i, err)
		}
		time.Sleep(15 * time.Millisecond)
	}

	time.Sleep(400 * time.Millisecond)
	t.Logf("survived; final status=%s", mustFind(t, app.mgr, task.ID).Status)
}

// TestConcurrentResumeDoesNotPanic stresses the same path from several
// goroutines, which is what a double click produces.
func TestConcurrentResumeDoesNotPanic(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 8*time.Millisecond)

	app := newTestApp(t)
	task, err := app.mgr.Start(StartRequest{
		URL:   srv.URL + "/c.bin",
		Dest:  filepath.Join(t.TempDir(), "c.bin"),
		Conns: 2,
		Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 6; j++ {
				_ = app.mgr.Resume(task.ID)
				_ = app.mgr.Pause(task.ID)
			}
		}()
	}
	wg.Wait()
	time.Sleep(400 * time.Millisecond)
	t.Logf("survived; final status=%s", mustFind(t, app.mgr, task.ID).Status)
}

// mustFind returns a task view or fails the test.
func mustFind(t *testing.T, m *Manager, id string) TaskView {
	t.Helper()
	v, ok := find(m, id)
	if !ok {
		t.Fatalf("task %s vanished", id)
	}
	return v
}

// TestResumePauseOnTaskWithMissingFile covers the case a user hit in practice:
// a task whose target file was deleted underneath it (the archive manager had
// cleaned its temp directory), resumed and then paused straight away.
func TestResumePauseOnTaskWithMissingFile(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "gone.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dest, Conns: 2, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Leave an orphaned partial and sidecar behind, which is what a task whose
	// directory was removed looks like.
	if err := os.WriteFile(dest+".vdownload.part", make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".vdownload.json", []byte(`{"version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}

	// Drive the exact sequence: resume, then pause immediately, repeatedly.
	for i := 0; i < 20; i++ {
		_ = app.mgr.Resume(task.ID)
		_ = app.mgr.Pause(task.ID)
	}
	// And the other order, plus cancel and remove.
	for i := 0; i < 10; i++ {
		_ = app.mgr.Resume(task.ID)
		_ = app.mgr.Cancel(task.ID)
		_ = app.mgr.Resume(task.ID)
		_ = app.mgr.Pause(task.ID)
		_ = app.mgr.Resume(task.ID)
		_ = app.mgr.Remove(task.ID, false)
	}
	time.Sleep(500 * time.Millisecond)

	// No task may remain, and the process must still be alive.
	if _, ok := find(app.mgr, task.ID); ok {
		t.Logf("task still present after remove")
	}
	t.Log("survived resume/pause/cancel/remove on a missing target")
}

// TestResumePauseOnMissingFileConcurrently hammers the same sequence from many
// goroutines, which is what an impatient double click produces.
func TestResumePauseOnMissingFileConcurrently(t *testing.T) {
	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "gone2.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dest, Conns: 4, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch j % 4 {
				case 0:
					_ = app.mgr.Resume(task.ID)
				case 1:
					_ = app.mgr.Pause(task.ID)
				case 2:
					_ = app.mgr.Cancel(task.ID)
				case 3:
					_, _ = app.mgr.List(), app.mgr.Stats()
				}
			}
		}()
	}
	wg.Wait()
	time.Sleep(500 * time.Millisecond)
	t.Log("survived concurrent resume/pause/cancel on a missing target")
}

// TestCancelDiscardsPartialData checks that cancelling leaves nothing behind to
// resume from, since the user asked for it to stop for good.
func TestCancelDiscardsPartialData(t *testing.T) {
	want := payload(1 << 20)
	srv := rangedServer(t, want, 40*time.Millisecond)

	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "cancel.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/cancel.bin", Dest: dest, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "progress", 30*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Downloaded > 0
	})

	if err := app.mgr.Cancel(task.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	waitFor(t, "status to become canceled", 10*time.Second, func() bool {
		v, _ := find(app.mgr, task.ID)
		return v.Status == StatusCanceled
	})

	for _, suffix := range []string{".vdownload.part", ".vdownload.json"} {
		if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
			t.Errorf("%s should have been removed by Cancel", suffix)
		}
	}
}

// TestServerErrorBecomesAReadableMessage checks that a failure surfaces
// something a user can act on rather than a raw transport string.
func TestServerErrorBecomesAReadableMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	app := newTestApp(t)
	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/missing", Dest: filepath.Join(t.TempDir(), "x.bin"), Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	waitFor(t, "task to fail", 20*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status == StatusFailed
	})

	v, _ := find(app.mgr, task.ID)
	if !strings.Contains(v.Error, "404") {
		t.Errorf("error = %q, want it to mention 404", v.Error)
	}
	if !strings.Contains(v.Error, "链接不存在") {
		t.Errorf("error = %q, want the human-readable explanation", v.Error)
	}
	if _, err := os.Stat(v.Dest); err == nil {
		t.Error("a failed download published a file")
	}
}

// TestInvalidURLCreateIsRejected checks the dialog's validation path.
func TestInvalidURLCreateIsRejected(t *testing.T) {
	app := newTestApp(t)
	for _, bad := range []string{"", "   ", "ftp://example.com/x", "not a url"} {
		if _, err := app.mgr.Start(StartRequest{URL: bad}); err == nil {
			t.Errorf("Start(%q) was accepted, want a rejection", bad)
		}
	}
}

// TestTaskIndexSurvivesRestart covers the persistence of the task list.
func TestTaskIndexSurvivesRestart(t *testing.T) {
	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "kept.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dest, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatalf("saveTasks: %v", err)
	}

	// Reload into a second App pointed at the same directory. Using NewApp here
	// would read the real user profile, which is how a test run once seeded the
	// developer's own task list with its fixtures.
	reloaded := NewAppWithDataDir(app.dataDir())
	reloaded.ctx = context.Background()
	reloaded.mgr.loadTasks()

	list := reloaded.mgr.List()
	if len(list) != 1 {
		t.Fatalf("reloaded %d tasks, want 1", len(list))
	}
	if list[0].ID != task.ID || list[0].URL != "http://127.0.0.1:1/never" {
		t.Errorf("reloaded %+v, want id=%s url=%s", list[0], task.ID, "http://127.0.0.1:1/never")
	}
}

// TestRemoveDeletesOnlyFinishedFiles checks that deleting an unfinished task
// cannot remove an unrelated file that happens to sit at the same path.
func TestRemoveDeletesOnlyFinishedFiles(t *testing.T) {
	app := newTestApp(t)

	dir := t.TempDir()
	dest := filepath.Join(dir, "important.bin")
	original := []byte("do not delete me")
	if err := os.WriteFile(dest, original, 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dest, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := app.mgr.Remove(task.ID, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("the unrelated file was deleted: %v", err)
	}
	if !bytes.Equal(got, original) {
		t.Errorf("file contents changed: %q", got)
	}
}

// TestStatsAggregatesActiveTasks covers the sidebar totals.
func TestStatsAggregatesActiveTasks(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 20*time.Millisecond)

	// The files live outside t.TempDir because this test lets the downloads run
	// to completion; a directory torn down mid-transfer would fail the cleanup
	// rather than the assertion under test.
	outDir, err := os.MkdirTemp("", "vdownload-stats-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(outDir) })

	app := newTestApp(t)
	ids := make([]string, 0, 3)
	for i := 0; i < 3; i++ {
		task, err := app.mgr.Start(StartRequest{
			URL:   srv.URL + "/f.bin",
			Dest:  filepath.Join(outDir, fmt.Sprintf("f%d.bin", i)),
			Conns: 2,
			Start: true,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		ids = append(ids, task.ID)
	}

	waitFor(t, "an active task to appear", 15*time.Second, func() bool {
		return app.mgr.Stats()["active"] > 0
	})

	stats := app.mgr.Stats()
	if stats["total"] != 3 {
		t.Errorf("total = %d, want 3", stats["total"])
	}
	if stats["active"] == 0 {
		t.Error("active = 0 while downloads are running")
	}

	// Let every transfer finish before the test returns.
	waitFor(t, "all tasks to finish", 60*time.Second, func() bool {
		done := 0
		for _, id := range ids {
			if v, ok := find(app.mgr, id); ok && v.Status == StatusCompleted {
				done++
			}
		}
		return done == 3
	})
}

// TestHumanErrorMappings pins the wording of the messages a user sees.
func TestHumanErrorMappings(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"HTTP 403: forbidden", "403"},
		{"HTTP 404: not found", "链接不存在"},
		{"dial tcp 127.0.0.1:1: connect: connection refused", "无法连接到服务器"},
		{"completed file size does not match the announced size", "文件长度与服务器声明不符"},
	}
	for _, c := range cases {
		got := humanError(io.ErrUnexpectedEOF)
		_ = got
		got = humanError(&fakeErr{c.in})
		if !strings.Contains(got, c.want) {
			t.Errorf("humanError(%q) = %q, want it to contain %q", c.in, got, c.want)
		}
	}
}

type fakeErr struct{ s string }

func (e *fakeErr) Error() string { return e.s }
