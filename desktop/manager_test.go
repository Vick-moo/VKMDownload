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

	"vkmdownload/internal/engine"
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
	t.Setenv("VKMDOWNLOAD_DEBUG", filepath.Join(os.TempDir(), "vkmdownload-engine-trace.log"))
	_ = os.Remove(filepath.Join(os.TempDir(), "vkmdownload-engine-trace.log"))
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
	for _, suffix := range []string{".vkmdownload.part", ".vkmdownload.json"} {
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
	partPath := dest + ".vkmdownload.part"
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

	waitFor(t, "the transfer to settle", 15*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status != StatusDownloading && v.Status != StatusQueued
	})
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
	waitFor(t, "the transfer to settle", 15*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status != StatusDownloading && v.Status != StatusQueued
	})
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
	if err := os.WriteFile(dest+".vkmdownload.part", make([]byte, 4096), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest+".vkmdownload.json", []byte(`{"version":1}`), 0o644); err != nil {
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
	// Removal drops the record synchronously; the wait below is for the
	// detached transfer goroutine to exit.
	waitFor(t, "transfers to settle", 15*time.Second, func() bool {
		for _, v := range app.mgr.List() {
			if v.Status == StatusDownloading || v.Status == StatusQueued {
				return false
			}
		}
		return true
	})

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
	waitFor(t, "transfers to settle", 15*time.Second, func() bool {
		for _, v := range app.mgr.List() {
			if v.Status == StatusDownloading || v.Status == StatusQueued {
				return false
			}
		}
		return true
	})
	t.Log("survived concurrent resume/pause/cancel on a missing target")
}

// TestTaskWithoutFilenameIsNotNamedAfterItsFolder covers the label a user saw
// on every task created without typing a filename: the destination is still the
// download directory at that point, so filepath.Base(dest) read "Downloads" and
// the row was named after the folder it was going into.
func TestTaskWithoutFilenameIsNotNamedAfterItsFolder(t *testing.T) {
	app := newTestApp(t)
	dir := filepath.Join(t.TempDir(), "MyDownloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dir, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if task.Dest != dir {
		t.Errorf("dest = %q, want the directory %q", task.Dest, dir)
	}
	if task.Name == filepath.Base(dir) {
		t.Errorf("task is named after its destination folder (%q); the name is unknown until the server answers", task.Name)
	}
	if task.Name != "" {
		t.Errorf("name = %q, want empty until the server supplies one", task.Name)
	}

	// An explicit filename is still honoured.
	named, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dir, Filename: "chosen.bin", Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if named.Name != "chosen.bin" {
		t.Errorf("name = %q, want chosen.bin", named.Name)
	}
	if named.Dest != filepath.Join(dir, "chosen.bin") {
		t.Errorf("dest = %q, want the file inside the directory", named.Dest)
	}
}

// TestBeforeCloseAllowsClosing pins the return contract of the close hook.
//
// Wails reads the value as `prevent`, so true refuses the close. Written the
// other way round — which it was — every close was refused and the window could
// not be shut at all.
func TestBeforeCloseAllowsClosing(t *testing.T) {
	app := newTestApp(t)
	app.ctx = context.Background()

	setTray := func(on bool) {
		app.mu.Lock()
		cfg := app.cfg
		cfg.MinimizeToTray = on
		app.cfg = cfg
		app.mu.Unlock()
	}

	// Minimise-to-tray on: the close always becomes a hide, because the tray
	// icon (with 显示窗口/退出) brings the window back. Real exits go through
	// the tray's 退出 item. The hide call is spied (not executed): the real
	// runtime.WindowHide aborts the process on a foreign context.
	hidden := false
	app.hide = func(context.Context) { hidden = true }
	setTray(true)
	if !app.beforeClose(context.Background()) {
		t.Error("close went through with minimise-to-tray on; there would be no way back except the tray")
	}
	if !hidden {
		t.Error("minimise-to-tray on but the window was never hidden")
	}

	// Minimise-to-tray off: never refuse.
	setTray(false)
	if app.beforeClose(context.Background()) {
		t.Error("close refused with minimise-to-tray disabled")
	}
}

// TestBeforeCloseQuitIntentLetsTrayExitThrough pins the tray-quit fix.
//
// Wails Windows Frontend.Quit consults OnBeforeClose first, so a tray 退出
// that calls runtime.Quit while MinimizeToTray is on used to be turned into
// a hide: the icon was already gone (systray.Quit ran first) and the process
// lingered headless. requestQuit must make the next beforeClose allow the
// quit through.
func TestBeforeCloseQuitIntentLetsTrayExitThrough(t *testing.T) {
	app := newTestApp(t)
	app.ctx = context.Background()
	app.hide = func(context.Context) {}

	app.mu.Lock()
	cfg := app.cfg
	cfg.MinimizeToTray = true
	app.cfg = cfg
	app.mu.Unlock()

	if !app.beforeClose(context.Background()) {
		t.Fatal("precondition broken: close should hide while minimise-to-tray is on")
	}
	app.requestQuit()
	if app.beforeClose(context.Background()) {
		t.Error("close refused after requestQuit; tray 退出 would linger headless")
	}
}

// TestNewAppEmitterDelegatesToRuntime guards the emitter against the regression
// that took the app down with a stack overflow: NewApp once assigned a closure
// whose body called the very field it was assigning, so the first event
// recursed until the stack ran out.
//
// The emitter cannot be exercised in a test — runtime.EventsEmit calls
// os.Exit(1) on a context that did not come from the Wails lifecycle hooks, and
// every other test replaces the emitter, which is exactly why this slipped
// through. So the guard reads the source instead: the closure NewApp installs
// must reach the runtime and must not call the field it is assigning.
func TestNewAppEmitterDelegatesToRuntime(t *testing.T) {
	src, err := os.ReadFile("app.go")
	if err != nil {
		t.Fatalf("reading app.go: %v", err)
	}

	body := functionBody(string(src), "func NewApp() *App {")
	if body == "" {
		t.Fatal("could not find NewApp in app.go")
	}

	if !strings.Contains(body, "runtime.EventsEmit(") {
		t.Error("NewApp's emitter does not call runtime.EventsEmit")
	}
	// A self-call is the failure this test exists for.
	if strings.Contains(body, "a.emit(name") || strings.Contains(body, "a.emit(n,") {
		t.Error("NewApp's emitter calls itself; that recurses until the stack overflows")
	}
}

// functionBody returns the text of the function whose signature line is marker,
// matched to its closing brace at column zero.
func functionBody(src, marker string) string {
	start := strings.Index(src, marker)
	if start < 0 {
		return ""
	}
	end := strings.Index(src[start:], "\n}\n")
	if end < 0 {
		return ""
	}
	return src[start : start+end+2]
}

// TestEmitSurvivesNilContext checks the pre-startup path, where events arrive
// before the runtime has handed over a context. The original emitter is used,
// because the nil check lives inside it and a nil context must not reach
// runtime.EventsEmit, which would abort the process.
func TestEmitSurvivesNilContext(t *testing.T) {
	a := NewAppWithDataDir(t.TempDir())
	a.ctx = nil

	done := make(chan struct{})
	go func() {
		defer close(done)
		a.emit("task:added", TaskView{ID: "t2"})
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("emit blocked with a nil context")
	}
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

	// The file deletion runs off the binding thread (the UI updates at once),
	// so allow it a moment rather than asserting synchronously.
	waitFor(t, "the partial data to be discarded", 10*time.Second, func() bool {
		for _, suffix := range []string{".vkmdownload.part", ".vkmdownload.json"} {
			if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
				return false
			}
		}
		return true
	})
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

// TestRemoveTasksBySelection covers the "delete what I can see" path.
//
// The caller passes the ids it is showing rather than a filter description, so
// one call serves every view — a status, a file type, a search — and the rule
// for what counts as visible stays in the one place that draws the list.
func TestRemoveTasksBySelection(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	// Two of one kind, one of another, as a file-type filter would show them.
	mk := func(name string) TaskView {
		t.Helper()
		v, err := app.mgr.Start(StartRequest{
			URL: "http://127.0.0.1:1/" + name, Dest: filepath.Join(dir, name),
		})
		if err != nil {
			t.Fatalf("Start(%s): %v", name, err)
		}
		return v
	}
	jarA, jarB, zip := mk("a.jar"), mk("b.jar"), mk("c.zip")

	// A partial file for one of the doomed tasks: unfinished tasks take their
	// partials with them, so it must be gone.
	partial := jarA.Dest + engine.PartSuffix
	if err := os.WriteFile(partial, []byte("drop me"), 0o644); err != nil {
		t.Fatal(err)
	}

	if n := app.mgr.RemoveTasks([]string{jarA.ID, jarB.ID}); n != 2 {
		t.Errorf("RemoveTasks() = %d, want 2", n)
	}

	for _, gone := range []TaskView{jarA, jarB} {
		if _, ok := find(app.mgr, gone.ID); ok {
			t.Errorf("%s is still listed", gone.Name)
		}
	}
	if _, ok := find(app.mgr, zip.ID); !ok {
		t.Error("a task outside the selection was removed")
	}

	waitFor(t, "the unfinished partial to be removed with its record", 10*time.Second, func() bool {
		_, err := os.Stat(partial)
		return os.IsNotExist(err)
	})
}

// TestRemoveTasksWithAnUnknownIdIsHarmless: the list can change between drawing
// it and confirming, so a stale id must not abort the rest of the sweep.
func TestRemoveTasksWithAnUnknownIdIsHarmless(t *testing.T) {
	app := newTestApp(t)

	kept, err := app.mgr.Start(StartRequest{URL: "http://127.0.0.1:1/kept.bin"})
	if err != nil {
		t.Fatal(err)
	}

	if n := app.mgr.RemoveTasks([]string{"no-such-id", kept.ID}); n != 1 {
		t.Errorf("RemoveTasks() = %d, want 1", n)
	}
	if len(app.mgr.List()) != 0 {
		t.Error("the valid id was not removed")
	}
}

// TestRemoveFailedClearsRecordsOnly covers the bulk cleanup, which exists
// because a burst of bad URLs can leave hundreds of failed rows: three hundred
// and twenty-six arrived in one four-second burst once.
//
// Failed tasks are unfinished by definition, so their partials go with the
// records; finished files elsewhere are never touched by the sweep.
func TestRemoveFailedClearsRecordsOnly(t *testing.T) {
	app := newTestApp(t)

	// One that fails: nothing is listening on port 1.
	failed, err := app.mgr.Start(StartRequest{
		URL:   "http://127.0.0.1:1/will-fail.bin",
		Dest:  filepath.Join(t.TempDir(), "will-fail.bin"),
		Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the task to fail", 20*time.Second, func() bool {
		v, ok := find(app.mgr, failed.ID)
		return ok && v.Status == StatusFailed
	})

	// A partial file, as a real failed transfer would leave behind.
	partial := failed.Dest + engine.PartSuffix
	if err := os.WriteFile(partial, []byte("half a download"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Two that must survive the sweep.
	kept, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/kept.bin", Dest: filepath.Join(t.TempDir(), "kept.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	completed, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/done.bin", Dest: filepath.Join(t.TempDir(), "done.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// Put it in the completed state directly: the point here is the sweep, not
	// how a task gets completed.
	ct := app.mgr.get(completed.ID)
	ct.mu.Lock()
	ct.view.Status = StatusCompleted
	ct.mu.Unlock()

	if n := app.mgr.RemoveFailed(); n != 1 {
		t.Errorf("RemoveFailed() = %d, want 1", n)
	}

	if _, ok := find(app.mgr, failed.ID); ok {
		t.Error("the failed task is still listed")
	}
	if _, ok := find(app.mgr, kept.ID); !ok {
		t.Error("an unrelated task was removed")
	}
	if _, ok := find(app.mgr, completed.ID); !ok {
		t.Error("a completed task was removed")
	}

	waitFor(t, "the failed partial to be removed with its record", 10*time.Second, func() bool {
		_, err := os.Stat(partial)
		return os.IsNotExist(err)
	})
}

// TestRemoveFailedWithNothingToDo is the empty case, which must not report a
// phantom success.
func TestRemoveFailedWithNothingToDo(t *testing.T) {
	app := newTestApp(t)

	if _, err := app.mgr.Start(StartRequest{URL: "http://127.0.0.1:1/a.bin"}); err != nil {
		t.Fatal(err)
	}
	if n := app.mgr.RemoveFailed(); n != 0 {
		t.Errorf("RemoveFailed() = %d, want 0", n)
	}
	if len(app.mgr.List()) != 1 {
		t.Error("a queued task was removed")
	}
}

// TestRemoveKeepsPlannedFileDropsPartials covers what the delete button does:
// removing an unfinished row drops its .part + sidecar (they could never
// resume without the index entry), but a file sitting at the task's path is
// never deleted as a side effect of tidying the list.
func TestRemoveKeepsPlannedFileDropsPartials(t *testing.T) {
	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "kept.bin")

	for _, p := range []string{dest, dest + engine.PartSuffix, dest + engine.StateSuffix} {
		if err := os.WriteFile(p, []byte("keep me"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never", Dest: dest, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := app.mgr.Remove(task.ID, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// The file itself must survive; the orphan partials must not.
	if _, err := os.Stat(dest); err != nil {
		t.Errorf("the file was removed with its record: %v", err)
	}
	waitFor(t, "the orphan partials to be removed", 10*time.Second, func() bool {
		_, err1 := os.Stat(dest + engine.PartSuffix)
		_, err2 := os.Stat(dest + engine.StateSuffix)
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})

	// Nothing may remain in the index either.
	if _, ok := find(app.mgr, task.ID); ok {
		t.Error("the task is still listed")
	}
}

// TestOrganizeTarget maps finished filenames to group folders.
func TestOrganizeTarget(t *testing.T) {
	groups := []OrganizeGroup{
		{Name: "Programs", Exts: []string{"exe", "msi"}},
		{Name: "Archives", Exts: []string{"zip"}},
	}
	cases := []struct {
		name, want string
	}{
		{"setup.exe", filepath.Join("dl", "Programs", "setup.exe")},
		{"SETUP.EXE", filepath.Join("dl", "Programs", "SETUP.EXE")},
		{"a.zip", filepath.Join("dl", "Archives", "a.zip")},
		{"notes.txt", ""},
		{"noext", ""},
		{"trailing.", ""},
	}
	for _, c := range cases {
		if got := organizeTarget(groups, "dl", c.name); got != c.want {
			t.Errorf("organizeTarget(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestNormalizeOrganizeGroups drops what cannot be a folder or an extension.
func TestNormalizeOrganizeGroups(t *testing.T) {
	in := []OrganizeGroup{
		{Name: "  Programs  ", Exts: []string{".EXE", " msi ", "", ".."}},
		{Name: "..", Exts: []string{"bin"}},
		{Name: "Empty", Exts: []string{}},
		{Name: "Bad/Name", Exts: []string{"bin"}},
	}
	got := normalizeOrganizeGroups(in)
	if len(got) != 2 {
		t.Fatalf("got %d groups, want 2 (%v)", len(got), got)
	}
	if got[0].Name != "Programs" {
		t.Errorf("name = %q, want trimmed Programs", got[0].Name)
	}
	if len(got[0].Exts) != 2 || got[0].Exts[0] != "exe" || got[0].Exts[1] != "msi" {
		t.Errorf("exts = %v, want [exe msi]", got[0].Exts)
	}
	if got[1].Name != "Name" || len(got[1].Exts) != 1 {
		t.Errorf("second = %+v, want Name/[bin]", got[1])
	}
}

// TestOrganizeTarget HonorsAbsoluteDir covers a group pointed elsewhere.
func TestOrganizeTargetHonorsAbsoluteDir(t *testing.T) {
	groups := []OrganizeGroup{
		{Name: "Programs", Exts: []string{"exe"}},
		{Name: "Elsewhere", Exts: []string{"zip"}, Dir: `D:\Vault`},
	}
	if got := organizeTarget(groups, `C:\Dl`, "a.zip"); got != filepath.Join(`D:\Vault`, "a.zip") {
		t.Errorf("absolute dir = %q", got)
	}
	if got := organizeTarget(groups, `C:\Dl`, "a.exe"); got != filepath.Join(`C:\Dl`, "Programs", "a.exe") {
		t.Errorf("relative dir = %q", got)
	}
}

// TestOrganizeMovesAutomaticTaskIntoGroup downloads end to end and checks the
// finished file lands in its group folder, not the download root.
func TestOrganizeMovesAutomaticTaskIntoGroup(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 5*time.Millisecond)

	app := newTestApp(t)
	cfg := app.Config()
	cfg.OrganizeEnabled = true
	cfg.OrganizeGroups = []OrganizeGroup{{Name: "Bins", Exts: []string{"bin"}}}
	dir := t.TempDir()
	cfg.OrganizeDir = dir
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/tool.bin", Dest: dir, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the task to complete", 60*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status == StatusCompleted
	})

	v, _ := find(app.mgr, task.ID)
	wantDest := filepath.Join(dir, "Bins", "tool.bin")
	if v.Dest != wantDest {
		t.Errorf("dest = %q, want %q", v.Dest, wantDest)
	}
	if _, err := os.Stat(wantDest); err != nil {
		t.Errorf("organized file missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tool.bin")); !os.IsNotExist(err) {
		t.Error("a copy was left in the download root")
	}
}

// TestOrganizeLeavesManualTasksAlone covers the dialog path: a task the user
// placed explicitly is never re-sorted on completion.
func TestOrganizeLeavesManualTasksAlone(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 5*time.Millisecond)

	app := newTestApp(t)
	cfg := app.Config()
	cfg.OrganizeEnabled = true
	cfg.OrganizeGroups = []OrganizeGroup{{Name: "Bins", Exts: []string{"bin"}}}
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	dir := t.TempDir()

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/tool.bin", Dest: dir, Conns: 2, Start: true, Manual: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the task to complete", 60*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status == StatusCompleted
	})

	v, _ := find(app.mgr, task.ID)
	if v.Dest != filepath.Join(dir, "tool.bin") {
		t.Errorf("manual task moved to %q, want it left alone", v.Dest)
	}
}

// TestStartAllAndStopAll covers the two bulk buttons: start everything that is
// waiting, then stop everything that is running.
func TestStartAllAndStopAll(t *testing.T) {
	want := payload(512 << 10)
	// Slow enough that the stop lands while transfers are still in flight.
	srv := rangedServer(t, want, 15*time.Millisecond)

	app := newTestApp(t)
	dir := t.TempDir()

	const count = 3
	for i := 0; i < count; i++ {
		if _, err := app.mgr.Start(StartRequest{
			URL:   srv.URL + "/bulk.bin",
			Dest:  filepath.Join(dir, fmt.Sprintf("bulk-%d.bin", i)),
			Conns: 2,
			Start: false,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	if n := app.mgr.StartAll(); n != 2 {
		t.Errorf("StartAll started %d, want 2 (MaxConcurrent)", n)
	}
	// The third task waits for a slot rather than starting.
	if s := app.mgr.Stats(); s["queued"] != 1 {
		t.Errorf("queued = %d, want 1", s["queued"])
	}
	// A second call has nothing left to start.
	if n := app.mgr.StartAll(); n != 0 {
		t.Errorf("a second StartAll started %d, want 0", n)
	}

	if n := app.mgr.StopAll(); n == 0 {
		t.Error("StopAll paused nothing while transfers were running")
	}

	waitFor(t, "every task to leave the running state", 30*time.Second, func() bool {
		s := app.mgr.Stats()
		return s["active"] == 0 && s["queued"] == 0
	})

	// Nothing may be left running. A task that happened to finish before the
	// stop landed is fine; one still transferring is not.
	for _, v := range app.mgr.List() {
		if v.Status == StatusDownloading || v.Status == StatusQueued {
			t.Errorf("task %s is still %s after StopAll", v.ID, v.Status)
		}
	}
}

// TestStartPlanRunsToCompletion covers the plan button: it starts everything,
// reports that it is running, and announces completion exactly once.
func TestStartPlanRunsToCompletion(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 5*time.Millisecond)

	app := newTestApp(t)

	var mu sync.Mutex
	planned := 0
	app.emit = func(name string, _ ...any) {
		if name == "plan:done" {
			mu.Lock()
			planned++
			mu.Unlock()
		}
	}

	for i := 0; i < 2; i++ {
		if _, err := app.mgr.Start(StartRequest{
			URL:   srv.URL + "/plan.bin",
			Dest:  filepath.Join(t.TempDir(), fmt.Sprintf("plan-%d.bin", i)),
			Conns: 2,
			Start: false,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	n, err := app.mgr.StartPlan()
	if err != nil {
		t.Fatalf("StartPlan: %v", err)
	}
	if n != 2 {
		t.Errorf("StartPlan started %d, want 2", n)
	}
	if !app.mgr.PlanRunning() {
		t.Error("PlanRunning is false immediately after StartPlan")
	}
	// A second plan must be refused rather than running two watchers.
	if _, err := app.mgr.StartPlan(); err == nil {
		t.Error("a second StartPlan was accepted while one was running")
	}

	waitFor(t, "the plan to report completion", 60*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return planned > 0
	})

	waitFor(t, "the plan to stop running", 5*time.Second, func() bool {
		return !app.mgr.PlanRunning()
	})

	// Give a stray second watcher time to show itself.
	time.Sleep(1200 * time.Millisecond)
	mu.Lock()
	got := planned
	mu.Unlock()
	if got != 1 {
		t.Errorf("plan:done fired %d times, want exactly 1", got)
	}
}

// TestStartPlanWithNothingToDoIsRefused checks the plan button's empty case.
func TestStartPlanWithNothingToDoIsRefused(t *testing.T) {
	app := newTestApp(t)

	if _, err := app.mgr.StartPlan(); err == nil {
		t.Error("StartPlan with no tasks was accepted")
	}
	if app.mgr.PlanRunning() {
		t.Error("PlanRunning is true after a refused plan; the slot was not released")
	}
}

// TestStartPlanWithFullSlotsStillRuns pins the plan-button fix: when every
// MaxConcurrent slot is already occupied, StartAll cannot start anything new,
// yet the running transfers plus the queued ones are still real work. The
// plan must be accepted (covering active + queued) rather than refused with
// "没有等待下载的任务".
func TestStartPlanWithFullSlotsStillRuns(t *testing.T) {
	want := payload(1 << 20)
	srv := rangedServer(t, want, 15*time.Millisecond)

	app := newTestApp(t)
	dir := t.TempDir()

	// Fill both concurrency slots.
	for i := 0; i < 2; i++ {
		if _, err := app.mgr.Start(StartRequest{
			URL:   srv.URL + "/full.bin",
			Dest:  filepath.Join(dir, fmt.Sprintf("full-%d.bin", i)),
			Conns: 2,
			Start: false,
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	if n := app.mgr.StartAll(); n != 2 {
		t.Fatalf("StartAll started %d, want 2 (MaxConcurrent)", n)
	}

	// One more task arrives while the slots are full: it can only queue.
	if _, err := app.mgr.Start(StartRequest{
		URL:   srv.URL + "/full.bin",
		Dest:  filepath.Join(dir, "full-2.bin"),
		Conns: 2,
		Start: false,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	n, err := app.mgr.StartPlan()
	if err != nil {
		t.Fatalf("StartPlan refused while 2 tasks were running and 1 queued: %v", err)
	}
	if n != 3 {
		t.Errorf("StartPlan covered %d, want 3 (2 active + 1 queued)", n)
	}
	if !app.mgr.PlanRunning() {
		t.Error("PlanRunning is false immediately after StartPlan")
	}

	// Settle everything; the watcher must then finish the plan.
	app.mgr.StopAll()
	waitFor(t, "the plan to stop running", 30*time.Second, func() bool {
		return !app.mgr.PlanRunning()
	})
}

// TestRemoveDropsUnfinishedPartials pins the no-junk rule: removing a task
// that never finished deletes its .part + sidecar (they could never resume
// without the index entry), while a completed task's file is never touched.
func TestRemoveDropsUnfinishedPartials(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	tv, err := app.mgr.Start(StartRequest{
		URL:   "https://example.com/junk.bin",
		Dest:  filepath.Join(dir, "junk.bin"),
		Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, suf := range []string{engine.PartSuffix, engine.StateSuffix} {
		if err := os.WriteFile(tv.Dest+suf, []byte("orphan"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.mgr.Remove(tv.ID, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "unfinished partials to be removed", 10*time.Second, func() bool {
		_, err1 := os.Stat(tv.Dest + engine.PartSuffix)
		_, err2 := os.Stat(tv.Dest + engine.StateSuffix)
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})

	// A finished file is the user's data: removing its record deletes nothing.
	tv2, err := app.mgr.Start(StartRequest{
		URL:   "https://example.com/done.bin",
		Dest:  filepath.Join(dir, "done.bin"),
		Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := os.WriteFile(tv2.Dest, []byte("finished"), 0o644); err != nil {
		t.Fatal(err)
	}
	tk := app.mgr.get(tv2.ID)
	tk.mu.Lock()
	tk.view.Status = StatusCompleted
	tk.mu.Unlock()
	// Removal never touches a finished file (not even from the detached
	// cleanup goroutine), so once the record is gone the assertion below is
	// stable without waiting.
	if err := app.mgr.Remove(tv2.ID, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "the record to be dropped", 10*time.Second, func() bool {
		_, ok := find(app.mgr, tv2.ID)
		return !ok
	})
	if _, err := os.Stat(tv2.Dest); err != nil {
		t.Errorf("finished file removed with its record: %v", err)
	}
}

// TestNewTasksRestPaused pins the no-auto-follow rule: a task added without
// Start stays paused — never queued — so starting one task cannot pull the
// rest along. Only an explicit Start All / plan or per-task resume runs them.
func TestNewTasksRestPaused(t *testing.T) {
	app := newTestApp(t)

	v, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/rest.bin", Dest: filepath.Join(t.TempDir(), "rest.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if v.Status != StatusPaused {
		t.Errorf("new idle task status = %s, want paused", v.Status)
	}
}

// TestDuplicateURLIsRejected pins the one-URL-one-task rule: re-adding an
// unfinished link to the same destination fails and points at the existing
// record, instead of spawning a second transfer that would fight over the
// same partial file (or burn a single-use token outright).
func TestDuplicateURLIsRejected(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	first, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/same.bin", Dest: filepath.Join(dir, "same.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/same.bin", Dest: filepath.Join(dir, "same.bin"),
	}); err == nil {
		t.Error("re-adding the same URL+dest was accepted")
	} else if !strings.Contains(err.Error(), "已在列表中") {
		t.Errorf("error = %v, want it to say the link is already listed", err)
	}

	// The same file deliberately sent to another folder is a different task.
	if _, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/same.bin", Dest: filepath.Join(dir, "elsewhere.bin"),
	}); err != nil {
		t.Errorf("same URL to another folder was rejected: %v", err)
	}

	// Completed tasks are exempt: re-downloading a finished file is legitimate.
	tk := app.mgr.get(first.ID)
	tk.mu.Lock()
	tk.view.Status = StatusCompleted
	tk.mu.Unlock()
	if _, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/same.bin", Dest: filepath.Join(dir, "same.bin"),
	}); err != nil {
		t.Errorf("re-adding a completed download was rejected: %v", err)
	}
}

// TestSameFileDifferentURLIsRejected pins the second duplicate layer:
// single-use links mint a fresh URL per click, so URL matching is blind to
// them. Two different URLs on one host advertising the same filename and
// size are the same file. Adding is never blocked on the network: the row
// appears unnamed at once, the probe fills it in, and a probed duplicate of
// a still-untouched row removes it again.
func TestSameFileDifferentURLIsRejected(t *testing.T) {
	const name = "same-file.bin"
	const size = 128 << 10
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", fmt.Sprint(size))
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", size))
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(body[:1])
	}))
	defer srv.Close()

	app := newTestApp(t)
	dir := t.TempDir()

	// The add itself never waits for the network: unnamed, paused, returned.
	// The add itself never waits for the network; the background probe fills
	// the row (asserted below, no racy blankness check: on loopback the
	// probe can win before Start returns).
	first, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/token-one", Dest: dir, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The background probe fills the row.
	waitFor(t, "the probe to name the row", 15*time.Second, func() bool {
		v, ok := find(app.mgr, first.ID)
		return ok && v.Name == name && v.Size == size
	})

	// Same file behind another token URL: accepted at first (nothing known
	// yet), then removed once its own probe names it — the original stays.
	// (Generous timeout: localhost stalls on a loaded Windows box must not
	// flake this.)
	second, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/token-two", Dest: dir, Start: false,
	})
	if err != nil {
		t.Fatalf("second Start should not block: %v", err)
	}
	waitFor(t, "the probed duplicate to be removed", 30*time.Second, func() bool {
		_, ok := find(app.mgr, second.ID)
		return !ok
	})
	if _, ok := find(app.mgr, first.ID); !ok {
		t.Error("the original task was removed instead of the duplicate")
	}

	// A genuinely different file on the same host is still accepted: give it
	// a name up front so no probe is even needed.
	third, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/token-one", Dest: filepath.Join(dir, "sub"),
		Filename: "other.bin", Start: false,
	})
	if err != nil {
		t.Errorf("same link to another folder was rejected: %v", err)
	} else if v, ok := find(app.mgr, third.ID); !ok || v.Name != "other.bin" {
		t.Errorf("third task = %+v, want it listed as other.bin", v)
	}
}

// TestHostReachableFastFails pins the add-time probe budget: a dead host
// must be reported unreachable quickly, not after seconds of retries.
func TestHostReachableFastFails(t *testing.T) {
	start := time.Now()
	if hostReachable("http://127.0.0.1:1/never") {
		t.Error("unlistening port reported reachable")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("dead host took %v to reject", took)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	if !hostReachable(srv.URL + "/x") {
		t.Error("live test server reported unreachable")
	}
}

// TestStartPlanSelectedCoversOnlyTheSelection pins the scoped plan: only the
// chosen ids are started and watched, and the end action fires once they
// settle even while other tasks sit untouched.
func TestStartPlanSelectedCoversOnlyTheSelection(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 5*time.Millisecond)

	app := newTestApp(t)
	dir := t.TempDir()

	mk := func(name string) string {
		v, err := app.mgr.Start(StartRequest{
			URL: srv.URL + "/plan-sel.bin", Dest: filepath.Join(dir, name),
			Conns: 2, Start: false,
		})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		return v.ID
	}
	// Same URL to different folders is allowed (first duplicate layer).
	in1, in2, out := mk("sel-1.bin"), mk("sel-2.bin"), mk("sel-out.bin")

	var mu sync.Mutex
	planned := 0
	app.emit = func(name string, _ ...any) {
		if name == "plan:done" {
			mu.Lock()
			planned++
			mu.Unlock()
		}
	}

	n, err := app.mgr.StartPlanSelected([]string{in1, in2, "no-such-id"})
	if err != nil {
		t.Fatalf("StartPlanSelected: %v", err)
	}
	if n != 2 {
		t.Errorf("StartPlanSelected covered %d, want 2", n)
	}
	if !app.mgr.PlanRunning() {
		t.Error("PlanRunning is false immediately after StartPlanSelected")
	}

	// The outsider must never start.
	if v, ok := find(app.mgr, out); !ok || (v.Status != StatusPaused && v.Status != StatusQueued) {
		t.Errorf("outsider status = %v, want it untouched", v.Status)
	}

	waitFor(t, "the scoped plan to report completion", 60*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return planned > 0
	})
	waitFor(t, "the plan to stop running", 5*time.Second, func() bool {
		return !app.mgr.PlanRunning()
	})
}

// TestRemoveFindsPartialsByServerName pins the path bug: a task created
// without a filename keeps Dest as the directory while the engine resolves
// dir+suggested-name underneath. Removing it must delete dir+name.part, not
// the nonexistent dir.part.
func TestRemoveFindsPartialsByServerName(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	tv, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/nameless.bin", Dest: dir, Start: false,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if tv.Dest != dir {
		t.Fatalf("dest = %q, want the directory %q", tv.Dest, dir)
	}

	// The server names it mid-transfer; the view picks the name up.
	tk := app.mgr.get(tv.ID)
	tk.mu.Lock()
	tk.view.Name = "real-name.bin"
	tk.mu.Unlock()

	part := filepath.Join(dir, "real-name.bin"+engine.PartSuffix)
	state := filepath.Join(dir, "real-name.bin"+engine.StateSuffix)
	for _, p := range []string{part, state} {
		if err := os.WriteFile(p, []byte("half"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.mgr.Remove(tv.ID, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	waitFor(t, "the server-named partials to be removed", 10*time.Second, func() bool {
		_, err1 := os.Stat(part)
		_, err2 := os.Stat(state)
		return os.IsNotExist(err1) && os.IsNotExist(err2)
	})
}

// TestStartEmitsProgressImmediately pins the responsive-button rule: starting
// tasks must report downloading/queued at once via task:progress, not wait
// for the engine's first progress event (seconds away on a slow link).
func TestStartEmitsProgressImmediately(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	for i := 0; i < 3; i++ {
		if _, err := app.mgr.Start(StartRequest{
			URL: "http://127.0.0.1:1/instant.bin", Dest: filepath.Join(dir, fmt.Sprintf("in-%d.bin", i)),
		}); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}

	var mu sync.Mutex
	seen := map[string]string{}
	app.emit = func(name string, data ...any) {
		if name != "task:progress" || len(data) == 0 {
			return
		}
		if v, ok := data[0].(TaskView); ok {
			mu.Lock()
			seen[v.ID] = string(v.Status)
			mu.Unlock()
		}
	}

	if n := app.mgr.StartAll(); n != 2 {
		t.Fatalf("StartAll started %d, want 2 (MaxConcurrent)", n)
	}

	// Synchronous: no waiting for engine progress. Two run, one queues
	// behind MaxConcurrent — all three must report at once.
	// (Copy out from under the lock first: StopAll below emits too, and
	// holding this lock across it would deadlock the recorder.)
	mu.Lock()
	got := make(map[string]string, len(seen))
	for id, st := range seen {
		got[id] = st
	}
	mu.Unlock()
	if len(got) != 3 {
		t.Fatalf("task:progress reported %d tasks, want 3 immediately", len(got))
	}
	downloading, queued := 0, 0
	for id, st := range got {
		switch st {
		case string(StatusDownloading):
			downloading++
		case string(StatusQueued):
			queued++
		default:
			t.Errorf("task %s reported %s, want downloading or queued", id, st)
		}
	}
	if downloading != 2 || queued != 1 {
		t.Errorf("got %d downloading + %d queued, want 2 + 1", downloading, queued)
	}
	app.mgr.StopAll()
}

// TestRefreshIntervalMapping pins the UI refresh tiers: fast is today's
// 250ms behaviour, and unknown values (including pre-upgrade configs that
// never had the setting) fall back to fast rather than stalling updates.
func TestRefreshIntervalMapping(t *testing.T) {
	cases := map[string]time.Duration{
		"fast":   250 * time.Millisecond,
		"medium": 650 * time.Millisecond,
		"slow":   1000 * time.Millisecond,
		"":       250 * time.Millisecond,
		"turbo":  250 * time.Millisecond,
	}
	for in, want := range cases {
		if got := refreshInterval(in); got != want {
			t.Errorf("refreshInterval(%q) = %v, want %v", in, got, want)
		}
	}

	cfg := DefaultConfig()
	if cfg.RefreshRate != "fast" {
		t.Errorf("default RefreshRate = %q, want fast", cfg.RefreshRate)
	}
	if cfg.ResumeFlushKB != 1024 || cfg.ResumeFlushMs != 250 {
		t.Errorf("default flush = %dKB/%dms, want 1024KB/250ms",
			cfg.ResumeFlushKB, cfg.ResumeFlushMs)
	}
	cfg.RefreshRate = "bogus"
	cfg.ResumeFlushKB = 1
	cfg.ResumeFlushMs = 99999
	cfg.normalize()
	if cfg.RefreshRate != "fast" {
		t.Errorf("normalized RefreshRate = %q, want fast", cfg.RefreshRate)
	}
	if cfg.ResumeFlushKB != 1024 || cfg.ResumeFlushMs != 10000 {
		t.Errorf("normalized flush = %dKB/%dms, want clamped 1024KB/10000ms",
			cfg.ResumeFlushKB, cfg.ResumeFlushMs)
	}
	if cfg.ResumeFlushPct != 0 {
		t.Errorf("default ResumeFlushPct = %d, want 0 (off)", cfg.ResumeFlushPct)
	}
	if !cfg.ResumeFlushBySize || !cfg.ResumeFlushByTime || cfg.ResumeFlushByPct {
		t.Errorf("default flush switches = size %v time %v pct %v, want true/true/false",
			cfg.ResumeFlushBySize, cfg.ResumeFlushByTime, cfg.ResumeFlushByPct)
	}

	// Switching everything off must flip the time trigger back on: a
	// transfer that never persists is a silent full re-download on crash.
	cfg.ResumeFlushBySize = false
	cfg.ResumeFlushByTime = false
	cfg.ResumeFlushPct = 0
	cfg.normalize()
	if !cfg.ResumeFlushByTime {
		t.Error("all triggers off was accepted; the time trigger must be forced back on")
	}
}

// TestApplySpeedLimitRetunesLive pins the no-restart rule for the speed
// setting: unlimited -> limited -> unlimited keeps one shared instance whose
// rate follows the setting, so running transfers feel it at once.
func TestApplySpeedLimitRetunesLive(t *testing.T) {
	app := newTestApp(t)

	app.mgr.applySpeedLimit(0)
	if app.mgr.limiter != nil {
		t.Error("unlimited created a limiter")
	}

	app.mgr.applySpeedLimit(1024)
	first := app.mgr.limiter
	if first == nil {
		t.Fatal("no limiter after enabling the cap")
	}
	if got := first.Rate(); got != 1024<<10 {
		t.Errorf("Rate = %d, want %d", got, 1024<<10)
	}

	app.mgr.applySpeedLimit(2048)
	if app.mgr.limiter != first {
		t.Error("rate change replaced the shared limiter; runners hold the old one")
	}
	if got := first.Rate(); got != 2048<<10 {
		t.Errorf("Rate = %d, want %d", got, 2048<<10)
	}

	app.mgr.applySpeedLimit(0)
	if app.mgr.limiter != first {
		t.Error("disabling the cap replaced the shared limiter")
	}
	if got := first.Rate(); got != 0 {
		t.Errorf("Rate = %d, want 0 (unlimited)", got)
	}
}

// TestApplyRefreshIntervalReachesLive pins the registry push: downloaders
// registered as running get the new cadence, and unregistered ones are
// simply not visited.
func TestApplyRefreshIntervalReachesLive(t *testing.T) {
	app := newTestApp(t)

	dl, err := engine.New(engine.Options{}.Normalize())
	if err != nil {
		t.Fatal(err)
	}
	defer dl.Close()

	untrack := app.mgr.trackLive("t-live", dl)
	app.mgr.applyRefreshInterval(650 * time.Millisecond)
	if got := dl.ProgressInterval(); got != 650*time.Millisecond {
		t.Errorf("live interval = %v, want 650ms", got)
	}
	untrack()
	app.mgr.applyRefreshInterval(1000 * time.Millisecond)
	if got := dl.ProgressInterval(); got != 650*time.Millisecond {
		t.Errorf("interval after unregister = %v, want it untouched at 650ms", got)
	}

	// Empty registry never panics.
	app.mgr.applyRefreshInterval(250 * time.Millisecond)
}

// TestResumeFlushSettingsReachEngine pins the wiring from the settings page
// to the transfer: aggressive flush thresholds must still produce a correct,
// fully published file with nothing left behind.
func TestResumeFlushSettingsReachEngine(t *testing.T) {
	want := payload(512 << 10)
	srv := rangedServer(t, want, 0)

	app := newTestApp(t)
	cfg := app.Config()
	cfg.ResumeFlushKB = 64
	cfg.ResumeFlushMs = 100
	cfg.ResumeFlushPct = 5
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	dir := t.TempDir()
	tv, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/flush.bin", Dest: filepath.Join(dir, "flush.bin"),
		Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the download to complete", 60*time.Second, func() bool {
		v, ok := find(app.mgr, tv.ID)
		return ok && v.Status == StatusCompleted
	})

	assertFileEquals(t, filepath.Join(dir, "flush.bin"), want)
	for _, suf := range []string{engine.PartSuffix, engine.StateSuffix} {
		if _, err := os.Stat(filepath.Join(dir, "flush.bin"+suf)); !os.IsNotExist(err) {
			t.Errorf("leftover %s after publish", suf)
		}
	}
}

// TestStatsAggregatesActiveTasks covers the sidebar totals.
func TestStatsAggregatesActiveTasks(t *testing.T) {
	want := payload(256 << 10)
	srv := rangedServer(t, want, 20*time.Millisecond)

	// The files live outside t.TempDir because this test lets the downloads run
	// to completion; a directory torn down mid-transfer would fail the cleanup
	// rather than the assertion under test.
	outDir, err := os.MkdirTemp("", "vkmdownload-stats-")
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

// TestStartConcurrentSameURL adds the same link from many goroutines at once.
// Before the inflight guard they all slipped past the duplicate check and
// each inserted a task; now exactly one wins and the rest are duplicates.
func TestStartConcurrentSameURL(t *testing.T) {
	a := newTestApp(t)
	dest := t.TempDir()

	const n = 16
	var wg sync.WaitGroup
	var mu sync.Mutex
	created := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A caller-supplied filename keeps the whole path synchronous
			// (no network probe), so this hammers the exact race window.
			_, err := a.mgr.Start(StartRequest{
				URL:      "https://example.com/same.bin",
				Dest:     dest,
				Filename: "same.bin",
			})
			if err == nil {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatalf("created = %d, want 1", created)
	}
	if got := len(a.mgr.List()); got != 1 {
		t.Fatalf("tasks = %d, want 1", got)
	}
}
