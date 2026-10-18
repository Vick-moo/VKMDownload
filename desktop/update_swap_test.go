package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------- fake sys

// fakeSys is the update path's outside world under test control. Everything
// file-shaped is delegated to the real implementation, so the tests exercise
// real files in t.TempDir(); only the calls that would need a genuine
// executable (replacing, starting, waiting for and killing a process) are
// answered from the fields below.
type fakeSys struct {
	t    *testing.T
	real realUpdateSys

	hardLinkErr error
	copyErr     error

	// replaceAlwaysErr fails every replacement; replaceFailFirst fails that
	// many and then succeeds.
	replaceAlwaysErr error
	replaceFailFirst int
	replaceCalls     []string

	startAlwaysErr error
	startFailFirst int
	startPid       int
	started        [][]string
	alive          map[int]bool
	window         map[int]bool
	waitErr        error
	killed         []int

	sleeps  []time.Duration
	clock   time.Time
	elapsed time.Duration
	onSleep func(elapsed time.Duration)
	logs    []string
}

func newFakeSys(t *testing.T, dir string) *fakeSys {
	t.Helper()
	return &fakeSys{
		t:        t,
		real:     realUpdateSys{logPath: filepath.Join(dir, "update.log")},
		startPid: 4242,
		alive:    map[int]bool{},
		window:   map[int]bool{},
		clock:    time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC),
	}
}

func (f *fakeSys) hardLink(src, dst string) error {
	if f.hardLinkErr != nil {
		return f.hardLinkErr
	}
	return f.real.hardLink(src, dst)
}

func (f *fakeSys) copyFile(src, dst string) error {
	if f.copyErr != nil {
		return f.copyErr
	}
	return f.real.copyFile(src, dst)
}

func (f *fakeSys) replaceFile(src, dst string) error {
	f.replaceCalls = append(f.replaceCalls, src+"->"+dst)
	if f.replaceAlwaysErr != nil {
		return f.replaceAlwaysErr
	}
	if f.replaceFailFirst > 0 {
		f.replaceFailFirst--
		return errors.New("MoveFileEx: The process cannot access the file because it is being used by another process.")
	}
	return f.real.replaceFile(src, dst)
}

func (f *fakeSys) rename(from, to string) error { return f.real.rename(from, to) }
func (f *fakeSys) remove(path string) error     { return f.real.remove(path) }
func (f *fakeSys) exists(path string) bool      { return f.real.exists(path) }
func (f *fakeSys) size(path string) int64       { return f.real.size(path) }

func (f *fakeSys) sha256File(path string) (string, error) { return hashFileSHA256(path) }

func (f *fakeSys) startProcess(exe string, args ...string) (int, error) {
	f.started = append(f.started, append([]string{exe}, args...))
	if f.startAlwaysErr != nil {
		return 0, f.startAlwaysErr
	}
	if f.startFailFirst > 0 {
		f.startFailFirst--
		return 0, errors.New("CreateProcess failed")
	}
	return f.startPid, nil
}

func (f *fakeSys) processAlive(pid int) bool     { return f.alive[pid] }
func (f *fakeSys) processHasWindow(pid int) bool { return f.window[pid] }

func (f *fakeSys) waitProcess(pid int, timeout time.Duration) error { return f.waitErr }

func (f *fakeSys) killProcess(pid int) error {
	f.killed = append(f.killed, pid)
	delete(f.alive, pid)
	delete(f.window, pid)
	return nil
}

func (f *fakeSys) sleep(d time.Duration) {
	f.sleeps = append(f.sleeps, d)
	f.clock = f.clock.Add(d)
	f.elapsed += d
	if f.onSleep != nil {
		f.onSleep(f.elapsed)
	}
}

// now is the injected clock. It only moves when the code under test sleeps, so
// a sixty-second deadline is reached in microseconds and the back-off ladder can
// be asserted exactly.
func (f *fakeSys) now() time.Time { return f.clock }

func (f *fakeSys) logf(format string, args ...any) {
	f.logs = append(f.logs, format)
}

// ------------------------------------------------------------- fixtures

const oldExeContent = "OLD-VERSION-BYTES"

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// writeFakeExe writes something that passes validateStaged: at least a
// megabyte, starting with the MZ header. It is not a program and never runs;
// it only has to look like one to the checks being tested.
func writeFakeExe(t *testing.T, path, tag string) {
	t.Helper()
	buf := make([]byte, (1<<20)+len(tag))
	copy(buf, "MZ")
	copy(buf[2:], tag)
	if err := os.WriteFile(path, buf, 0o755); err != nil {
		t.Fatal(err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// helperFixture is a directory shaped like an installed, updating app: the live
// executable, the backup the running app made of it, and the downloaded
// replacement — all ordinary files in t.TempDir().
type helperFixture struct {
	dir    string
	exe    string
	bak    string
	staged string
	state  string
	okDir  string
	args   helperArgs
	sys    *fakeSys
}

func newHelperFixture(t *testing.T) *helperFixture {
	t.Helper()
	dir := t.TempDir()
	f := &helperFixture{
		dir:    dir,
		exe:    filepath.Join(dir, "VKMDownload.exe"),
		bak:    filepath.Join(dir, "VKMDownload.exe.bak"),
		staged: filepath.Join(dir, "VKMDownload.exe.new"),
		state:  filepath.Join(dir, "update-state.json"),
		okDir:  filepath.Join(dir, "update"),
	}
	writeFile(t, f.exe, oldExeContent)
	writeFile(t, f.bak, oldExeContent)
	writeFakeExe(t, f.staged, "NEW-VERSION")
	if err := os.MkdirAll(f.okDir, 0o755); err != nil {
		t.Fatal(err)
	}
	f.sys = newFakeSys(t, dir)
	f.args = helperArgs{
		Mode:   modeApply,
		Exe:    f.exe,
		Bak:    f.bak,
		Staged: f.staged,
		State:  f.state,
		OkDir:  f.okDir,
		Token:  strings.Repeat("ab", 32),
		Parent: 1234,
	}
	return f
}

func (f *helperFixture) stage(t *testing.T) string {
	t.Helper()
	st, err := updateStateLoad(f.state)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	if st == nil {
		return ""
	}
	return st.Stage
}

func (f *helperFixture) writeState(t *testing.T, st updateState) {
	t.Helper()
	if err := updateStateSave(f.state, st); err != nil {
		t.Fatal(err)
	}
}

func (f *helperFixture) failedFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), ".failed-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// ------------------------------------------------------------- the swap

func TestSwapFailureLeavesTheOldVersionInPlace(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.replaceAlwaysErr = errors.New("MoveFileEx: sharing violation")

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a failed replacement reported success")
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("the live executable changed to %q, want the old content", got)
	}
	if !f.sys.exists(f.bak) {
		t.Error("the backup was removed; it is the only other copy of the old version")
	}
	// The old version must be running again: the process that launched this
	// helper has already quit, so a failed swap that starts nothing leaves the
	// machine with the program icon on the desktop and nothing behind it.
	if len(f.sys.started) != 1 || len(f.sys.started[0]) == 0 || f.sys.started[0][0] != f.exe {
		t.Errorf("started = %v, want exactly the old version relaunched at %s", f.sys.started, f.exe)
	}
	if got := f.stage(t); got != stageAborted {
		t.Errorf("stage = %q, want %q", got, stageAborted)
	}
	// The staged file is kept: the retry from the settings page must be able to
	// reuse it rather than re-download 13 MB.
	if !f.sys.exists(f.staged) {
		t.Error("the staged download was consumed or deleted by a failed swap")
	}
}

func TestSwapRetriesThenSucceeds(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.replaceFailFirst = 3
	f.sys.alive[f.sys.startPid] = true
	// Confirm on the first poll so the helper stops cleanly.
	if err := os.MkdirAll(f.okDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeHandshake(filepath.Join(f.okDir, f.args.Token+".ok"), f.sys.startPid); err != nil {
		t.Fatal(err)
	}

	if code := runHelper(f.args, f.sys); code != 0 {
		t.Fatalf("helper exit code = %d, want 0", code)
	}
	if got := f.stage(t); got != stageConfirmed {
		t.Errorf("stage = %q, want %q", got, stageConfirmed)
	}
	if len(f.sys.replaceCalls) != 4 {
		t.Errorf("replace attempts = %d, want 4 (3 failures then a success)", len(f.sys.replaceCalls))
	}
	want := []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, time.Second}
	if len(f.sys.sleeps) < len(want) {
		t.Fatalf("slept %v, want at least %v", f.sys.sleeps, want)
	}
	for i, w := range want {
		if f.sys.sleeps[i] != w {
			t.Errorf("back-off %d = %v, want %v (sequence %v)", i, f.sys.sleeps[i], w, f.sys.sleeps)
		}
	}
}

func TestSwapGivesUpAtTheDeadline(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.replaceAlwaysErr = errors.New("locked")

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a permanently locked file reported success")
	}
	if got := f.stage(t); got != stageAborted {
		t.Errorf("stage = %q, want %q", got, stageAborted)
	}
	// The loop must have consumed at least the documented 60 seconds of
	// back-off rather than spinning or giving up on the first failure.
	var total time.Duration
	for _, d := range f.sys.sleeps {
		total += d
	}
	if total < swapTimeout {
		t.Errorf("total back-off = %v, want at least %v", total, swapTimeout)
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("the live executable changed to %q", got)
	}
}

func TestBackupExeFallsBackToACopy(t *testing.T) {
	f := newHelperFixture(t)
	if err := os.Remove(f.bak); err != nil {
		t.Fatal(err)
	}
	f.sys.hardLinkErr = errors.New("this file system does not support hard links")

	if err := backupExe(f.sys, f.exe, f.bak); err != nil {
		t.Fatalf("backupExe: %v", err)
	}
	if got := readFileString(t, f.bak); got != oldExeContent {
		t.Errorf("backup content = %q, want %q", got, oldExeContent)
	}
	if f.sys.size(f.bak) != f.sys.size(f.exe) {
		t.Error("backup size differs from the original")
	}
}

func TestBackupExeRejectsATruncatedCopy(t *testing.T) {
	f := newHelperFixture(t)
	if err := os.Remove(f.bak); err != nil {
		t.Fatal(err)
	}
	f.sys.hardLinkErr = errors.New("no links")
	// A copy that quietly writes less than the original is not a backup; the
	// size check inside backupExe must catch it.
	shrunk := &shrinkingCopySys{fakeSys: f.sys}
	if err := backupExe(shrunk, f.exe, f.bak); err == nil {
		t.Fatal("a truncated backup was accepted")
	}
}

// shrinkingCopySys writes a short file instead of a real copy, standing in for
// a disk that filled up mid-copy.
type shrinkingCopySys struct {
	*fakeSys
}

func (s *shrinkingCopySys) copyFile(src, dst string) error {
	return os.WriteFile(dst, []byte("short"), 0o755)
}

func TestValidateStagedRefusesBadFiles(t *testing.T) {
	dir := t.TempDir()
	sys := newFakeSys(t, dir)

	good := filepath.Join(dir, "good.exe")
	writeFakeExe(t, good, "GOOD")
	sha, err := hashFileSHA256(good)
	if err != nil {
		t.Fatal(err)
	}

	small := filepath.Join(dir, "small.exe")
	writeFile(t, small, "MZtiny")

	notExe := filepath.Join(dir, "notes.txt")
	writeFile(t, notExe, strings.Repeat("x", 1<<20))

	cases := []struct {
		name   string
		path   string
		size   int64
		sha    string
		expect string
	}{
		{"missing", filepath.Join(dir, "gone.exe"), 0, "", "不在了"},
		{"too small", small, 0, "", "字节"},
		{"not an executable", notExe, 0, "", "可执行文件"},
		{"hash mismatch", good, 0, strings.Repeat("0", 64), "校验失败"},
		{"size mismatch", good, 12345, sha, "不一致"},
		{"good", good, 0, sha, ""},
	}
	for _, c := range cases {
		err := validateStaged(sys, c.path, c.size, c.sha)
		if c.expect == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", c.name, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%s: accepted, want a refusal", c.name)
			continue
		}
		if !strings.Contains(err.Error(), c.expect) {
			t.Errorf("%s: error = %q, want it to mention %q", c.name, err, c.expect)
		}
	}
}

// ------------------------------------------------------------- handshake

func TestHelperConfirmsWhenTheNewVersionWritesTheHandshake(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.alive[f.sys.startPid] = true
	if err := writeHandshake(filepath.Join(f.okDir, f.args.Token+".ok"), f.sys.startPid); err != nil {
		t.Fatal(err)
	}

	if code := runHelper(f.args, f.sys); code != 0 {
		t.Fatalf("helper exit = %d, want 0", code)
	}
	if got := f.stage(t); got != stageConfirmed {
		t.Errorf("stage = %q, want %q", got, stageConfirmed)
	}
	// The live executable is now the staged build, and the backup — the only
	// other copy of a working program — is still there for the new version to
	// clean up (this process runs *from* it and cannot delete its own image).
	if got := f.sys.size(f.exe); got < 1<<20 {
		t.Errorf("live executable is %d bytes, want the staged build", got)
	}
	if got := readFileString(t, f.bak); got != oldExeContent {
		t.Errorf("backup content = %q, want %q", got, oldExeContent)
	}
	if len(f.sys.started) != 1 {
		t.Errorf("started %v, want exactly the new version", f.sys.started)
	}
	if len(f.sys.killed) != 0 {
		t.Errorf("killed %v, want nothing killed", f.sys.killed)
	}
}

func TestHelperAcceptsAHandshakeOnlyForItsOwnPid(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.alive[f.sys.startPid] = true
	// A handshake left behind by an earlier, unrelated attempt: same token, but
	// a different process. It must not pass as this attempt's confirmation.
	if err := writeHandshake(filepath.Join(f.okDir, f.args.Token+".ok"), 999999); err != nil {
		t.Fatal(err)
	}
	// The stale file never matches, the process never dies and shows no window,
	// so the helper must time out and roll back.
	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a handshake for another pid was treated as confirmation")
	}
	if got := f.stage(t); got != stageRolledBack {
		t.Errorf("stage = %q, want %q", got, stageRolledBack)
	}
}

func TestHelperRollsBackWhenTheNewVersionDiesAtOnce(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.alive[f.sys.startPid] = false // gone before it said anything

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a new version that exited immediately was reported as a success")
	}
	if got := f.stage(t); got != stageRolledBack {
		t.Errorf("stage = %q, want %q", got, stageRolledBack)
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("live executable = %q, want the old version restored", got)
	}
	failed := f.failedFiles(t)
	if len(failed) != 1 {
		t.Fatalf("failed-build evidence = %v, want exactly one file", failed)
	}
	if got := readFileString(t, filepath.Join(f.dir, failed[0])); !strings.HasPrefix(got, "MZ") {
		t.Error("the kept-aside build is not the one that failed")
	}
	if len(f.sys.started) != 2 {
		t.Errorf("started %v, want the new version then the restored old one", f.sys.started)
	}
}

func TestHelperHandshakeTimeoutRollsBack(t *testing.T) {
	f := newHelperFixture(t)
	// Alive but invisible: no handshake and no window, so it is not running in
	// any usable sense.
	f.sys.alive[f.sys.startPid] = true

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a silent new version was reported as a success")
	}
	if got := f.stage(t); got != stageRolledBack {
		t.Errorf("stage = %q, want %q", got, stageRolledBack)
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("live executable = %q, want the old version restored", got)
	}
	if len(f.sys.killed) != 1 || f.sys.killed[0] != f.sys.startPid {
		t.Errorf("killed %v, want exactly the pid this helper started", f.sys.killed)
	}
}

func TestHelperKeepsAnUnconfirmedButVisibleNewVersion(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.alive[f.sys.startPid] = true
	f.sys.window[f.sys.startPid] = true

	code := runHelper(f.args, f.sys)
	if code == 0 {
		t.Fatal("an unconfirmed start was reported as a full success")
	}
	if got := f.stage(t); got != stageLaunched {
		t.Errorf("stage = %q, want %q (not a rollback)", got, stageLaunched)
	}
	if got := readFileString(t, f.exe); strings.HasPrefix(got, "MZ") == false {
		t.Errorf("the running new version was replaced by %q; rolling back a visible process is worse than reporting it", got)
	}
	if !f.sys.exists(f.bak) {
		t.Error("the backup was removed; the user must still be able to restore it by hand")
	}
	if len(f.sys.killed) != 0 {
		t.Errorf("killed %v, want nothing killed", f.sys.killed)
	}
}

func TestHelperTouchesNothingWhenTheOldProcessDoesNotExit(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.alive[f.args.Parent] = true
	f.sys.waitErr = errors.New("进程 1234 在 30s 内没有退出")

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a helper that never got a free executable reported success")
	}
	if got := f.stage(t); got != stageAborted {
		t.Errorf("stage = %q, want %q", got, stageAborted)
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("live executable = %q, want it untouched", got)
	}
	if !f.sys.exists(f.bak) || !f.sys.exists(f.staged) {
		t.Error("files were touched while the old process was still running")
	}
	if len(f.sys.replaceCalls) != 0 || len(f.sys.started) != 0 {
		t.Errorf("files were replaced (%v) or processes started (%v) before the old process exited",
			f.sys.replaceCalls, f.sys.started)
	}
}

func TestHelperRollsBackWhenStartingTheNewVersionFails(t *testing.T) {
	f := newHelperFixture(t)
	f.sys.startFailFirst = 1 // the new version cannot be started at all

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a failed launch reported success")
	}
	if got := f.stage(t); got != stageRolledBack {
		t.Errorf("stage = %q, want %q", got, stageRolledBack)
	}
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("live executable = %q, want the old version back", got)
	}
	if len(f.failedFiles(t)) != 1 {
		t.Errorf("failed-build evidence = %v, want one file", f.failedFiles(t))
	}
	// The old version is restarted; a rollback that leaves the user with no
	// running program is how the reported incident ended.
	if len(f.sys.started) != 2 {
		t.Errorf("started %v, want the attempted new version then the restored old one", f.sys.started)
	}
}

func TestRollbackKeepsTheBackupWhenThereIsNothingToRestoreFrom(t *testing.T) {
	f := newHelperFixture(t)
	if err := os.Remove(f.bak); err != nil {
		t.Fatal(err)
	}
	f.args.Mode = modeRollback

	if code := runHelper(f.args, f.sys); code == 0 {
		t.Fatal("a rollback without a backup reported success")
	}
	if got := f.stage(t); got != stageAborted {
		t.Errorf("stage = %q, want %q", got, stageAborted)
	}
	// Deleting the only remaining executable would be the worst possible
	// outcome; it must still be there, untouched.
	if got := readFileString(t, f.exe); got != oldExeContent {
		t.Errorf("the live executable was disturbed: %q", got)
	}
	if len(f.sys.started) != 0 {
		t.Errorf("a process was started from a rollback that could not happen: %v", f.sys.started)
	}
}

func TestMarkStageNeverDowngradesAConfirmedUpdate(t *testing.T) {
	f := newHelperFixture(t)
	f.writeState(t, updateState{Stage: stageConfirmed, Exe: f.exe, Bak: f.bak,
		NewSHA: "abc", Version: "0.2.7"})

	// The second, losing helper of a racing pair writes "aborted" after the
	// winner already confirmed. The record must keep saying confirmed, or the
	// next launch tells the user an update that worked never happened.
	markStage(f.sys, f.state, stageAborted, "替换文件失败")

	st, err := updateStateLoad(f.state)
	if err != nil || st == nil {
		t.Fatalf("state: %v", err)
	}
	if st.Stage != stageConfirmed {
		t.Errorf("stage = %q, want %q", st.Stage, stageConfirmed)
	}
	if st.NewSHA != "abc" || st.Bak != f.bak {
		t.Errorf("the confirmed record lost its fields: %+v", st)
	}
}

// -------------------------------------------------------- helper argument

func TestHelperArgsRoundTrip(t *testing.T) {
	f := newHelperFixture(t)
	got, ok := parseHelperArgs(f.args.argv())
	if !ok {
		t.Fatal("parseHelperArgs did not recognise the helper invocation")
	}
	if got != f.args {
		t.Errorf("round trip = %+v, want %+v", got, f.args)
	}

	if _, ok := parseHelperArgs([]string{"--update-handshake", strings.Repeat("ab", 32)}); ok {
		t.Error("a normal launch with a handshake token was parsed as a helper invocation")
	}
	if tok := parseHandshakeToken([]string{"--update-handshake", strings.Repeat("ab", 32)}); tok == "" {
		t.Error("the handshake token was not recognised")
	}
	if tok := parseHandshakeToken([]string{"--update-handshake", "../../evil"}); tok != "" {
		t.Errorf("an unsafe token was accepted: %q", tok)
	}
	if safeUpdateToken(strings.Repeat("z", 64)) {
		t.Error("a non-hex token was accepted as a file name")
	}
}

// ------------------------------------------------------------- the notice

func TestConsumeUpdateNoticeSpeaksOncePerAttempt(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)
	a.sys = newSysStub(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "NEW-BUILD")
	sha, err := hashFileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}
	st := updateState{
		Stage:  stageLaunched,
		Exe:    exe,
		Bak:    filepath.Join(dir, "VKMDownload.exe.bak"),
		NewSHA: sha,
		Reason: "新版在运行但没有确认握手",
	}
	if err := updateStateSave(a.updateStatePath(), st); err != nil {
		t.Fatal(err)
	}

	first := a.ConsumeUpdateNotice()
	if first == "" {
		t.Fatal("no notice for an update that was never confirmed")
	}
	if !strings.Contains(first, "没有最终确认") {
		t.Errorf("notice = %q, want it to describe the unconfirmed update", first)
	}
	if second := a.ConsumeUpdateNotice(); second != "" {
		t.Errorf("the notice was handed out twice: %q", second)
	}
	// The record survives (so a manual restore still has the backup path), with
	// only the "already shown" flag set.
	reloaded, err := updateStateLoad(a.updateStatePath())
	if err != nil || reloaded == nil {
		t.Fatalf("state after the notice: %v", err)
	}
	if !reloaded.Notified {
		t.Error("the state was not marked as notified")
	}
	if reloaded.Bak != st.Bak {
		t.Error("the notice consumed the backup path from the record")
	}
}

func TestConsumeUpdateNoticeForEachStage(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)
	a.sys = newSysStub(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "OLD-BUILD")
	oldSHA, err := hashFileSHA256(exe)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		st     updateState
		noFile bool
		want   string
		empty  bool
	}{
		{"no record", updateState{}, true, "", true},
		{"rolled back", updateState{Stage: stageRolledBack, Exe: exe, NewSHA: "deadbeef"}, false, "已自动恢复", false},
		{"never started", updateState{Stage: stagePrepared, Exe: exe}, false, "没有开始", false},
		{"backed up only", updateState{Stage: stageBackedUp, Exe: exe}, false, "没有开始", false},
		{"aborted", updateState{Stage: stageAborted, Exe: exe, Reason: "替换文件失败"}, false, "没有完成", false},
		{"still the old build", updateState{Stage: stageLaunched, Exe: exe, OldSHA: oldSHA, NewSHA: "beef"}, false, "没有生效", false},
		{"unknown stage", updateState{Stage: "something-else", Exe: exe}, false, "状态未知", false},
	}
	for _, c := range cases {
		_ = os.Remove(a.updateStatePath())
		if !c.noFile {
			if err := updateStateSave(a.updateStatePath(), c.st); err != nil {
				t.Fatal(err)
			}
		}
		got := a.ConsumeUpdateNotice()
		if c.empty {
			if got != "" {
				t.Errorf("%s: notice = %q, want none", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: notice = %q, want it to mention %q", c.name, got, c.want)
		}
	}
}

func TestConsumeUpdateNoticeLeavesEverythingAloneWhenTheRecordIsCorrupt(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)
	a.sys = newSysStub(dir)

	bak := filepath.Join(dir, "VKMDownload.exe.bak")
	writeFile(t, bak, oldExeContent)
	if err := os.WriteFile(a.updateStatePath(), []byte("{ this is not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := a.ConsumeUpdateNotice(); got != "" {
		t.Errorf("notice = %q, want silence for an unreadable record", got)
	}
	// The backup may be the only working copy of the program on this machine;
	// an unparsable marker must not be a reason to touch it.
	if got := readFileString(t, bak); got != oldExeContent {
		t.Errorf("the backup was modified: %q", got)
	}
	if !a.syscalls().exists(bak) {
		t.Error("the backup was deleted because the marker could not be parsed")
	}
	if !a.syscalls().exists(a.updateStatePath()) {
		t.Error("the corrupt marker was deleted instead of being left for inspection")
	}
}

func TestConsumeUpdateNoticeIsQuietAfterASuccessfulUpdate(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)
	a.sys = newSysStub(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "NEW-BUILD")
	sha, _ := hashFileSHA256(exe)
	st := updateState{Stage: stageConfirmed, Exe: exe, NewSHA: sha,
		Bak: filepath.Join(dir, "VKMDownload.exe.bak")}
	writeFile(t, st.Bak, "OLD-BUILD")
	if err := updateStateSave(a.updateStatePath(), st); err != nil {
		t.Fatal(err)
	}

	if got := a.ConsumeUpdateNotice(); got != "" {
		t.Errorf("notice = %q, want silence after a confirmed update", got)
	}
	// startup sweeps the confirmed leftovers: the backup is no longer needed
	// and the record has served its purpose.
	a.cleanupStaleUpdate()
	if a.syscalls().exists(st.Bak) {
		t.Error("the backup of a confirmed update was not cleaned up")
	}
	if a.syscalls().exists(a.updateStatePath()) {
		t.Error("the record of a confirmed update was not cleaned up")
	}
}

func TestCleanupStaleUpdateKeepsTheBackupWhenTheBuildDoesNotMatch(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)
	a.sys = newSysStub(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "SOMETHING-ELSE")
	bak := filepath.Join(dir, "VKMDownload.exe.bak")
	writeFile(t, bak, oldExeContent)
	st := updateState{Stage: stageConfirmed, Exe: exe, Bak: bak, NewSHA: "not-this-build"}
	if err := updateStateSave(a.updateStatePath(), st); err != nil {
		t.Fatal(err)
	}

	a.cleanupStaleUpdate()
	if !a.syscalls().exists(bak) {
		t.Error("the backup was deleted even though the running build did not match the record")
	}
}

// newSysStub is the real implementation with a log file inside the test's temp
// directory, so nothing the notice path does can reach the user's own files.
func newSysStub(dir string) updateSys {
	return realUpdateSys{logPath: filepath.Join(dir, "update.log")}
}

// ------------------------------------------------------------ ApplyUpdate

// TestApplyUpdateUnfreezesWhenTheQuitIsRefused is the regression test for the
// reported incident. Wails' Windows Frontend.Quit consults OnBeforeClose first,
// and with "minimise to tray" on that hook hides the window and refuses the
// exit. The old code launched its updater and then carried on as if the process
// were about to die, leaving every task frozen in "updating" for ever.
func TestApplyUpdateUnfreezesWhenTheQuitIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	staged := filepath.Join(dir, "VKMDownload.exe.new")
	writeFile(t, exe, oldExeContent)
	writeFakeExe(t, staged, "NEW-BUILD")

	sys := newFakeSys(t, dir)
	a.sys = sys
	a.exePaths = func() (string, string, error) { return exe, staged, nil }
	// The quit is accepted in the sense that it returns, but nothing exits —
	// which is precisely what a vetoed Quit looks like from in here.
	a.quit = func(context.Context) {}
	a.sleepFor = func(time.Duration) {}

	sha, err := hashFileSHA256(staged)
	if err != nil {
		t.Fatal(err)
	}
	a.updMu.Lock()
	a.upd = UpdateStatus{State: "ready", Version: "0.2.7"}
	a.updStagedSize, a.updStagedSHA = sys.size(staged), sha
	a.updMu.Unlock()

	if err := a.ApplyUpdate(); err == nil {
		t.Fatal("ApplyUpdate reported success although the process never exited")
	}
	if a.mgr.updating {
		t.Error("the scheduler is still frozen: every task would wait for ever")
	}
	if st := a.UpdateProgress(); st.State != "failed" {
		t.Errorf("update state = %q, want %q", st.State, "failed")
	}
	if len(sys.started) != 1 {
		t.Fatalf("started %v, want exactly one helper", sys.started)
	}
	if got := sys.started[0][0]; got != exe+".bak" {
		t.Errorf("helper started from %q, want the backup %q", got, exe+".bak")
	}
	// The backup is a real copy of the running version, made before anything
	// else happened.
	if got := readFileString(t, exe+".bak"); got != oldExeContent {
		t.Errorf("backup content = %q, want %q", got, oldExeContent)
	}
	st, err := updateStateLoad(a.updateStatePath())
	if err != nil || st == nil {
		t.Fatalf("state: %v", err)
	}
	if st.Stage != stageBackedUp {
		t.Errorf("stage = %q, want %q", st.Stage, stageBackedUp)
	}
	if st.NewSHA != sha {
		t.Error("the staged build's hash was not recorded in the marker")
	}
	// Nothing was swapped: the helper is the only thing that touches files, and
	// it is still waiting for a process that never went away.
	if got := readFileString(t, exe); got != oldExeContent {
		t.Errorf("the live executable changed to %q", got)
	}
	if !sys.exists(staged) {
		t.Error("the staged file was consumed")
	}
}

func TestApplyUpdateRefusesReentryAndAnUnpreparedDownload(t *testing.T) {
	a := NewAppWithDataDir(t.TempDir())
	a.sys = newFakeSys(t, t.TempDir())

	a.updMu.Lock()
	a.upd = UpdateStatus{State: "applying"}
	a.updMu.Unlock()
	if err := a.ApplyUpdate(); err == nil || !strings.Contains(err.Error(), "正在进行") {
		t.Errorf("second ApplyUpdate = %v, want a refusal while one is in flight", err)
	}

	a.updMu.Lock()
	a.upd = UpdateStatus{State: "idle"}
	a.updMu.Unlock()
	if err := a.ApplyUpdate(); err == nil || !strings.Contains(err.Error(), "还没下载好") {
		t.Errorf("ApplyUpdate without a ready download = %v, want a refusal", err)
	}
}

func TestApplyUpdateRefusesABadStagedFile(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	staged := filepath.Join(dir, "VKMDownload.exe.new")
	writeFile(t, exe, oldExeContent)
	// Right size range, wrong content: not an executable at all.
	notExe := make([]byte, 2<<20)
	copy(notExe, "PK")
	if err := os.WriteFile(staged, notExe, 0o755); err != nil {
		t.Fatal(err)
	}

	sys := newFakeSys(t, dir)
	a.sys = sys
	a.exePaths = func() (string, string, error) { return exe, staged, nil }
	a.sleepFor = func(time.Duration) {}

	a.updMu.Lock()
	a.upd = UpdateStatus{State: "ready"}
	a.updStagedSize, a.updStagedSHA = sys.size(staged), ""
	a.updMu.Unlock()

	if err := a.ApplyUpdate(); err == nil {
		t.Fatal("a non-executable staged file was accepted")
	}
	if a.mgr.updating {
		t.Error("the scheduler stayed frozen after a refused staging")
	}
	// Refusing before the backup is the cheap outcome: nothing was started and
	// no backup was left behind to confuse the next attempt.
	if len(sys.started) != 0 {
		t.Errorf("a helper was started for a bad staged file: %v", sys.started)
	}
	if sys.exists(exe + ".bak") {
		t.Error("a backup was made for a staged file that failed validation")
	}
}

func TestRollBackUpdateRefusesWithoutSomethingToRestore(t *testing.T) {
	a := NewAppWithDataDir(t.TempDir())
	a.sys = newFakeSys(t, t.TempDir())

	if err := a.RollBackUpdate(); err == nil {
		t.Error("RollBackUpdate without a record = nil, want a refusal")
	}

	dir := t.TempDir()
	b := NewAppWithDataDir(dir)
	b.sys = newFakeSys(t, dir)
	st := updateState{Stage: stageLaunched, Exe: filepath.Join(dir, "VKMDownload.exe"),
		Bak: filepath.Join(dir, "VKMDownload.exe.bak")}
	if err := updateStateSave(b.updateStatePath(), st); err != nil {
		t.Fatal(err)
	}
	if err := b.RollBackUpdate(); err == nil || !strings.Contains(err.Error(), "备份已不在") {
		t.Errorf("RollBackUpdate with the backup gone = %v, want a refusal", err)
	}
}

// TestWriteUpdateHandshakeIsWhatConfirmsTheUpdate pins the two halves of the
// handshake: the new process writes it at domReady, and the marker it leaves
// behind says the update was confirmed, which is what permits the next start to
// clean the backup up.
func TestWriteUpdateHandshakeIsWhatConfirmsTheUpdate(t *testing.T) {
	dir := t.TempDir()
	a := NewAppWithDataDir(dir)

	exe := filepath.Join(dir, "VKMDownload.exe")
	writeFile(t, exe, "NEW-BUILD")
	sha, _ := hashFileSHA256(exe)
	// The confirmation is only allowed from the build the record names, so the
	// running image has to be the fixture rather than the test binary.
	a.selfPath = func() (string, error) { return exe, nil }
	st := updateState{Stage: stageLaunched, Exe: exe, NewSHA: sha, Version: "0.2.7",
		Bak: filepath.Join(dir, "VKMDownload.exe.bak")}
	writeFile(t, st.Bak, oldExeContent)
	if err := updateStateSave(a.updateStatePath(), st); err != nil {
		t.Fatal(err)
	}

	// A normal launch has no token and must write nothing.
	a.writeUpdateHandshake()
	if entries, _ := os.ReadDir(a.updateDir()); len(entries) != 0 {
		t.Errorf("a launch without a token wrote a handshake: %v", entries)
	}

	tok := strings.Repeat("cd", 32)
	a.updateToken = tok
	a.writeUpdateHandshake()

	pid, ok := readHandshakePID(a.syscalls(), filepath.Join(a.updateDir(), tok+".ok"))
	if !ok {
		t.Fatal("no handshake file after writeUpdateHandshake")
	}
	if pid != os.Getpid() {
		t.Errorf("handshake pid = %d, want this process (%d)", pid, os.Getpid())
	}
	after, err := updateStateLoad(a.updateStatePath())
	if err != nil || after == nil {
		t.Fatalf("state: %v", err)
	}
	if after.Stage != stageConfirmed {
		t.Errorf("stage = %q, want %q", after.Stage, stageConfirmed)
	}
}

// TestHandshakeWillNotConfirmFromAnotherBuild pins the P2-6 rule: the evidence
// must come from the build the record names. Otherwise any executable launched
// with the flag stamps somebody else's update as done, and the next start then
// deletes the backup of a version that was never replaced.
func TestHandshakeWillNotConfirmFromAnotherBuild(t *testing.T) {
	cases := map[string]func(*App, string){
		"different image": func(a *App, exe string) {
			a.selfPath = func() (string, error) { return exe + ".other", nil }
		},
		"same path, wrong bytes": func(a *App, exe string) {
			a.selfPath = func() (string, error) { return exe, nil }
			// Corrupt the file after the record was written: the path matches,
			// the bytes no longer do, and the hash check must catch it.
			writeFile(t, exe, "TAMPERED")
		},
		"record has no hash": func(a *App, exe string) {
			a.selfPath = func() (string, error) { return exe, nil }
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a := NewAppWithDataDir(dir)
			a.emit = func(string, ...any) {}
			exe := filepath.Join(dir, "VKMDownload.exe")
			writeFile(t, exe, "NEW-BUILD")
			sha, _ := hashFileSHA256(exe)
			st := updateState{Stage: stageLaunched, Exe: exe, NewSHA: sha, Version: "0.2.7",
				Bak: filepath.Join(dir, "VKMDownload.exe.bak")}
			if name == "record has no hash" {
				st.NewSHA = ""
			}
			writeFile(t, st.Bak, oldExeContent)
			if err := updateStateSave(a.updateStatePath(), st); err != nil {
				t.Fatal(err)
			}
			setup(a, exe)

			a.updateToken = strings.Repeat("ef", 32)
			a.writeUpdateHandshake()

			after, err := updateStateLoad(a.updateStatePath())
			if err != nil || after == nil {
				t.Fatalf("state: %v", err)
			}
			if after.Stage == stageConfirmed {
				t.Errorf("stage = confirmed, want it left at %q", stageLaunched)
			}
		})
	}
}
