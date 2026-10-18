package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vkmdownload/testsrv"
)

// Tests promoted from the audit's throw-away overlay probes. Each one pins a
// race or leak the audit reproduced by hand; they live here so the guarantee
// stays checked by `go test` (and by -race) instead of by a file in %TEMP%.

// TestConfigAliasCannotMutateLiveSettings is the P1-5 regression.
//
// SetConfig used to normalise the caller's own Config in place, and Config()
// handed back a value whose OrganizeGroups aliased the live backing array
// (copying a struct copies the slice header, not the array). Editing a returned
// Config — exactly what the settings page does — then wrote into the live
// settings outside the lock; -race caught it as a data race. A deep copy on both
// sides is what makes either direction impossible.
func TestConfigAliasCannotMutateLiveSettings(t *testing.T) {
	app := newTestApp(t)

	// A caller editing a returned config must not change the live one.
	got := app.Config()
	if len(got.OrganizeGroups) == 0 {
		t.Fatal("no organize groups to test with")
	}
	before := app.Config().OrganizeGroups[0].Name
	got.OrganizeGroups[0].Name = "HIJACKED"
	got.OrganizeGroups[0].Exts[0] = "hijacked"
	if after := app.Config(); after.OrganizeGroups[0].Name != before ||
		after.OrganizeGroups[0].Exts[0] == "hijacked" {
		t.Fatalf("editing a returned Config changed the live settings: %+v", after.OrganizeGroups[0])
	}

	// A caller keeping its own value must not see it rewritten by SetConfig.
	mine := app.Config()
	mine.OrganizeGroups[0].Name = "MineGames"
	mine.OrganizeGroups[0].Exts = []string{".EXE", " 7z "}
	if err := app.SetConfig(mine); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if mine.OrganizeGroups[0].Exts[0] != ".EXE" {
		t.Errorf("SetConfig rewrote the caller's slice in place: %+v", mine.OrganizeGroups[0])
	}
	stored := app.Config()
	if len(stored.OrganizeGroups) == 0 || stored.OrganizeGroups[0].Name != "MineGames" {
		t.Fatalf("stored groups = %+v, want the edited group kept", stored.OrganizeGroups)
	}
	if stored.OrganizeGroups[0].Exts[0] != "exe" {
		t.Errorf("stored extension = %q, want the normalised \"exe\"", stored.OrganizeGroups[0].Exts[0])
	}

	// And the two must not share memory afterwards.
	stored.OrganizeGroups[0].Name = "ChangedAfterStore"
	if again := app.Config(); again.OrganizeGroups[0].Name != "MineGames" {
		t.Error("the value returned alongside the stored one shares its memory")
	}
}

// TestConfigConcurrentSetAndReadIsRaceFree drives SetConfig and Config from
// different goroutines while reading the group table, which is the shape that
// produced the audit's DATA RACE report. Run under -race to be meaningful.
func TestConfigConcurrentSetAndReadIsRaceFree(t *testing.T) {
	app := newTestApp(t)

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(done)
		for i := 0; i < 200; i++ {
			c := app.Config()
			c.ExtensionEnabled = i%2 == 0
			c.OrganizeGroups = append(c.OrganizeGroups[:0:0], c.OrganizeGroups...)
			if len(c.OrganizeGroups) > 0 {
				c.OrganizeGroups[0].Name = "G" + strings.Repeat("x", i%7)
			}
			if err := app.SetConfig(c); err != nil {
				t.Errorf("SetConfig: %v", err)
				return
			}
		}
	}()

	deadline := time.After(30 * time.Second)
	for {
		select {
		case <-done:
			wg.Wait()
			return
		case <-deadline:
			t.Fatal("concurrent config updates did not finish")
		default:
		}
		c := app.Config()
		_ = organizeTarget(c.OrganizeGroups, "root", "a.exe")
	}
}

// TestRecoveryNoticeNeverPersistedForStoppedTask is the P2-8 regression.
//
// The "retrying in N s" notice describes a wait. A Pause during that wait used
// to leave the notice in tasks.json — the save below it ran with the notice
// still set — so the next start showed a retry that was never going to happen.
// The audit reproduced it by hand; this pins it.
func TestRecoveryNoticeNeverPersistedForStoppedTask(t *testing.T) {
	_, url := flapServer(t, 64<<10, func(c *testsrv.Config) {
		c.FlapAfter = 0
		c.FlapNotFound = 1 << 30
	})
	withRecoveryDelays(t, time.Hour)

	dir, dest := flapDest(t, "notice.bin")
	plantSidecarWithProgress(t, dest, 4096)

	app := newTestApp(t)
	id := startFlapTask(t, app, url, dir, "notice.bin")

	waitFor(t, "the recovery notice", 30*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusDownloading && v.Notice != ""
	})
	if err := app.mgr.Pause(id); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	waitFor(t, "the paused task to settle", 10*time.Second, func() bool {
		v, _ := find(app.mgr, id)
		return v.Status == StatusPaused && v.Notice == ""
	})

	// The save that records the pause must not carry the notice either.
	waitFor(t, "tasks.json to record the pause", 10*time.Second, func() bool {
		b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
		if err != nil {
			return false
		}
		var list []persistedTask
		if json.Unmarshal(b, &list) != nil {
			return false
		}
		for _, p := range list {
			if p.ID == id {
				return p.Status == StatusPaused
			}
		}
		return false
	})
	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var list []persistedTask
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if p.ID != id {
			continue
		}
		if p.Notice != "" {
			t.Errorf("tasks.json holds a retry notice for a paused task: %q", p.Notice)
		}
		if p.Error != "" && strings.Contains(p.Error, "?") {
			t.Errorf("unexpected error text on the paused task: %q", p.Error)
		}
	}

	// A restart must not resurrect it: loadTasks clears any notice it reads.
	reloaded := newTestApp(t)
	_ = reloaded
	app2 := NewAppWithDataDir(app.dataDir())
	app2.emit = func(string, ...any) {}
	app2.mgr.loadTasks()
	if v, ok := find(app2.mgr, id); ok && v.Notice != "" {
		t.Errorf("a restored task came back with a notice: %q", v.Notice)
	}
}

// TestPauseResumeRaceKeepsStatusHonest is the P1-4 regression, driven the way a
// user produces it: pause, then resume before the paused run has finished
// unwinding. The old code bumped the generation only inside run, so the exit
// branch of the paused run could see the resumed task as "downloading" and
// stamp it "canceled" (or delete its partial data).
func TestPauseResumeRaceKeepsStatusHonest(t *testing.T) {
	body := payload(4 << 20)
	srv := rangedServer(t, body, 3*time.Millisecond)

	app := newTestApp(t)
	dest := filepath.Join(t.TempDir(), "race.bin")
	task, err := app.mgr.Start(StartRequest{
		URL: srv.URL + "/race.bin", Dest: dest, Conns: 2, Start: true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if task.Status != StatusDownloading && task.Status != StatusQueued {
		t.Fatalf("task did not start: %s", task.Status)
	}

	clobbered := 0
	rounds := 0
	for i := 0; i < 60; i++ {
		v, ok := find(app.mgr, task.ID)
		if !ok || v.Status == StatusCompleted || v.Status == StatusFailed {
			break
		}
		if v.Status != StatusDownloading {
			_ = app.mgr.Resume(task.ID)
			time.Sleep(2 * time.Millisecond)
			continue
		}
		rounds++
		if err := app.mgr.Pause(task.ID); err != nil {
			continue
		}
		// The impatient click: resume before the cancelled run has unwound.
		if err := app.mgr.Resume(task.ID); err != nil {
			continue
		}
		time.Sleep(3 * time.Millisecond)
		if v2, _ := find(app.mgr, task.ID); v2.Status == StatusCanceled {
			clobbered++
			_ = app.mgr.Resume(task.ID)
		}
	}
	if rounds == 0 {
		t.Skip("the transfer finished before a single pause/resume round")
	}
	if clobbered > 0 {
		t.Errorf("a stale run's exit path overwrote a resumed task %d times (of %d rounds)", clobbered, rounds)
	}

	// Whatever the interleaving, the task must end in a state a user can act
	// on: nothing may be left claiming to download with no engine behind it.
	waitFor(t, "the task to settle after the race loop", 60*time.Second, func() bool {
		v, ok := find(app.mgr, task.ID)
		return ok && v.Status != StatusDownloading
	})
	_ = app.mgr.Cancel(task.ID)
}

// TestHumanErrorNeverLeaksTokens pins P2-9: neither the mapped branches nor the
// fallback may copy a signed link's query string into what the user sees and
// what tasks.json stores.
func TestHumanErrorNeverLeaksTokens(t *testing.T) {
	const secret = "token=SUPERSECRET123"

	cases := []string{
		"HEAD https://host.example/file.bin?" + secret + ": HTTP 503",
		"HTTP 500: GET with Range https://host.example/file.bin?" + secret,
		"dial tcp 10.0.0.1:443: connect: connection refused (https://host.example/f?" + secret + ")",
		"some unmapped failure for https://host.example/f?" + secret + " that nothing matches",
		"HTTP 429: https://host.example/f?" + secret,
		"read tcp: i/o timeout https://host.example/f?" + secret,
	}
	for _, in := range cases {
		got := humanError(errString(in))
		if strings.Contains(got, "SUPERSECRET123") || strings.Contains(got, "token=") {
			t.Errorf("humanError(%q) leaked the credential: %q", in, got)
		}
		if strings.Contains(got, "host.example") && strings.Contains(got, "?") {
			t.Errorf("humanError(%q) kept a query string: %q", in, got)
		}
		if got == "" {
			t.Errorf("humanError(%q) produced no message", in)
		}
	}

	// The mapped branches still do their job.
	if got := humanError(errString("HEAD https://x/y: HTTP 503")); !strings.Contains(got, "503") {
		t.Errorf("503 mapped to %q, want a message naming the code", got)
	}
	if got := humanError(errString("HTTP 429: https://x/y")); !strings.Contains(got, "限流") {
		t.Errorf("429 mapped to %q, want the rate-limit wording", got)
	}
}

// errString is an error with a fixed message.
type errString string

func (e errString) Error() string { return string(e) }

// TestLateProgressCannotReviveFailedRow is the P2-11 regression: a frame that
// was already queued when the task failed must not be delivered afterwards.
func TestLateProgressCannotReviveFailedRow(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	seen := map[string]int{}
	app.emit = func(name string, _ ...any) {
		mu.Lock()
		seen[name]++
		mu.Unlock()
	}

	task, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never.bin", Dest: filepath.Join(t.TempDir(), "never.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusFailed
	tk.view.Error = "boom"
	tk.mu.Unlock()

	// A progress frame for the failed task, queued as the engine's last word.
	app.mgr.emit(TaskView{ID: task.ID, Status: StatusDownloading, Downloaded: 10})

	ctx, cancel := context.WithCancel(app.ctx)
	defer cancel()
	go app.mgr.Run(ctx)

	// Let the pump drain: any frame it delivers is counted by the emitter.
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	delivered := seen["task:progress"]
	mu.Unlock()
	if delivered > 0 {
		t.Fatalf("a queued progress frame for a failed task was delivered %d times", delivered)
	}

	// Control: the same frame for a running task is delivered, so the test is
	// exercising the pump rather than passing because nothing runs.
	running, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/other.bin", Dest: filepath.Join(t.TempDir(), "other.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	app.mgr.emit(TaskView{ID: running.ID, Status: StatusDownloading, Downloaded: 1})
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	delivered = seen["task:progress"]
	mu.Unlock()
	if delivered == 0 {
		t.Fatal("a progress frame for a downloading task was dropped too")
	}
}
