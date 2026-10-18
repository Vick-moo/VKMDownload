package main

// The self-update hand-off. This file holds everything that happens between
// "the user clicked update" and "a new executable is running in place", split
// into small functions over an injected updateSys, so the whole state machine
// can be exercised against t.TempDir() files rather than the real application
// image.
//
// Two field incidents shaped it:
//
//   - A PowerShell helper waited for the app to exit, but the app never did:
//     Wails' Windows Frontend.Quit consults OnBeforeClose first, and
//     "minimise to tray" turns that into a hide and vetoes the quit. The helper
//     is now the app's own image, and the caller records the exit intent
//     (requestQuit) before asking to quit, then watches for the exit to really
//     happen.
//   - That script removed the backup on every retry round, so a failed move
//     destroyed the only copy of the old version. Nothing here ever deletes the
//     backup except the new version, after it has confirmed it is alive.

import (
	"crypto/rand"
	"sync/atomic"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ------------------------------------------------------------ helper flags

const (
	helperFlagApply    = "--apply-update"
	helperFlagRollback = "--rollback"
	handshakeFlag      = "--update-handshake"

	modeApply    = "apply"
	modeRollback = "rollback"
)

// Timings. Each one is deliberately generous: a slow antivirus scan of a fresh
// 13 MB executable is the normal case, not an error.
const (
	// helperParentTimeout is how long the helper waits for the process that
	// started it to disappear before giving up and touching nothing.
	helperParentTimeout = 30 * time.Second
	// swapTimeout caps the retry loop around the single atomic replace. The
	// replacement is normally instantaneous; the loop exists for the Windows
	// case where Defender still holds the freshly written staged file.
	swapTimeout = 60 * time.Second
	// handshakeTimeout bounds the wait for the new executable to reach
	// domReady and write its handshake file. WebView2's cold start is 1-3
	// seconds; a Defender first-scan of a brand-new exe can take 10-30.
	handshakeTimeout = 60 * time.Second
	// handshakePoll is the helper's check interval.
	handshakePoll = 200 * time.Millisecond
)

// updateSys is every operating-system call the swap path makes, behind one
// interface so tests can drive failures (a locked file, a process that dies at
// once, a clock they control) without a real executable anywhere in sight.
type updateSys interface {
	// hardLink creates dst as a second name for src (same volume, no copy).
	hardLink(src, dst string) error
	// copyFile copies src to dst and is only used when hardLink is
	// unavailable (different volume, FAT, a share).
	copyFile(src, dst string) error
	// replaceFile atomically replaces dst with src, leaving no window where
	// dst does not exist. The old dst content survives a failure untouched.
	replaceFile(src, dst string) error
	// rename moves a file inside one volume, failing if dst exists.
	rename(from, to string) error
	remove(path string) error
	exists(path string) bool
	size(path string) int64
	sha256File(path string) (string, error)

	// startProcess launches a detached process with no console window and
	// returns its pid.
	startProcess(exe string, args ...string) (int, error)
	processAlive(pid int) bool
	processHasWindow(pid int) bool
	// waitProcess blocks until pid exits or the timeout passes.
	waitProcess(pid int, timeout time.Duration) error
	killProcess(pid int) error

	sleep(d time.Duration)
	now() time.Time
	logf(format string, args ...any)
}

// realUpdateSys is the production implementation. logPath keeps the helper's
// diagnostics next to the app's own log; the zero value falls back to the temp
// directory, which is all the in-process caller ever needs (it logs through
// App.debugLog instead).
type realUpdateSys struct {
	logPath string
}

func (s realUpdateSys) logf(format string, args ...any) {
	path := s.logPath
	if path == "" {
		path = filepath.Join(os.TempDir(), "vkmdownload-update.log")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = fmt.Fprintf(f, "%s [update] %s\n",
		time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func (realUpdateSys) now() time.Time               { return time.Now() }
func (realUpdateSys) sleep(d time.Duration)        { time.Sleep(d) }
func (realUpdateSys) rename(from, to string) error { return os.Rename(from, to) }
func (realUpdateSys) remove(path string) error     { return os.Remove(path) }

func (realUpdateSys) exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func (realUpdateSys) size(path string) int64 {
	st, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return st.Size()
}

func (realUpdateSys) sha256File(path string) (string, error) {
	return hashFileSHA256(path)
}

// copyFile mirrors the file byte for byte. It is the hardLink fallback, so it
// also verifies the length: a truncated copy would be a backup that cannot
// restore anything, and the whole point of the backup is that the update can be
// undone.
func (s realUpdateSys) copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	want := s.size(src)
	if want < 0 {
		return fmt.Errorf("备份源文件不存在: %s", src)
	}

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, in)
	if err != nil {
		_ = out.Close()
		return err
	}
	// The copy is only a usable backup once it is on disk, not in the page
	// cache: the whole point is to survive a crash right after this.
	_ = out.Sync()
	if err := out.Close(); err != nil {
		return err
	}
	if n != want {
		return fmt.Errorf("备份不完整：写入 %d 字节，应为 %d", n, want)
	}
	return nil
}

// ------------------------------------------------------------ state markers

// updateState is the stage record the helper and the next launch share. It
// lives in the data directory, never next to the executable: an MSI-style
// install under Program Files is not writable for the user running the app.
type updateState struct {
	Stage   string `json:"stage"`
	Version string `json:"version"`
	Exe     string `json:"exe"`
	Bak     string `json:"bak"`
	Staged  string `json:"staged"`
	// OldSHA/NewSHA let the next launch tell which of the two builds is
	// actually running now, which is the only reliable way to know whether an
	// update that was never confirmed in fact succeeded.
	OldSHA string `json:"oldSha256"`
	NewSHA string `json:"newSha256"`
	// NewSize is the byte count verified at download time. The helper re-checks
	// it before swapping: a staged file that was truncated after it was
	// verified (a partial antivirus quarantine, a half-written copy) would
	// otherwise be installed as the program.
	NewSize int64 `json:"newSize,omitempty"`
	Failed   string `json:"failedPath,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Notified bool   `json:"notified,omitempty"`
	Time     string `json:"time"`
}

// The stage ladder. prepared -> backed-up -> replaced -> launched -> confirmed,
// with rolled-back and aborted as the two terminal failures. "launched" is also
// where a start that timed out but looks alive ends up (Reason explains it): an
// automatic rollback there would replace a working new version with an old one,
// which is strictly worse than an unconfirmed update.
const (
	stagePrepared   = "prepared"
	stageBackedUp   = "backed-up"
	stageReplaced   = "replaced"
	stageLaunched   = "launched"
	stageConfirmed  = "confirmed"
	stageRolledBack = "rolled-back"
	stageAborted    = "aborted"
)

// updateStateLoad reads the marker. A missing file is (nil, nil) — the normal
// state of a machine that has never updated. A corrupt file is an error and the
// caller must not delete anything on its behalf: the backup may be the only
// copy of a working executable.
func updateStateLoad(path string) (*updateState, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var st updateState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("更新状态文件损坏: %w", err)
	}
	return &st, nil
}

// stateLockTries/stateLockWait bound how long a writer waits for the marker's
// lock file before going ahead anyway. Waiting forever would hang the helper on
// a crashed peer; not writing at all would lose the record of an update that
// really happened, which is the worse failure of the two.
const (
	stateLockTries = 100
	stateLockWait  = 20 * time.Millisecond
)

// updateStateSave writes the marker as one process at a time.
//
// Two writers share this file: the running app (prepared/backed-up/confirmed)
// and the helper it launches (replaced/launched/rolled-back/aborted). They used
// to read-modify-write it independently, so a helper's "launched" could be
// overwritten by the app's stale "backed-up" — after which the next start said
// the update never happened and offered a restore of a healthy build. A lock
// file makes the whole read-modify-write exclusive; markStage is the caller that
// needs the merge to be atomic.
func updateStateSave(path string, st updateState) error {
	unlock := lockUpdateState(path)
	defer unlock()
	return writeStateLocked(path, st)
}

// writeStateLocked writes the marker; the caller holds the lock.
func writeStateLocked(path string, st updateState) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// lockUpdateState takes an exclusive lock file next to the marker and returns
// the release func. A lock that cannot be taken within the budget is reported
// and then ignored: correctness of the record beats mutual exclusion, and the
// atomic rename still prevents a torn file.
func lockUpdateState(path string) func() {
	lock := path + ".lock"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return func() {}
	}
	for i := 0; i < stateLockTries; i++ {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			_, _ = f.WriteString(fmt.Sprintf("%d", os.Getpid()))
			_ = f.Close()
			return func() { _ = os.Remove(lock) }
		}
		time.Sleep(stateLockWait)
	}
	// A lock left behind by a crashed process is worth clearing once, so the
	// next writer is not blocked for the life of the install.
	if st, err := os.Stat(lock); err == nil && time.Since(st.ModTime()) > time.Minute {
		_ = os.Remove(lock)
	}
	return func() {}
}

// writeFileAtomic writes through a temporary file and a rename, so a crash can
// never leave a half-written JSON file behind that fails to parse next start.
func writeFileAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// The temporary name carries the pid and a counter. Both the app and the
	// update helper write this marker, and they are separate processes: with a
	// fixed ".tmp" name they truncated each other's write and whichever
	// renamed last won, so a stage one of them had just recorded vanished.
	tmp := fmt.Sprintf("%s.%d-%d.tmp", path, os.Getpid(), nextTmpSeq())
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	_ = f.Sync()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ------------------------------------------------------------ helper args

// helperArgs is the hand-off contract between the running app and the helper it
// launches from its own backup copy.
type helperArgs struct {
	Mode string
	// Exe is the live executable to replace; Bak is the running helper's own
	// path (the previous version); Staged is the downloaded replacement.
	Exe    string
	Bak    string
	Staged string
	// State is the marker file, OkDir the directory the new version drops its
	// handshake into, Token the secret tying that handshake to this attempt.
	State  string
	OkDir  string
	Token  string
	Parent int
}

func (h helperArgs) okPath() string { return filepath.Join(h.OkDir, h.Token+".ok") }

func (h helperArgs) argv() []string {
	out := []string{"--exe", h.Exe, "--bak", h.Bak, "--state", h.State, "--parent", strconv.Itoa(h.Parent)}
	switch h.Mode {
	case modeRollback:
		out = append(out, helperFlagRollback)
	default:
		out = append(out, helperFlagApply,
			"--staged", h.Staged, "--ok-dir", h.OkDir, "--token", h.Token)
	}
	return out
}

// parseHelperArgs recognises a helper invocation. ok is false for a normal
// launch, which is the only case main() may hand to Wails.
func parseHelperArgs(argv []string) (helperArgs, bool) {
	args := helperArgs{}
	value := func(i *int) string {
		if *i+1 >= len(argv) {
			return ""
		}
		*i++
		return argv[*i]
	}
	for i := 0; i < len(argv); i++ {
		switch argv[i] {
		case helperFlagApply:
			args.Mode = modeApply
		case helperFlagRollback:
			args.Mode = modeRollback
		case "--exe":
			args.Exe = value(&i)
		case "--bak":
			args.Bak = value(&i)
		case "--staged":
			args.Staged = value(&i)
		case "--state":
			args.State = value(&i)
		case "--ok-dir":
			args.OkDir = value(&i)
		case "--token":
			args.Token = value(&i)
		case "--parent":
			args.Parent, _ = strconv.Atoi(value(&i))
		case handshakeFlag:
			// Handled by the app itself; not a helper invocation.
			_ = value(&i)
		}
	}
	return args, args.Mode != ""
}

// parseHandshakeToken extracts the token a fresh version was started with.
func parseHandshakeToken(argv []string) string {
	for i := 0; i < len(argv); i++ {
		if argv[i] != handshakeFlag {
			continue
		}
		if i+1 >= len(argv) {
			return ""
		}
		tok := argv[i+1]
		if safeUpdateToken(tok) {
			return tok
		}
		return ""
	}
	return ""
}

// newUpdateToken makes the 32 random bytes that pair one helper with one new
// process.
func newUpdateToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on Windows; a token derived from the wall
		// clock is still better than aborting the update.
		return fmt.Sprintf("%064x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// safeUpdateToken guards the one piece of update state that becomes a file
// name. Strict hex-only: the token arrives on a command line, and a token like
// "..\\..\\x" would otherwise decide where the handshake file lands.
func safeUpdateToken(tok string) bool {
	if len(tok) != 64 {
		return false
	}
	for _, r := range tok {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// ------------------------------------------------------------ the swap

// validateStaged re-checks the downloaded file immediately before it is put
// into service. The download already did this, but a file that sat on disk
// (possibly for hours, possibly scanned, possibly edited) is not the file that
// was verified then, and this is the last moment a bad one can be refused
// before it replaces a working executable.
func validateStaged(sys updateSys, staged string, wantSize int64, wantSHA string) error {
	const minExeBytes = 1 << 20
	if !sys.exists(staged) {
		return fmt.Errorf("新版文件不在了，请重新下载")
	}
	got := sys.size(staged)
	if got < minExeBytes {
		return fmt.Errorf("新版文件只有 %d 字节，不像一个程序", got)
	}
	if wantSize > 0 && got != wantSize {
		return fmt.Errorf("新版文件大小 %d 与下载时记录的 %d 不一致", got, wantSize)
	}
	magic, err := readPrefix(staged, 2)
	if err != nil {
		return err
	}
	if string(magic) != "MZ" {
		return fmt.Errorf("新版文件不是可执行文件")
	}
	if wantSHA != "" {
		sha, err := sys.sha256File(staged)
		if err != nil {
			return err
		}
		if !strings.EqualFold(sha, wantSHA) {
			return fmt.Errorf("新版文件校验失败，已丢弃")
		}
	}
	return nil
}

func readPrefix(path string, n int) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	buf := make([]byte, n)
	if _, err := io.ReadFull(f, buf); err != nil {
		return nil, fmt.Errorf("读取 %s 头部失败: %w", filepath.Base(path), err)
	}
	return buf, nil
}

// backupExe keeps the running version under a second name before anything is
// replaced. It always copies.
//
// The backup used to be a hard link when the filesystem allowed one, on the
// argument that it is free and that no crash window can exist between "backup
// made" and "backup complete". Measured on this machine (Windows 11 26100,
// NTFS, same volume) that combination does work — the helper running from the
// link, the original path replaced in about 2 ms — because replacing a name
// only deletes a directory entry, and the running image keeps its data alive
// through the other link. It is not worth relying on. A real copy removes the
// shared-identity property entirely, so "the backup", "the running image" and
// "the file being replaced" are three independent things: rollback can rename
// and replace without reasoning about link semantics, and a future change that
// deletes one path cannot pull the rug out from under a running process. The
// price is one ~13 MB copy per update.
func backupExe(sys updateSys, exe, bak string) error {
	if sys.exists(bak) {
		if err := sys.remove(bak); err != nil {
			return fmt.Errorf("清理上次的备份失败: %w", err)
		}
	}
	if err := sys.copyFile(exe, bak); err != nil {
		return fmt.Errorf("备份当前版本失败: %w", err)
	}
	if a, b := sys.size(exe), sys.size(bak); a < 0 || a != b {
		return fmt.Errorf("备份大小不一致：%d 与 %d", a, b)
	}
	sys.logf("backup of %s created by copy", exe)
	return nil
}

// swapExe replaces the live executable with the staged one in a single atomic
// step: MoveFileEx with REPLACE_EXISTING|WRITE_THROUGH either does the whole
// thing or leaves the old file exactly where it was. There is therefore no
// moment at which the executable path is missing, which is what the old script
// created whenever its rename succeeded and its move did not.
//
// The retry loop covers a staged file an antivirus product still has open.
func swapExe(sys updateSys, exe, staged, bak string) error {
	if err := validateStaged(sys, staged, 0, ""); err != nil {
		return err
	}
	err := retryUpdate(sys, swapTimeout, func() error {
		if cerr := sys.replaceFile(staged, exe); cerr != nil {
			sys.logf("replace %s -> %s failed: %v", staged, exe, cerr)
			return cerr
		}
		return nil
	})
	if err != nil {
		sys.logf("replace gave up after %s: %v (old executable untouched at %s)", swapTimeout, err, exe)
		return err
	}
	sys.logf("replaced %s with %s (backup kept at %s)", exe, staged, bak)
	return nil
}

// retryUpdate runs fn until it succeeds or timeout elapses, with a short
// back-off between attempts (0.2s, 0.5s, 1s, 2s, then 3s).
//
// It measures elapsed time with sys.now() rather than time.Now() so a test can
// drive the loop to its deadline without waiting a minute for it.
func retryUpdate(sys updateSys, timeout time.Duration, fn func() error) error {
	start := sys.now()
	var last error
	for attempt := 0; ; attempt++ {
		if last = fn(); last == nil {
			return nil
		}
		if attempt > 0 && sys.now().Sub(start) >= timeout {
			return last
		}
		sys.sleep(updateBackoff(attempt))
	}
}

// updateBackoff is the delay after the n-th consecutive failure. It is written
// out rather than computed: the exact ladder matters (0.2s covers a lock that is
// already releasing, 3s keeps a long Defender scan from becoming a spin).
func updateBackoff(n int) time.Duration {
	switch n {
	case 0:
		return 200 * time.Millisecond
	case 1:
		return 500 * time.Millisecond
	case 2:
		return time.Second
	case 3:
		return 2 * time.Second
	default:
		return 3 * time.Second
	}
}

// waitForExit blocks until pid is gone or the timeout passes.
func waitForExit(sys updateSys, pid int, timeout time.Duration) error {
	if pid <= 0 || !sys.processAlive(pid) {
		return nil
	}
	return sys.waitProcess(pid, timeout)
}

// ------------------------------------------------------------ handshake

// hsOutcome is how the helper's wait for the new version ended.
type hsOutcome int

const (
	// hsConfirmed: the new version wrote the handshake file naming our pid.
	hsConfirmed hsOutcome = iota
	// hsExited: the process we started is gone without confirming. It is
	// either a crash or an immediate error exit.
	hsExited
	// hsUnconfirmed: the deadline passed, but the process is alive and shows
	// a window, so it is running and only the handshake is missing.
	hsUnconfirmed
	// hsTimeout: the deadline passed with no evidence the new version runs.
	hsTimeout
)

// awaitHandshake waits for the new version to prove it reached domReady.
//
// A handshake file rather than a window title or a delay: an event emitted from
// startup would be dropped (Wails buffers no events), so the new process writes
// a file at the one moment the UI is definitely up, and the file names the pid
// it belongs to — a file left over from an earlier attempt must never be
// mistaken for this one's confirmation.
func awaitHandshake(sys updateSys, okPath string, pid int, timeout time.Duration) hsOutcome {
	start := sys.now()
	// Whether a window was ever seen, not whether one is on screen at the
	// deadline. A new build that showed its window and then minimised itself
	// (or was minimised by the user, or painted and hid during startup) is
	// running; judging only the final instant demoted it to "never came up"
	// and rolled a working update back. The reverse — no window at any point —
	// is still a timeout.
	sawWindow := false
	for {
		if got, ok := readHandshakePID(sys, okPath); ok && got == pid {
			sys.logf("handshake confirmed by pid %d", pid)
			return hsConfirmed
		}
		if !sys.processAlive(pid) {
			sys.logf("pid %d exited without a handshake", pid)
			return hsExited
		}
		if !sawWindow && sys.processHasWindow(pid) {
			sawWindow = true
			sys.logf("pid %d showed a window", pid)
		}
		if sys.now().Sub(start) >= timeout {
			if sawWindow {
				sys.logf("pid %d showed a window but never confirmed after %s", pid, timeout)
				return hsUnconfirmed
			}
			sys.logf("pid %d alive but never showed a window and never confirmed after %s", pid, timeout)
			return hsTimeout
		}
		sys.sleep(handshakePoll)
	}
}

// readHandshakePID reads a handshake file written by a previous or current
// attempt. A malformed or half-written file is "not yet", never a match.
func readHandshakePID(sys updateSys, path string) (int, bool) {
	if !sys.exists(path) {
		return 0, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	var h struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(b, &h); err != nil || h.PID <= 0 {
		return 0, false
	}
	return h.PID, true
}

// writeHandshake is what the *new* process does at domReady.
func writeHandshake(path string, pid int) error {
	b, err := json.Marshal(struct {
		PID     int    `json:"pid"`
		Version string `json:"version"`
		Time    string `json:"time"`
	}{pid, appVersion, time.Now().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	return writeFileAtomic(path, b)
}

// ------------------------------------------------------------ the helper

// markStage records the given stage, keeping everything the caller already
// wrote into the marker (paths, hashes, version).
//
// A record that already says "confirmed" is never downgraded. Two helpers can
// end up racing after a quit was refused and the user tried again: the loser
// fails to find its staged file and would otherwise stamp "aborted" over a
// success that has already been proven, leaving the next launch to claim the
// update never happened.
func markStage(sys updateSys, statePath, stage, reason string) {
	// Held across the read, the merge and the write: the running app writes
	// this marker too, and an unlocked read-modify-write is how a helper's
	// "launched" got overwritten by the app's stale "backed-up".
	unlock := lockUpdateState(statePath)
	defer unlock()

	st, err := updateStateLoad(statePath)
	if err != nil || st == nil {
		// A marker that cannot be read is not a reason to touch the files
		// under it; the helper logs and carries on with its own stage only if
		// it can write one.
		sys.logf("cannot read update state %s: %v", statePath, err)
		st = &updateState{}
	}
	if st.Stage == stageConfirmed && stage != stageConfirmed {
		sys.logf("ignoring stage %q: the update is already confirmed", stage)
		return
	}
	st.Stage = stage
	if reason != "" {
		st.Reason = reason
	}
	st.Time = sys.now().Format(time.RFC3339)
	if err := writeStateLocked(statePath, *st); err != nil {
		sys.logf("cannot write update state %s: %v", statePath, err)
	}
}

// runHelper is the whole body of "--apply-update": it is started from the
// previous version's backup copy, waits for the process that started it to
// disappear, swaps the executable, starts the new one and waits for it to
// confirm. Its exit code is diagnostic only.
func runHelper(args helperArgs, sys updateSys) int {
	if args.Mode == "" || args.Exe == "" || args.State == "" || args.Bak == "" {
		sys.logf("helper started with an incomplete argument set: %+v", args)
		return 2
	}
	if args.Mode == modeApply {
		if args.Staged == "" || args.OkDir == "" || !safeUpdateToken(args.Token) {
			sys.logf("apply helper started with an incomplete argument set: %+v", args)
			return 2
		}
	}


	// Nothing may be touched while the old process still holds its image: the
	// one thing this order guarantees is that a stuck exit costs nothing.
	if err := waitForExit(sys, args.Parent, helperParentTimeout); err != nil {
		sys.logf("old process %d did not exit: %v; no files touched", args.Parent, err)
		markStage(sys, args.State, stageAborted,
			fmt.Sprintf("旧进程没有退出：%v", err))
		return 1
	}

	if args.Mode == modeRollback {
		return rollback(sys, args, 0, "用户手动还原")
	}

	// The helper re-checks the staged file against the hashes the app recorded.
	// It used to trust only "starts with MZ and is bigger than 1 MiB", which is
	// satisfied by any executable — including a truncated download whose first
	// megabyte happens to be intact.
	var wantSHA string
	var wantSize int64
	if st, err := updateStateLoad(args.State); err == nil && st != nil {
		wantSHA, wantSize = st.NewSHA, st.NewSize
	}
	if err := validateStaged(sys, args.Staged, wantSize, wantSHA); err != nil {
		markStage(sys, args.State, stageAborted, fmt.Sprintf("新版文件校验失败：%v", err))
		// Nothing was touched: bring the current build back up rather than
		// leaving the machine with no running program.
		if _, serr := sys.startProcess(args.Exe); serr != nil {
			sys.logf("could not restart %s after a failed validation: %v", args.Exe, serr)
		}
		return 1
	}

	if err := swapExe(sys, args.Exe, args.Staged, args.Bak); err != nil {
		// The atomic replace left the old executable untouched, so there is
		// nothing to roll back — but nothing is running either, because the
		// process that started this helper has already quit. Leaving it at
		// that is how a failed update ends with no program on screen at all.
		markStage(sys, args.State, stageAborted, fmt.Sprintf("替换文件失败：%v", err))
		if _, serr := sys.startProcess(args.Exe); serr != nil {
			sys.logf("could not restart the old version at %s: %v", args.Exe, serr)
		}
		return 1
	}
	markStage(sys, args.State, stageReplaced, "")

	pid, err := sys.startProcess(args.Exe, handshakeFlag, args.Token)
	if err != nil {
		return rollback(sys, args, 0, fmt.Sprintf("新版启动失败：%v", err))
	}
	markStage(sys, args.State, stageLaunched, "")
	sys.logf("new version started as pid %d", pid)

	switch awaitHandshake(sys, args.okPath(), pid, handshakeTimeout) {
	case hsConfirmed:
		markStage(sys, args.State, stageConfirmed, "")
		// The backup is deliberately kept: this process *is* the backup, so it
		// cannot delete its own image. The new version removes it once it has
		// cleaned up a handshake it confirmed itself.
		return 0
	case hsUnconfirmed:
		// Running but unconfirmed. Rolling back here would replace a working
		// new version with an old one — strictly worse than saying so.
		markStage(sys, args.State, stageLaunched, "新版在运行但没有确认握手")
		return 3
	default:
		return rollback(sys, args, pid, "新版没有确认启动")
	}
}

// rollback undoes an update that did not come up. It kills only the pid it
// started itself, so an instance the user launched by hand is never touched.
func rollback(sys updateSys, args helperArgs, pid int, reason string) int {
	sys.logf("rolling back: %s", reason)

	if pid > 0 && sys.processAlive(pid) {
		if err := sys.killProcess(pid); err != nil {
			sys.logf("cannot kill pid %d: %v", pid, err)
		} else if err := waitForExit(sys, pid, 10*time.Second); err != nil {
			sys.logf("pid %d still alive after kill: %v", pid, err)
		}
	}

	if !sys.exists(args.Bak) {
		// Nothing to restore from. Leave the current executable in place:
		// deleting or moving it would turn a bad update into no program at all,
		// which is exactly how the old script left this machine.
		sys.logf("no backup at %s to restore from", args.Bak)
		markStage(sys, args.State, stageAborted, "回滚失败：备份不存在")
		return 1
	}

	failed := args.Exe + ".failed-" + sys.now().Format("20060102-150405")
	// The executable may already be gone: a swap that renamed it aside and
	// then failed leaves no path to move, and "restore the old version" from
	// the UI hit exactly that state and refused. With the target missing there
	// is nothing to preserve, so the backup simply takes its place.
	if sys.exists(args.Exe) {
		if err := sys.rename(args.Exe, failed); err != nil {
			sys.logf("cannot move the bad executable aside: %v", err)
			markStage(sys, args.State, stageAborted, fmt.Sprintf("回滚失败：%v", err))
			return 1
		}
	} else {
		sys.logf("no executable at %s; restoring the backup directly", args.Exe)
		failed = ""
	}
	if err := sys.replaceFile(args.Bak, args.Exe); err != nil {
		// The failed build is out of the way and the backup is still on disk;
		// putting the failed build back would be worse than leaving the path
		// empty for a retry, so this is reported, not papered over.
		sys.logf("cannot restore the backup: %v", err)
		markStage(sys, args.State, stageRolledBack,
			fmt.Sprintf("回滚未完成：%v（旧版仍在 %s）", err, args.Bak))
		return 1
	}

	unlock := lockUpdateState(args.State)
	st, _ := updateStateLoad(args.State)
	if st == nil {
		st = &updateState{}
	}
	st.Stage = stageRolledBack
	st.Reason = reason
	st.Failed = failed
	st.Time = sys.now().Format(time.RFC3339)
	if err := writeStateLocked(args.State, *st); err != nil {
		sys.logf("cannot write update state: %v", err)
	}
	unlock()

	if _, err := sys.startProcess(args.Exe); err != nil {
		sys.logf("old version restored but not restarted: %v", err)
	}
	return 1
}

// tmpSeq numbers temporary files within this process, so two writers that share
// a data directory never pick the same name twice in the same millisecond.
var tmpSeq atomic.Uint64

func nextTmpSeq() uint64 { return tmpSeq.Add(1) }

// samePathText compares two paths for identity, tolerant of case and separator
// spelling: Windows paths are case-insensitive and the same file is routinely
// spelled with either separator.
func samePathText(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// hashFileSHA256 is the one hashing routine the update path uses, both to
// verify a download and to identify which build is currently on disk.
func hashFileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
