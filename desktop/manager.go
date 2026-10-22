package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"vkmdownload/internal/engine"
)

// debugLog appends a diagnostic line to the app's log file. It exists so a
// failure that would otherwise be invisible — a panic swallowed by a
// goroutine — leaves a trace the user can hand back.
//
// It writes through the app's data directory rather than computing its own, so
// a test's override applies here too. When it did not, every test run appended
// to the real log and, worse, created the real directory as a side effect —
// which then defeated the migration that moves the folder after a rename.
//
// The log is capped: past debugLogMaxBytes it is rotated to a single backup,
// so a hot loop can never fill the disk.
const debugLogMaxBytes = 2 << 20

// debugLogMu serialises log writes: concurrent task goroutines otherwise
// interleave partial lines and race the rotation check.
var debugLogMu sync.Mutex

func (a *App) debugLog(format string, args ...any) {
	// Read through the atomic mirror of cfg.LoggingEnabled, not Config():
	// debugLog is called from loadConfig itself while the config lock is held,
	// so taking that lock again here would deadlock the startup path.
	if !a.logEnabled.Load() {
		return
	}
	debugLogMu.Lock()
	defer debugLogMu.Unlock()
	dir := a.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := filepath.Join(dir, "vkmdownload.log")
	if st, err := os.Stat(path); err == nil && st.Size() > debugLogMaxBytes {
		_ = os.Remove(path + ".1")
		_ = os.Rename(path, path+".1")
	}
	f, err := os.OpenFile(path,
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	// Sanitised on the way out, not at each call site. One call site that
	// forgets logURL is all it takes for a signed link to be written here for
	// good, and this file is exactly what gets attached to a bug report. The
	// existing line above still uses logURL for its own readability; this is the
	// backstop that makes a mistake in a new call site harmless.
	line := sanitizeLogText(fmt.Sprintf(format, args...))
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), line)
}

// TaskStatus is the lifecycle of a task; types.ts mirrors these values.
type TaskStatus string

const (
	StatusQueued      TaskStatus = "queued"
	StatusDownloading TaskStatus = "downloading"
	StatusPaused      TaskStatus = "paused"
	StatusCompleted   TaskStatus = "completed"
	StatusFailed      TaskStatus = "failed"
	StatusCanceled    TaskStatus = "canceled"
)

// TaskView is a task as the front-end needs it.
//
// The engine reports segment *counts* rather than segment boundaries, so the
// UI renders one bar plus "分片 done/total" rather than independent blocks.
// Guessing the boundaries would draw blocks that do not correspond to what is
// actually transferring.
type TaskView struct {
	ID         string     `json:"id"`
	URL        string     `json:"url"`
	Name       string     `json:"name"`
	Dest       string     `json:"dest"`
	Size       int64      `json:"size"`
	Downloaded int64      `json:"downloaded"`
	Speed      int64      `json:"speed"`
	ETA        int64      `json:"eta"`
	Status     TaskStatus `json:"status"`
	Error      string     `json:"error"`
	// Notice carries a transient message for a task that is still working —
	// today, the "server is temporarily returning 404, retrying in N s"
	// recovery wait. It is kept apart from Error because it does not mean the
	// task failed: the row stays in the downloading state throughout.
	Notice   string `json:"notice,omitempty"`
	Conns    int    `json:"connections"`
	Segments int    `json:"segments"`
	SegsDone int    `json:"segmentsDone"`
	Created  string `json:"createdAt"`
	Finished string `json:"finishedAt"`
}

// task is the mutable record behind a TaskView.
type task struct {
	mu   sync.Mutex
	view TaskView

	// cancel stops the in-flight transfer; nil when nothing is running.
	cancel context.CancelFunc
	// done closes when the transfer goroutine has fully exited. File cleanup
	// must wait on it: cancelling only asks the goroutine to stop, and it can
	// still be mid-write, recreating the partial file just after it was
	// deleted.
	done chan struct{}
	// generation increments on every start, so a progress callback from a
	// superseded run cannot overwrite the state of the current one.
	generation int64

	// stopIntent is what the user asked for when a transfer was stopped.
	//
	// The goroutine that notices the cancellation reads this rather than the
	// view's status, because it can observe the cancellation before the action
	// that caused it has finished writing the status — which turned a pause
	// into a cancel and cost the user the ability to resume.
	stopIntent TaskStatus

	// manual mirrors StartRequest.Manual: the user placed this task
	// explicitly, so the organize step leaves it alone on completion.
	manual bool

	// referer is sent with every request for this task. Set once at creation and
	// never changed, so it needs no lock.
	referer string
	// cookie and userAgent are the browser's credentials replayed for this
	// task, like referer above. Set once at creation, never changed.
	cookie    string
	userAgent string

	// host caches urlHost(view.URL) for the content-based duplicate check,
	// which walks every task per add. Loaded tasks fill it lazily on first
	// use; an empty value simply falls back to parsing.
	host string
}

// waitStopped blocks until the transfer goroutine has exited, or the timeout
// elapses. A task that was never started returns immediately.
func (t *task) waitStopped(timeout time.Duration) {
	t.mu.Lock()
	done := t.done
	t.mu.Unlock()
	if done == nil {
		return
	}
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

// StartRequest is what the new-task dialog sends.
type StartRequest struct {
	URL      string `json:"url"`
	Dest     string `json:"dest"`
	Filename string `json:"filename"`
	Conns    int    `json:"connections"`
	Start    bool   `json:"start"`
	// Referer is sent with every request for this task. The browser extension
	// supplies it, because some hosts refuse a download whose request does not
	// appear to come from their own page.
	Referer string `json:"referer"`
	// Cookie is the browser's cookies for the URL, replayed verbatim. Without
	// it the engine looks like a stranger to hosts that require a login
	// (Google Classroom / Drive private files) and is bounced to a sign-in
	// page. Empty for manual and clipboard tasks, which have no browser
	// session to borrow.
	Cookie string `json:"cookie"`
	// UserAgent overrides the engine's default non-browser UA. Some
	// hosts/WAFs refuse unknown agents, so the browser's own string is
	// replayed when the extension supplied one.
	UserAgent string `json:"userAgent"`
	// Size is the total size the caller already knows — the browser read it
	// from Content-Length before handing the download over. It fills the row
	// immediately, so a paused task does not sit there reading "0 B" until
	// someone presses start, and the same-content duplicate check gets a
	// signal it would otherwise lack. 0 means unknown: nothing happens.
	Size int64 `json:"size"`
	// Manual marks a task the user placed explicitly (the dialog). Automatic
	// arrivals leave it false, which is what the organize step keys on: only
	// unplaced tasks are sorted into group folders on completion.
	Manual bool `json:"manual"`
}

// Manager owns every task.
type Manager struct {
	app *App

	mu    sync.Mutex
	tasks map[string]*task
	order []string // newest first

	// pump carries progress events to a single emitter goroutine, so a slow
	// webview can never block a download goroutine.
	pump chan TaskView

	seq int64

	// limiter is shared by every transfer, so the configured speed limit is a
	// ceiling on the total rather than per task.
	limMu   sync.Mutex
	limiter *engine.Limiter
	limRate int64

	// livedl tracks running engine instances by task id, so settings that
	// retune live (refresh cadence) can reach transfers already in flight.
	livedlMu sync.Mutex
	livedl   map[string]*engine.Downloader

	// saveDepth/saveDirty coalesce index writes across bulk verbs: StartAll
	// over a hundred tasks used to rewrite tasks.json a hundred times.
	// While depth > 0, saveTasks only marks dirty; batch flushes once.
	saveDirty bool
	saveDepth int

	// saveMu serialises index writes: concurrent settlers (completion,
	// failure, probe fill) otherwise interleave a read-modify-write and drop
	// each other's updates. m.mu is never held across it.
	saveMu sync.Mutex

	// planRunning is set while a plan is working through its tasks.
	planRunning bool
	// planTasks is the task set the running plan waits for. Membership, not a
	// global tally, decides when the plan is done and whether it was
	// intervened with.
	planTasks *planCoverage
	// planDone records the tasks that had already completed when the plan
	// started. They are the only members that may be counted as settled
	// without the plan having done anything: every other member — running,
	// queued, or paused at the start and resumed by StartAll — is work the
	// plan took on. A member that ends paused, failed or cancelled after being
	// taken on is the user (or the server) stopping it, so the plan settles as
	// intervened and the end-of-plan actions are skipped.
	planDone map[string]bool

	// updating freezes scheduling: set while an update is being applied, so
	// pausing everything for the restart cannot be undone by the queue
	// starter waking a queued task back up.
	updating bool

	// inflight marks url+dest pairs currently being added, so two /add
	// arriving in the same instant cannot both slip past findUnfinished.
	// The check-then-insert in Start is not atomic on its own: both would
	// see "no duplicate" before either inserted.
	inflight map[string]struct{}

	// probeSem bounds concurrent filename probes: a burst of unnamed adds
	// (the extension can deliver dozens at once) otherwise opens dozens of
	// simultaneous dials and engine instances against one weak host.
	probeSem chan struct{}

	// bgWait tracks the manager's background goroutines — transfers, settle
	// waiters, name probes — so a caller can wait for true quiescence. Test
	// teardown needs exactly that: a goroutine holding tasks.json open at
	// the cleanup instant is what made Windows temp-dir removal flaky.
	bgWait sync.WaitGroup
}

func NewManager(app *App) *Manager {
	return &Manager{
		app:      app,
		tasks:    make(map[string]*task),
		pump:     make(chan TaskView, 512),
		livedl:   make(map[string]*engine.Downloader),
		inflight: make(map[string]struct{}),
		probeSem: make(chan struct{}, 4),
	}
}

// goBg runs fn on a tracked background goroutine.
func (m *Manager) goBg(fn func()) {
	m.bgWait.Add(1)
	go func() {
		defer m.bgWait.Done()
		fn()
	}()
}

// WaitBackground blocks until every tracked goroutine has settled. Test
// teardown calls it before temp directories are cleaned up; the app itself
// exits with the process, so nothing there needs it.
func (m *Manager) WaitBackground() {
	m.bgWait.Wait()
}

// trackLive registers a running engine so live settings can reach it, and
// returns an unregister func for when the run settles.
func (m *Manager) trackLive(id string, dl *engine.Downloader) func() {
	m.livedlMu.Lock()
	m.livedl[id] = dl
	m.livedlMu.Unlock()
	return func() {
		m.livedlMu.Lock()
		delete(m.livedl, id)
		m.livedlMu.Unlock()
	}
}

// applyRefreshInterval pushes the UI refresh cadence to every running
// transfer at once.
func (m *Manager) applyRefreshInterval(iv time.Duration) {
	m.livedlMu.Lock()
	dls := make([]*engine.Downloader, 0, len(m.livedl))
	for _, dl := range m.livedl {
		dls = append(dls, dl)
	}
	m.livedlMu.Unlock()
	for _, dl := range dls {
		dl.SetProgressInterval(iv)
	}
}

// limiterFor returns the limiter shared by every transfer for the given rate.
//
// One limiter for all tasks is the point: a "2 MB/s" setting that each of five
// concurrent downloads obeyed separately would allow 10. The instance is
// created on first use and then mutated in place by applySpeedLimit, never
// replaced: running transfers hold the pointer, so only an in-place rate
// change reaches them without a stop/start cycle.
func (m *Manager) limiterFor(kilobytesPerSecond int) *engine.Limiter {
	rate := int64(kilobytesPerSecond) * 1024

	m.limMu.Lock()
	defer m.limMu.Unlock()

	if m.limiter == nil && rate > 0 {
		m.limiter = engine.NewLimiter(rate)
		m.limRate = rate
	}
	return m.limiter
}

// applySpeedLimit moves the shared limiter to the configured rate right now,
// so the setting takes effect on running transfers instead of waiting for a
// restart. It is a no-op when nothing changed.
func (m *Manager) applySpeedLimit(kilobytesPerSecond int) {
	rate := int64(kilobytesPerSecond) * 1024

	m.limMu.Lock()
	defer m.limMu.Unlock()

	if m.limRate == rate && (rate <= 0 || m.limiter != nil) {
		return
	}
	if m.limiter == nil {
		m.limiter = engine.NewLimiter(rate)
	} else {
		m.limiter.SetRate(rate)
	}
	m.limRate = rate
}

// Run drains the event pump until ctx is cancelled.
//
// Events go through the app's emitter rather than the runtime directly, so a
// test can substitute a no-op and still exercise the whole download path.
func (m *Manager) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case v := <-m.pump:
			// Progress travels through this queue while the terminal events
			// (task:done / task:failed) go out immediately. A frame that was
			// already queued when the task settled would therefore arrive
			// afterwards and repaint a finished row as "downloading". The
			// front-end keeps a weak guard for this; the decision belongs here,
			// where the task's current status is known exactly.
			if v.Status == StatusDownloading && m.settledNow(v.ID) {
				continue
			}
			m.app.emit("task:progress", v)
		}
	}
}

// settledNow reports whether the task with this id has already settled: it was
// removed from the list, or it holds a status that no progress frame may undo.
func (m *Manager) settledNow(id string) bool {
	t := m.get(id)
	if t == nil {
		return true
	}
	switch snapshot(t).Status {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return true
	}
	return false
}

// emit queues a progress event. Only progress travels the pump; terminal and
// structural events (done/failed/added/removed) go direct. When saturated,
// the stale backlog is shed in favour of the newest snapshot: under flood the
// UI would otherwise replay outdated positions first and visibly jump
// backwards before correcting itself.
func (m *Manager) emit(v TaskView) {
	select {
	case m.pump <- v:
	default:
		// Saturated: shed the whole stale backlog in favour of the newest
		// snapshot. Dropping a single frame still left the pump to replay the
		// rest of the queue — old positions first, so the UI visibly jumped
		// backwards before correcting itself. Every task's next throttled
		// tick re-fills whatever was shed here.
		for len(m.pump) > 0 {
			<-m.pump
		}
		select {
		case m.pump <- v:
		default:
		}
	}
}

func (m *Manager) get(id string) *task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// taskStillListed reports whether t is still the record its id maps to. The
// caller holds t.mu (that is what makes reading the id safe) and this takes
// m.mu; every m.mu holder only then takes one t.mu, so the two orders cannot
// deadlock.
//
// It detects a task the user removed while its transfer was still unwinding:
// the record is detached from the map, so anything this run reports — progress,
// failure, completion — belongs to a row that is gone.
func (m *Manager) taskStillListed(t *task) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[t.view.ID] == t
}

// view returns one task's view by id, and whether it exists.
func (m *Manager) view(id string) (TaskView, bool) {
	t := m.get(id)
	if t == nil {
		return TaskView{}, false
	}
	return snapshot(t), true
}

// snapshot copies a task's view.
func snapshot(t *task) TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view
}

// applyReportedSize records the total size the engine saw for a transfer.
//
// engine.Progress.Total is -1 when the server never said (a chunked response,
// or a HEAD the host refused). That sentinel is not "zero bytes": writing it
// over a size that is already known — the browser extension supplies one with
// StartRequest — made the row show 0 B and a frozen percentage for the whole
// download. Callers must hold t.mu.
func (t *task) applyReportedSize(total int64) {
	if total >= 0 {
		t.view.Size = total
	}
}

// List returns every task, newest first.
func (m *Manager) List() []TaskView {
	// One manager lock for the whole sweep: the per-id get() walk took and
	// released m.mu once per task, so a list of N tasks paid N lock cycles
	// on top of the N task locks below.
	m.mu.Lock()
	tasks := make([]*task, 0, len(m.order))
	for _, id := range m.order {
		if t := m.tasks[id]; t != nil {
			tasks = append(tasks, t)
		}
	}
	m.mu.Unlock()

	out := make([]TaskView, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, snapshot(t))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// duplicateError reports an attempt to add a URL that already has an
// unfinished task. It carries the existing record so callers can point at it
// instead of just complaining.
type duplicateError struct {
	ID     string
	Status TaskStatus
	Name   string
}

func (e *duplicateError) Error() string {
	label := string(e.Status)
	switch e.Status {
	case StatusDownloading:
		label = "下载中"
	case StatusQueued:
		label = "排队中"
	case StatusPaused:
		label = "已暂停"
	case StatusFailed:
		label = "已失败"
	case StatusCanceled:
		label = "已取消"
	}
	if e.Name != "" {
		return fmt.Sprintf("该链接已在列表中（%s：%s），无需重复添加", label, e.Name)
	}
	return fmt.Sprintf("该链接已在列表中（%s），无需重复添加", label)
}

// findUnfinished returns the first task for url that has not completed, or
// nil. An empty dest matches any destination; otherwise both must match, so
// the same file deliberately downloaded to two folders counts as two tasks.
// Completed tasks are never matches: re-downloading a finished file is
// a legitimate new task.
func (m *Manager) findUnfinished(url, dest string) *task {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	for _, id := range ids {
		t := m.get(id)
		if t == nil {
			continue
		}
		t.mu.Lock()
		match := t.view.URL == url && t.view.Status != StatusCompleted &&
			(dest == "" || t.view.Dest == dest)
		t.mu.Unlock()
		if match {
			return t
		}
	}
	return nil
}

// claimOrWaitDuplicate guards the exact-URL duplicate check against a race.
//
// The first caller for a url+dest pair claims the key and proceeds; a second
// caller arriving before the first inserted waits for that task to land and
// is told about it as a duplicate. The wait path always answers
// duplicateError, never a generic error: the extension falls back to a
// browser download on any other error, which would fetch the file twice.
func (m *Manager) claimOrWaitDuplicate(url, dest string) (TaskView, error, bool) {
	key := url + "\x00" + dest
	m.mu.Lock()
	if _, ok := m.inflight[key]; !ok {
		m.inflight[key] = struct{}{}
		m.mu.Unlock()
		// The caller drops the claim via releaseInflight, deferred in Start.
		return TaskView{}, nil, true
	}
	m.mu.Unlock()

	if dup := m.waitUnfinished(url, dest, 3*time.Second); dup != nil {
		v := snapshot(dup)
		return v, &duplicateError{ID: v.ID, Status: v.Status, Name: v.Name}, false
	}
	// The other add never landed (it was rejected after claiming); take the
	// slot and continue as new. The holder's remaining path to insert holds
	// no I/O, so it cannot still be in flight after this wait.
	m.mu.Lock()
	m.inflight[key] = struct{}{}
	m.mu.Unlock()
	return TaskView{}, nil, true
}

// releaseInflight drops the claim taken by claimOrWaitDuplicate.
func (m *Manager) releaseInflight(url, dest string) {
	key := url + "\x00" + dest
	m.mu.Lock()
	delete(m.inflight, key)
	m.mu.Unlock()
}

// waitUnfinished polls findUnfinished until a task for url+dest appears or
// the timeout elapses.
func (m *Manager) waitUnfinished(url, dest string, timeout time.Duration) *task {
	deadline := time.Now().Add(timeout)
	for {
		if dup := m.findUnfinished(url, dest); dup != nil {
			return dup
		}
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// matchSameContent is the predicate behind findSameContent, shared with the
// insertion check in Start so both layers agree on what "same file" means.
// Both hosts arrive pre-parsed: the callers walk O(n) candidates per add, and
// a url.Parse per candidate made a burst of adds quadratic.
func matchSameContent(v TaskView, url, host, vHost, name string, size int64) bool {
	if host == "" || name == "" {
		return false
	}
	if vHost == "" {
		// A task loaded from disk has no cached host yet.
		vHost = urlHost(v.URL)
	}
	// Identical URLs are the first layer's business (same link to
	// another folder is allowed); this layer is only for different
	// URLs pointing at the same file.
	return v.URL != url &&
		v.Status != StatusCompleted &&
		v.Name != "" && v.Name == name &&
		vHost == host &&
		(size <= 0 || v.Size <= 0 || v.Size == size)
}

// findSameContent returns an unfinished task that is most likely the same
// file under a different URL: same host, same filename, and compatible size.
// (Sizes are compatible when either side is unknown — a row that never
// started reports 0 — or both agree.) Completed tasks never match, and an
// explicit escape hatch always exists: rename the newcomer and it no longer
// matches. excludeID skips one task, for the post-probe check of the newcomer
// itself.
func (m *Manager) findSameContent(url, name string, size int64, excludeID string) *task {
	// One parse for the incoming URL; the cached per-task hosts below cover
	// the candidate side.
	host := urlHost(url)
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	for _, id := range ids {
		if id == excludeID {
			continue
		}
		t := m.get(id)
		if t == nil {
			continue
		}
		t.mu.Lock()
		v := t.view
		if t.host == "" && v.URL != "" {
			t.host = urlHost(v.URL)
		}
		match := matchSameContent(v, url, host, t.host, name, size)
		t.mu.Unlock()
		if match {
			return t
		}
	}
	return nil
}

// logURL renders a URL safe for the log: host+path only. Query strings and
// fragments routinely carry session tokens, single-use signatures and file
// ids — persisting them to disk turns the log into a credential store.
func logURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(invalid-url)"
	}
	p := u.EscapedPath()
	if p == "" {
		p = "/"
	}
	return u.Host + p
}

// sanitizeLogText rewrites every URL in text to its log-safe form.
//
// Query strings and fragments carry session tokens, single-use signatures and
// SAS credentials. The regex is deliberately greedy up to whitespace or a
// closing quote, which is how these URLs appear inside Go error strings such as
// `Get "https://host/f?token=...": dial tcp ...`.
func sanitizeLogText(text string) string {
	return reURLQuery.ReplaceAllStringFunc(text, func(m string) string {
		return logURL(m)
	})
}

// reURLQuery matches an absolute http(s) URL up to the next delimiter.
var reURLQuery = regexp.MustCompile(`https?://[^\s"'<>]+`)

// urlHost returns the lower-cased hostname without a www. prefix, or "".
func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimPrefix(u.Hostname(), "www."))
}

// probeName asks the server for the real filename and size without
// downloading anything. It backs add-time naming and the content-based
// duplicate check. It fails open: any error means ("", 0) and the caller
// keeps the old unnamed behaviour.
func (m *Manager) probeName(rawURL string, req StartRequest) (string, int64) {
	base := m.app.ctx
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, 10*time.Second)
	defer cancel()

	// Fast-fail for dead hosts: a refused/unreachable address fails the dial
	// in milliseconds, while a full probe would work through retries for
	// seconds. A failed dial skips the probe entirely (fail open). Note this
	// bypasses any configured proxy, so proxy users simply keep the old
	// unnamed-until-start behaviour.
	if !hostReachable(rawURL) {
		return "", 0
	}

	// Same credentials the transfer itself would send: some hosts only name
	// the file for a client that looks like its own user.
	opt := engine.Options{}.Normalize()
	// One quick look, not a campaign: this runs on every add, and the real
	// transfer re-probes with the full budget anyway, so a dead host must
	// fail fast rather than work through retries here.
	opt.Backoff = engine.Backoff{
		Base: 300 * time.Millisecond, Max: time.Second,
		Factor: 2, Jitter: 0, MaxTries: 1,
	}
	opt.Headers = http.Header{}
	if r := strings.TrimSpace(req.Referer); r != "" {
		opt.Headers.Set("Referer", r)
	}
	if c := strings.TrimSpace(req.Cookie); c != "" {
		opt.Headers.Set("Cookie", c)
	}
	if u := strings.TrimSpace(req.UserAgent); u != "" {
		opt.Headers.Set("User-Agent", u)
	}

	dl, err := engine.New(opt)
	if err != nil {
		return "", 0
	}
	defer dl.Close()

	p, err := dl.Probe(ctx, rawURL)
	if err != nil {
		return "", 0
	}
	if p.Size < 0 {
		p.Size = 0
	}
	return p.SuggestedName, p.Size
}

// hostReachable reports whether the URL's host accepts a TCP connection
// within a short budget. It is only a pre-check for the add-time probe.
func hostReachable(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return false
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	d := net.Dialer{Timeout: 2 * time.Second}
	c, err := d.Dial("tcp", net.JoinHostPort(u.Hostname(), port))
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// Start creates a task and begins transferring it when req.Start is set.
func (m *Manager) Start(req StartRequest) (TaskView, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return TaskView{}, fmt.Errorf("请填写下载链接")
	}
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return TaskView{}, fmt.Errorf("链接必须以 http:// 或 https:// 开头")
	}

	cfg := m.app.Config()

	m.mu.Lock()
	m.seq++
	id := fmt.Sprintf("t%d-%d", time.Now().UnixMilli(), m.seq)
	m.mu.Unlock()

	dest := strings.TrimSpace(req.Dest)
	if dest == "" {
		dest = cfg.DownloadDir
	}

	// An explicit filename wins; otherwise the engine derives one from the
	// server's Content-Disposition or the URL path.
	//
	// Until it does, the task has no name. Taking filepath.Base(dest) here — as
	// this did — reads the *directory* when no filename was given, which
	// labelled every such download after the folder it was going into.
	//
	// The filename is sanitised like a browser suggestion: the dialog is a
	// local trust boundary away from a remote server, but "../" or an absolute
	// path here would still escape the download folder.
	name := ""
	if fn := sanitizeSuggestedName(req.Filename); fn != "" {
		dest = filepath.Join(dest, fn)
		name = fn
	}

	// One URL, one unfinished task per destination. A second record for the
	// same link in the same folder would either fight over the same partial
	// file or — for single-use links — fail outright while burning the token.
	// Completed tasks are exempt: re-downloading a finished file is legitimate.
	// (Checked against the resolved dest, so the same file deliberately sent
	// to two folders still counts as two tasks. Checked before probing, so
	// an exact repeat never pays for a network round trip.)
	//
	// The claim comes first: the check below cannot see an add that has not
	// inserted yet, so without the inflight guard two adds arriving together
	// both pass and both insert.
	if v, err, proceed := m.claimOrWaitDuplicate(url, dest); !proceed {
		return v, err
	}
	defer m.releaseInflight(url, dest)
	if dup := m.findUnfinished(url, dest); dup != nil {
		v := snapshot(dup)
		return v, &duplicateError{ID: v.ID, Status: v.Status, Name: v.Name}
	}

	// When the caller supplies no filename, the server is asked once up
	// front — asynchronously, so adding never blocks on the network. The row
	// is created unnamed first; fillProbedName fills the name and size when
	// the probe lands (failures keep the old behaviour) and then runs the
	// content-based duplicate check. A caller-supplied filename is checked
	// synchronously instead: no network involved.
	// The caller may already know the total size (the browser read it before
	// handing over). It lands on the row immediately so a paused task does
	// not read "0 B" until started, and the content-based duplicate check
	// gets the signal. Negative is treated as unknown.
	size := req.Size
	if size < 0 {
		size = 0
	}

	needsProbe := name == ""
	if !needsProbe {
		if dup := m.findSameContent(url, name, size, ""); dup != nil {
			v := snapshot(dup)
			return v, &duplicateError{ID: v.ID, Status: v.Status, Name: v.Name}
		}
	}

	conns := req.Conns
	if conns <= 0 {
		conns = cfg.Connections
	}

	t := &task{
		// Strip CR/LF so a crafted value cannot smuggle an extra header line.
		referer:   strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(req.Referer), "\r", ""), "\n", ""),
		cookie:    strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(req.Cookie), "\r", ""), "\n", ""),
		userAgent: strings.ReplaceAll(strings.ReplaceAll(strings.TrimSpace(req.UserAgent), "\r", ""), "\n", ""),
		manual:    req.Manual,
		host:      urlHost(url),
	}
	t.view = TaskView{
		ID:      id,
		URL:     url,
		Dest:    dest,
		Name:    name,
		Size:    size,
		Status:  StatusPaused,
		Conns:   conns,
		Created: time.Now().Format(time.RFC3339Nano),
	}

	m.mu.Lock()
	// Re-check the content layer inside the insertion critical section: the
	// synchronous check above raced with a concurrent add carrying the same
	// host+name, and both would have inserted (the url+dest inflight guard
	// does not cover different URLs for the same file).
	if !needsProbe {
		host := urlHost(url)
		for _, other := range m.tasks {
			other.mu.Lock()
			v := other.view
			dup := matchSameContent(v, url, host, other.host, name, size)
			other.mu.Unlock()
			if dup {
				m.mu.Unlock()
				return v, &duplicateError{ID: v.ID, Status: v.Status, Name: v.Name}
			}
		}
	}
	m.tasks[id] = t
	m.order = append([]string{id}, m.order...)
	m.mu.Unlock()

	m.app.emit("task:added", snapshot(t))

	if needsProbe {
		m.goBg(func() { m.fillProbedName(id, url, req) })
	}

	if req.Start {
		m.startRun(t)
	}
	_ = m.saveTasks()
	return snapshot(t), nil
}

// pickDuplicateVictim decides which of two same-content tasks goes: the
// later-created one, so a probe that merely returned later cannot evict the
// task the user added first. Created is an RFC3339Nano stamp, so string order
// is time order; an exact tie (same nanosecond) falls back to the id sequence
// suffix, where the larger seq is the later add. It returns "" when neither
// side is strictly later, in which case the caller keeps both rather than
// guessing.
func pickDuplicateVictim(mine, other TaskView) string {
	switch {
	case other.Created > mine.Created:
		return other.ID
	case mine.Created > other.Created:
		return mine.ID
	}
	mSeq, mOK := parseTaskSeq(mine.ID)
	oSeq, oOK := parseTaskSeq(other.ID)
	switch {
	case mOK && oOK && oSeq > mSeq:
		return other.ID
	case mOK && oOK && mSeq > oSeq:
		return mine.ID
	default:
		return ""
	}
}

// untouchedTask reports whether the task never really started: still paused
// with nothing downloaded. Only such a task may be removed as a duplicate;
// anything the user already started is left alone.
func untouchedTask(t *task) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view.Status == StatusPaused && t.view.Downloaded == 0
}

// fillProbedName resolves the server filename and size for a task added
// without one, then runs the content-based duplicate check. It never blocks
// Start and never fails it: any probe failure leaves the row as created. A
// probed duplicate removes the later-created task only while it is still
// untouched (paused with nothing downloaded); a task the user already started
// is left alone, and so is anything whose row already vanished.
func (m *Manager) fillProbedName(id, url string, req StartRequest) {
	// Bound concurrent probes: a burst of unnamed adds (the extension can
	// deliver dozens at once) otherwise opens a dial plus an engine instance
	// per add against the same host.
	m.probeSem <- struct{}{}
	defer func() { <-m.probeSem }()

	pn, ps := m.probeName(url, req)
	if pn == "" && ps <= 0 {
		return
	}
	t := m.get(id)
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.view.Name == "" {
		t.view.Name = pn
	}
	if t.view.Size == 0 && ps > 0 {
		t.view.Size = ps
	}
	name, size := t.view.Name, t.view.Size
	t.mu.Unlock()
	_ = m.saveTasks()
	m.app.emit("task:progress", snapshot(t))

	if name == "" {
		return
	}
	m.resolveDuplicate(id, url, name, size)
}

// resolveDuplicate removes the later-created of two same-content tasks, when
// the victim is still untouched. It is split out of fillProbedName so the
// victim choice is testable without the network probe in the way.
func (m *Manager) resolveDuplicate(id, url, name string, size int64) {
	t := m.get(id)
	if t == nil {
		return
	}
	dup := m.findSameContent(url, name, size, id)
	if dup == nil {
		return
	}
	dv := snapshot(dup)
	victim := pickDuplicateVictim(snapshot(t), dv)
	if victim == "" {
		return
	}
	vt := m.get(victim)
	if vt == nil || !untouchedTask(vt) {
		return
	}
	m.app.debugLog("duplicate of %s (%s), removing %s", dv.ID, dv.Name, victim)
	_ = m.Remove(victim, false)
}

// startRun launches the transfer goroutine for t, honouring MaxConcurrent.
//
// The done channel is created here, under the same lock as cancel, so a
// Pause/Cancel arriving while the goroutine is still starting always finds
// something to wait on. Creating it inside run instead left a window where
// done was nil and the stop returned without waiting — the transfer then
// carried on after the user had stopped it.
//
// When no slot is free the task stays queued and pumpQueue starts it once a
// transfer settles.
func (m *Manager) startRun(t *task) {
	// Count first, lock second: activeCount inspects every task including t,
	// so holding t.mu across it would deadlock against itself.
	// (A sequential overshoot by concurrent starters is benign: the next pump
	// corrects it, and StartAll/pumpQueue both start tasks one at a time.)
	m.mu.Lock()
	frozen := m.updating
	m.mu.Unlock()
	if frozen {
		// An update is being applied: park the task instead of starting it.
		t.mu.Lock()
		// Never park a run a concurrent starter already launched: flipping a
		// transferring task back to queued leaves an engine writing while
		// the row claims to be waiting for a slot.
		if t.view.Status == StatusDownloading {
			t.mu.Unlock()
			return
		}
		t.view.Status = StatusQueued
		t.mu.Unlock()
		_ = m.saveTasks()
		m.app.emit("task:progress", snapshot(t))
		return
	}
	if m.activeCount() >= m.maxConcurrent() {
		t.mu.Lock()
		if t.view.Status == StatusDownloading {
			t.mu.Unlock()
			return
		}
		t.view.Status = StatusQueued
		t.mu.Unlock()
		_ = m.saveTasks()
		// Report the queueing at once: without this the row sits on its old
		// status until the engine's first progress event, which on a slow
		// link arrives seconds later and reads as a dead button.
		m.app.emit("task:progress", snapshot(t))
		return
	}
	// Resolve the filename before spawning, while the task is still queued. The
	// engine treats Dest as a directory when no name is known and resolves
	// dir+suggested under it, but the manager's Dest stays the directory — and a
	// later partial-path computation then pointed at "<dir>.part", which the
	// engine created as a *directory*. Probing here keeps Dest a file.
	t.mu.Lock()
	needName := t.view.Name == "" && !t.manual
	t.mu.Unlock()
	if needName {
		m.fillProbedName(t.view.ID, t.view.URL, StartRequest{})
	}

	// The done channel is created here, under the same lock as cancel, so a
	// Pause/Cancel arriving while the goroutine is still starting always finds
	// something to wait on. Creating it inside run instead left a window where
	// done was nil and the stop returned without waiting — the transfer then
	// carried on after the user had stopped it.
	ctx, cancel := context.WithCancel(m.app.ctx)
	t.mu.Lock()
	if t.view.Status == StatusDownloading {
		// A concurrent starter already launched this task — a double-clicked
		// resume, two settle points pumping the queue at once, StartAll
		// racing a freed slot. Spawning a second engine would leave two
		// run goroutines writing the same partial file, and the second
		// startRun would overwrite cancel/done so the first run could never
		// be stopped again. Give up: the launch the winner performed is
		// exactly what this call wanted anyway.
		t.mu.Unlock()
		cancel()
		return
	}
	// The generation advances here, at the commit point, and is handed to run.
	// It used to be bumped inside run, so between "spawned" and "run started"
	// the task still carried the previous run's generation: a pause followed by
	// a resume in that window let the old run's exit branch pass its
	// "still downloading" test and write the new run's status — a fresh
	// download showing as canceled, with Cancel's cleanup deleting the partial
	// data of the run that had just started.
	t.generation++
	gen := t.generation
	t.cancel = cancel
	// A fresh run has no stop to report yet.
	t.stopIntent = ""
	t.view.Status = StatusDownloading
	t.view.Error = ""
	t.view.Notice = ""
	t.view.Speed = 0
	t.view.ETA = 0
	done := make(chan struct{})
	t.done = done
	t.mu.Unlock()

	// Report the start at once, for the same reason as the queueing above:
	// the engine's first progress event can lag seconds behind on a slow
	// link, and until then the UI shows the pre-start state.
	m.app.emit("task:progress", snapshot(t))

	m.goBg(func() { m.run(t, ctx, cancel, done, gen) })
}

// refreshInterval maps the UI refresh setting to an engine reporting
// interval. Unknown values fall back to fast (today's behaviour).
func refreshInterval(rate string) time.Duration {
	switch strings.ToLower(strings.TrimSpace(rate)) {
	case "medium":
		return 650 * time.Millisecond
	case "slow":
		return 1000 * time.Millisecond
	default:
		return 250 * time.Millisecond
	}
}

// maxConcurrent bounds how many tasks transfer at once; the rest queue.
// It honours the setting's own clamp rather than trusting it.
func (m *Manager) maxConcurrent() int {
	n := m.app.Config().MaxConcurrent
	if n < 1 {
		n = 1
	}
	if n > 16 {
		n = 16
	}
	return n
}

// taskCounts is the aggregate figure set the sidebar, the plan watcher and
// Stats need, tallied in one pass over the table.
type taskCounts struct {
	active, completed, failed, queued, paused, canceled int64
	speed                                               int64
	total                                               int64
}

// intervened reports whether a settled plan was interrupted: a cancelled,
// paused or failed task means the user stepped in, so end-of-plan actions
// (sound, shutdown) must be skipped.
func (c taskCounts) intervened() bool {
	return c.canceled > 0 || c.paused > 0 || c.failed > 0
}

// countByStatus tallies the table once. It snapshots the task pointers under
// m.mu and reads each view under the task's own lock — the same lock order as
// everywhere else — but skips the TaskView copies and the sort that List pays
// for. The plan watcher polls this every 500ms; routing it through List made
// the idle app sort the whole table twice a second for two numbers.
func (m *Manager) countByStatus() taskCounts {
	m.mu.Lock()
	tasks := make([]*task, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()

	var c taskCounts
	for _, t := range tasks {
		t.mu.Lock()
		switch t.view.Status {
		case StatusDownloading:
			c.active++
			c.speed += t.view.Speed
		case StatusCompleted:
			c.completed++
		case StatusFailed:
			c.failed++
		case StatusQueued:
			c.queued++
		case StatusPaused:
			c.paused++
		case StatusCanceled:
			c.canceled++
		}
		c.total++
		t.mu.Unlock()
	}
	return c
}

// activeCount counts transferring tasks.
func (m *Manager) activeCount() int {
	return int(m.countByStatus().active)
}

// pumpQueue starts queued tasks while slots are free. It runs after every
// settle point (completion, failure, pause, cancel, removal), so a freed slot
// is never left idle while work waits. It does nothing while an update is
// being applied: waking work up mid-shutdown would defeat the pause-all.
func (m *Manager) pumpQueue() {
	m.mu.Lock()
	frozen := m.updating
	m.mu.Unlock()
	if frozen {
		return
	}
	// One snapshot per pump. The old loop re-ran List (full copy + sort) and
	// activeCount (full scan) for every task it started, which made StartAll
	// over a hundred queued tasks quadratic.
	list := m.List()
	active := 0
	for _, v := range list {
		if v.Status == StatusDownloading {
			active++
		}
	}
	max := m.maxConcurrent()
	// List returns newest first; walk from the end so the oldest queued task
	// wins the freed slot.
	for i := len(list) - 1; i >= 0 && active < max; i-- {
		if list[i].Status != StatusQueued {
			continue
		}
		next := m.get(list[i].ID)
		if next == nil {
			continue
		}
		next.mu.Lock()
		stillQueued := next.view.Status == StatusQueued
		next.mu.Unlock()
		if !stillQueued {
			continue
		}
		m.startRun(next)
		// Count only what actually started: startRun re-checks the live count
		// and parks the task when a concurrent starter took the slot, and
		// over-counting here would leave a free slot idle until the next
		// settle point pumped the queue again.
		next.mu.Lock()
		if next.view.Status == StatusDownloading {
			active++
		}
		next.mu.Unlock()
	}
}

// Pause stops the transfer but keeps the partial file and its sidecar, so
// Resume continues instead of restarting.
func (m *Manager) Pause(id string) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	t.mu.Lock()
	if t.view.Status != StatusDownloading && t.view.Status != StatusQueued {
		st := t.view.Status
		t.mu.Unlock()
		return fmt.Errorf("当前状态无法暂停（%s）", st)
	}
	cancel := t.cancel
	// Recorded before cancelling, so the goroutine waking up on the cancelled
	// context already sees it.
	t.stopIntent = StatusPaused
	t.view.Status = StatusPaused
	t.view.Speed = 0
	t.view.ETA = 0
	// The notice belongs to the run being stopped ("retrying in N s"). Left
	// here it was written to tasks.json by the save below and came back after
	// a restart, so the row flashed a retry that would never happen.
	t.view.Notice = ""
	v := t.view
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.app.emit("task:progress", v)
	_ = m.saveTasks()
	m.pumpQueue()
	return nil
}

// errUpdating rejects starts while an update is being applied. It is a
// sentinel so callers (notably StartPlanSelected) can tell a freeze apart
// from a task that merely refused the resume.
var errUpdating = errors.New("正在更新，稍后再试")

// Resume restarts a paused, failed or cancelled task from its partial data.
func (m *Manager) Resume(id string) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	t.mu.Lock()
	switch t.view.Status {
	case StatusPaused, StatusFailed, StatusCanceled, StatusQueued:
	default:
		st := t.view.Status
		t.mu.Unlock()
		return fmt.Errorf("当前状态无法继续（%s）", st)
	}
	// A completed task whose file was moved away is reported as failed, but its
	// stored URL has already been redacted — credentials are dropped once a
	// download finishes. Retrying it would replay a link without its signature and
	// fail for a reason that has nothing to do with the real problem.
	if t.view.Error == fileGoneMessage && !strings.Contains(t.view.URL, "?") {
		t.mu.Unlock()
		return fmt.Errorf("原文件已不在保存位置，请重新添加下载链接")
	}
	t.mu.Unlock()

	m.mu.Lock()
	frozen := m.updating
	m.mu.Unlock()
	if frozen {
		return errUpdating
	}

	// The spawn (and the MaxConcurrent gate) lives in startRun: it creates
	// cancel and done under one lock, so a pause arriving mid-start cannot
	// miss them. No pump here: nothing settled, so no slot was freed — and
	// pumping would start a sibling the caller's own loop is about to reach,
	// making bulk counts wrong.
	m.startRun(t)
	return nil
}

// partialPaths returns the .part + sidecar paths for a task. A task created
// without a filename keeps Dest as the directory while the engine resolves
// dir+suggested-name underneath, so dest+suffix alone points at a file that
// was never created and the real partials survive every deletion.
func partialPaths(dest, name string) (part, state string) {
	target := dest
	if name != "" {
		if st, err := os.Stat(dest); err == nil && st.IsDir() {
			target = filepath.Join(dest, name)
		}
	}
	return target + engine.PartSuffix, target + engine.StateSuffix
}

// Cancel stops a task and discards its partial data.
func (m *Manager) Cancel(id string) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("任务不存在")
	}
	t.mu.Lock()
	if t.view.Status == StatusCompleted {
		// Cancelling a finished download used to flip it to "canceled",
		// after which Remove(deleteFiles=true) would find wasComplete
		// false and quietly refuse to delete the file the user asked
		// about — a record that lies about what removing it will do.
		t.mu.Unlock()
		return fmt.Errorf("已完成的任务无法取消，直接删除即可")
	}
	cancel := t.cancel
	dest := t.view.Dest
	gen := t.generation
	t.stopIntent = StatusCanceled
	t.view.Status = StatusCanceled
	t.view.Speed = 0
	t.view.ETA = 0
	// Same reasoning as Pause: a cancelled task must not keep a retry notice.
	t.view.Notice = ""
	v := t.view
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	m.app.emit("task:progress", v)
	_ = m.saveTasks()

	// The wait and the deletion run off the binding thread: the UI already
	// shows the task as cancelled, and a 3s block per task made bulk cancels
	// feel frozen. The generation guard is what keeps this safe: if the task
	// was resumed again before the old goroutine exited, the generation has
	// moved on and there is nothing left for us to delete.
	m.goBg(func() {
		// Wait for the transfer goroutine to stop before deleting: cancelling
		// only asks it to stop, and a write in flight would recreate the
		// partial file immediately after it was removed.
		t.waitStopped(3 * time.Second)

		t.mu.Lock()
		superseded := t.generation != gen
		// Re-read the name: at cancel time it can still be empty (the probe
		// or the first progress event fills it milliseconds later), and the
		// deletion must aim at the partial file that actually exists — the
		// snapshot above would point at "<directory>.part", which never
		// matches, and the real partial would survive the "discard".
		name := t.view.Name
		t.mu.Unlock()
		if superseded {
			return
		}

		// A cancelled task should start clean, so the partial file and the
		// sidecar that would resume it are both removed.
		part, state := partialPaths(dest, name)
		_ = os.Remove(part)
		_ = os.Remove(state)
		m.pumpQueue()
	})
	return nil
}

// removal is what a detached task's waiter still needs to finish cleaning up.
type removal struct {
	t           *task
	dest        string
	name        string
	wasComplete bool
	deleteFiles bool
}

// detach cancels a task and unlinks it from the index. The wait for the
// transfer goroutine, any file deletion and the queue pump stay with the
// caller, so a bulk remove can share one waiter instead of spawning a
// goroutine per row.
func (m *Manager) detach(id string) (removal, error) {
	t := m.get(id)
	if t == nil {
		return removal{}, fmt.Errorf("任务不存在")
	}
	t.mu.Lock()
	cancel := t.cancel
	dest := t.view.Dest
	name := t.view.Name
	wasComplete := t.view.Status == StatusCompleted
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}

	// The record leaves the list synchronously so the UI updates at once; the
	// wait and any file deletion happen off the binding thread. No generation
	// guard here (unlike Cancel): this object is detached from the map, so a
	// task re-added for the same destination is a different object and must
	// not be second-guessed — the wait below is what separates the old
	// goroutine's last write from the deletion.
	m.mu.Lock()
	delete(m.tasks, id)
	for i, v := range m.order {
		if v == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
	m.mu.Unlock()

	m.app.emit("task:removed", id)
	return removal{t: t, dest: dest, name: name, wasComplete: wasComplete}, nil
}

// settleRemoval waits for the task's transfer goroutine to stop and deletes
// the files the record owned. It does not pump the queue: the caller decides
// when one pump is worth it.
func (m *Manager) settleRemoval(r removal) {
	// Same reasoning as Cancel: the goroutine may still be writing, so
	// waiting avoids a race between removal and a final flush.
	r.t.waitStopped(3 * time.Second)

	// Records go; an unfinished task's partial data goes with them. A
	// .part + sidecar without their index entry can never resume (the
	// next attempt at the same URL starts from scratch), so leaving them
	// behind only accumulates junk named after URL tokens. A completed
	// task's file is the user's data: it is removed only when the caller
	// explicitly asked for it, never as a side effect of tidying the list.
	// partialPaths mirrors the engine's dir+suggested-name resolution:
	// tasks created without a filename keep Dest as the directory.
	if !r.wasComplete {
		part, state := partialPaths(r.dest, r.name)
		_ = os.Remove(part)
		_ = os.Remove(state)
	}
	if r.deleteFiles {
		// Only delete the published file when the task actually produced
		// one, so a failed task cannot remove an unrelated file sharing
		// the name.
		if r.wasComplete {
			_ = os.Remove(r.dest)
		}
	}
}

// Remove drops a task, optionally deleting the files it produced.
func (m *Manager) Remove(id string, deleteFiles bool) error {
	r, err := m.detach(id)
	if err != nil {
		return err
	}
	r.deleteFiles = deleteFiles
	_ = m.saveTasks()

	m.goBg(func() {
		m.settleRemoval(r)
		m.pumpQueue()
	})
	return nil
}

// RemoveTasks drops the given records in one go.
//
// The caller decides which, so "delete everything I can see" works for any
// filter — by status, by file type, by search — without this needing to know
// what the filter was. Finished files are never touched; unfinished partials
// go with their records.
func (m *Manager) RemoveTasks(ids []string) int {
	var rs []removal
	m.batch(func() {
		for _, id := range ids {
			if r, err := m.detach(id); err == nil {
				rs = append(rs, r)
			}
		}
		// Marks dirty inside the batch; one real write happens at the end.
		_ = m.saveTasks()
	})
	if len(rs) == 0 {
		return 0
	}
	// One waiter for the whole batch: a goroutine per row, each waking into
	// its own pumpQueue, made a hundred-row delete a hundred table sweeps
	// plus a goroutine spike.
	m.goBg(func() {
		for _, r := range rs {
			m.settleRemoval(r)
		}
		m.pumpQueue()
	})
	return len(rs)
}

// RemoveFailed drops every failed record.
//
// RemoveTasks with a filtered list would do the same job, but this keeps the
// rule in one place for callers that do not have the list to hand.
func (m *Manager) RemoveFailed() int {
	m.mu.Lock()
	var ids []string
	for _, id := range m.order {
		t := m.tasks[id]
		if t == nil {
			continue
		}
		t.mu.Lock()
		failed := t.view.Status == StatusFailed
		t.mu.Unlock()
		if failed {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()

	return m.RemoveTasks(ids)
}

// ------------------------------------------------------------- bulk verbs

// StartAll resumes every task that is waiting to run, up to MaxConcurrent.
//
// Tasks already transferring are left alone, as are completed ones: "start all"
// means "get everything going", not "start everything over". Tasks beyond the
// concurrency budget stay queued (and uncounted); pumpQueue starts them as
// slots free up.
func (m *Manager) StartAll() int {
	n := 0
	m.batch(func() {
		for _, v := range m.List() {
			switch v.Status {
			case StatusPaused, StatusFailed, StatusCanceled, StatusQueued:
				if err := m.Resume(v.ID); err != nil {
					continue
				}
				if t := m.get(v.ID); t != nil {
					t.mu.Lock()
					running := t.view.Status == StatusDownloading
					t.mu.Unlock()
					if running {
						n++
					}
				}
			}
		}
	})
	return n
}

// StopAll pauses every task that is transferring, keeping its partial data so
// it can be continued later. Tasks that already settled are untouched.
func (m *Manager) StopAll() int {
	n := 0
	m.batch(func() {
		for _, v := range m.List() {
			if v.Status == StatusDownloading || v.Status == StatusQueued {
				if err := m.Pause(v.ID); err == nil {
					n++
				}
			}
		}
	})
	return n
}

// StartPlan starts every waiting task and, once they have all settled, runs the
// action configured for the end of a plan.
func (m *Manager) StartPlan() (int, error) {
	m.mu.Lock()
	if m.planRunning {
		m.mu.Unlock()
		return 0, fmt.Errorf("计划已经在运行")
	}
	// Claim the slot before starting anything, so two quick clicks cannot both
	// spawn a watcher.
	m.planRunning = true
	m.mu.Unlock()

	// A freeze is not a queue of work. Refusing up front keeps the button's
	// message honest; starting anyway would park a watcher whose tasks all
	// refuse to launch and which can only ever end as "intervened" once the
	// update's StopAll lands. The scoped entry (StartPlanSelected) reaches
	// this same refusal through the per-task Resume errors.
	m.mu.Lock()
	frozen := m.updating
	m.mu.Unlock()
	if frozen {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, errUpdating
	}

	// Membership is fixed here, before anything starts: every task that is
	// running or waiting right now. A plan covers all of them, not just the
	// tasks this call managed to start — when every MaxConcurrent slot is
	// already occupied, StartAll starts nothing new yet there is still real
	// work, and refusing the plan then ("没有等待下载的任务") made the button
	// dead exactly when the list was full of queued tasks.
	plan := &planCoverage{}
	for _, v := range m.List() {
		// Everything that is running, waiting or paused: those are exactly the
		// tasks StartAll is about to resume.
		if startsWithPlan(v.Status) {
			plan.add(v.ID)
		}
	}
	if len(plan.ids) == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, fmt.Errorf("没有等待下载的任务")
	}
	m.mu.Lock()
	m.planTasks = plan
	m.mu.Unlock()

	// Try to start everything waiting; tasks beyond the concurrency budget
	// stay queued and are pulled in by pumpQueue as slots free up.
	m.StartAll()

	m.goBg(m.watchPlan)
	return len(plan.ids), nil
}

// StartPlanSelected starts the given tasks and, once they have all settled,
// runs the end-of-plan action. It is the selection-scoped twin of StartPlan:
// only the listed ids are started and watched, everything else is ignored.
func (m *Manager) StartPlanSelected(ids []string) (int, error) {
	m.mu.Lock()
	if m.planRunning {
		m.mu.Unlock()
		return 0, fmt.Errorf("计划已经在运行")
	}
	// Claim the slot before starting anything, so two quick clicks cannot both
	// spawn a watcher.
	m.planRunning = true
	m.mu.Unlock()

	want := map[string]bool{}
	for _, id := range ids {
		if m.get(id) != nil {
			want[id] = true
		}
	}
	if len(want) == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, fmt.Errorf("没有选中的任务")
	}

	covered := 0
	var frozenErr error
	m.batch(func() {
		for _, v := range m.List() {
			if !want[v.ID] {
				continue
			}
			switch v.Status {
			case StatusPaused, StatusFailed, StatusCanceled, StatusQueued:
				// Counted at the successful resume, not by re-reading the
				// status afterwards: a resume refused (frozen for update, or
				// the task moved on) must not be covered.
				if err := m.Resume(v.ID); err == nil {
					covered++
				} else if frozenErr == nil && errors.Is(err, errUpdating) {
					frozenErr = err
				}
			}
		}
	})

	if covered == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		// A freeze is not an empty selection: propagate it so the caller can
		// tell "try later" apart from "nothing to download".
		if frozenErr != nil {
			return 0, frozenErr
		}
		return 0, fmt.Errorf("选中的任务没有可下载的")
	}

	m.mu.Lock()
	m.planTasks = &planCoverage{ids: want}
	m.mu.Unlock()

	m.goBg(m.watchPlanSelected)
	return covered, nil
}

// intervenedStatus reports whether a settled task means the user stepped in:
// a cancelled, paused or failed task means the plan did not run to
// completion, so the end-of-plan actions (sound, shutdown) must be skipped.
func intervenedStatus(s TaskStatus) bool {
	return s == StatusCanceled || s == StatusPaused || s == StatusFailed
}

// planCoverage is the set of tasks a running plan waits for.
//
// Membership, not a tally over the whole table, is what decides when the plan
// is done and whether the user intervened. The global plan used to judge
// "intervened" from a count of paused/failed/canceled tasks anywhere in the
// list, so adding an unrelated paused task while a plan ran silently turned its
// end actions off — no sound, no shutdown, nothing on screen explaining why.
// The selection plan already looked only at its own tasks, so the same user
// action gave two different outcomes depending on which button started the
// plan. Both watchers now walk this set with the one rule in planStatusOf.
type planCoverage struct {
	ids map[string]bool
	// done records members that had already completed when the plan was
	// captured. Everything else is work the plan took on, so ending paused,
	// failed or cancelled is an intervention.
	done map[string]bool
}

// planStatus is what one planned task contributes to the plan.
type planStatus int

const (
	// planWaiting: a running or queued task — the plan is not finished while
	// one exists. (A task paused at the plan's start is also waiting; that is
	// handled by scanPlan through planStartPaused.)
	planWaiting planStatus = iota
	// planSettled: completed — done, no intervention.
	planSettled
	// planIntervene: paused, failed or cancelled — done, the user stepped in,
	// so the end-of-plan actions (sound, shutdown) are skipped.
	planIntervene
)

// planStatusOf maps a task status onto its plan meaning. A task that is gone
// from the table counts as planIntervene (see scanPlan): removing a planned
// task is an intervention, not a quiet success.
//
// A paused task is an intervention during a plan, but not at its start: the
// plan resumes the tasks that were paused when it began (see planStartPaused),
// so those are work it is waiting for; a task paused while the plan runs is the
// user stopping it, exactly like Cancel.
func planStatusOf(s TaskStatus) planStatus {
	switch s {
	case StatusDownloading, StatusQueued:
		return planWaiting
	case StatusCompleted:
		return planSettled
	default:
		return planIntervene
	}
}

// startsWithPlan reports whether a task with this status is one a plan takes
// over: everything except what already settled (completed, failed, canceled).
// Paused tasks are included — StartAll resumes them.
func startsWithPlan(s TaskStatus) bool {
	switch s {
	case StatusCompleted, StatusFailed, StatusCanceled:
		return false
	}
	return true
}

func (p *planCoverage) add(id string) {
	if p == nil {
		return
	}
	if p.ids == nil {
		p.ids = make(map[string]bool)
	}
	p.ids[id] = true
}

// scanPlan walks a plan's tasks and reports (done, intervened).
func (m *Manager) scanPlan(plan *planCoverage) (done, intervened bool) {
	done = true
	for id := range plan.ids {
		t := m.get(id)
		if t == nil {
			intervened = true
			continue
		}
		switch snapshot(t).Status {
		case StatusCompleted:
			// Settled: either it was already done when the plan was captured,
			// or it finished while the plan ran. Neither is an intervention.
		case StatusDownloading, StatusQueued:
			done = false
		default:
			// Paused, failed or cancelled: the user stepped in (or the server
			// did), so the plan did not run to completion.
			intervened = true
		}
	}
	return done, intervened
}

// currentPlan returns the plan in flight, or nil.
func (m *Manager) currentPlan() *planCoverage {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.planTasks
}

// adoptIntoPlan records a task that started running while a plan was in
// flight, so the plan waits for it too: the user started it during the plan,
// and a shutdown that ignored it would power off mid-download.
func (m *Manager) adoptIntoPlan(id string) {
	m.mu.Lock()
	if plan := m.planTasks; plan != nil && m.planRunning {
		plan.add(id)
	}
	m.mu.Unlock()
}

// watchPlanSelected waits until every task in the selection plan has settled,
// then finishes the plan.
func (m *Manager) watchPlanSelected() {
	plan := m.currentPlan()
	if plan == nil {
		m.finishPlan(false)
		return
	}
	m.watchPlanUntilDone(plan)
}

// watchPlanUntilDone is the poll shared by both watchers: it ends the plan once
// every covered task has settled, using scanPlan's single rule for what counts
// as an intervention.
//
// It polls rather than being notified: every path that can end a transfer —
// completion, failure, cancellation, removal — would otherwise have to remember
// to poke the plan, and the one that forgot would leave it hanging forever.
func (m *Manager) watchPlanUntilDone(plan *planCoverage) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		if done, intervened := m.scanPlan(plan); done {
			m.finishPlan(intervened)
			return
		}
	}
}

// PlanRunning reports whether a plan is working.
func (m *Manager) PlanRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.planRunning
}

// watchPlan waits until the global plan's tasks have settled, then finishes
// the plan. Only the set captured when the plan started is consulted: an
// unrelated task added or paused mid-plan used to decide whether the user got
// their sound and shutdown.
func (m *Manager) watchPlan() {
	plan := m.currentPlan()
	if plan == nil {
		m.finishPlan(false)
		return
	}
	m.watchPlanUntilDone(plan)
}

// finishPlan ends the plan in flight.
func (m *Manager) finishPlan(intervened bool) {
	m.mu.Lock()
	m.planRunning = false
	m.planTasks = nil
	m.mu.Unlock()

	// A plan that ends with cancelled, paused or failed tasks did not run to
	// completion: the user intervened (or something broke). Firing the
	// end-of-plan actions anyway — especially a machine shutdown — would
	// punish cancelling.
	m.finishPlanEnd(intervened)
}

// finishPlanEnd emits plan:done and runs the configured end actions, unless
// the plan was intervened with.
func (m *Manager) finishPlanEnd(intervened bool) {
	if intervened {
		m.app.emit("plan:done", nil)
		m.app.debugLog("plan finished as intervened (paused/failed/canceled present): end actions skipped")
		return
	}

	cfg := m.app.Config()
	m.app.emit("plan:done", nil)
	m.app.debugLog("plan finished: sound=%v shutdown=%v", cfg.PlanSound, cfg.PlanShutdown)

	if cfg.PlanSound {
		playAlert()
	}
	if cfg.PlanShutdown {
		launchShutdown(cfg.ShutdownDelaySeconds)
	}
}

// playAlert makes an audible signal, so a plan that finishes while the user is
// away from the screen is still noticed.
//
// Two rising tones through kernel32!Beep, with no asset to ship and no
// dependency on the user's sound scheme. This used to run PowerShell for the
// sound, which flashed a console window every time a plan finished.
func playAlert() {
	beepAlert()
}

// launchShutdown powers the machine off after the configured delay.
//
// A variable, not a plain function: the end-of-plan path is the one place the
// setting reaches the OS, and a test has to be able to see the delay it chose
// without actually powering the machine off.
var launchShutdown = func(seconds int) {
	go func() {
		// shutdown.exe is a console application; without CREATE_NO_WINDOW it
		// pops a black window over whatever the user is doing.
		_ = hiddenCmd("shutdown", shutdownArgs(seconds)...).Run()
	}()
}

// shutdownArgs builds the shutdown.exe arguments for a plan-completion
// shutdown.
//
// seconds is the cancel window: /t gives the user time to run "shutdown /a",
// and the comment says so. 0 is an explicit "now" — the machine powers off
// immediately and there is nothing to cancel, so the comment must not promise
// a countdown that does not exist. Values outside 0..600 are clamped here as
// well as in normalize, because this is the last stop before the OS.
func shutdownArgs(seconds int) []string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds > 600 {
		seconds = 600
	}
	notice := fmt.Sprintf("VKMDownload 计划已完成，%d 秒后关机（运行 shutdown /a 取消）", seconds)
	if seconds == 0 {
		notice = "VKMDownload 计划已完成，立即关机"
	}
	return []string{"/s", "/t", strconv.Itoa(seconds), "/c", notice}
}

// sessionRetryDelays is how long the manager waits before replaying a whole
// download session after the server 404s mid-transfer.
//
// It is the session-level analogue of the engine's per-segment backoff: the
// segment budget absorbs a blip, this absorbs the minute-scale outage behind
// an uploadhaven link. Exhausting the list settles the task as failed. It is a
// variable so tests can drive the cycle at millisecond scale.
var sessionRetryDelays = []time.Duration{
	20 * time.Second,
	40 * time.Second,
	60 * time.Second,
}

// serverStalledMessage is the terminal text for a link that kept answering 404
// across the whole recovery cycle.
//
// It is written literally rather than derived by humanError: its generic 404
// wording ("the link does not exist") would be misleading once bytes had
// arrived, and this text has to fit both a flapping CDN and a single-use link
// that was already consumed.
const serverStalledMessage = "服务器持续返回 404，链接可能已失效或正被限流 · 请稍后重试，或重新获取下载链接"

// errServerStalled carries serverStalledMessage as an error, so the mapping in
// humanError recognises it explicitly instead of falling into the generic
// HTTP-404 case.
var errServerStalled = errors.New(serverStalledMessage)

// updateFrozen reports whether an update is being applied, which freezes
// scheduling.
func (m *Manager) updateFrozen() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.updating
}

// awaitRetry announces a pending session-level retry and waits for it, both
// cancellable by the run's context, and reports whether the caller should try
// the download again.
//
// The task keeps its "downloading" status and its concurrency slot for the
// whole wait: it is still working, only waiting, and that is also what keeps a
// running plan (watchPlan) from declaring itself finished and intervened.
func (m *Manager) awaitRetry(ctx context.Context, t *task, gen int64, attempt int, d time.Duration) bool {
	seconds := int((d + time.Second - 1) / time.Second)

	t.mu.Lock()
	if t.generation != gen {
		t.mu.Unlock()
		return false
	}
	t.view.Notice = fmt.Sprintf("服务器临时故障（404），%d 秒后自动重试（第 %d/%d 次）",
		seconds, attempt, len(sessionRetryDelays))
	v := t.view
	t.mu.Unlock()
	m.app.emit("task:progress", v)
	// Persisted on purpose: the wait can last a minute, and a crash during it
	// must come back showing that a retry was pending rather than a task that
	// silently claims to be downloading. Every path that ends the wait clears
	// it and saves again, which is what keeps it from outliving the wait.
	_ = m.saveTasks()

	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// Pause, Cancel, StopAll during an update, or shutdown: the caller's
		// stop branch honours stopIntent.
		return false
	case <-timer.C:
	}

	// Never restart a transfer while an update is being applied. The update's
	// own StopAll has usually cancelled ctx already; this covers the race
	// where the freeze landed in the instant after the timer fired.
	if m.updateFrozen() {
		return false
	}

	// Clear the notice before retrying: the retry's own progress events replace
	// it, and a failure or an interrupted wait would otherwise leave it in
	// tasks.json as if it were still pending.
	t.mu.Lock()
	if t.generation == gen {
		t.view.Notice = ""
	}
	t.mu.Unlock()
	return true
}

// run performs the transfer for one task.
//
// ctx, cancel, done and gen come from startRun, which created them under t.mu
// before launching the goroutine: a pause arriving immediately still cancels
// this run and waits on the right channel, and a superseded run can always be
// told apart from the current one.
//
// A panic in a task goroutine would otherwise take the whole app down without a
// word, which is exactly the kind of failure a user can only report as "it
// crashed". The recover turns it into a failed task plus a log line, so the
// cause survives.
func (m *Manager) run(t *task, ctx context.Context, cancel context.CancelFunc, done chan struct{}, gen int64) {
	// Pump last: queued tasks start only after this run's done channel has
	// closed, so a waiter on the old run never observes the new one.
	defer m.pumpQueue()

	t.mu.Lock()
	url, dest, conns := t.view.URL, t.view.Dest, t.view.Conns
	t.done = done
	t.mu.Unlock()

	defer func() {
		cancel()
		close(done)
		if r := recover(); r != nil {
			// The id is read through snapshot: this recover path was the one
			// place that touched the view without taking the task lock.
			m.app.debugLog("panic in task %s: %v\n%s", snapshot(t).ID, r, debug.Stack())
			m.fail(t, gen, fmt.Errorf("内部错误: %v", r))
		}
	}()

	cfg := m.app.Config()
	opt := engine.Options{}.Normalize()
	opt.Connections = conns
	opt.MinSegmentSize = cfg.MinSegmentSize
	opt.Limiter = m.limiterFor(cfg.SpeedLimitKB)
	opt.ProgressInterval = refreshInterval(cfg.RefreshRate)
	opt.ProgressBytes = int64(cfg.ResumeFlushKB) << 10
	opt.SaveInterval = time.Duration(cfg.ResumeFlushMs) * time.Millisecond
	opt.SaveBytes = int64(cfg.ResumeFlushKB) << 10
	opt.SavePercent = 0
	if cfg.ResumeFlushByPct {
		opt.SavePercent = cfg.ResumeFlushPct
	}
	if !cfg.ResumeFlushBySize {
		opt.SaveBytes = -1
	}
	if !cfg.ResumeFlushByTime {
		opt.SaveInterval = -1
	}
	if cfg.Proxy != "" {
		opt.Transport.ProxyURL = cfg.Proxy
	}
	// Sent when the task came from the browser extension. Some hosts refuse a
	// download whose request does not appear to come from their own page, and
	// hosts that require a login reject a client that arrives without the
	// browser's cookies at all.
	opt.Headers = http.Header{}
	if t.referer != "" {
		opt.Headers.Set("Referer", t.referer)
	}
	if t.cookie != "" {
		opt.Headers.Set("Cookie", t.cookie)
	}
	if t.userAgent != "" {
		opt.Headers.Set("User-Agent", t.userAgent)
	}

	dl, err := engine.New(opt)
	if err != nil {
		m.fail(t, gen, err)
		return
	}
	defer dl.Close()
	untrack := m.trackLive(snapshot(t).ID, dl)
	defer untrack()

	req := engine.Request{
		URL:    url,
		Dest:   dest,
		Resume: true,
		Progress: func(p engine.Progress) {
			t.mu.Lock()
			// A callback from a superseded run must not resurrect old state;
			// the generation check is what makes that impossible. The removal
			// test covers a task deleted while it was starting: it is off the
			// list, so a late frame must not put it back.
			if t.generation != gen || !m.taskStillListed(t) {
				t.mu.Unlock()
				return
			}
			t.view.Downloaded = p.Downloaded
			t.applyReportedSize(p.Total)
			t.view.Speed = int64(p.Speed)
			// The server's suggested name arrives with the first progress,
			// long before completion renames the row. Take it once: a name
			// the user typed wins over a later suggestion.
			if t.view.Name == "" && p.Suggested != "" {
				t.view.Name = p.Suggested
			}
			if p.ETA > 0 {
				t.view.ETA = int64(p.ETA.Seconds())
			} else {
				t.view.ETA = 0
			}
			t.view.Conns = p.Connections
			t.view.Segments = p.Segments
			t.view.SegsDone = p.SegmentsDone
			v := t.view
			t.mu.Unlock()
			m.emit(v)
		},
	}

	res, err := dl.Download(ctx, req)

	// Session-level recovery for a server whose links flap. A 404 that arrived
	// after this session had already written bytes is a temporary outage, not a
	// dead link, so the whole session is replayed from its sidecar instead of
	// failing the task. The task stays "downloading" throughout: flipping it to
	// failed would flash a failure the user did not ask for, end a running plan
	// early (marking it intervened), and drop the run goroutine whose context
	// is what makes Pause/Cancel instant.
	recovered := 0
	for err != nil && recovered < len(sessionRetryDelays) {
		if ctx.Err() != nil {
			break
		}
		if recovered == 0 {
			// Only a 404 that happened after real progress — or that has a
			// resumable sidecar to come back to — is worth a whole-session
			// retry. A first-request 404 stays a fast, permanent failure.
			t.mu.Lock()
			name := t.view.Name
			t.mu.Unlock()
			part, _ := partialPaths(dest, name)
			if !engine.RecoverableNotFound(err, strings.TrimSuffix(part, engine.PartSuffix)) {
				break
			}
		} else if !engine.IsNotFound(err) {
			// A later attempt may 404 at probe time, before any bytes are
			// written; that still means the outage is on. Anything else —
			// disk full, size mismatch, TLS — must surface at once instead
			// of after minutes of waiting.
			break
		}
		recovered++
		if !m.awaitRetry(ctx, t, gen, recovered, sessionRetryDelays[recovered-1]) {
			break
		}
		res, err = dl.Download(ctx, req)
	}

	if err != nil {
		// A task the user removed while this run was starting is no longer
		// in the list; there is nothing to settle, and writing a status to
		// the detached record only feeds a late event for a row that is gone.
		t.mu.Lock()
		listed := m.taskStillListed(t)
		t.mu.Unlock()
		if !listed {
			return
		}
		if frozen := m.updateFrozen(); ctx.Err() != nil || frozen {
			// The transfer was stopped. Whichever action did that also recorded
			// what it wanted the status to become, and that record is the one to
			// trust: reading the view here can race the setter, and mistaking a
			// pause for a cancel throws away the partial data's ability to
			// resume. An update freeze is a pause too: the partial data stays
			// valid and the task resumes after the restart.
			t.mu.Lock()
			// Only a run that is still the current one may write the status.
			// Without the generation test, a run that was paused and then
			// resumed before its goroutine unwound found the task downloading
			// again — the *new* run's status — and stamped the old intent
			// ("paused", or "canceled") onto it.
			if t.generation == gen && t.view.Status == StatusDownloading {
				switch {
				case t.stopIntent != "":
					t.view.Status = t.stopIntent
				case frozen:
					t.view.Status = StatusPaused
				default:
					t.view.Status = StatusCanceled
				}
			}
			t.view.Notice = ""
			v := t.view
			t.mu.Unlock()
			m.app.emit("task:progress", v)
			// Persist: this branch clears the notice and used to return
			// without a save, leaving the notice it had just announced in
			// tasks.json for a task that is no longer retrying.
			_ = m.saveTasks()
			return
		}
		if recovered > 0 && engine.IsNotFound(err) {
			// The recovery cycle ran out: the link has answered 404 for
			// minutes. A flapping CDN that never recovered and a single-use
			// token that was already spent look identical here.
			m.failMessage(t, gen, serverStalledMessage)
			return
		}
		m.fail(t, gen, err)
		return
	}

	t.mu.Lock()
	if t.generation != gen {
		t.mu.Unlock()
		return
	}
	// The user removed this task while its last bytes were landing. The file
	// is complete on disk, but the record is gone: resurrecting it would put a
	// row the user deleted back on screen, and its partial cleanup has already
	// run against this very file.
	if !m.taskStillListed(t) {
		t.mu.Unlock()
		return
	}
	// Name and Dest first; Status flips last, after organizing below, so no
	// observer ever sees "completed" pointing at the pre-organize location.
	t.view.Name = filepath.Base(res.Path)
	t.view.Dest = res.Path
	manual := t.manual
	name := t.view.Name
	t.mu.Unlock()

	// Organize: automatic arrivals with a matching extension move into their
	// group folder now that the final name is known. Manual tasks skip this —
	// the user already placed them.
	// (cfg is the settings snapshot taken above for the engine options.)
	if !manual && cfg.OrganizeEnabled {
		if dst := organizeTarget(cfg.OrganizeGroups,
			cfg.OrganizeDir, name); dst != "" && dst != res.Path {
			if dst, err := moveIntoPlace(res.Path, dst); err != nil {
				// Surfaced on the task, not just the log: the download
				// itself succeeded, so the task stays completed — but a
				// log-only failure left users hunting a file that was never
				// where the UI said it was.
				t.mu.Lock()
				if t.generation == gen && m.taskStillListed(t) {
					t.view.Error = "整理失败，已保留在原位置：" + err.Error()
				}
				t.mu.Unlock()
				m.app.debugLog("organize %s: %v", res.Path, err)
			} else {
				t.mu.Lock()
				if t.generation == gen && m.taskStillListed(t) {
					t.view.Dest = dst
				}
				t.mu.Unlock()
			}
		}
	}

	t.mu.Lock()
	// Re-checked under one lock: organizing above released it, and a removal
	// can land in that window.
	if t.generation != gen || !m.taskStillListed(t) {
		t.mu.Unlock()
		return
	}
	t.view.Status = StatusCompleted
	t.view.Size = res.Size
	t.view.Downloaded = res.Size
	t.view.Speed = 0
	t.view.ETA = 0
	t.view.Notice = ""
	t.view.Segments = res.Segments
	t.view.SegsDone = res.Segments
	t.view.Conns = res.Connections
	t.view.Finished = time.Now().Format(time.RFC3339Nano)
	v := t.view
	t.mu.Unlock()

	m.app.emit("task:done", v)
	m.app.afterTaskFinished(v)
	_ = m.saveTasks()
}

// moveIntoPlace moves a finished file into its group folder and returns the
// path it landed at. A Rename is tried first; when source and destination
// turn out to be on different volumes (a picked group Dir on another drive)
// it falls back to copy+remove. An occupied destination is never overwritten:
// the name grows a " (1)" suffix until it is free.
func moveIntoPlace(src, dst string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	dst = uniqueDest(dst)
	// Any rename failure falls through to the copy below — cross-volume
	// (EXDEV / Windows syscall error) or otherwise. A successful copy+remove
	// leaves the same end state a rename would have, so classifying the errno
	// buys nothing.
	if err := os.Rename(src, dst); err == nil {
		return dst, nil
	}
	if err := copyFile(src, dst); err != nil {
		return "", err
	}
	if err := os.Remove(src); err != nil {
		// The bytes are safe at dst; report the leftover rather than failing.
		return dst, fmt.Errorf("已复制到 %s，但源文件删除失败：%v", dst, err)
	}
	return dst, nil
}

// uniqueDest appends " (n)" before the extension until the path is free.
// A non-NotExist stat error (permissions, transient I/O) is treated as
// "occupied" so an unreadable file is never silently overwritten; the loop is
// capped so a persistently failing stat cannot spin forever — the caller
// surfaces the resulting rename/copy error on the task.
func uniqueDest(dst string) string {
	if _, err := os.Stat(dst); os.IsNotExist(err) {
		return dst
	}
	ext := filepath.Ext(dst)
	base := strings.TrimSuffix(dst, ext)
	cand := dst
	for i := 1; i <= 10000; i++ {
		cand = fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return cand
}

// copyFile duplicates src to dst, flushing before returning so the caller can
// remove the source knowing the bytes are stable.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	// Best effort on Windows, same rationale as saveTasks.
	_ = out.Sync()
	return out.Close()
}

// fail records a task failure with a message meant for a human.
func (m *Manager) fail(t *task, gen int64, err error) {
	m.failMessage(t, gen, humanError(err))
}

// failMessage settles a task as failed with a message that is already meant
// for a human.
//
// It exists for the one failure whose text must not be re-derived: a link that
// kept answering 404 after bytes had arrived, where humanError's generic "the
// link does not exist" would be a lie.
func (m *Manager) failMessage(t *task, gen int64, msg string) {
	t.mu.Lock()
	if t.generation != gen {
		t.mu.Unlock()
		return
	}
	t.view.Status = StatusFailed
	t.view.Error = msg
	t.view.Notice = ""
	t.view.Speed = 0
	t.view.ETA = 0
	t.view.Finished = time.Now().Format(time.RFC3339Nano)
	v := t.view
	t.mu.Unlock()

	m.app.emit("task:failed", v)
	m.app.afterTaskFinished(v)
	_ = m.saveTasks()
}

// humanError turns an engine error into something actionable.
//
// "HTTP 403" on its own tells the user nothing; naming the likely cause and
// what to try next is what makes an error worth showing.
//
// No URL leaves this function with its query string. Engine errors wrap the
// request URL, and every branch below — the per-status mappings included —
// would otherwise copy a signed link's credential into tasks.json and into the
// row on screen. The text is rewritten once, up front, into the log-safe form
// (host and path); the protocol words the status checks look for survive.
func humanError(err error) string {
	msg := redactURIs(err.Error())
	lower := strings.ToLower(msg)
	switch {
	case errors.Is(err, errServerStalled):
		// Returned verbatim, not re-derived: after bytes have arrived, "the
		// link does not exist" is the wrong story — it is a server outage or a
		// single-use link that was already consumed.
		return serverStalledMessage
	case errors.Is(err, engine.ErrNeedsSignIn):
		// Checked before ErrNotAFile: both arrive as an HTML body, but the fix
		// is different and "find a direct link" would send the user looking for
		// a link that was already right.
		return "这个文件需要登录才能下载 · 把分享权限改成「知道链接的任何人」，或者用浏览器下载"
	case errors.Is(err, engine.ErrNotAFile):
		// The most common cause by far is pasting the page a file is shown on
		// rather than the file itself, so the message names the fix.
		return "这是网页地址，不是文件下载地址 · 请改用文件的直链（例如网盘的「直接下载」链接）"
	case errors.Is(err, engine.ErrSizeMismatch):
		return "文件长度与服务器声明不符 · 已阻止发布，避免留下损坏的文件"
	case strings.Contains(msg, "HTTP 403") || strings.Contains(msg, "status 403") || strings.Contains(msg, "(403)"):
		return "服务器拒绝了请求（403）· 可能需要登录，或该链接已失效"
	case strings.Contains(msg, "HTTP 404") || strings.Contains(msg, "status 404") || strings.Contains(msg, "(404)"):
		return "链接不存在（404）· 请检查地址是否写错"
	case strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "status 401") || strings.Contains(msg, "(401)"):
		return "需要登录（401）· 该资源要求身份验证"
	case errors.Is(err, engine.ErrRangeUnusable):
		return "服务器返回的数据与请求的不一致 · 这个链接不支持分段下载，请稍后重试或改用浏览器下载"
	case strings.Contains(msg, "HTTP 416") || strings.Contains(msg, "status 416") || strings.Contains(msg, "(416)"):
		return "服务器无法满足该范围请求（416）· 文件可能已变化"
	case strings.Contains(msg, "HTTP 429") || strings.Contains(msg, "status 429") || strings.Contains(msg, "(429)"):
		return "请求太频繁，被服务器限流（429）· 请等几分钟再试，或减少同时下载的任务数"
	case strings.Contains(msg, "HTTP 408") || strings.Contains(msg, "status 408") || strings.Contains(msg, "(408)"):
		return "服务器等待超时（408）· 网络太慢或服务器繁忙，请稍后重试"
	case strings.Contains(msg, "HTTP 500") || strings.Contains(msg, "status 500") || strings.Contains(msg, "(500)"):
		return "服务器内部出错（500）· 不是你的问题，请稍后重试"
	case strings.Contains(msg, "HTTP 502") || strings.Contains(msg, "status 502") || strings.Contains(msg, "(502)"):
		return "网关错误（502）· 服务器上游节点异常，请稍后重试"
	case strings.Contains(msg, "HTTP 503") || strings.Contains(msg, "status 503") || strings.Contains(msg, "(503)"):
		return "服务器暂时不可用（503）· 正在维护或过载，请稍后重试"
	case strings.Contains(msg, "HTTP 504") || strings.Contains(msg, "status 504") || strings.Contains(msg, "(504)"):
		return "网关超时（504）· 服务器上游响应太慢，请稍后重试"
	case strings.Contains(lower, "no such host"),
		strings.Contains(lower, "dial tcp"),
		strings.Contains(lower, "connection refused"):
		return "无法连接到服务器 · 请检查网络或链接是否有效"
	case strings.Contains(lower, "certificate"), strings.Contains(lower, "x509"):
		return "TLS 证书校验失败 · 站点证书有问题，或被中间环节拦截"
	case strings.Contains(lower, "announced size"):
		return "文件长度与服务器声明不符 · 已阻止发布，避免留下损坏的文件"
	case strings.Contains(lower, "timeout"), strings.Contains(lower, "deadline exceeded"):
		return "连接超时 · 服务器响应过慢或网络不稳定"
	case strings.Contains(lower, "no space"), strings.Contains(lower, "not enough space"),
		strings.Contains(lower, "insufficient disk space"), strings.Contains(lower, "disk full"):
		return "磁盘空间不足 · 请清理后重试"
	case strings.Contains(lower, "access is denied"), strings.Contains(lower, "permission denied"):
		return "没有写入权限 · 请换一个保存目录"
	}
	return "下载失败：" + msg
}

// redactURIs replaces every http(s) URL inside text with its log-safe form, so
// an unmapped error can be shown and stored without carrying a signed link's
// query string along.
func redactURIs(text string) string {
	var b strings.Builder
	rest := text
	for {
		i, j := strings.Index(rest, "http://"), strings.Index(rest, "https://")
		if i < 0 || (j >= 0 && j < i) {
			i = j
		}
		if i < 0 {
			b.WriteString(rest)
			return b.String()
		}
		b.WriteString(rest[:i])
		// A URL runs until whitespace or a closing quote.
		end := len(rest)
		if k := strings.IndexAny(rest[i:], " \t\r\n\"'<>"); k >= 0 {
			end = i + k
		}
		b.WriteString(logURL(rest[i:end]))
		rest = rest[end:]
	}
}

// Stats reports the aggregate figures the sidebar shows.
//
// "queued" counts only tasks actually waiting for a slot. A paused task is
// deliberately not folded into it: the plan watcher treats "nothing active and
// nothing queued" as "the plan has finished", and a paused task counted as
// queued would keep a plan running forever.
func (m *Manager) Stats() map[string]int64 {
	c := m.countByStatus()
	return map[string]int64{
		"active": c.active, "completed": c.completed, "failed": c.failed,
		"queued": c.queued, "paused": c.paused, "canceled": c.canceled,
		"total": c.total, "speed": c.speed,
	}
}

// persistedTask is the on-disk form of a task: the TaskView the UI shows plus
// the creation-time fields the view does not carry. The view alone used to be
// persisted, so manual placement, referer, cookie and userAgent were silently
// dropped on every restart — a resumed download then arrived without the
// credentials it started with.
//
// NOTE: cookie is stored in PLAINTEXT in tasks.json (it must be replayable
// verbatim on resume). Anyone with read access to the data directory can lift
// the browser session it came from.
type persistedTask struct {
	TaskView
	Manual    bool   `json:"manual"`
	Referer   string `json:"referer,omitempty"`
	Cookie    string `json:"cookie,omitempty"`
	UserAgent string `json:"userAgent,omitempty"`
}

// saveTasks writes the task index so the list survives a restart.
//
// The engine's sidecar beside each partial file stays the source of truth for
// how many bytes arrived; this index only records what the user asked for.
//
// Indent is deliberate: humans and agents read this file when diagnosing the
// app, and the batching below keeps the cost to one write per bulk verb.
func (m *Manager) saveTasks() error {
	m.mu.Lock()
	if m.saveDepth > 0 {
		m.saveDirty = true
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()

	// Serialised against concurrent settlers; m.mu is not held across it.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	path := filepath.Join(m.app.dataDir(), "tasks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.persistedList(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	// Flush to stable storage before the rename, so a crash cannot leave a
	// truncated tasks.json behind. Best effort on Windows: Sync is honoured
	// where the OS supports it.
	_ = f.Sync()
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// redactedForHistory returns the URL a finished task should keep on disk.
//
// A signed download link carries its own credential in the query string —
// token=, sig=, X-Amz-Signature=, se/skt (Azure SAS) and friends — and a
// completed task will never be fetched again, so keeping the credential serves
// no purpose. It only leaks: tasks.json is a plain file in the user's profile,
// routinely swept up by backup tools and sync clients. Host and path stay, which
// is what the row needs to display and what the user needs to recognise it.
func redactedForHistory(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if u.RawQuery == "" && u.Fragment == "" {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

// finishedForHistory reports whether a task will never be fetched again, which
// is what makes redacting its URL safe. Cancelled tasks are deliberately not
// included: Resume restarts them, so their URL is still needed.
func finishedForHistory(s TaskStatus) bool {
	return s == StatusCompleted
}

// persistedList snapshots every task into its on-disk form.
func (m *Manager) persistedList() []persistedTask {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	out := make([]persistedTask, 0, len(ids))
	for _, id := range ids {
		t := m.get(id)
		if t == nil {
			continue
		}
		t.mu.Lock()
		p := persistedTask{
			TaskView: t.view,
			Manual:   t.manual,
		}
		// Credentials are only persisted while they can still be used. A finished
		// download replayed no credentials, so storing them is pure exposure:
		// cookie is plaintext, and the URL may carry a signed token.
		if !finishedForHistory(t.view.Status) {
			p.Referer = t.referer
			// The cookie is sealed with DPAPI. It has to be stored — resuming a
			// login-protected download without it is rejected by the host — but
			// it does not have to be readable by anything that happens to open
			// this file.
			if sealed, serr := sealCookie(t.cookie); serr != nil {
				m.app.debugLog("cannot encrypt the cookie for task %s, storing the task without it: %v", t.view.ID, serr)
			} else {
				p.Cookie = sealed
			}
			p.UserAgent = t.userAgent
		} else {
			p.TaskView.URL = redactedForHistory(p.TaskView.URL)
		}
		out = append(out, p)
		t.mu.Unlock()
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Created > out[j].Created })
	return out
}

// batch runs fn with index writes coalesced: every saveTasks inside only
// marks dirty, and one real write happens at the end (if anything changed).
// Nesting is safe: only the outermost batch flushes.
func (m *Manager) batch(fn func()) {
	m.mu.Lock()
	m.saveDepth++
	m.mu.Unlock()

	fn()

	m.mu.Lock()
	m.saveDepth--
	dirty := m.saveDirty
	if m.saveDepth == 0 {
		m.saveDirty = false
	}
	m.mu.Unlock()

	if dirty {
		_ = m.saveTasks()
	}
}

// loadTasks restores the index written by saveTasks.
//
// Tasks that were mid-transfer when the app closed come back as paused: the
// bytes are still on disk, but nothing is running, so "paused" is the only
// honest status to report. Queued tasks keep their place in the queue — the
// user asked for those downloads and they were only waiting for a slot, so
// the startup pump resumes the wait instead of silently parking them.
func (m *Manager) loadTasks() {
	dir := m.app.dataDir()
	path := filepath.Join(dir, "tasks.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var list []persistedTask
	if err := json.Unmarshal(b, &list); err != nil {
		// Keep the evidence: a corrupt index is renamed aside (never deleted)
		// so the failure stays diagnosable, and the app starts empty rather
		// than refusing to start.
		kept := fmt.Sprintf("tasks.json.corrupt-%s", time.Now().Format("20060102-150405.000"))
		_ = os.Rename(path, filepath.Join(dir, kept))
		m.app.debugLog("tasks.json corrupt (%v), moved to %s", err, kept)
		m.app.emit("tasks:corrupt", kept)
		return
	}

	// Stat every completed destination before taking the lock. A missing
	// network drive can stall a Stat for seconds, and holding m.mu across
	// the whole restore would freeze List/Stats/Start with it. The lock only
	// needs to cover the map insertion below.
	gone := make([]bool, len(list))
	for i := range list {
		if list[i].TaskView.Status != StatusCompleted {
			continue
		}
		if _, statErr := os.Stat(list[i].TaskView.Dest); os.IsNotExist(statErr) {
			gone[i] = true
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	seen := map[string]bool{}
	for i := range list {
		p := list[i]
		v := p.TaskView
		if v.ID == "" || seen[v.ID] {
			m.app.debugLog("tasks.json: skipping duplicate/empty id %q", v.ID)
			continue
		}
		seen[v.ID] = true
		// Only mid-transfer tasks flip to paused; queued ones keep waiting
		// (see the function comment).
		if v.Status == StatusDownloading {
			v.Status = StatusPaused
		}
		// A retry notice describes a wait that the app closing interrupted.
		// Nothing is waiting now, so keeping it would make the first resume
		// flash "retrying in N s" before it retried anything.
		v.Notice = ""
		v.Speed, v.ETA = 0, 0
		// A completed task whose file has since vanished must not keep
		// claiming to be complete. Only a missing file flips it: any other
		// stat error (permissions, transient I/O) leaves the record alone,
		// so a flaky disk cannot mass-fail the library.
		if gone[i] {
			v.Status = StatusFailed
			v.Error = fileGoneMessage
		}
		// Decrypted here, once, so the rest of the app keeps working with a plain
		// value. A cookie that cannot be opened (the file was copied from another
		// account, the profile was reset) is dropped with a log line: the task
		// still resumes, just without its session.
		cookie := p.Cookie
		if cookie != "" {
			if plain, cerr := openCookie(cookie); cerr != nil {
				m.app.debugLog("cannot decrypt the stored cookie for task %s: %v", v.ID, cerr)
				cookie = ""
			} else {
				cookie = plain
			}
		}
		m.tasks[v.ID] = &task{
			view:      v,
			manual:    p.Manual,
			referer:   p.Referer,
			cookie:    cookie,
			userAgent: p.UserAgent,
		}
		m.order = append(m.order, v.ID)
		// Re-seed the id counter past anything on disk: ids are
		// t<millis>-<seq>, and a counter reset to zero would eventually mint
		// a colliding id for a task created in the same millisecond.
		if n, ok := parseTaskSeq(v.ID); ok && n > m.seq {
			m.seq = n
		}
	}
}

// fileGoneMessage marks a task whose finished file has disappeared. Resume
// refuses it: the stored URL has no credentials any more (they are dropped when a
// download completes), so retrying would fail for a reason unrelated to the real
// problem.
const fileGoneMessage = "文件已被移动或删除"

// parseTaskSeq extracts the sequence suffix from an id of the form
// t<millis>-<seq> minted by Start.
func parseTaskSeq(id string) (int64, bool) {
	i := strings.LastIndex(id, "-")
	if i < 0 || !strings.HasPrefix(id, "t") {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(id[i+1:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}
