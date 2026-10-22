package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAutostart replaces the registry with an in-memory entry.
//
// This is how the launch-at-login tests avoid touching the user's real
// HKCU\Software\Microsoft\Windows\CurrentVersion\Run: every backend call goes
// through the package variables in autostart.go, and install() swaps all of
// them (plus the executable path) for closures over this struct. Nothing in
// this file opens a registry key, so `go test` cannot add, change or delete a
// real startup entry — not even briefly, and not even if the test process is
// killed mid-run.
type fakeAutostart struct {
	value   string
	present bool
	writes  []string
	deletes int

	readErr error
	exe     string
	exeErr  error
}

// swap points the package at the fake and returns the undo.
func (f *fakeAutostart) swap() func() {
	oldRead, oldWrite, oldDelete, oldExec := autostartRead, autostartWrite, autostartDelete, autostartExec
	autostartRead = func(string) (string, bool, error) {
		return f.value, f.present, f.readErr
	}
	autostartWrite = func(_ string, value string) error {
		f.value, f.present = value, true
		f.writes = append(f.writes, value)
		return nil
	}
	autostartDelete = func(string) error {
		f.value, f.present = "", false
		f.deletes++
		return nil
	}
	autostartExec = func() (string, error) { return f.exe, f.exeErr }
	return func() {
		autostartRead, autostartWrite, autostartDelete, autostartExec = oldRead, oldWrite, oldDelete, oldExec
	}
}

// install points the package at the fake until the test finishes.
func (f *fakeAutostart) install(t *testing.T) {
	t.Helper()
	t.Cleanup(f.swap())
}

func newFakeAutostart() *fakeAutostart {
	return &fakeAutostart{exe: `C:\Program Files\VKMDownload\VKMDownload.exe`}
}

// packageAutostart is the registry stand-in every test in this package gets by
// default.
//
// SetConfig applies a LaunchAtLogin change immediately, so any test that flips
// that field would otherwise write the user's real Run key. TestMain installs
// this in-memory fake before the suite runs: the registry is unreachable from
// tests unless one explicitly reaches for the real backend, which only the
// read-only Windows test does.
var packageAutostart = &fakeAutostart{exe: `C:\Test\VKMDownload\VKMDownload.exe`}

// TestMain installs the safe backend for the whole test binary and restores the
// real one afterwards. It lives here, next to the fake it installs, so the
// isolation rule is impossible to miss when adding a test that changes
// LaunchAtLogin.
func TestMain(m *testing.M) {
	restore := packageAutostart.swap()
	code := m.Run()
	restore()
	os.Exit(code)
}

// resetPackageAutostart clears the shared fake and returns it, so a test can
// set the starting registry state and read back what the code under test did.
func resetPackageAutostart() *fakeAutostart {
	packageAutostart.value = ""
	packageAutostart.present = false
	packageAutostart.writes = nil
	packageAutostart.deletes = 0
	packageAutostart.readErr = nil
	packageAutostart.exe = `C:\Test\VKMDownload\VKMDownload.exe`
	packageAutostart.exeErr = nil
	return packageAutostart
}

// TestSetLaunchAtLoginWritesTheQuotedExe covers the first launch: no value yet,
// so exactly one write, holding the quoted path (a Run value with spaces must
// be quoted or Windows splits it).
func TestSetLaunchAtLoginWritesTheQuotedExe(t *testing.T) {
	f := newFakeAutostart()
	f.install(t)

	if err := SetLaunchAtLogin(true); err != nil {
		t.Fatalf("SetLaunchAtLogin(true): %v", err)
	}
	want := `"C:\Program Files\VKMDownload\VKMDownload.exe"`
	if len(f.writes) != 1 || f.writes[0] != want {
		t.Fatalf("writes = %q, want exactly [%q]", f.writes, want)
	}
	if !f.present || f.value != want {
		t.Errorf("stored value = %q (present=%v), want %q", f.value, f.present, want)
	}
}

// TestSetLaunchAtLoginSkipsAnUnchangedValue is the "only when it changes" rule:
// a second launch with the same executable must not touch the registry, which
// is what keeps a frequently restarted app from tripping registry monitors.
func TestSetLaunchAtLoginSkipsAnUnchangedValue(t *testing.T) {
	f := newFakeAutostart()
	f.install(t)

	if err := SetLaunchAtLogin(true); err != nil {
		t.Fatal(err)
	}
	if err := SetLaunchAtLogin(true); err != nil {
		t.Fatal(err)
	}
	if len(f.writes) != 1 {
		t.Errorf("writes = %q, want the second sync to skip the write", f.writes)
	}
}

// TestSetLaunchAtLoginRepairsAMovedExe covers an update or a move: a different
// stored path is corrected rather than trusted.
func TestSetLaunchAtLoginRepairsAMovedExe(t *testing.T) {
	f := newFakeAutostart()
	f.value, f.present = `"D:\Old\VKMDownload.exe"`, true
	f.install(t)

	if err := SetLaunchAtLogin(true); err != nil {
		t.Fatal(err)
	}
	want := `"C:\Program Files\VKMDownload\VKMDownload.exe"`
	if len(f.writes) != 1 || f.writes[0] != want {
		t.Errorf("writes = %q, want the moved executable to be rewritten as %q", f.writes, want)
	}
}

// TestSetLaunchAtLoginRemovesWhenDisabled covers turning the setting off: an
// existing entry is deleted, and an absent one is a success with no call.
func TestSetLaunchAtLoginRemovesWhenDisabled(t *testing.T) {
	f := newFakeAutostart()
	f.value, f.present = `"C:\Program Files\VKMDownload\VKMDownload.exe"`, true
	f.install(t)

	if err := SetLaunchAtLogin(false); err != nil {
		t.Fatalf("SetLaunchAtLogin(false): %v", err)
	}
	if f.deletes != 1 || f.present {
		t.Errorf("deletes = %d present = %v, want one delete and nothing left", f.deletes, f.present)
	}

	// Already gone: nothing to remove, and still not an error.
	if err := SetLaunchAtLogin(false); err != nil {
		t.Fatalf("SetLaunchAtLogin(false) on an empty key: %v", err)
	}
	if f.deletes != 1 {
		t.Errorf("deletes = %d, want no second delete", f.deletes)
	}
}

// TestSetLaunchAtLoginReportsReadFailures pins the rule that a read failure is
// returned rather than written around: overwriting an entry the app could not
// inspect is worse than not syncing this launch.
func TestSetLaunchAtLoginReportsReadFailures(t *testing.T) {
	denied := errors.New("access denied")

	enabled := newFakeAutostart()
	enabled.readErr = denied
	enabled.install(t)
	if err := SetLaunchAtLogin(true); !errors.Is(err, denied) {
		t.Errorf("SetLaunchAtLogin(true) with a read failure = %v, want %v", err, denied)
	}
	if len(enabled.writes) != 0 {
		t.Errorf("writes = %q, want none after a failed read", enabled.writes)
	}

	disabled := newFakeAutostart()
	disabled.readErr = denied
	disabled.install(t)
	if err := SetLaunchAtLogin(false); !errors.Is(err, denied) {
		t.Errorf("SetLaunchAtLogin(false) with a read failure = %v, want %v", err, denied)
	}
}

// TestSetLaunchAtLoginNeedsAnExePath covers os.Executable failing or returning
// nothing: there is no path to store, so the call fails instead of writing a
// broken entry.
func TestSetLaunchAtLoginNeedsAnExePath(t *testing.T) {
	boom := errors.New("no executable")

	failing := newFakeAutostart()
	failing.exeErr = boom
	failing.install(t)
	if err := SetLaunchAtLogin(true); !errors.Is(err, boom) {
		t.Errorf("error = %v, want it to wrap %v", err, boom)
	}
	if len(failing.writes) != 0 {
		t.Errorf("writes = %q, want none without an executable", failing.writes)
	}

	blank := newFakeAutostart()
	blank.exe = "   "
	blank.install(t)
	err := SetLaunchAtLogin(true)
	if err == nil || !strings.Contains(err.Error(), "程序路径") {
		t.Errorf("error = %v, want a missing-path error", err)
	}
	if len(blank.writes) != 0 {
		t.Errorf("writes = %q, want none for a blank path", blank.writes)
	}
}

// The four tests below cover the save-time sync: the setting has to reach the
// registry while the user is still on the settings page, not at the next start.
// They all run against packageAutostart (the in-memory fake TestMain installed),
// so a save here cannot touch the real Run key.

// TestSetConfigRemovesLaunchAtLoginImmediately covers turning the switch off:
// the Run value must be gone by the time SetConfig returns, otherwise the next
// boot auto-starts the app one more time and the switch looks broken.
func TestSetConfigRemovesLaunchAtLoginImmediately(t *testing.T) {
	f := resetPackageAutostart()
	f.value, f.present = `"C:\Old\VKMDownload.exe"`, true

	app := newTestApp(t) // LaunchAtLogin defaults to true
	cfg := app.Config()
	cfg.LaunchAtLogin = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if f.deletes != 1 {
		t.Errorf("deletes = %d, want the Run value removed at save time", f.deletes)
	}
	if f.present {
		t.Error("the Run value is still present after turning launch-at-login off")
	}
	if app.Config().LaunchAtLogin {
		t.Error("stored LaunchAtLogin = true, want false")
	}
}

// TestSetConfigAddsLaunchAtLoginImmediately is the other direction: the value
// appears (quoted) as soon as the save succeeds.
func TestSetConfigAddsLaunchAtLoginImmediately(t *testing.T) {
	f := resetPackageAutostart()

	app := newTestApp(t)
	// Start from off without going through SetConfig: this test is about the
	// transition into "on", not about how the app got to "off".
	app.mu.Lock()
	app.cfg.LaunchAtLogin = false
	app.mu.Unlock()

	cfg := app.Config()
	cfg.LaunchAtLogin = true
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	want := `"C:\Test\VKMDownload\VKMDownload.exe"`
	if len(f.writes) != 1 || f.writes[0] != want {
		t.Errorf("writes = %q, want exactly [%q]", f.writes, want)
	}
	if f.deletes != 0 {
		t.Errorf("deletes = %d, want none", f.deletes)
	}
}

// TestSetConfigLeavesLaunchAtLoginAloneWhenUnchanged keeps unrelated saves away
// from the registry: changing the connection count must not cause a Run-key
// read or write.
func TestSetConfigLeavesLaunchAtLoginAloneWhenUnchanged(t *testing.T) {
	f := resetPackageAutostart()
	f.value, f.present = `"C:\Test\VKMDownload\VKMDownload.exe"`, true

	app := newTestApp(t)
	cfg := app.Config()
	cfg.Connections = 4
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if len(f.writes) != 0 || f.deletes != 0 {
		t.Errorf("writes = %q deletes = %d, want no autostart activity", f.writes, f.deletes)
	}
}

// TestSetConfigFailedSaveLeavesLaunchAtLoginAlone pins the rollback: a save
// that fails must not change the registry, and must put both live switches
// (launch-at-login and logging) back where they were.
func TestSetConfigFailedSaveLeavesLaunchAtLoginAlone(t *testing.T) {
	f := resetPackageAutostart()
	f.value, f.present = `"C:\Old\VKMDownload.exe"`, true

	app := newTestApp(t)
	// A regular file where the settings directory belongs makes saveConfig's
	// MkdirAll fail, so the failure happens before anything is written.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	app.dataDirOverride = blocked

	cfg := app.Config()
	cfg.LaunchAtLogin = false
	cfg.LoggingEnabled = false
	if err := app.SetConfig(cfg); err == nil {
		t.Fatal("SetConfig succeeded with an unwritable settings path")
	}

	if f.deletes != 0 || len(f.writes) != 0 {
		t.Errorf("writes = %q deletes = %d, want no autostart activity after a failed save",
			f.writes, f.deletes)
	}
	if !app.Config().LaunchAtLogin {
		t.Error("rollback did not restore LaunchAtLogin")
	}
	if !app.logEnabled.Load() {
		t.Error("rollback did not restore the logging switch")
	}
}
