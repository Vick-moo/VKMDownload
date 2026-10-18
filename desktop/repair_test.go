package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestPersistedFieldsRoundTrip pins the tasks.json field fix: manual,
// referer, cookie and userAgent must survive a save/load cycle, so a resumed
// download replays the credentials it started with.
func TestPersistedFieldsRoundTrip(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()
	dest := filepath.Join(dir, "keep.bin")

	v, err := app.mgr.Start(StartRequest{
		URL:       "http://127.0.0.1:1/keep.bin",
		Dest:      dest,
		Filename:  "keep.bin",
		Start:     false,
		Referer:   "http://127.0.0.1:1/page",
		Cookie:    "session=s3cret",
		UserAgent: "TestBrowser/1.0",
		Manual:    true,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatalf("saveTasks: %v", err)
	}

	reloaded := NewAppWithDataDir(app.dataDir())
	reloaded.emit = func(string, ...any) {}
	reloaded.ctx = context.Background()
	reloaded.mgr.loadTasks()

	tk := reloaded.mgr.get(v.ID)
	if tk == nil {
		t.Fatalf("task %s did not survive reload", v.ID)
	}
	tk.mu.Lock()
	defer tk.mu.Unlock()
	if !tk.manual {
		t.Error("manual = false after reload, want true")
	}
	if tk.referer != "http://127.0.0.1:1/page" {
		t.Errorf("referer = %q after reload", tk.referer)
	}
	if tk.cookie != "session=s3cret" {
		t.Errorf("cookie = %q after reload", tk.cookie)
	}
	if tk.userAgent != "TestBrowser/1.0" {
		t.Errorf("userAgent = %q after reload", tk.userAgent)
	}
}

// TestLoadTasksKeepsQueuedTasksQueued pins the restart rule: mid-transfer
// tasks come back paused, but queued ones keep waiting — the startup pump
// then resumes them as slots free up, so a restart never silently parks a
// queue the user asked for.
func TestLoadTasksKeepsQueuedTasksQueued(t *testing.T) {
	app := newTestApp(t)
	dir := app.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(id string, status TaskStatus) persistedTask {
		return persistedTask{TaskView: TaskView{
			ID: id, URL: "http://127.0.0.1:1/" + id + ".bin",
			Dest: filepath.Join(dir, id+".bin"), Status: status,
			Created: time.Now().Format(time.RFC3339Nano),
		}}
	}
	list := []persistedTask{mk("t1-1", StatusDownloading), mk("t1-2", StatusQueued)}
	b, _ := json.Marshal(list)
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	app.mgr.loadTasks()

	running, ok := find(app.mgr, "t1-1")
	if !ok {
		t.Fatal("mid-transfer task did not survive reload")
	}
	if running.Status != StatusPaused {
		t.Errorf("mid-transfer task reloaded as %v, want paused", running.Status)
	}
	waiting, ok := find(app.mgr, "t1-2")
	if !ok {
		t.Fatal("queued task did not survive reload")
	}
	if waiting.Status != StatusQueued {
		t.Errorf("queued task reloaded as %v, want still queued", waiting.Status)
	}
}

// TestLoadTasksCorruptPreserved pins the corrupt-index rule: a broken
// tasks.json is renamed aside (never deleted), the app starts empty, and the
// failure is logged.
func TestLoadTasksCorruptPreserved(t *testing.T) {
	app := newTestApp(t)
	path := filepath.Join(app.dataDir(), "tasks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	app.mgr.loadTasks()

	if len(app.mgr.List()) != 0 {
		t.Errorf("%d tasks loaded from a corrupt index, want 0", len(app.mgr.List()))
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("corrupt tasks.json is still in place; it must be moved aside")
	}
	entries, err := os.ReadDir(app.dataDir())
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "tasks.json.corrupt-") {
			kept = true
		}
	}
	if !kept {
		t.Errorf("no tasks.json.corrupt-* kept in %s", app.dataDir())
	}
}

// TestLoadTasksDedupesIDsAndReseedsSeq pins two load-time rules: a repeated id
// loads once, and the id counter restarts past the largest seq on disk so a
// new task cannot collide with a loaded one.
func TestLoadTasksDedupesIDsAndReseedsSeq(t *testing.T) {
	app := newTestApp(t)
	dir := app.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(id string) persistedTask {
		return persistedTask{TaskView: TaskView{
			ID: id, URL: "http://127.0.0.1:1/" + id + ".bin",
			Dest: filepath.Join(dir, id+".bin"), Status: StatusPaused,
			Created: time.Now().Format(time.RFC3339Nano),
		}}
	}
	// t1-900 carries seq 900; the duplicate must be dropped.
	list := []persistedTask{mk("t1-900"), mk("t1-900"), mk("t1-12")}
	b, _ := json.Marshal(list)
	if err := os.WriteFile(filepath.Join(dir, "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	app.mgr.loadTasks()

	if n := len(app.mgr.List()); n != 2 {
		t.Fatalf("loaded %d tasks, want 2 (duplicate dropped)", n)
	}
	v, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/fresh.bin", Dest: filepath.Join(dir, "fresh.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	seq, ok := parseTaskSeq(v.ID)
	if !ok {
		t.Fatalf("new id %q does not parse", v.ID)
	}
	if seq <= 900 {
		t.Errorf("new task seq = %d, want it past the loaded max 900", seq)
	}
}

// TestPlanIntervenedSemantics pins the end-of-plan rule: paused and failed
// settle a plan exactly like canceled does — the watcher finishes, and the
// plan counts as intervened so sound/shutdown are skipped.
func TestPlanIntervenedSemantics(t *testing.T) {
	for s, want := range map[TaskStatus]planStatus{
		StatusCompleted: planSettled, StatusDownloading: planWaiting, StatusQueued: planWaiting,
		StatusPaused: planIntervene, StatusCanceled: planIntervene, StatusFailed: planIntervene,
	} {
		if got := planStatusOf(s); got != want {
			t.Errorf("planStatusOf(%s) = %v, want %v", s, got, want)
		}
	}
	// intervenedStatus keeps its original meaning: a paused, failed or
	// cancelled task means someone stepped in, which is what the tally in
	// Stats reports.
	for s, want := range map[TaskStatus]bool{
		StatusCompleted: false, StatusDownloading: false, StatusQueued: false,
		StatusPaused: true, StatusCanceled: true, StatusFailed: true,
	} {
		if got := intervenedStatus(s); got != want {
			t.Errorf("intervenedStatus(%s) = %v, want %v", s, got, want)
		}
	}
	counts := taskCounts{paused: 1}
	if !counts.intervened() {
		t.Error("taskCounts.intervened missed a paused task")
	}
	counts = taskCounts{failed: 1}
	if !counts.intervened() {
		t.Error("taskCounts.intervened missed a failed task")
	}
	counts = taskCounts{canceled: 1}
	if !counts.intervened() {
		t.Error("taskCounts.intervened missed a canceled task")
	}
	counts = taskCounts{}
	if counts.intervened() {
		t.Error("taskCounts.intervened fired on a clean plan")
	}

	// A failed task must settle a scoped plan, not hang its watcher: failed
	// is neither downloading nor queued, so the watch ends on the first tick.
	app := newTestApp(t)
	tv, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/never.bin", Dest: filepath.Join(t.TempDir(), "never.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	tk := app.mgr.get(tv.ID)
	tk.mu.Lock()
	tk.view.Status = StatusFailed
	tk.mu.Unlock()

	var mu sync.Mutex
	planned := 0
	app.emit = func(name string, _ ...any) {
		if name == "plan:done" {
			mu.Lock()
			planned++
			mu.Unlock()
		}
	}
	app.mgr.mu.Lock()
	app.mgr.planRunning = true
	app.mgr.planTasks = &planCoverage{ids: map[string]bool{tv.ID: true}}
	app.mgr.mu.Unlock()
	go app.mgr.watchPlanSelected()

	waitFor(t, "the scoped plan to settle on a failed task", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return planned > 0 && !app.mgr.PlanRunning()
	})
}

// TestStartPlanSelectedPropagatesFreeze pins the updating rule: when resumes
// are refused by the update freeze, the caller gets the freeze error rather
// than a misleading "nothing to download".
func TestStartPlanSelectedPropagatesFreeze(t *testing.T) {
	app := newTestApp(t)
	tv, err := app.mgr.Start(StartRequest{
		URL: "http://127.0.0.1:1/frozen.bin", Dest: filepath.Join(t.TempDir(), "frozen.bin"),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	app.mgr.mu.Lock()
	app.mgr.updating = true
	app.mgr.mu.Unlock()
	defer func() {
		app.mgr.mu.Lock()
		app.mgr.updating = false
		app.mgr.mu.Unlock()
	}()

	_, err = app.mgr.StartPlanSelected([]string{tv.ID})
	if !errors.Is(err, errUpdating) {
		t.Errorf("StartPlanSelected error = %v, want the update-freeze error", err)
	}
	if app.mgr.PlanRunning() {
		t.Error("PlanRunning is true after a refused plan; the slot was not released")
	}
}

// TestMoveIntoPlaceUniqueDestAndCopy pins the organize move rules: an
// occupied destination grows a " (n)" suffix, and the copy path preserves
// bytes exactly.
func TestMoveIntoPlaceUniqueDestAndCopy(t *testing.T) {
	dir := t.TempDir()
	if got := uniqueDest(filepath.Join(dir, "free.bin")); got != filepath.Join(dir, "free.bin") {
		t.Errorf("uniqueDest on a free path = %q, want it unchanged", got)
	}

	want := []byte("organized bytes")
	src := filepath.Join(dir, "src.bin")
	if err := os.WriteFile(src, want, 0o644); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "dst.bin")
	if err := os.WriteFile(dst, []byte("someone was here"), 0o644); err != nil {
		t.Fatal(err)
	}

	landed, err := moveIntoPlace(src, dst)
	if err != nil {
		t.Fatalf("moveIntoPlace: %v", err)
	}
	if landed != filepath.Join(dir, "dst (1).bin") {
		t.Errorf("landed = %q, want dst (1).bin", landed)
	}
	got, err := os.ReadFile(landed)
	if err != nil {
		t.Fatalf("read landed file: %v", err)
	}
	if string(got) != string(want) {
		t.Error("landed file contents differ from the source")
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source still exists after the move")
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error("the pre-existing destination was clobbered")
	}

	// The copy fallback itself, exercised directly (a same-volume rename
	// cannot be forced to fail without another drive).
	cs, cd := filepath.Join(dir, "c-src.bin"), filepath.Join(dir, "c-dst.bin")
	if err := os.WriteFile(cs, want, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := copyFile(cs, cd); err != nil {
		t.Fatalf("copyFile: %v", err)
	}
	assertFileEquals(t, cd, want)
}

// disableAutoStart pins direct creation without transferring, so the test
// leaves no background transfer goroutine behind to slow later tests.
func disableAutoStart(t *testing.T, app *App) {
	t.Helper()
	cfg := app.Config()
	cfg.AutoStart = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
}

// TestApiAddFieldLimits pins the /add caps: URL/Referer past 8KB and Filename
// past 255 runes are rejected with 400.
func TestApiAddFieldLimits(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	disableAutoStart(t, app)
	base := startAPI(t, app)

	post := func(body string) int {
		resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop", body)
		defer resp.Body.Close()
		return resp.StatusCode
	}

	bigURL := "http://127.0.0.1:1/" + strings.Repeat("a", 8<<10)
	if code := post(`{"url":` + jsonString(bigURL) + `}`); code != http.StatusBadRequest {
		t.Errorf("oversize url status = %d, want 400", code)
	}
	bigRef := strings.Repeat("r", (8<<10)+1)
	if code := post(`{"url":"http://127.0.0.1:1/x.bin","referer":` + jsonString(bigRef) + `}`); code != http.StatusBadRequest {
		t.Errorf("oversize referer status = %d, want 400", code)
	}
	longName := strings.Repeat("名", 256)
	if code := post(`{"url":"http://127.0.0.1:1/x.bin","filename":` + jsonString(longName) + `}`); code != http.StatusBadRequest {
		t.Errorf("oversize filename status = %d, want 400", code)
	}
	// Boundary values still pass.
	okName := strings.Repeat("n", 255)
	if code := post(`{"url":"http://127.0.0.1:1/ok.bin","filename":` + jsonString(okName) + `}`); code != http.StatusOK {
		t.Errorf("255-rune filename status = %d, want 200", code)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestConcurrentSameContentSingleWinner pins the content-layer race fix: many
// adds for the same host+name at once must leave exactly one task, the rest
// rejected as duplicates.
func TestConcurrentSameContentSingleWinner(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	dupes := 0
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := app.mgr.Start(StartRequest{
				URL:      fmt.Sprintf("http://127.0.0.1:1/token-%d.bin", i),
				Dest:     dir,
				Filename: "race.bin",
				Start:    false,
			})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				won++
				return
			}
			var dup *duplicateError
			if errors.As(err, &dup) {
				dupes++
			} else {
				t.Errorf("add %d error = %v, want nil or duplicate", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if won != 1 {
		t.Errorf("%d adds won, want exactly 1", won)
	}
	if dupes != n-1 {
		t.Errorf("%d duplicates, want %d", dupes, n-1)
	}
}

// TestExtractURLTrimsPunct pins the clipboard rule: a URL lifted out of a
// sentence must not carry the sentence's trailing punctuation with it.
func TestExtractURLTrimsPunct(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://example.com/f.zip", "https://example.com/f.zip"},
		{"see https://example.com/f.zip.", "https://example.com/f.zip"},
		{"（链接https://example.com/文件.zip），请查收", "https://example.com/文件.zip"},
		{"dl: http://127.0.0.1:1/a.bin!)", "http://127.0.0.1:1/a.bin"},
		{"\"https://example.com/q.zip\";", "https://example.com/q.zip"},
		{"no url here", ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := extractURL(c.in); got != c.want {
			t.Errorf("extractURL(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLogURLDesensitised pins the log rule: only host+path may land on disk;
// query strings and fragments (tokens, signatures) must not.
func TestLogURLDesensitised(t *testing.T) {
	got := logURL("https://drive.example.com/uc?id=abc&sig=deadbeef#frag")
	if got != "drive.example.com/uc" {
		t.Errorf("logURL = %q, want host+path only", got)
	}
	if got := logURL("http://127.0.0.1:1/a.bin?token=x"); got != "127.0.0.1:1/a.bin" {
		t.Errorf("logURL = %q, want the query stripped", got)
	}
	if got := logURL(":://bad"); got != "(invalid-url)" {
		t.Errorf("logURL(bad) = %q, want the sentinel", got)
	}
}

// TestPickDuplicateVictimKeepsTheFirstAdd pins the probe-race rule: when two
// probes name their rows out of order, the later-created task goes — never
// the one the user added first.
func TestPickDuplicateVictimKeepsTheFirstAdd(t *testing.T) {
	older := TaskView{ID: "t1-1", Created: "2026-10-16T12:00:00.000000001+08:00"}
	newer := TaskView{ID: "t1-2", Created: "2026-10-16T12:00:00.000000002+08:00"}
	if got := pickDuplicateVictim(older, newer); got != newer.ID {
		t.Errorf("victim = %q, want the newer %q", got, newer.ID)
	}
	if got := pickDuplicateVictim(newer, older); got != newer.ID {
		t.Errorf("victim = %q, want the newer %q regardless of argument order", got, newer.ID)
	}
	// Exact tie breaks on the id sequence.
	tiedOld := TaskView{ID: "t1-7", Created: "2026-10-16T12:00:00+08:00"}
	tiedNew := TaskView{ID: "t1-9", Created: "2026-10-16T12:00:00+08:00"}
	if got := pickDuplicateVictim(tiedOld, tiedNew); got != tiedNew.ID {
		t.Errorf("tied victim = %q, want the larger seq %q", got, tiedNew.ID)
	}
	// Unparseable ids with identical stamps: keep both rather than guess.
	weird1 := TaskView{ID: "a", Created: "2026-10-16T12:00:00+08:00"}
	weird2 := TaskView{ID: "b", Created: "2026-10-16T12:00:00+08:00"}
	if got := pickDuplicateVictim(weird1, weird2); got != "" {
		t.Errorf("tied victim = %q, want empty (keep both)", got)
	}
}

// TestResolveDuplicateLateProbeKeepsFirstTask replays the out-of-order probe
// race deterministically: A added first, B second, both unnamed; B's probe
// lands first (A still unnamed, so no match), then A's probe lands and must
// evict B — never A itself.
func TestResolveDuplicateLateProbeKeepsFirstTask(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	mkUnnamed := func(url, dest string) string {
		v, err := app.mgr.Start(StartRequest{URL: url, Dest: dest, Start: false})
		if err != nil {
			t.Fatalf("Start: %v", err)
		}
		// Simulate "added without a filename, probe pending": Start fills a
		// name only when one is given, and the background probe is what
		// would name these rows — here the test drives the naming order.
		tk := app.mgr.get(v.ID)
		tk.mu.Lock()
		tk.view.Name = ""
		tk.mu.Unlock()
		return v.ID
	}
	// Same host, distinct URLs (first duplicate layer stays out of the way).
	a := mkUnnamed("http://127.0.0.1:1/token-one", filepath.Join(dir, "a.bin"))
	b := mkUnnamed("http://127.0.0.1:1/token-two", filepath.Join(dir, "b.bin"))

	nameRows := func(id, name string) {
		tk := app.mgr.get(id)
		tk.mu.Lock()
		tk.view.Name = name
		tk.mu.Unlock()
	}
	// B's probe lands first: A is still unnamed, so nothing matches and both
	// rows survive.
	nameRows(b, "same-file.bin")
	app.mgr.resolveDuplicate(b, "http://127.0.0.1:1/token-two", "same-file.bin", 0)
	if _, ok := find(app.mgr, a); !ok {
		t.Fatal("A vanished before its own probe landed")
	}
	if _, ok := find(app.mgr, b); !ok {
		t.Fatal("B vanished even though nothing matched it yet")
	}

	// A's probe lands late: it must evict B (the later add), never itself.
	nameRows(a, "same-file.bin")
	app.mgr.resolveDuplicate(a, "http://127.0.0.1:1/token-one", "same-file.bin", 0)
	waitFor(t, "the later add to be evicted", 10*time.Second, func() bool {
		_, ok := find(app.mgr, b)
		return !ok
	})
	if _, ok := find(app.mgr, a); !ok {
		t.Error("A (the first add) was evicted instead of B")
	}
}

// TestSaveTasksConcurrentNoLoss pins the saveMu rule: concurrent settlers
// must not drop each other's tasks from the index.
func TestSaveTasksConcurrentNoLoss(t *testing.T) {
	app := newTestApp(t)
	dir := t.TempDir()

	const n = 16
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = app.mgr.Start(StartRequest{
				URL:  fmt.Sprintf("http://127.0.0.1:1/par-%d.bin", i),
				Dest: filepath.Join(dir, fmt.Sprintf("par-%d.bin", i)),
			})
		}(i)
	}
	wg.Wait()
	if err := app.mgr.saveTasks(); err != nil {
		t.Fatalf("saveTasks: %v", err)
	}

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "tasks.json"))
	if err != nil {
		t.Fatalf("read tasks.json: %v", err)
	}
	var list []persistedTask
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("unmarshal tasks.json: %v", err)
	}
	if len(list) != n {
		t.Errorf("tasks.json holds %d tasks, want %d", len(list), n)
	}
}

// TestDebugLogConcurrent exercises the log from many goroutines; the race
// detector owns the verdict.
func TestDebugLogConcurrent(t *testing.T) {
	app := newTestApp(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 25; j++ {
				app.debugLog("race probe %d/%d", i, j)
			}
		}(i)
	}
	wg.Wait()
}

// stubPeer swaps the OS-touching peer primitives for fakes. Fake exe paths
// never exist on disk, so the trust cache always misses and the stubbed
// verdict is used.
func stubPeer(t *testing.T, exe string, trust bool, tableErr error) {
	t.Helper()
	oldTable, oldImage, oldTrust := tcpOwnerOfPort, processImageOf, verifyTrustOf
	tcpOwnerOfPort = func(uint16) (uint32, error) { return 4242, tableErr }
	processImageOf = func(uint32) (string, error) { return exe, nil }
	verifyTrustOf = func(string) bool { return trust }
	t.Cleanup(func() { tcpOwnerOfPort, processImageOf, verifyTrustOf = oldTable, oldImage, oldTrust })
}

func peerReq() *http.Request {
	return &http.Request{RemoteAddr: "127.0.0.1:54321"}
}

func TestPeerCheckDefaultOn(t *testing.T) {
	if !DefaultConfig().VerifyPeerProcess {
		t.Fatal("VerifyPeerProcess must default on")
	}
}

func TestPeerAllowlistTable(t *testing.T) {
	for _, exe := range []string{"brave.exe", "chrome.exe", "msedge.exe", "opera.exe", "vivaldi.exe", "msedge_beta.exe"} {
		if !allowedBrowserExes[exe] {
			t.Errorf("%s should be allowed", exe)
		}
	}
	for _, exe := range []string{"curl.exe", "notepad.exe", "python.exe", "chrome-updater.exe"} {
		if allowedBrowserExes[exe] {
			t.Errorf("%s should not be allowed", exe)
		}
	}
}

func TestPeerCheckAdmitsSignedBrowser(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, true, nil)
	ok, detail := app.checkPeerProcess(peerReq())
	if !ok || detail != "" {
		t.Fatalf("brave+trusted = %v %q, want true", ok, detail)
	}
}

func TestPeerCheckRejectsUnknownExe(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, `C:\fakepair-vkm\curl.exe`, true, nil)
	ok, detail := app.checkPeerProcess(peerReq())
	if ok {
		t.Fatal("curl.exe must be rejected")
	}
	if detail == "" {
		t.Fatal("rejection should name the exe")
	}
}

func TestPeerCheckRejectsBadSignature(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, false, nil)
	if ok, _ := app.checkPeerProcess(peerReq()); ok {
		t.Fatal("untrusted brave.exe must be rejected")
	}
}

func TestPeerCheckFailOpenOnLookupError(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, "", false, errPeerCheckUnsupported)
	if ok, _ := app.checkPeerProcess(peerReq()); !ok {
		t.Fatal("infrastructure failure must fail open, not lock the user out")
	}
}

func TestPeerGuardEndToEnd(t *testing.T) {
	app := newTestApp(t)
	app.cfg.VerifyPeerProcess = true
	base := startAPI(t, app)

	stubPeer(t, `C:\fakepair-vkm\chrome.exe`, true, nil)
	resp := postJSON(t, base+"/add", "chrome-extension://abc", `{"url":"http://127.0.0.1:1/peer.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed browser got %d, want 200", resp.StatusCode)
	}

	stubPeer(t, `C:\fakepair-vkm\curl.exe`, true, nil)
	resp = postJSON(t, base+"/add", "chrome-extension://abc", `{"url":"http://127.0.0.1:1/peer2.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("curl.exe got %d, want 403", resp.StatusCode)
	}
}

func TestVerifyFileTrustReal(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Authenticode only exists on Windows")
	}
	// Manual-only: calling WinVerifyTrust from a fresh unsigned test binary
	// trips Defender SmartScreen (seen 2026-10-16: two block dialogs).
	// Run with VKMDOWNLOAD_MANUAL_SIGTEST=1 when needed.
	if os.Getenv("VKMDOWNLOAD_MANUAL_SIGTEST") == "" {
		t.Skip("set VKMDOWNLOAD_MANUAL_SIGTEST=1 to run")
	}
	notepad := filepath.Join(os.Getenv("SystemRoot"), "System32", "notepad.exe")
	if _, err := os.Stat(notepad); err != nil {
		t.Skip("no notepad to verify against")
	}
	if !verifyFileTrust(notepad) {
		t.Fatal("Microsoft-signed notepad.exe must verify")
	}
	if verifyFileTrust(filepath.Join(t.TempDir(), "nope.exe")) {
		t.Fatal("missing file must not verify")
	}
}

func TestPeerTrustCacheHonored(t *testing.T) {
	app := newTestApp(t)
	// A real file named like an allowed browser, so the cache (keyed on
	// mtime+size) engages. None of this exists outside the temp dir.
	exe := filepath.Join(t.TempDir(), "brave.exe")
	if err := os.WriteFile(exe, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTable, oldImage, oldTrust := tcpOwnerOfPort, processImageOf, verifyTrustOf
	tcpOwnerOfPort = func(uint16) (uint32, error) { return 4242, nil }
	processImageOf = func(uint32) (string, error) { return exe, nil }
	verifyTrustOf = func(string) bool { return true }
	t.Cleanup(func() { tcpOwnerOfPort, processImageOf, verifyTrustOf = oldTable, oldImage, oldTrust })
	if ok, _ := app.checkPeerProcess(peerReq()); !ok {
		t.Fatal("trusted brave.exe must pass and populate the cache")
	}
	// Flip the stub: a cache hit must still admit without consulting it.
	// Fresh .exe files in Temp may be briefly unreadable while Defender
	// scans them, so poll for the warm cache instead of asserting at once.
	consulted := false
	verifyTrustOf = func(string) bool {
		consulted = true
		return false
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		consulted = false
		ok, _ := app.checkPeerProcess(peerReq())
		if !ok {
			t.Fatal("warm cache must admit")
		}
		if !consulted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cache never warmed: verifier consulted every time")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Change the file: mtime/size key breaks, the (now false) stub rules.
	if err := os.WriteFile(exe, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _ := app.checkPeerProcess(peerReq()); ok {
		t.Fatal("changed binary must re-verify")
	}
}
