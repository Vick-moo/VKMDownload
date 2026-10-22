package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNormalizeClampsClipboardIntervalToFloor covers the clipboard polling
// period.
//
// The old normaliser reset anything below 200ms to the 800ms default, so a user
// who typed 100 to poll faster got a poll eight times slower instead — silently.
// The floor is what the input promises: too-small values are raised to it, and
// values at or above it are left alone (the 10s ceiling still applies).
func TestNormalizeClampsClipboardIntervalToFloor(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{100, 200},
		{199, 200},
		{200, 200},
		{800, 800}, // the shipped default must survive normalising untouched
		{10001, 10000},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.ClipboardIntervalMs = c.in
		cfg.normalize()
		if cfg.ClipboardIntervalMs != c.want {
			t.Errorf("normalize(%dms) = %dms, want %dms",
				c.in, cfg.ClipboardIntervalMs, c.want)
		}
	}

	// Clamping a hand-edited value must not move the default a fresh install
	// starts with.
	if got := DefaultConfig().ClipboardIntervalMs; got != 800 {
		t.Errorf("DefaultConfig().ClipboardIntervalMs = %d, want 800", got)
	}
}

// TestNormalizeDefaultsResumeFlushPercentWhenEnabled covers the "按百分比保存"
// trigger.
//
// The engine reads SavePercent=0 as "off" (internal/engine download.go), so an
// enabled trigger with an empty/cleared field was silently doing nothing: the
// checkbox said the transfer would be flushed by percentage while the engine
// never saw a percentage. normalize fills in the 10% default for an enabled
// trigger; an unticked switch keeps 0 as an explicit "off".
func TestNormalizeDefaultsResumeFlushPercentWhenEnabled(t *testing.T) {
	cases := []struct {
		name  string
		byPct bool
		pct   int
		want  int
	}{
		{"enabled with 0 gets the default", true, 0, 10},
		{"enabled with a value keeps it", true, 25, 25},
		{"enabled with a negative value is clamped then defaulted", true, -5, 10},
		{"disabled with 0 stays off", false, 0, 0},
		{"disabled keeps a leftover value", false, 25, 25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.ResumeFlushByPct = c.byPct
			cfg.ResumeFlushPct = c.pct
			cfg.normalize()
			if cfg.ResumeFlushPct != c.want {
				t.Errorf("normalize(byPct=%v, pct=%d) = %d, want %d",
					c.byPct, c.pct, cfg.ResumeFlushPct, c.want)
			}
		})
	}

	// A pct-only configuration must now really persist. With the value filled
	// in, the "at least one trigger" rescue must not have to force the time
	// trigger back on; before the fallback, this combination was saved as
	// "only the time trigger", which is not what the switches said.
	cfg := DefaultConfig()
	cfg.ResumeFlushBySize = false
	cfg.ResumeFlushByTime = false
	cfg.ResumeFlushByPct = true
	cfg.ResumeFlushPct = 0
	cfg.normalize()
	if cfg.ResumeFlushPct != 10 {
		t.Errorf("pct-only: ResumeFlushPct = %d, want the 10%% default", cfg.ResumeFlushPct)
	}
	if cfg.ResumeFlushByTime {
		t.Error("pct-only: the time trigger was forced back on although the pct trigger now has a value")
	}
	if cfg.ResumeFlushByPct != true {
		t.Error("pct-only: the pct trigger was turned off")
	}
}

// TestDefaultConfigForTheNewSettings pins the defaults the settings page will
// show. Two of them are booleans whose zero value is "off", so an omission from
// DefaultConfig would silently change behaviour rather than fail to compile.
func TestDefaultConfigForTheNewSettings(t *testing.T) {
	cfg := DefaultConfig()
	if !cfg.LoggingEnabled {
		t.Error("LoggingEnabled default = false, want true")
	}
	if !cfg.LaunchAtLogin {
		t.Error("LaunchAtLogin default = false, want true")
	}
	if cfg.HistoryRetentionDays != 90 {
		t.Errorf("HistoryRetentionDays default = %d, want 90", cfg.HistoryRetentionDays)
	}
	if cfg.ShutdownDelaySeconds != 60 {
		t.Errorf("ShutdownDelaySeconds default = %d, want 60", cfg.ShutdownDelaySeconds)
	}
}

// TestLoadConfigKeepsTheNewDefaultsForAnOldFile covers the upgrade path: a
// config.json written before these fields existed has no keys for them, and
// json.Unmarshal leaves the values already in the struct — which is why
// loadConfig starts from DefaultConfig rather than a zero Config.
func TestLoadConfigKeepsTheNewDefaultsForAnOldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// The shape of a file from a build before this batch: no loggingEnabled,
	// launchAtLogin, historyRetentionDays or shutdownDelaySeconds.
	if err := os.WriteFile(path, []byte(`{"connections":8,"downloadDir":"C:\\dl"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := loadConfig(path, nil)
	if !cfg.LoggingEnabled {
		t.Error("an old config disabled logging; absent must mean the default (on)")
	}
	if !cfg.LaunchAtLogin {
		t.Error("an old config disabled launch-at-login; absent must mean the default (on)")
	}
	if cfg.HistoryRetentionDays != 90 {
		t.Errorf("HistoryRetentionDays from an old config = %d, want 90", cfg.HistoryRetentionDays)
	}
	if cfg.ShutdownDelaySeconds != 60 {
		t.Errorf("ShutdownDelaySeconds from an old config = %d, want 60", cfg.ShutdownDelaySeconds)
	}
}

// TestNormalizeNewSettingRanges pins the bounds, including what 0 means for
// each: "keep forever" for the retention and "shut down now" for the delay.
func TestNormalizeNewSettingRanges(t *testing.T) {
	cases := []struct {
		name      string
		days      int
		delay     int
		wantDays  int
		wantDelay int
	}{
		{"negative retention falls back to the default", -1, 60, 90, 60},
		{"zero retention means keep forever", 0, 60, 0, 60},
		{"a normal retention survives", 7, 60, 7, 60},
		{"retention above the ceiling is clamped", 3651, 60, 3650, 60},
		{"negative delay falls back to the default", 90, -1, 90, 60},
		{"zero delay means shut down now", 90, 0, 90, 0},
		{"delay above the ceiling is clamped", 90, 601, 90, 600},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.HistoryRetentionDays = c.days
			cfg.ShutdownDelaySeconds = c.delay
			cfg.normalize()
			if cfg.HistoryRetentionDays != c.wantDays {
				t.Errorf("HistoryRetentionDays = %d, want %d", cfg.HistoryRetentionDays, c.wantDays)
			}
			if cfg.ShutdownDelaySeconds != c.wantDelay {
				t.Errorf("ShutdownDelaySeconds = %d, want %d", cfg.ShutdownDelaySeconds, c.wantDelay)
			}
		})
	}
}

// TestNormalizeCompletionActionsAreExclusive pins the one-of-two rule. Opening
// the file and the folder both fight for the foreground, and afterTaskFinished
// already gives the folder priority, so the stored pair must say the same.
func TestNormalizeCompletionActionsAreExclusive(t *testing.T) {
	cases := []struct {
		name              string
		file, dir         bool
		wantFile, wantDir bool
	}{
		{"both on keeps the folder", true, true, false, true},
		{"file only survives", true, false, true, false},
		{"folder only survives", false, true, false, true},
		{"both off stays off", false, false, false, false},
	}
	for _, c := range cases {
		cfg := DefaultConfig()
		cfg.OpenFileWhenDone = c.file
		cfg.OpenDirWhenDone = c.dir
		cfg.normalize()
		if cfg.OpenFileWhenDone != c.wantFile || cfg.OpenDirWhenDone != c.wantDir {
			t.Errorf("%s: file=%v dir=%v, want file=%v dir=%v",
				c.name, cfg.OpenFileWhenDone, cfg.OpenDirWhenDone, c.wantFile, c.wantDir)
		}
	}
}

// TestTrimHistoryHonoursRetentionDays covers the behaviour behind the setting:
// 0 keeps everything, 7 drops a month-old record but not a recent one, and the
// 90-day default still drops a hundred-day-old one.
func TestTrimHistoryHonoursRetentionDays(t *testing.T) {
	cases := []struct {
		days    int
		ageDays int
		want    bool // kept?
	}{
		{0, 400, true},   // 0 = forever, even for a year-old record
		{7, 3, true},     // inside a short window
		{7, 30, false},   // outside a short window
		{90, 100, false}, // the default still trims
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%dd-retention-%dd-old", c.days, c.ageDays), func(t *testing.T) {
			app := newTestApp(t)
			cfg := app.Config()
			cfg.HistoryRetentionDays = c.days
			if err := app.SetConfig(cfg); err != nil {
				t.Fatalf("SetConfig: %v", err)
			}

			created := time.Now().
				Add(-time.Duration(c.ageDays) * 24 * time.Hour).
				Format(time.RFC3339Nano)
			app.mgr.mu.Lock()
			app.mgr.tasks["old"] = &task{view: TaskView{
				ID: "old", URL: "https://h/old", Status: StatusCompleted, Created: created,
			}}
			app.mgr.order = append(app.mgr.order, "old")
			app.mgr.mu.Unlock()

			app.trimHistory()

			kept := app.mgr.get("old") != nil
			if kept != c.want {
				t.Errorf("kept = %v, want %v", kept, c.want)
			}
		})
	}
}

// TestDebugLogHonoursTheSwitch covers the log file switch end to end: with it
// off debugLog must not create or append to the file, and turning it back on
// must resume writing. The switch is applied through SetConfig, which is how
// the settings page changes it.
func TestDebugLogHonoursTheSwitch(t *testing.T) {
	app := newTestApp(t)
	logPath := filepath.Join(app.dataDir(), "vkmdownload.log")
	_ = os.Remove(logPath)

	cfg := app.Config()
	cfg.LoggingEnabled = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	app.debugLog("this line must not reach the disk")
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("log file exists while logging is off (stat err = %v)", err)
	}

	cfg = app.Config()
	cfg.LoggingEnabled = true
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	app.debugLog("this line must be written")

	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("log file missing after logging was re-enabled: %v", err)
	}
	if !strings.Contains(string(b), "this line must be written") {
		t.Errorf("log = %q, want the enabled line", b)
	}
	if strings.Contains(string(b), "must not reach the disk") {
		t.Errorf("log = %q, contains a line written while logging was off", b)
	}
}

// TestShutdownArgsKeepTheCancelWindow pins the shutdown command, especially the
// two things a delay setting can break: the countdown value that shutdown.exe
// actually receives, and the comment that tells the user how to cancel it.
func TestShutdownArgsKeepTheCancelWindow(t *testing.T) {
	args := shutdownArgs(60)
	if len(args) != 5 {
		t.Fatalf("args = %v, want 5 elements", args)
	}
	if args[0] != "/s" || args[1] != "/t" || args[2] != "60" || args[3] != "/c" {
		t.Fatalf("args = %v, want /s /t 60 /c <comment>", args)
	}
	if !strings.Contains(args[4], "60 秒后关机") || !strings.Contains(args[4], "shutdown /a") {
		t.Errorf("comment = %q, want both the delay and the cancel hint", args[4])
	}

	for _, c := range []struct {
		in   int
		want string
	}{
		{0, "0"},
		{-5, "0"},
		{600, "600"},
		{5000, "600"},
	} {
		if got := shutdownArgs(c.in)[2]; got != c.want {
			t.Errorf("shutdownArgs(%d) delay = %q, want %q", c.in, got, c.want)
		}
	}

	// An immediate shutdown has no window to cancel; promising one would be a
	// lie the user could not act on.
	if comment := shutdownArgs(0)[4]; strings.Contains(comment, "秒后") || !strings.Contains(comment, "立即") {
		t.Errorf("immediate shutdown comment = %q, want it to say 立即关机", comment)
	}
}

// TestPlanShutdownUsesTheConfiguredDelay covers the wiring from the setting to
// the shutdown command: the plan's end must pass ShutdownDelaySeconds, and
// must not launch anything while the switch is off. launchShutdown is replaced
// so the test never asks Windows to power the machine off.
func TestPlanShutdownUsesTheConfiguredDelay(t *testing.T) {
	app := newTestApp(t)
	cfg := app.Config()
	cfg.PlanShutdown = true
	cfg.ShutdownDelaySeconds = 120
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	old := launchShutdown
	var got []int
	launchShutdown = func(seconds int) { got = append(got, seconds) }
	t.Cleanup(func() { launchShutdown = old })

	app.mgr.finishPlanEnd(false)
	if len(got) != 1 || got[0] != 120 {
		t.Fatalf("plan shutdown delays = %v, want [120]", got)
	}

	cfg = app.Config()
	cfg.PlanShutdown = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	app.mgr.finishPlanEnd(false)
	if len(got) != 1 {
		t.Errorf("plan shutdown launched while disabled: %v", got)
	}
}
