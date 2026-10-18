package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// appVersion is what Version() reports and the browser extension displays.
const appVersion = "0.2.7"

// maxDroppedRead bounds how much of a dropped file is read looking for a
// link: shortcuts and notes are tiny, and the bridge should not carry
// megabytes of text.
const maxDroppedRead = 256 << 10

// droppedText turns one dropped path into the text the UI scans for a
// download link. A dropped shortcut or text file carries its link inside it
// (a .url file is an INI whose URL= line holds the target), so small text
// files are handed over by content; anything else by path, which the UI
// scans just the same. Only the first match matters to the UI.
func droppedText(path string) string {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxDroppedRead {
		return path
	}
	b, err := os.ReadFile(path)
	if err != nil || !utf8.Valid(b) {
		return path
	}
	return string(b)
}

// App is the object exposed to the front-end. Every exported method becomes a
// callable binding, so internal state is kept unexported.
type App struct {
	ctx context.Context

	// emit publishes a bridge event. It is a field rather than a direct call to
	// runtime.EventsEmit because that function requires the exact context Wails
	// hands to the lifecycle hooks, which tests cannot supply; injecting a no-op
	// keeps the download logic testable end to end.
	emit func(name string, data ...any)

	// hide conceals the window. Same reason as emit: runtime.WindowHide aborts
	// the process on a foreign context (log.Fatalf), so tests inject a spy.
	hide func(ctx context.Context)

	mu  sync.Mutex
	cfg Config

	mgr *Manager

	// updMu guards upd, the self-update state machine in update.go, and the
	// release-notes cache below.
	updMu sync.Mutex
	upd   UpdateStatus
	// updStagedSHA/updStagedSize identify the downloaded replacement. They are
	// recorded when the download finished, re-checked immediately before the
	// swap, and written into the update marker so the next launch can tell
	// which of the two builds is actually running.
	updStagedSHA  string
	updStagedSize int64
	// notesVersion/notesBody cache the last fetched release notes so opening
	// the "what's new" dialog twice costs one network round trip.
	notesVersion string
	notesBody    string

	// updateToken is the secret this process was started with by an update
	// helper. Non-empty means "I am the new version": domReady then writes the
	// handshake file the helper is waiting for, which is the only proof the
	// update worked.
	updateToken string

	// sys, quit, sleepFor and exePaths are the update path's outside world,
	// injected so the whole hand-off (including a quit that never happens) can
	// be tested without a real executable anywhere near it.
	sys      updateSys
	quit     func(ctx context.Context)
	sleepFor func(d time.Duration)
	exePaths func() (exe, staged string, err error)

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

	// quitFlag records an explicit exit intent (tray 退出 / Quit binding).
	// Wails Windows Frontend.Quit consults OnBeforeClose first, so without
	// this flag a Quit requested while MinimizeToTray is on is vetoed by
	// beforeClose and the process lingers in the background with no window
	// and no tray icon.
	quitFlag atomic.Bool
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
	a.hide = func(ctx context.Context) {
		runtime.WindowHide(ctx)
	}
	a.quit = runtime.Quit
	a.sys = realUpdateSys{}
	a.sleepFor = time.Sleep
	a.exePaths = stagedUpdatePath
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

	// Write the settings only when the file does not exist yet: a fresh
	// install otherwise has no file on disk until the user changes something,
	// which makes "where are my settings?" unanswerable and hides a broken
	// data directory. Writing unconditionally on every start just churned
	// the disk (and the antivirus real-time scan) to re-persist values the
	// file already holds.
	if _, err := os.Stat(a.configPath()); os.IsNotExist(err) {
		if err := saveConfig(a.configPath(), a.cfg); err != nil {
			runtime.LogErrorf(ctx, "保存设置失败: %v", err)
		}
	}

	// Everything that does not read the task table starts first: the tray
	// and the update check used to queue behind loadTasks for no reason of
	// their own. The clipboard watch and the extension endpoint stay after
	// the restore — a capture or an /add arriving mid-restore could otherwise
	// slip past a duplicate check that has not seen the disk tasks yet.

	// System-tray icon. Without it "minimise to tray" hides the window with
	// nothing to bring it back from, which reads as a broken close button.
	go a.startTray()

	// Files dropped onto the window become new tasks. Small text files are
	// handed over by content so a dropped .url shortcut actually works; the
	// UI scans whatever arrives for an http(s) link.
	runtime.OnFileDrop(ctx, func(_, _ int, paths []string) {
		for _, p := range paths {
			a.emit("files:dropped", droppedText(p))
		}
	})

	// Update check never blocks startup and never fails it: offline machines
	// start exactly as fast as online ones.
	go a.checkUpdateOnce()

	// Whatever the previous update left behind (a backup copy of the old
	// version, an old handshake file) is swept in the background. It never
	// touches anything unless the record proves the update succeeded.
	go a.cleanupStaleUpdate()

	a.mgr.loadTasks()
	go a.mgr.Run(ctx)
	// Tasks that were queued when the app closed resume waiting for a slot;
	// without this pump they would display as queued while the queue stood
	// still.
	a.mgr.pumpQueue()

	// The browser extension's endpoint. It lives and dies with the app's
	// context rather than the window's, so closing the window to the tray does
	// not cut the extension off.
	go a.startLocalAPI(ctx)

	a.RestartClipboardWatch()
}

// domReady runs after the front-end has loaded.
func (a *App) domReady(ctx context.Context) {
	_ = ctx
	// The "restore finished" nudge lives here, not in startup: Wails buffers
	// no events, and an emit from startup races the webview — it is gone
	// before any JS listener exists. domReady fires after the page (and the
	// React app's first effects, which subscribe) has loaded, so this is the
	// first moment the event can actually be received. It covers the narrow
	// case where the initial listTasks call slipped in before loadTasks
	// finished and saw an empty table.
	a.emit("tasks:loaded", nil)

	// The update handshake belongs here for the same reason: the file it writes
	// is this process saying "the new build reached a running window", and that
	// is only true once the page has loaded.
	a.writeUpdateHandshake()
}

// shutdown releases resources as the application exits.
func (a *App) shutdown(ctx context.Context) {
	a.stopTray()
	_ = a.mgr.saveTasks()
	_ = ctx
}

// beforeClose is consulted when the user closes the window.
//
// The return value is `prevent`: true refuses the close. (Getting this backwards
// made the window impossible to close at all, since both of the early exits
// returned true.)
//
// When "minimise to tray" is on the close always becomes a hide: silently
// killing a download because the window was closed is exactly the behaviour a
// download manager must not have, and the tray icon (with 显示窗口/退出) is
// what brings the window back. Real exits go through the tray's 退出 item
// (or Quit binding), which sets quitFlag via requestQuit first — Wails
// Windows Frontend.Quit consults this same hook, so without the bypass below
// every explicit quit is vetoed and the process lingers headless.
func (a *App) beforeClose(ctx context.Context) (prevent bool) {
	if a.quitFlag.Load() {
		return false
	}
	cfg := a.Config()
	if !cfg.MinimizeToTray {
		return false
	}
	a.hide(ctx)
	return true
}

// requestQuit records an explicit exit intent so the next beforeClose lets
// the quit through instead of turning it into a hide.
func (a *App) requestQuit() { a.quitFlag.Store(true) }

// onSecondInstance runs in the first instance when the user launches the app
// again. The second process exits on its own; this only brings the existing
// window back. Both calls matter: Unminimise covers a taskbar-minimised
// window, Show covers a tray-hidden one — the same pair the tray's 显示窗口
// item relies on.
func (a *App) onSecondInstance(_ options.SecondInstanceData) {
	if a.ctx == nil {
		return
	}
	runtime.WindowUnminimise(a.ctx)
	runtime.WindowShow(a.ctx)
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
	oldMax := a.cfg.MaxConcurrent
	a.cfg = cfg
	a.mu.Unlock()

	if err := saveConfig(a.configPath(), cfg); err != nil {
		return err
	}
	a.RestartClipboardWatch()
	// Live-apply without restarting transfers: the shared limiter retunes in
	// place, and a raised task budget pulls queued work in at once. A lowered
	// budget needs no action — running transfers finish, and the queue gate
	// already honours the new value.
	a.mgr.applySpeedLimit(cfg.SpeedLimitKB)
	a.mgr.applyRefreshInterval(refreshInterval(cfg.RefreshRate))
	if cfg.MaxConcurrent > oldMax {
		a.mgr.pumpQueue()
	}
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
	// The dialog is an explicit placement either way: even "add to list" means
	// the user saw and confirmed the destination, so the organize step must
	// not re-sort it on completion.
	req.Manual = true
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
// Failed tasks are unfinished by definition, so their partials go with the
// records (same rule as a single removal); finished files elsewhere are
// never touched.
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
// Finished files are never touched; unfinished partials go with their records.
func (a *App) RemoveTasks(ids []string) int {
	n := a.mgr.RemoveTasks(ids)
	a.debugLog("removed %d tasks by selection", n)
	return n
}

// StartPlan starts every waiting task and runs the end-of-plan action once they
// have all settled.
func (a *App) StartPlan() (int, error) { return a.mgr.StartPlan() }

// StartPlanSelected starts the selected tasks and runs the end-of-plan action
// once they have all settled. Tasks outside the selection are ignored.
func (a *App) StartPlanSelected(ids []string) (int, error) { return a.mgr.StartPlanSelected(ids) }

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
		// what a double-click in Explorer would do. It must run without a
		// console: cmd is a console application, and starting one without
		// CREATE_NO_WINDOW flashes a black window over the user's work.
		return hiddenCmd("cmd", "/c", "start", "", v.Dest).Start()
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
// urlRe finds a URL inside clipboard text. CJK punctuation is excluded from
// the match outright: unlike ASCII closers (which may be legal inside a URL
// and are only stripped when trailing by extractURL below), a full-width mark
// essentially never is, and Chinese prose has no spaces to stop the match.
var urlRe = regexp.MustCompile(`(?i)https?://[^\s"'<>（）【】「」，。；：！？]+`)

// trailingPunct is stripped from an extracted URL: copying "see
// https://x/y.zip." out of a sentence otherwise bakes the full stop into the
// download link. Both CJK and ASCII closers are covered.
const trailingPunct = ".,;:!?)]}'\"" + "，。；：！？）】」"

// extractURL returns the first URL in text, without trailing punctuation.
// clipboardLoop and ClipboardProbe share it so the poller and the settings
// "test" button agree on what counts as a URL.
func extractURL(text string) string {
	return strings.TrimRight(urlRe.FindString(strings.TrimSpace(text)), trailingPunct)
}

func (a *App) clipboardLoop(intervalMs int, gen int64) {
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
				if m := extractURL(trimmed); m != "" {
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
	if cfg.AskLocation {
		// The front-end opens the new-task dialog prefilled; closing it
		// abandons the download, so nothing is created here.
		a.emit("location:ask", map[string]string{"url": url})
		return
	}
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
	if m := extractURL(text); m != "" {
		out["url"] = m
	}
	return out, nil
}

// --------------------------------------------------------------- lifecycle

// Version reports the running version shown in the UI.
//
// It reads the same constant the loopback endpoint hands the browser extension,
// so the two cannot drift: they did, briefly, because this was a literal.
func (a *App) Version() string { return appVersion }

// BringToFront pulls the main window in front of other windows.
//
// It exists for the ask-location dialog: a download arrives while the user is
// in the browser, and the dialog opens inside our window — which sits behind
// the browser, forcing the user to minimise it to find the question. Wails
// has no focus API, so this uses the standard always-on-top flash: pin, show,
// unpin. The window ends up frontmost in z-order without staying pinned.
func (a *App) BringToFront() {
	ctx := a.ctx
	if ctx == nil {
		return
	}
	runtime.WindowUnminimise(ctx)
	runtime.WindowShow(ctx)
	runtime.WindowSetAlwaysOnTop(ctx, true)
	runtime.WindowSetAlwaysOnTop(ctx, false)
}

// FindUnfinishedByURL returns the id of the unfinished task for url, or "".
//
// It backs the new-task dialog's "already listed" path so the UI can point at
// the existing record instead of just complaining.
func (a *App) FindUnfinishedByURL(url string) string {
	if t := a.mgr.findUnfinished(strings.TrimSpace(url), ""); t != nil {
		return snapshot(t).ID
	}
	return ""
}

// Quit terminates the application, backing the front-end's own exit control.
func (a *App) Quit() {
	a.requestQuit()
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
		_ = hiddenCmd("cmd", "/c", "start", "", v.Dest).Start()
	}
}
