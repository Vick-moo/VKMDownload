package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stage 4 tests: what the app keeps on disk about the user.

// TestFinishedTaskLosesItsSignedURL pins the core privacy rule.
//
// A signed download link carries its credential in the query string, and a
// completed task is never fetched again — so keeping it serves no purpose and
// only leaks. Measured on the developer's machine before this change: 69 of 329
// stored tasks had a query string, including Azure SAS signatures and Google
// Drive export tokens.
func TestFinishedTaskLosesItsSignedURL(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	const signed = "https://cdn.example.com/dl/file.bin?token=SUPERSECRET&X-Amz-Signature=deadbeef"
	task, err := app.mgr.Start(StartRequest{
		URL: signed, Dest: filepath.Join(dir, "file.bin"), Cookie: "sid=COOKIEVALUE",
		Referer: "https://site.example/page", UserAgent: "SomeUA/1.0",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Simulate the download finishing.
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusCompleted
	tk.view.Dest = filepath.Join(dir, "file.bin")
	tk.mu.Unlock()

	if err := app.mgr.saveTasks(); err != nil {
		t.Fatalf("saveTasks: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, secret := range []string{"SUPERSECRET", "deadbeef", "COOKIEVALUE", "site.example/page", "SomeUA"} {
		if strings.Contains(got, secret) {
			t.Errorf("tasks.json still holds %q after the task completed:\n%s", secret, got)
		}
	}
	// The row must still be recognisable: host and path stay.
	if !strings.Contains(got, "cdn.example.com/dl/file.bin") {
		t.Errorf("the redacted URL lost its host and path:\n%s", got)
	}
}

// TestUnfinishedTaskKeepsWhatItNeeds pins the other side: a paused or failed
// download must keep its URL and cookie, because that is what makes resume work.
func TestUnfinishedTaskKeepsWhatItNeeds(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	const signed = "https://cdn.example.com/dl/file.bin?token=NEEDEDTORESUME"
	task, err := app.mgr.Start(StartRequest{
		URL: signed, Dest: filepath.Join(dir, "file.bin"), Cookie: "sid=NEEDED",
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

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "NEEDEDTORESUME") {
		t.Error("a paused task lost the URL it needs to resume")
	}
	if !strings.Contains(got, "NEEDED") {
		t.Error("a paused task lost the cookie it may need to resume")
	}
}

// TestStartupScrubCleansWhatEarlierVersionsWrote pins the one-time cleanup: a
// tasks.json and log left by an older build must come back clean, because
// otherwise a user who never triggers another save keeps the leak forever.
func TestStartupScrubCleansWhatEarlierVersionsWrote(t *testing.T) {
	app := newTestApp(t)

	old := []persistedTask{
		{
			TaskView: TaskView{
				ID: "t1-1", Status: StatusCompleted,
				URL: "https://host.example/f.bin?token=OLDSECRET&sig=abc",
			},
			Cookie: "sid=OLDCOOKIE", Referer: "https://site.example/p", UserAgent: "OLDAGENT",
		},
		{
			TaskView: TaskView{
				ID: "t1-2", Status: StatusPaused,
				URL: "https://host.example/g.bin?token=KEEPME",
			},
			Cookie: "sid=KEEPCOOKIE",
		},
	}
	b, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.dataDir(), "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	logLine := `2026-10-18T00:00:00Z extension added https://host.example/f.bin?token=LOGSECRET (origin="chrome-extension://x")` + "\n"
	if err := os.WriteFile(filepath.Join(app.dataDir(), "vkmdownload.log"), []byte(logLine), 0o644); err != nil {
		t.Fatal(err)
	}

	app.scrubStoredSecrets()

	tasks, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"OLDSECRET", "OLDCOOKIE", "OLDAGENT", "sig=abc"} {
		if strings.Contains(string(tasks), secret) {
			t.Errorf("tasks.json still holds %q after the scrub", secret)
		}
	}
	// The paused record keeps what resume needs.
	if !strings.Contains(string(tasks), "KEEPME") || !strings.Contains(string(tasks), "KEEPCOOKIE") {
		t.Errorf("the scrub damaged a resumable task:\n%s", tasks)
	}

	logged, err := os.ReadFile(filepath.Join(app.dataDir(), "vkmdownload.log"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(logged), "LOGSECRET") {
		t.Errorf("the log still holds a signed URL after the scrub: %s", logged)
	}
	if !strings.Contains(string(logged), "host.example/f.bin") {
		t.Errorf("the scrub removed the useful part of the log line: %s", logged)
	}
}

// TestDebugLogSanitisesEveryLine pins the backstop: even a call site that
// forgets logURL cannot write a credential, because the writer cleans the line.
func TestDebugLogSanitisesEveryLine(t *testing.T) {
	app := newTestApp(t)

	app.debugLog("download failed: Get %q: dial tcp 1.2.3.4:443: timeout",
		"https://host.example/f.bin?token=RAWSECRET&X-Amz-Signature=abc")

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "vkmdownload.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if strings.Contains(got, "RAWSECRET") || strings.Contains(got, "X-Amz-Signature") {
		t.Errorf("the log kept a credential: %s", got)
	}
	if !strings.Contains(got, "host.example/f.bin") {
		t.Errorf("the log lost the useful part of the URL: %s", got)
	}
}

// TestClearDownloadHistoryKeepsTheFiles pins the destructive-action boundary: a
// privacy action removes records, never the user's downloads.
func TestClearDownloadHistoryKeepsTheFiles(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "keep.bin")
	if err := os.WriteFile(file, []byte("user data"), 0o644); err != nil {
		t.Fatal(err)
	}

	task, err := app.mgr.Start(StartRequest{URL: "https://host.example/keep.bin", Dest: file})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tk := app.mgr.get(task.ID)
	tk.mu.Lock()
	tk.view.Status = StatusCompleted
	tk.mu.Unlock()
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.dataDir(), "vkmdownload.log"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := app.ClearDownloadHistory(); err != nil {
		t.Fatalf("ClearDownloadHistory: %v", err)
	}

	if _, err := os.Stat(file); err != nil {
		t.Errorf("the downloaded file was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.dataDir(), "tasks.json")); !os.IsNotExist(err) {
		t.Error("tasks.json survived the clear")
	}
	if _, err := os.Stat(filepath.Join(app.dataDir(), "vkmdownload.log")); !os.IsNotExist(err) {
		t.Error("the log survived the clear")
	}
	if len(app.mgr.List()) != 0 {
		t.Error("the in-memory task list survived the clear")
	}
}

// TestStoredSecretsReportsWithoutRevealing pins the summary the settings page
// shows: counts only, never values.
func TestStoredSecretsReportsWithoutRevealing(t *testing.T) {
	app := newTestApp(t)
	list := []persistedTask{
		{TaskView: TaskView{ID: "a", Status: StatusPaused, URL: "https://h/f?token=SECRETONE"}, Cookie: "c=SECRETCOOKIE"},
		{TaskView: TaskView{ID: "b", Status: StatusCompleted, URL: "https://h/g"}},
	}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(app.dataDir(), "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	got := app.StoredSecrets()
	if got.TaskCount != 2 {
		t.Errorf("TaskCount = %d, want 2", got.TaskCount)
	}
	if got.WithCookie != 1 {
		t.Errorf("WithCookie = %d, want 1", got.WithCookie)
	}
	if got.WithQueryURL != 1 {
		t.Errorf("WithQueryURL = %d, want 1", got.WithQueryURL)
	}

	// The struct must not be able to carry a value: every field is a count.
	raw, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"SECRETONE", "SECRETCOOKIE"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("the summary leaked %q: %s", secret, raw)
		}
	}
}

// TestTrimHistoryDropsOnlyOldFinishedTasks pins the retention rule.
func TestTrimHistoryDropsOnlyOldFinishedTasks(t *testing.T) {
	app := newTestApp(t)
	old := time.Now().Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	fresh := time.Now().Format(time.RFC3339Nano)

	for _, tc := range []struct {
		id     string
		status TaskStatus
		at     string
		keep   bool
	}{
		{"old-done", StatusCompleted, old, false},
		{"old-failed", StatusFailed, old, false},
		{"old-paused", StatusPaused, old, true},
		{"new-done", StatusCompleted, fresh, true},
	} {
		app.mgr.mu.Lock()
		app.mgr.tasks[tc.id] = &task{view: TaskView{
			ID: tc.id, URL: "https://h/" + tc.id, Status: tc.status, Created: tc.at,
		}}
		app.mgr.order = append(app.mgr.order, tc.id)
		app.mgr.mu.Unlock()
	}

	app.trimHistory()

	kept := map[string]bool{}
	for _, v := range app.mgr.List() {
		kept[v.ID] = true
	}
	for _, tc := range []struct {
		id     string
		status TaskStatus
		at     string
		keep   bool
	}{
		{"old-done", StatusCompleted, old, false},
		{"old-failed", StatusFailed, old, false},
		{"old-paused", StatusPaused, old, true},
		{"new-done", StatusCompleted, fresh, true},
	} {
		if kept[tc.id] != tc.keep {
			t.Errorf("%s: kept=%v, want %v", tc.id, kept[tc.id], tc.keep)
		}
	}
}
