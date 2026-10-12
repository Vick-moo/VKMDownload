package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// appVersion is what Version() reports and the browser extension displays.
const appVersion = "0.2.3"

// App is the object exposed to the front-end. Every exported method becomes a
// callable binding, so internal state is kept unexported.
type App struct {
	ctx context.Context

	// emit publishes a bridge event. It is a field rather than a direct call to
	// runtime.EventsEmit because that function requires the exact context Wails
	// hands to the lifecycle hooks, which tests cannot supply; injecting a no-op
	// keeps the download logic testable end to end.
	emit func(name string, data ...any)

	mu  sync.Mutex
	cfg Config

	mgr *Manager

	// updMu guards upd, the self-update state machine in update.go.
	updMu sync.Mutex
	upd   UpdateStatus

	// clipboard monitoring state
	lastClip    string
	lastClipHit string
	clipRunning bool
	// clipGen increments on every restart, so a poller from a previous
	// generation always exits even if a new one already set clipRunning back
	// to true — a bare bool let the old loop survive as a duplicate.
	clipGen int64

	// api is the loopback endpoint the browser extension talks to.
	api apiState

	// dataDirOverride, when set, replaces the directory settings and the task
	// index are read from. Tests set it so they cannot touch the real user
	// profile: os.UserConfigDir reads the environment, which t.Setenv does not
	// reliably redirect on Windows, and a test that silently writes the user's
	// own task list is worse than no test.
	dataDirOverride string
}

// NewApp creates the application object and its task manager.
//
// The emitter is a field rather than a direct call to runtime.EventsEmit so
// that tests can replace it: emitting without a window aborts the process.
// Before the runtime is up there is no context, and the event is dropped by the
// nil check below.
func NewApp() *App {
	a := &App{cfg: DefaultConfig()}
	a.emit = func(name string, data ...any) {
		if a.ctx == nil {
			return
		}
		runtime.EventsEmit(a.ctx, name, data...)
	}
	a.mgr = NewManager(a)
	return a
}

// NewAppWithDataDir is NewApp with the settings directory pinned, which is what
// tests use to stay off the real profile.
func NewAppWithDataDir(dir string) *App {
	a := NewApp()
	a.dataDirOverride = dir
	return a
}

// ---------------------------------------------------------------- lifecycle

// startup is called once the runtime is ready but before the window is shown.
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx

	a.mu.Lock()
	a.cfg = loadConfig(a.configPath())
	a.mu.Unlock()

	// Write the settings back immediately. A fresh install otherwise has no
	// file on disk until the user changes something, which makes "where are my
	// settings?" unanswerable and hides a broken data directory until later.
	if err := saveConfig(a.configPath(), a.cfg); err != nil {
		runtime.LogErrorf(ctx, "保存设置失败: %v", err)
	}

	a.mgr.loadTasks()
	go a.mgr.Run(ctx)

	// The browser extension's endpoint. It lives and dies with the app's
	// context rather than the window's, so closing the window to the tray does
	// not cut the extension off.
	go a.startLocalAPI(ctx)

	// Files dropped onto the window become new tasks.
	runtime.OnFileDrop(ctx, func(_, _ int, paths []string) {
		for _, p := range paths {
			a.emit("files:dropped", p)
		}
	})

	a.RestartClipboardWatch()

	// Update check never blocks startup and never fails it: offline machines
	// start exactly as fast as online ones.
	go a.checkUpdateOnce()
}

// domReady runs after the front-end has loaded.
func (a *App) domReady(ctx context.Context) {
	_ = ctx
}

// shutdown releases resources as the application exits.
func (a *App) shutdown(ctx context.Context) {
	_ = a.mgr.saveTasks()
	_ = ctx
}

// beforeClose is consulted when the user closes the window.
//
// The return value is `prevent`: true refuses the close. (Getting this backwards
// made the window impossible to close at all, since both of the early exits
// returned true.)
//
// When "minimise to tray" is on and something is still transferring, refusing is
// the right answer: silently killing a download because the window was closed is
// exactly the behaviour a download manager must not have. With nothing running
// the close goes through, because there is no tray icon to bring the window
// back from.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	cfg := a.Config()
	if !cfg.MinimizeToTray {
		return false
	}
	if a.mgr.Stats()["active"] == 0 {
		return false
	}
	runtime.WindowHide(ctx)
	return true
}

// ------------------------------------------------------------------ paths

const (
	// appDirName is the folder under the user's config directory that holds the
	// settings and the task index. It follows the product name.
	appDirName = "VKMDownload"
)

// dataDir is where settings and the task index live.
func (a *App) dataDir() string {
	if a.dataDirOverride != "" {
		return a.dataDirOverride
	}
	return filepath.Join(configBase(), appDirName)
}

// configBase is the user's config directory, or the working directory when the
// shell does not report one.
func configBase() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return "."
	}
	return base
}

func (a *App) configPath() string { return filepath.Join(a.dataDir(), "config.json") }

// ------------------------------------------------------------------ config

// Config returns a copy of the current settings.
func (a *App) Config() Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// SetConfig replaces the settings and persists them.
func (a *App) SetConfig(cfg Config) error {
	cfg.normalize()
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()

	if err := saveConfig(a.configPath(), cfg); err != nil {
		return err
	}
	a.RestartClipboardWatch()
	return nil
}

// PickDirectory opens a folder chooser and returns the selection.
func (a *App) PickDirectory() (string, error) {
	return runtime.OpenDirectoryDialog(a.ctx, runtime.OpenDialogOptions{
		Title:            "选择保存目录",
		DefaultDirectory: a.Config().DownloadDir,
	})
}

// -------------------------------------------------------------- task verbs

// NewTask creates a task. When req.Start is set the transfer begins at once;
// otherwise it is only added to the list.
func (a *App) NewTask(req StartRequest) (TaskView, error) {
	return a.mgr.Start(req)
}

// ListTasks returns every task, newest first.
func (a *App) ListTasks() []TaskView { return a.mgr.List() }

// TaskStats returns the aggregate figures the sidebar shows.
func (a *App) TaskStats() map[string]int64 { return a.mgr.Stats() }

// PauseTask keeps the partial data so the task can continue later.
func (a *App) PauseTask(id string) error { return a.mgr.Pause(id) }

// ResumeTask continues a paused, failed or cancelled task.
func (a *App) ResumeTask(id string) error { return a.mgr.Resume(id) }

// CancelTask stops a task and discards its partial data.
func (a *App) CancelTask(id string) error { return a.mgr.Cancel(id) }

// RemoveTask drops a task from the list. Nothing on disk is touched unless
// deleteFiles is set.
func (a *App) RemoveTask(id string, deleteFiles bool) error {
	return a.mgr.Remove(id, deleteFiles)
}

// StartAllTasks resumes every waiting task, returning how many were started.
func (a *App) StartAllTasks() int { return a.mgr.StartAll() }

// StopAllTasks pauses every transferring task, returning how many were paused.
func (a *App) StopAllTasks() int { return a.mgr.StopAll() }

// ClearFailedTasks removes every failed record, returning how many went.
//
// Records only: the partial files stay, since they are what a later attempt at
// the same URL resumes from.
func (a *App) ClearFailedTasks() int {
	n := a.mgr.RemoveFailed()
	a.debugLog("cleared %d failed tasks", n)
	return n
}

// RemoveTasks removes the given records, returning how many went.
//
// It takes ids rather than a filter so the front-end can offer "delete
// everything I can see" over whatever it is currently showing — a file type, a
// status, a search — without the rule for what is visible living in two places.
// Records only, as with every other removal.
func (a *App) RemoveTasks(ids []string) int {
	n := a.mgr.RemoveTasks(ids)
	a.debugLog("removed %d tasks by selection", n)
	return n
}

// StartPlan starts every waiting task and runs the end-of-plan action once they
// have all settled.
func (a *App) StartPlan() (int, error) { return a.mgr.StartPlan() }

// PlanRunning reports whether a plan is in progress.
func (a *App) PlanRunning() bool { return a.mgr.PlanRunning() }

// ------------------------------------------------------------- file actions

// RevealInExplorer opens the containing folder with the file selected.
func (a *App) RevealInExplorer(id string) error {
	for _, v := range a.mgr.List() {
		if v.ID != id {
			continue
		}
		// explorer /select highlights the file; on a path that does not exist
		// yet the call would fail, so fall back to the directory.
		if _, err := os.Stat(v.Dest); err == nil {
			return exec.Command("explorer", "/select,"+v.Dest).Start()
		}
		dir := filepath.Dir(v.Dest)
		if _, err := os.Stat(dir); err == nil {
			return exec.Command("explorer", dir).Start()
		}
		return os.ErrNotExist
	}
	return os.ErrNotExist
}

// OpenFile launches a finished download with the system default application.
func (a *App) OpenFile(id string) error {
	for _, v := range a.mgr.List() {
		if v.ID != id {
			continue
		}
		if _, err := os.Stat(v.Dest); err != nil {
			return os.ErrNotExist
		}
		// "cmd /c start" hands the file to its registered handler, which is
		// what a double-click in Explorer would do.
		return exec.Command("cmd", "/c", "start", "", v.Dest).Start()
	}
	return os.ErrNotExist
}

// --------------------------------------------------------- clipboard watch

// RestartClipboardWatch starts or stops the clipboard poller to match the
// current setting. It is safe to call repeatedly.
func (a *App) RestartClipboardWatch() {
	a.mu.Lock()
	// Invalidate any running poller first: it exits on its next tick.
	a.clipGen++
	a.clipRunning = false
	want := a.cfg.ClipboardWatch
	interval := a.cfg.ClipboardIntervalMs
	gen := a.clipGen
	a.mu.Unlock()

	if !want || a.ctx == nil {
		return
	}
	a.mu.Lock()
	a.clipRunning = true
	a.mu.Unlock()
	go a.clipboardLoop(interval, gen)
}

// clipboardLoop polls the clipboard for URLs.
//
// Wails v2 offers no clipboard change event, so polling is the only option. The
// OS clipboard is queried at a fixed interval and the text is compared against
// the previous value, which also means an unchanged clipboard costs one cheap
// call and nothing else.
func (a *App) clipboardLoop(intervalMs int, gen int64) {
	urlRe := regexp.MustCompile(`(?i)https?://[^\s"'<>]+`)
	timer := time.NewTimer(time.Duration(intervalMs) * time.Millisecond)
	defer timer.Stop()

	for {
		a.mu.Lock()
		running := a.clipRunning && a.clipGen == gen
		a.mu.Unlock()
		if !running || a.ctx == nil {
			return
		}

		text, err := runtime.ClipboardGetText(a.ctx)
		if err == nil && text != "" {
			trimmed := strings.TrimSpace(text)
			a.mu.Lock()
			changed := trimmed != a.lastClip
			a.lastClip = trimmed
			a.mu.Unlock()

			// Only act on text the user copied, and only once per value: a URL
			// already turned into a task must not be turned into another on
			// every poll.
			if changed {
				if m := urlRe.FindString(trimmed); m != "" {
					a.mu.Lock()
					dupe := m == a.lastClipHit
					a.lastClipHit = m
					a.mu.Unlock()
					if !dupe {
						a.offerClipboardURL(m)
					}
				}
			}
		}

		select {
		case <-a.ctx.Done():
			return
		case <-timer.C:
			timer.Reset(time.Duration(intervalMs) * time.Millisecond)
		}
	}
}

// offerClipboardURL acts on a URL found on the clipboard.
func (a *App) offerClipboardURL(url string) {
	cfg := a.Config()
	a.emit("clipboard:url", url)

	if cfg.ClipboardConfirm {
		// The front-end shows the dialog; it calls NewTask when accepted.
		return
	}
	if !cfg.ClipboardAutoStart {
		// Add it to the list without transferring.
		_, _ = a.mgr.Start(StartRequest{URL: url, Start: false})
		return
	}
	_, _ = a.mgr.Start(StartRequest{URL: url, Start: true})
}

// ClipboardProbe reads the clipboard once, for the settings page's "test"
// button, and reports what would happen.
func (a *App) ClipboardProbe() (map[string]string, error) {
	text, err := runtime.ClipboardGetText(a.ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{"text": text, "url": ""}
	if m := regexp.MustCompile(`(?i)https?://[^\s"'<>]+`).FindString(strings.TrimSpace(text)); m != "" {
		out["url"] = m
	}
	return out, nil
}

// --------------------------------------------------------------- lifecycle

// Version reports the build identifier shown in the UI.
// Version reports the running version.
//
// It reads the same constant the loopback endpoint hands the browser extension,
// so the two cannot drift: they did, briefly, because this was a literal.
func (a *App) Version() string { return appVersion }

// Quit terminates the application, backing the front-end's own exit control.
func (a *App) Quit() {
	_ = a.mgr.saveTasks()
	runtime.Quit(a.ctx)
}

// afterTaskFinished runs the completions actions the settings ask for.
func (a *App) afterTaskFinished(v TaskView) {
	cfg := a.Config()
	if v.Status != StatusCompleted {
		return
	}
	if cfg.OpenDirWhenDone {
		_ = exec.Command("explorer", "/select,"+v.Dest).Start()
		return
	}
	if cfg.OpenFileWhenDone {
		_ = exec.Command("cmd", "/c", "start", "", v.Dest).Start()
	}
}
