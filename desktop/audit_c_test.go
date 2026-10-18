package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Batch C tests: the updater's backup, failure and state handling.

// TestBackupIsAlwaysACopy pins P0-2. The backup used to be a hard link when the
// filesystem allowed it, which made the backup and the running image the same
// file. Measured on Windows 11 26100 that combination happens to work, but it
// leaves three things sharing one identity — the backup, the running helper and
// the file being replaced — and every rollback step then depends on link
// semantics. The rule is now unconditionally a copy.
func TestBackupIsAlwaysACopy(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)
	exe := filepath.Join(dir, "app.exe")
	bak := exe + ".bak"
	writeFile(t, exe, "OLD-BYTES")

	if err := backupExe(sys, exe, bak); err != nil {
		t.Fatalf("backupExe: %v", err)
	}
	if got := readFileString(t, bak); got != "OLD-BYTES" {
		t.Errorf("backup content = %q, want a copy of the executable", got)
	}
	if sys.size(exe) != int64(len("OLD-BYTES")) || sys.size(bak) != sys.size(exe) {
		t.Errorf("sizes: exe=%d bak=%d, want equal", sys.size(exe), sys.size(bak))
	}

	// The two must be independent files: changing the backup must not change
	// the executable, which is exactly what a hard link would do.
	writeFile(t, bak, "CHANGED")
	if got := readFileString(t, exe); got != "OLD-BYTES" {
		t.Errorf("the executable followed the backup (%q); they are the same file", got)
	}
}

// TestBackupFailureIsReported keeps the error path honest: a copy that cannot be
// made must fail the update rather than proceeding without a backup.
func TestBackupFailureIsReported(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)
	sys.copyErr = errors.New("disk full")
	exe := filepath.Join(dir, "app.exe")
	writeFile(t, exe, "OLD-BYTES")

	err := backupExe(sys, exe, exe+".bak")
	if err == nil {
		t.Fatal("backupExe reported success with a failing copy")
	}
	if !strings.Contains(err.Error(), "备份") {
		t.Errorf("error = %v, want it to say the backup failed", err)
	}
}

// TestRollbackRestoresWhenTheExecutableIsGone pins P1-6's second half: after a
// swap that renamed the executable aside and then failed, the path is missing
// and "restore the old version" used to refuse because there was nothing to move
// aside. With the target missing, the backup simply takes its place.
func TestRollbackRestoresWhenTheExecutableIsGone(t *testing.T) {
	f := newHelperFixture(t)
	// Simulate the half-finished state: the executable was moved away.
	if err := os.Remove(f.exe); err != nil {
		t.Fatal(err)
	}

	code := runHelper(helperArgs{
		Mode: modeRollback, Exe: f.exe, Bak: f.bak, State: f.args.State, Parent: 0,
	}, f.sys)
	if code == 0 {
		t.Errorf("rollback exit code = 0, want the diagnostic non-zero code")
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("executable after rollback = %q, want the old version restored", got)
	}
	if got := f.stage(t); got != stageRolledBack {
		t.Errorf("stage = %q, want %q", got, stageRolledBack)
	}
	if len(f.sys.started) == 0 {
		t.Error("the restored old version was not started")
	}
}

// TestRollbackStillRefusesWithoutABackup keeps the safety rule: no backup means
// no touching anything, because the current executable may be the only copy.
func TestRollbackStillRefusesWithoutABackup(t *testing.T) {
	f := newHelperFixture(t)
	if err := os.Remove(f.bak); err != nil {
		t.Fatal(err)
	}
	before := readFileString(t, f.exe)

	_ = runHelper(helperArgs{
		Mode: modeRollback, Exe: f.exe, Bak: f.bak, State: f.args.State, Parent: 0,
	}, f.sys)

	if got := readFileString(t, f.exe); got != before {
		t.Errorf("the executable changed without a backup: %q", got)
	}
	if got := f.stage(t); got != stageAborted {
		t.Errorf("stage = %q, want %q", got, stageAborted)
	}
}

// TestStartUpdateClaimsTheSlotBeforeTheNetwork pins P1-7. The guard used to sit
// on one side of the release lookup, so two clicks in the same instant both saw
// "not downloading" and both opened the same staged file with O_TRUNC.
func TestStartUpdateClaimsTheSlotBeforeTheNetwork(t *testing.T) {
	app := NewAppWithDataDir(t.TempDir())
	app.emit = func(string, ...any) {}

	// The release lookup blocks, so the first call is guaranteed to still be
	// inside it when the second one arrives. Before the fix the state was only
	// claimed after this returned, so both callers passed the guard.
	release := make(chan struct{})
	var lookups int
	var mu sync.Mutex
	app.fetchRelease = func(context.Context) (releaseAsset, error) {
		mu.Lock()
		lookups++
		mu.Unlock()
		<-release
		return releaseAsset{tag: "v99.0.0", exeURL: "http://127.0.0.1:1/x.exe"}, nil
	}

	firstDone := make(chan UpdateStatus, 1)
	go func() {
		st, _ := app.StartUpdate()
		firstDone <- st
	}()

	// The claim is in place before the lookup finishes.
	waitFor(t, "the update slot to be claimed", 5*time.Second, func() bool {
		return app.UpdateProgress().State == "downloading"
	})

	second, err := app.StartUpdate()
	if err != nil {
		t.Fatalf("a second StartUpdate during the first one failed: %v", err)
	}
	if second.State != "downloading" {
		t.Errorf("second state = %q, want it to report the in-flight download", second.State)
	}

	close(release)
	<-firstDone

	mu.Lock()
	n := lookups
	mu.Unlock()
	if n != 1 {
		t.Errorf("release lookups = %d, want exactly 1 (the second call must not start work)", n)
	}
}

// TestStartUpdateRefusesWhileApplying pins P1-8's first half: applying means the
// executable is being replaced, and a download started then would write the very
// file the helper is about to install.
func TestStartUpdateRefusesWhileApplying(t *testing.T) {
	app := NewAppWithDataDir(t.TempDir())
	app.emit = func(string, ...any) {}
	app.upd.State = "applying"

	if _, err := app.StartUpdate(); err == nil {
		t.Fatal("StartUpdate was accepted while an update was being applied")
	}
	if got := app.UpdateProgress().State; got != "applying" {
		t.Errorf("state = %q, want it left at applying", got)
	}
}

// TestAlreadyCurrentIsNotAFailure pins the P3 fix: the normal "you are up to
// date" answer used to be recorded as a failure, so the settings row painted an
// error and offered a retry for the expected outcome.
func TestAlreadyCurrentIsNotAFailure(t *testing.T) {
	app := NewAppWithDataDir(t.TempDir())
	app.emit = func(string, ...any) {}
	// An older tag than the build under test: "nothing to install".
	app.fetchRelease = func(context.Context) (releaseAsset, error) {
		return releaseAsset{tag: "v0.0.1", exeURL: "http://127.0.0.1:1/x.exe"}, nil
	}

	st, err := app.StartUpdate()
	if err == nil {
		t.Fatal("StartUpdate on an old release reported success")
	}
	if st.State == "failed" {
		t.Errorf("state = failed for \"already current\"; it must not look like an error")
	}
	if st.State != "current" {
		t.Errorf("state = %q, want %q", st.State, "current")
	}
}

// TestStagedFileHandleIsReleasedOnFailure pins P2-2. The download used to return
// with the file still open on the success path, and the failure paths deleted a
// file they were still writing — which fails on Windows and whose error was
// discarded, so "the bad file was discarded" was untrue and the next attempt
// tripped over the leftover.
func TestStagedFileHandleIsReleasedOnFailure(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}

	staged := filepath.Join(dir, "app.exe.new")

	// A body shorter than the advertised length: the truncation check fires
	// after the file has been written.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()

	rel := releaseAsset{tag: "v9.9.9", exeURL: srv.URL, digest: ""}
	app.downloadUpdate(rel, staged)

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("the failed download left %s behind: %v", staged, err)
	}
	// Being able to create the file is the proof the handle was released: on
	// Windows an open handle makes this fail.
	f, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		t.Fatalf("the staged path is still locked: %v", err)
	}
	_ = f.Close()

	// And the state must say failed, not downloading forever.
	if got := app.UpdateProgress(); got.State != "failed" {
		t.Errorf("state = %q, want failed", got.State)
	}
}

// TestDownloadRefusesWithoutLengthOrDigest pins the P3 rule: with neither a
// Content-Length nor a digest there is nothing tying the file on disk to the
// release, and every remaining check passes for any executable whose first
// megabyte survived.
func TestDownloadRefusesWithoutLengthOrDigest(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}

	staged := filepath.Join(dir, "app.exe.new")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Chunked, no length, no digest in the release.
		_, _ = w.Write([]byte("MZ" + strings.Repeat("x", 2<<20)))
	}))
	defer srv.Close()

	app.downloadUpdate(releaseAsset{tag: "v9.9.9", exeURL: srv.URL}, staged)

	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("an unverifiable download was kept at %s", staged)
	}
	if got := app.UpdateProgress(); got.State != "failed" {
		t.Errorf("state = %q, want failed", got.State)
	}
}

// TestHandshakeSeesAWindowThatClosed pins P2-5. The timeout used to ask whether
// a window was on screen at the deadline, so a new build that showed its window
// and then minimised — or was minimised by the user — was judged as never having
// started and rolled back.
func TestHandshakeSeesAWindowThatClosed(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)
	pid := 777
	sys.alive[pid] = true
	// A window appears on the first poll and is gone for the rest of the wait.
	calls := 0
	windowSys := &windowOnceSys{fakeSys: sys, onCall: func() bool {
		calls++
		return calls == 1
	}}
	// Ignore the clock: the point is the deadline outcome.
	out := awaitHandshakeWindowHook(windowSys, filepath.Join(dir, "missing.ok"), pid, 3*time.Second)
	if out != hsUnconfirmed {
		t.Errorf("outcome = %v, want hsUnconfirmed (a window was seen at some point)", out)
	}
}

// TestHandshakeTimesOutWithNoWindowEver pins the other side: no window at any
// point is still a timeout, so a build that never paints is rolled back.
func TestHandshakeTimesOutWithNoWindowEver(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)
	pid := 778
	sys.alive[pid] = true

	out := awaitHandshake(sys, filepath.Join(dir, "missing.ok"), pid, 2*time.Second)
	if out != hsTimeout {
		t.Errorf("outcome = %v, want hsTimeout", out)
	}
}

// windowOnceSys answers processHasWindow from a callback, so a test can make a
// window appear once and then disappear.
type windowOnceSys struct {
	*fakeSys
	onCall func() bool
}

func (w *windowOnceSys) processHasWindow(int) bool { return w.onCall() }

// awaitHandshakeWindowHook runs the real wait with the window-answering sys.
func awaitHandshakeWindowHook(sys updateSys, okPath string, pid int, timeout time.Duration) hsOutcome {
	return awaitHandshake(sys, okPath, pid, timeout)
}

// TestValidationRefusesATruncatedStagedFile pins P1-8's second half: the helper
// used to check only "starts with MZ and is over 1 MiB", which a truncated
// download satisfies. It now re-checks the size and hash the app recorded.
func TestValidationRefusesATruncatedStagedFile(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)
	staged := filepath.Join(dir, "app.exe.new")
	writeFile(t, staged, "MZ"+strings.Repeat("y", 2<<20))
	want, _ := hashFileSHA256(staged)

	if err := validateStaged(sys, staged, sys.size(staged), want); err != nil {
		t.Fatalf("validation rejected a matching file: %v", err)
	}
	if err := validateStaged(sys, staged, sys.size(staged)+1, want); err == nil {
		t.Error("validation accepted a file whose size does not match the record")
	}
	if err := validateStaged(sys, staged, sys.size(staged), "deadbeef"); err == nil {
		t.Error("validation accepted a file whose hash does not match the record")
	}
}

// TestStateLockSerialisesWriters pins P2-7: the app and the helper are separate
// processes sharing one marker, and an unlocked read-modify-write is how a
// helper's "launched" was overwritten by the app's stale "backed-up".
func TestStateLockSerialisesWriters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update-state.json")
	if err := updateStateSave(path, updateState{Stage: stagePrepared}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			st, err := updateStateLoad(path)
			if err != nil || st == nil {
				t.Errorf("load: %v", err)
				return
			}
			st.Stage = stageLaunched
			if err := updateStateSave(path, *st); err != nil {
				t.Errorf("save: %v", err)
			}
		}(i)
	}
	wg.Wait()

	st, err := updateStateLoad(path)
	if err != nil || st == nil {
		t.Fatalf("final load: %v", err)
	}
	if st.Stage != stageLaunched {
		t.Errorf("stage = %q, want %q", st.Stage, stageLaunched)
	}
	// The temporary and lock files must be gone: leaving a .lock behind blocks
	// every later writer for the life of the install.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") || strings.Contains(e.Name(), ".tmp") {
			t.Errorf("leftover helper file: %s", e.Name())
		}
	}
}

// TestMarkStageKeepsConfirmed pins the merge rule that made the lock necessary.
func TestMarkStageKeepsConfirmed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update-state.json")
	if err := updateStateSave(path, updateState{Stage: stageConfirmed}); err != nil {
		t.Fatal(err)
	}
	sys := newFakeSys(t, dir)
	markStage(sys, path, stageAborted, "should be ignored")

	st, err := updateStateLoad(path)
	if err != nil || st == nil {
		t.Fatalf("load: %v", err)
	}
	if st.Stage != stageConfirmed {
		t.Errorf("stage = %q, want the confirmed stage kept", st.Stage)
	}
}

// TestCleanupKeepsTheBackupWithoutAHash pins P2-6's cleanup rule: a confirmed
// record with no hash proves nothing about which build is running, and the
// backup may be the only working copy left.
func TestCleanupKeepsTheBackupWithoutAHash(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}

	exe := filepath.Join(dir, "app.exe")
	bak := exe + ".bak"
	writeFile(t, exe, "NEW")
	writeFile(t, bak, "OLD")
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageConfirmed, Exe: exe, Bak: bak, NewSHA: "",
	}); err != nil {
		t.Fatal(err)
	}

	app.cleanupStaleUpdate()

	if _, err := os.Stat(bak); err != nil {
		t.Errorf("the backup was deleted on an unverifiable record: %v", err)
	}
	if _, err := os.Stat(app.updateStatePath()); err != nil {
		t.Errorf("the record was deleted even though the backup was kept: %v", err)
	}
}

// TestCleanupKeepsTheBackupWhenTheDeleteFails pins the other half of P2-6: the
// marker is only dropped once the backup is really gone, because it is the only
// thing that still records that a backup exists.
func TestCleanupKeepsTheBackupWhenTheDeleteFails(t *testing.T) {
	dir := t.TempDir()
	app := NewAppWithDataDir(dir)
	app.emit = func(string, ...any) {}

	exe := filepath.Join(dir, "app.exe")
	bak := exe + ".bak"
	writeFile(t, exe, "NEW")
	writeFile(t, bak, "OLD")
	sha, _ := hashFileSHA256(exe)
	if err := updateStateSave(app.updateStatePath(), updateState{
		Stage: stageConfirmed, Exe: exe, Bak: bak, NewSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	// A *non-empty* directory in place of the backup makes the remove fail on
	// Windows: os.Remove deletes an empty directory, so it needs a child.
	if err := os.Remove(bak); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(bak, "child"), 0o755); err != nil {
		t.Fatal(err)
	}

	app.cleanupStaleUpdate()

	if _, err := os.Stat(app.updateStatePath()); err != nil {
		t.Errorf("the record was dropped although the backup is still there: %v", err)
	}
}

// TestPreflightRefusesAPathTooLongForTheBackupName pins the MAX_PATH guard: the
// updater appends ".bak" and ".failed-<stamp>", and discovering that after the
// app has quit leaves the user with no running program.
func TestPreflightRefusesAPathTooLongForTheBackupName(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("d", 250), "VKMDownload.exe")
	err := preflightExeDir(long, 0)
	if err == nil {
		t.Fatal("a path too long for the backup name was accepted")
	}
	if !strings.Contains(err.Error(), "太长") {
		t.Errorf("error = %v, want it to name the path length", err)
	}
}

// TestFreezeRestoresWhatWasDownloading pins the P3 rule: an abandoned update used
// to leave every task paused, which looks like the app quietly stopped the user's
// downloads.
func TestFreezeRestoresWhatWasDownloading(t *testing.T) {
	app := newTestApp(t)
	body := payload(2 << 20)
	srv := rangedServer(t, body, 10*time.Millisecond)
	dest := filepath.Join(t.TempDir(), "freeze.bin")

	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/freeze.bin", Dest: dest, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitFor(t, "the task to be downloading", 10*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status == StatusDownloading
	})

	unfreeze, err := app.freezeForUpdate()
	if err != nil {
		t.Fatalf("freezeForUpdate: %v", err)
	}
	if v, _ := find(app.mgr, task.ID); v.Status != StatusPaused {
		t.Fatalf("task status during the freeze = %s, want paused", v.Status)
	}

	unfreeze()

	waitFor(t, "the task to resume after an abandoned update", 20*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && (v.Status == StatusDownloading || v.Status == StatusCompleted)
	})
	_ = app.mgr.Cancel(task.ID)
}

// TestStartUpdateRejectsConcurrentCalls drives the claim path twice in a row
// without any network in between: the second call must observe the claim.
func TestStartUpdateRejectsConcurrentCalls(t *testing.T) {
	app := NewAppWithDataDir(t.TempDir())
	app.emit = func(string, ...any) {}
	app.upd.State = "downloading"

	st, err := app.StartUpdate()
	if err != nil {
		t.Fatalf("StartUpdate on an in-flight download: %v", err)
	}
	if st.State != "downloading" {
		t.Errorf("state = %q, want downloading", st.State)
	}
}
