package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the two things the first pass left open: a plaintext cookie for an
// unfinished download, and a way to switch the update check off.

// TestStoredCookieIsNotReadableOnDisk is the core of the DPAPI change: whatever
// the encoding, the raw cookie value must not appear in tasks.json.
//
// Why it matters: an unfinished download must keep the browser's cookies to
// resume (a private Drive file rejects a client without them), and that file is
// read by backup tools, sync clients, and any program running as this user.
func TestStoredCookieIsNotReadableOnDisk(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	const cookie = "SID=SUPERSECRETSESSION; HSID=ANOTHERSECRET"
	task, err := app.mgr.Start(StartRequest{
		URL: "https://host.example/private.bin", Dest: filepath.Join(dir, "private.bin"),
		Cookie: cookie,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusPaused // unfinished: the cookie is kept
	tk.mu.Unlock()
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatalf("saveTasks: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	if strings.Contains(raw, "SUPERSECRETSESSION") || strings.Contains(raw, "ANOTHERSECRET") {
		t.Errorf("the cookie is readable in tasks.json:\n%s", raw)
	}
	// It must still be there, sealed — not silently dropped.
	if !strings.Contains(raw, secretPrefix) && !strings.Contains(raw, plainPrefix) {
		t.Errorf("the cookie was not stored at all, so the task can no longer resume:\n%s", raw)
	}
}

// TestSealedCookieSurvivesARestart pins the round trip: what saveTasks writes,
// loadTasks must recover, or every restart would silently log the user out.
func TestSealedCookieSurvivesARestart(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	const cookie = "SID=ROUNDTRIPVALUE"

	task, err := app.mgr.Start(StartRequest{
		URL: "https://host.example/f.bin", Dest: filepath.Join(dir, "f.bin"), Cookie: cookie,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusPaused
	tk.mu.Unlock()
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatal(err)
	}

	reloaded := NewAppWithDataDir(app.dataDir())
	reloaded.emit = func(string, ...any) {}
	reloaded.mgr.loadTasks()

	got := reloaded.mgr.get(task.ID)
	if got == nil {
		t.Fatal("the task did not come back after a reload")
	}
	if got.cookie != cookie {
		t.Errorf("cookie after reload = %q, want the original value", got.cookie)
	}
}

// TestUnreadableSealedCookieDegradesGracefully pins the failure direction: a
// blob that cannot be opened (copied from another account, profile reset) must
// cost the session, never the task.
func TestUnreadableSealedCookieDegradesGracefully(t *testing.T) {
	app := newTestApp(t)
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	// A prefix that claims DPAPI but holds bytes no key can open.
	bogus := secretPrefix + "Zm9yZ2VkLWJ5dGVz"
	list := []persistedTask{{
		TaskView: TaskView{ID: "t1-1", Status: StatusPaused, URL: "https://h/f.bin"},
		Cookie:   bogus,
	}}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(filepath.Join(app.dataDir(), "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	app.mgr.loadTasks()

	tk := app.mgr.get("t1-1")
	if tk == nil {
		t.Fatal("a task with an unreadable cookie was dropped entirely")
	}
	if tk.cookie != "" {
		t.Errorf("cookie = %q, want it cleared when it cannot be opened", tk.cookie)
	}
}

// TestStartupSealsPlaintextCookiesFromOlderBuilds pins the migration: a record
// written before this change is only protected if something rewrites it, and a
// task paused months ago never will be by itself.
func TestStartupSealsPlaintextCookiesFromOlderBuilds(t *testing.T) {
	app := newTestApp(t)
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	list := []persistedTask{{
		TaskView: TaskView{ID: "t1-1", Status: StatusPaused, URL: "https://h/f.bin"},
		Cookie:   "SID=OLDCLEARTEXT",
	}}
	b, _ := json.MarshalIndent(list, "", "  ")
	path := filepath.Join(app.dataDir(), "tasks.json")
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}

	app.sealStoredCookies()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(after), "OLDCLEARTEXT") {
		t.Errorf("an old plaintext cookie was left in place:\n%s", after)
	}

	// And it still opens.
	var reloaded []persistedTask
	if err := json.Unmarshal(after, &reloaded); err != nil {
		t.Fatalf("rewritten file is not valid JSON: %v", err)
	}
	plain, err := openCookie(reloaded[0].Cookie)
	if err != nil {
		t.Fatalf("the sealed cookie cannot be reopened: %v", err)
	}
	if plain != "SID=OLDCLEARTEXT" {
		t.Errorf("reopened cookie = %q, want the original", plain)
	}
}

// TestFinishedTasksStillDropTheirCookie keeps the earlier behaviour intact: a
// completed download has no reason to carry a session at all.
func TestFinishedTasksStillDropTheirCookie(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	task, err := app.mgr.Start(StartRequest{
		URL: "https://host.example/f.bin", Dest: filepath.Join(dir, "f.bin"), Cookie: "SID=DONE",
	})
	if err != nil {
		t.Fatal(err)
	}
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusCompleted
	tk.mu.Unlock()
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SID=DONE") || strings.Contains(string(b), secretPrefix) {
		t.Errorf("a completed task kept a cookie:\n%s", b)
	}
}

// TestUpdateCheckToggleDefaultsOnAndIsHonoured pins the new setting: on by
// default (an update nobody hears about is not a feature), and switching it off
// must actually stop the startup request.
func TestUpdateCheckToggleDefaultsOnAndIsHonoured(t *testing.T) {
	if !DefaultConfig().CheckUpdatesOnStart {
		t.Fatal("CheckUpdatesOnStart must default to true")
	}

	app := newTestApp(t)
	called := 0
	app.emit = func(string, ...any) {}
	app.fetchRelease = func(_ context.Context) (releaseAsset, error) {
		called++
		return releaseAsset{tag: "v0.0.1"}, nil
	}

	// Off: startup must not ask.
	cfg := app.Config()
	cfg.CheckUpdatesOnStart = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	if app.Config().CheckUpdatesOnStart {
		t.Fatal("the setting did not persist")
	}
	app.checkUpdateOnceIfEnabled()
	if called != 0 {
		t.Errorf("the update check ran %d times with the switch off", called)
	}

	// On: it asks exactly once.
	cfg = app.Config()
	cfg.CheckUpdatesOnStart = true
	if err := app.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	app.checkUpdateOnceIfEnabled()
	if called != 1 {
		t.Errorf("the update check ran %d times with the switch on, want 1", called)
	}
}
