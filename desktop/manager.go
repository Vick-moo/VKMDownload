package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
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

func (a *App) debugLog(format string, args ...any) {
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
	_, _ = fmt.Fprintf(f, "%s %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
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
	Conns      int        `json:"connections"`
	Segments   int        `json:"segments"`
	SegsDone   int        `json:"segmentsDone"`
	Created    string     `json:"createdAt"`
	Finished   string     `json:"finishedAt"`
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

	// planRunning is set while a plan is working through its tasks.
	planRunning bool

	// updating freezes scheduling: set while an update is being applied, so
	// pausing everything for the restart cannot be undone by the queue
	// starter waking a queued task back up.
	updating bool
}

func NewManager(app *App) *Manager {
	return &Manager{
		app:    app,
		tasks:  make(map[string]*task),
		pump:   make(chan TaskView, 512),
		livedl: make(map[string]*engine.Downloader),
	}
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
			m.app.emit("task:progress", v)
		}
	}
}

// emit queues a progress event, dropping it when the pump is saturated.
// Dropping is safe: a snapshot is stale as soon as a newer one exists, and
// another arrives within a few hundred milliseconds.
func (m *Manager) emit(v TaskView) {
	select {
	case m.pump <- v:
	default:
	}
}

func (m *Manager) get(id string) *task {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tasks[id]
}

// snapshot copies a task's view.
func snapshot(t *task) TaskView {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.view
}

// List returns every task, newest first.
func (m *Manager) List() []TaskView {
	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	out := make([]TaskView, 0, len(ids))
	for _, id := range ids {
		if t := m.get(id); t != nil {
			out = append(out, snapshot(t))
		}
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

// findSameContent returns an unfinished task that is most likely the same
// file under a different URL: same host, same filename, and compatible size.
// (Sizes are compatible when either side is unknown — a row that never
// started reports 0 — or both agree.) Completed tasks never match, and an
// explicit escape hatch always exists: rename the newcomer and it no longer
// matches.
func (m *Manager) findSameContent(url, name string, size int64) *task {
	host := urlHost(url)
	if host == "" || name == "" {
		return nil
	}

	m.mu.Lock()
	ids := append([]string(nil), m.order...)
	m.mu.Unlock()

	for _, id := range ids {
		t := m.get(id)
		if t == nil {
			continue
		}
		t.mu.Lock()
		v := t.view
		// Identical URLs are the first layer's business (same link to
		// another folder is allowed); this layer is only for different
		// URLs pointing at the same file.
		match := v.URL != url &&
			v.Status != StatusCompleted &&
			v.Name != "" && v.Name == name &&
			urlHost(v.URL) == host &&
			(size <= 0 || v.Size <= 0 || v.Size == size)
		t.mu.Unlock()
		if match {
			return t
		}
	}
	return nil
}

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
	if dup := m.findUnfinished(url, dest); dup != nil {
		v := snapshot(dup)
		return v, &duplicateError{ID: v.ID, Status: v.Status, Name: v.Name}
	}

	// When the caller supplies no filename, ask the server once up front.
	// This fills the row's name and size immediately (instead of a blank
	// name and 0 B until the transfer starts) and powers the content-based
	// duplicate check below. Any probe failure keeps the old behaviour:
	// unnamed, unknown size, created anyway.
	var probedSize int64
	if name == "" {
		pn, ps := m.probeName(url, req)
		if pn != "" {
			name = pn
		}
		probedSize = ps
	}

	// Second layer: single-use links mint a fresh URL per click, so the check
	// above is blind to them. Same host + same filename + compatible size
	// means the same file with overwhelming probability.
	if name != "" {
		if dup := m.findSameContent(url, name, probedSize); dup != nil {
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
	}
	t.view = TaskView{
		ID:      id,
		URL:     url,
		Dest:    dest,
		Name:    name,
		Size:    probedSize,
		Status:  StatusPaused,
		Conns:   conns,
		Created: time.Now().Format(time.RFC3339Nano),
	}

	m.mu.Lock()
	m.tasks[id] = t
	m.order = append([]string{id}, m.order...)
	m.mu.Unlock()

	m.app.emit("task:added", snapshot(t))

	if req.Start {
		m.startRun(t)
	}
	_ = m.saveTasks()
	return snapshot(t), nil
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
		t.view.Status = StatusQueued
		t.mu.Unlock()
		_ = m.saveTasks()
		m.app.emit("task:progress", snapshot(t))
		return
	}
	if m.activeCount() >= m.maxConcurrent() {
		t.mu.Lock()
		t.view.Status = StatusQueued
		t.mu.Unlock()
		_ = m.saveTasks()
		// Report the queueing at once: without this the row sits on its old
		// status until the engine's first progress event, which on a slow
		// link arrives seconds later and reads as a dead button.
		m.app.emit("task:progress", snapshot(t))
		return
	}
	// The done channel is created here, under the same lock as cancel, so a
	// Pause/Cancel arriving while the goroutine is still starting always finds
	// something to wait on. Creating it inside run instead left a window where
	// done was nil and the stop returned without waiting — the transfer then
	// carried on after the user had stopped it.
	ctx, cancel := context.WithCancel(m.app.ctx)
	t.mu.Lock()
	t.cancel = cancel
	// A fresh run has no stop to report yet.
	t.stopIntent = ""
	t.view.Status = StatusDownloading
	t.view.Error = ""
	done := make(chan struct{})
	t.done = done
	t.mu.Unlock()

	// Report the start at once, for the same reason as the queueing above:
	// the engine's first progress event can lag seconds behind on a slow
	// link, and until then the UI shows the pre-start state.
	m.app.emit("task:progress", snapshot(t))

	go m.run(t, ctx, cancel, done)
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

// activeCount counts transferring tasks. It copies the task pointers under
// m.mu and inspects them after releasing it: locking a task while holding m.mu
// would invert the lock order used everywhere else (task first, manager
// second) and could deadlock against a concurrent Pause.
func (m *Manager) activeCount() int {
	m.mu.Lock()
	tasks := make([]*task, 0, len(m.tasks))
	for _, t := range m.tasks {
		tasks = append(tasks, t)
	}
	m.mu.Unlock()

	n := 0
	for _, t := range tasks {
		t.mu.Lock()
		running := t.view.Status == StatusDownloading
		t.mu.Unlock()
		if running {
			n++
		}
	}
	return n
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
	for {
		if m.activeCount() >= m.maxConcurrent() {
			return
		}
		// List returns newest first; walk from the end so the oldest queued
		// task wins the freed slot.
		list := m.List()
		var nextID string
		for i := len(list) - 1; i >= 0; i-- {
			if list[i].Status == StatusQueued {
				nextID = list[i].ID
				break
			}
		}
		if nextID == "" {
			return
		}
		next := m.get(nextID)
		if next == nil {
			return
		}
		next.mu.Lock()
		stillQueued := next.view.Status == StatusQueued
		next.mu.Unlock()
		if !stillQueued {
			continue
		}
		m.startRun(next)
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
	t.mu.Unlock()

	m.mu.Lock()
	frozen := m.updating
	m.mu.Unlock()
	if frozen {
		return fmt.Errorf("正在更新，稍后再试")
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
	cancel := t.cancel
	dest := t.view.Dest
	gen := t.generation
	t.stopIntent = StatusCanceled
	t.view.Status = StatusCanceled
	t.view.Speed = 0
	t.view.ETA = 0
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
	go func() {
		// Wait for the transfer goroutine to stop before deleting: cancelling
		// only asks it to stop, and a write in flight would recreate the
		// partial file immediately after it was removed.
		t.waitStopped(3 * time.Second)

		t.mu.Lock()
		superseded := t.generation != gen
		t.mu.Unlock()
		if superseded {
			return
		}

		// A cancelled task should start clean, so the partial file and the
		// sidecar that would resume it are both removed.
		part, state := partialPaths(dest, v.Name)
		_ = os.Remove(part)
		_ = os.Remove(state)
		m.pumpQueue()
	}()
	return nil
}

// Remove drops a task, optionally deleting the files it produced.
func (m *Manager) Remove(id string, deleteFiles bool) error {
	t := m.get(id)
	if t == nil {
		return fmt.Errorf("任务不存在")
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
	_ = m.saveTasks()

	go func() {
		// Same reasoning as Cancel: the goroutine may still be writing, so
		// waiting avoids a race between removal and a final flush.
		t.waitStopped(3 * time.Second)

		// Records go; an unfinished task's partial data goes with them. A
		// .part + sidecar without their index entry can never resume (the
		// next attempt at the same URL starts from scratch), so leaving them
		// behind only accumulates junk named after URL tokens. A completed
		// task's file is the user's data: it is removed only when the caller
		// explicitly asked for it, never as a side effect of tidying the list.
		// partialPaths mirrors the engine's dir+suggested-name resolution:
		// tasks created without a filename keep Dest as the directory.
		if !wasComplete {
			part, state := partialPaths(dest, name)
			_ = os.Remove(part)
			_ = os.Remove(state)
		}
		if deleteFiles {
			// Only delete the published file when the task actually produced
			// one, so a failed task cannot remove an unrelated file sharing
			// the name.
			if wasComplete {
				_ = os.Remove(dest)
			}
		}
		m.pumpQueue()
	}()
	return nil
}

// RemoveTasks drops the given records in one go.
//
// The caller decides which, so "delete everything I can see" works for any
// filter — by status, by file type, by search — without this needing to know
// what the filter was. Finished files are never touched; unfinished partials
// go with their records.
func (m *Manager) RemoveTasks(ids []string) int {
	removed := 0
	for _, id := range ids {
		if err := m.Remove(id, false); err == nil {
			removed++
		}
	}
	return removed
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
	return n
}

// StopAll pauses every task that is transferring, keeping its partial data so
// it can be continued later. Tasks that already settled are untouched.
func (m *Manager) StopAll() int {
	n := 0
	for _, v := range m.List() {
		if v.Status == StatusDownloading || v.Status == StatusQueued {
			if err := m.Pause(v.ID); err == nil {
				n++
			}
		}
	}
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

	// Try to start everything waiting; tasks beyond the concurrency budget
	// stay queued and are pulled in by pumpQueue as slots free up.
	m.StartAll()
	// A plan covers everything that is running or waiting, not just the tasks
	// this call managed to start: when every MaxConcurrent slot is already
	// occupied, StartAll cannot start anything new (started == 0) yet there
	// is still real work — the running transfers plus the queued ones
	// pumpQueue pulls in as slots free up. Refusing the plan in that case
	// ("没有等待下载的任务") made the plan button dead exactly when the list
	// was full of queued tasks.
	stats := m.Stats()
	covered := int(stats["active"] + stats["queued"])
	if covered == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, fmt.Errorf("没有等待下载的任务")
	}

	go m.watchPlan()
	return covered, nil
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

	for _, v := range m.List() {
		if !want[v.ID] {
			continue
		}
		switch v.Status {
		case StatusPaused, StatusFailed, StatusCanceled, StatusQueued:
			_ = m.Resume(v.ID)
		}
	}

	covered := 0
	for _, v := range m.List() {
		if want[v.ID] && (v.Status == StatusDownloading || v.Status == StatusQueued) {
			covered++
		}
	}
	if covered == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, fmt.Errorf("选中的任务没有可下载的")
	}

	go m.watchPlanSelected(want)
	return covered, nil
}

// watchPlanSelected waits until every planned task has settled, then finishes
// the plan. Removal or cancellation of a planned task counts as settling but
// marks the plan as intervened, which skips the end actions — same rule as
// the global watcher.
func (m *Manager) watchPlanSelected(want map[string]bool) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		done, intervened := true, false
		for id := range want {
			t := m.get(id)
			if t == nil {
				intervened = true
				continue
			}
			switch snapshot(t).Status {
			case StatusDownloading, StatusQueued:
				done = false
			case StatusCanceled:
				intervened = true
			}
		}
		if done {
			m.finishPlanScoped(intervened)
			return
		}
	}
}

// finishPlanScoped ends a selection-scoped plan. It shares the end actions
// with the global plan.
func (m *Manager) finishPlanScoped(intervened bool) {
	m.mu.Lock()
	m.planRunning = false
	m.mu.Unlock()
	m.finishPlanEnd(intervened)
}

// PlanRunning reports whether a plan is working.
func (m *Manager) PlanRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.planRunning
}

// watchPlan waits until nothing is transferring, then finishes the plan.
//
// It polls rather than being notified: every path that can end a transfer —
// completion, failure, cancellation, removal — would otherwise have to remember
// to poke the plan, and the one that forgot would leave it hanging forever.
func (m *Manager) watchPlan() {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		stats := m.Stats()
		if stats["active"] == 0 && stats["queued"] == 0 {
			m.finishPlan()
			return
		}
	}
}

// finishPlan performs whatever the user asked for at the end of a plan.
func (m *Manager) finishPlan() {
	m.mu.Lock()
	m.planRunning = false
	m.mu.Unlock()

	// A plan that ends with cancelled tasks did not run to completion: the
	// user intervened. Firing the end-of-plan actions anyway — especially a
	// machine shutdown — would punish cancelling.
	intervened := false
	if stats := m.Stats(); stats["canceled"] > 0 {
		intervened = true
	}
	m.finishPlanEnd(intervened)
}

// finishPlanEnd emits plan:done and runs the configured end actions, unless
// the plan was intervened with.
func (m *Manager) finishPlanEnd(intervened bool) {
	if intervened {
		m.app.emit("plan:done", nil)
		m.app.debugLog("plan finished with cancellations: end actions skipped")
		return
	}

	cfg := m.app.Config()
	m.app.emit("plan:done", nil)
	m.app.debugLog("plan finished: sound=%v shutdown=%v", cfg.PlanSound, cfg.PlanShutdown)

	if cfg.PlanSound {
		playAlert()
	}
	if cfg.PlanShutdown {
		shutdownMachine()
	}
}

// playAlert makes an audible signal, so a plan that finishes while the user is
// away from the screen is still noticed.
func playAlert() {
	go func() {
		// Two rising tones through the console. No asset to ship, and it does
		// not depend on the user's sound scheme being configured.
		const script = "[console]::beep(880,250); [console]::beep(1175,400)"
		_ = exec.Command("powershell", "-NoProfile", "-Command", script).Run()
	}()
}

// shutdownMachine powers the machine off, with enough delay to change one's
// mind: "shutdown /a" cancels it.
func shutdownMachine() {
	go func() {
		_ = exec.Command("shutdown", "/s", "/t", "60",
			"/c", "VKMDownload 计划已完成，60 秒后关机（运行 shutdown /a 取消）").Run()
	}()
}

// run performs the transfer for one task.
//
// ctx, cancel and done come from the caller: startRun creates them before
// launching the goroutine so that a pause arriving immediately still cancels
// this run and waits on the right channel.
//
// A panic in a task goroutine would otherwise take the whole app down without a
// word, which is exactly the kind of failure a user can only report as "it
// crashed". The recover turns it into a failed task plus a log line, so the
// cause survives.
func (m *Manager) run(t *task, ctx context.Context, cancel context.CancelFunc, done chan struct{}) {
	// Pump last: queued tasks start only after this run's done channel has
	// closed, so a waiter on the old run never observes the new one.
	defer m.pumpQueue()

	t.mu.Lock()
	t.generation++
	gen := t.generation
	t.view.Status = StatusDownloading
	t.view.Error = ""
	t.view.Speed = 0
	t.view.ETA = 0
	url, dest, conns := t.view.URL, t.view.Dest, t.view.Conns
	t.done = done
	t.mu.Unlock()

	defer func() {
		cancel()
		close(done)
		if r := recover(); r != nil {
			m.app.debugLog("panic in task %s: %v\n%s", t.view.ID, r, debug.Stack())
			m.fail(t, gen, fmt.Errorf("内部错误: %v", r))
		}
	}()

	cfg := m.app.Config()
	opt := engine.Options{}.Normalize()
	opt.Connections = conns
	opt.MinSegmentSize = cfg.MinSegmentSize
	opt.Limiter = m.limiterFor(cfg.SpeedLimitKB)
	opt.ProgressInterval = refreshInterval(cfg.RefreshRate)
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

	res, err := dl.Download(ctx, engine.Request{
		URL:    url,
		Dest:   dest,
		Resume: true,
		Progress: func(p engine.Progress) {
			t.mu.Lock()
			// A callback from a superseded run must not resurrect old state;
			// the generation check is what makes that impossible.
			if t.generation != gen {
				t.mu.Unlock()
				return
			}
			t.view.Downloaded = p.Downloaded
			t.view.Size = p.Total
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
	})
	if err != nil {
		if ctx.Err() != nil {
			// The transfer was stopped. Whichever action did that also recorded
			// what it wanted the status to become, and that record is the one to
			// trust: reading the view here can race the setter, and mistaking a
			// pause for a cancel throws away the partial data's ability to
			// resume. Only an unclaimed cancellation — the app shutting down —
			// falls back to "canceled".
			t.mu.Lock()
			if t.view.Status == StatusDownloading {
				if t.stopIntent != "" {
					t.view.Status = t.stopIntent
				} else {
					t.view.Status = StatusCanceled
				}
			}
			v := t.view
			t.mu.Unlock()
			m.app.emit("task:progress", v)
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
	// Name and Dest first; Status flips last, after organizing below, so no
	// observer ever sees "completed" pointing at the pre-organize location.
	t.view.Name = filepath.Base(res.Path)
	t.view.Dest = res.Path
	manual := t.manual
	name := t.view.Name
	t.mu.Unlock()

	// Organize: automatic arrivals with a matching extension move into their
	// group folder now that the final name is known. Manual tasks skip this —
	// the user already placed them. A failed move keeps the file where it
	// landed and only logs: the download itself succeeded.
	// (cfg is the settings snapshot taken above for the engine options.)
	if !manual && cfg.OrganizeEnabled {
		if dst := organizeTarget(cfg.OrganizeGroups,
			cfg.OrganizeDir, name); dst != "" && dst != res.Path {
			if err := moveIntoPlace(res.Path, dst); err != nil {
				m.app.debugLog("organize %s: %v", res.Path, err)
			} else {
				t.mu.Lock()
				if t.generation == gen {
					t.view.Dest = dst
				}
				t.mu.Unlock()
			}
		}
	}

	t.mu.Lock()
	t.view.Status = StatusCompleted
	t.view.Size = res.Size
	t.view.Downloaded = res.Size
	t.view.Speed = 0
	t.view.ETA = 0
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

// moveIntoPlace moves a finished file into its group folder. Source and
// destination always share a volume (one is inside the other's directory),
// so MkdirAll plus Rename is atomic and sufficient — no cross-volume copy
// path is needed.
func moveIntoPlace(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	return os.Rename(src, dst)
}

// fail records a task failure with a message meant for a human.
func (m *Manager) fail(t *task, gen int64, err error) {
	t.mu.Lock()
	if t.generation != gen {
		t.mu.Unlock()
		return
	}
	t.view.Status = StatusFailed
	t.view.Error = humanError(err)
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
func humanError(err error) string {
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
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
	case strings.Contains(msg, "HTTP 416") || strings.Contains(msg, "status 416") || strings.Contains(msg, "(416)"):
		return "服务器无法满足该范围请求（416）· 文件可能已变化"
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
	return msg
}

// Stats reports the aggregate figures the sidebar shows.
//
// "queued" counts only tasks actually waiting for a slot. A paused task is
// deliberately not folded into it: the plan watcher treats "nothing active and
// nothing queued" as "the plan has finished", and a paused task counted as
// queued would keep a plan running forever.
func (m *Manager) Stats() map[string]int64 {
	list := m.List()
	out := map[string]int64{
		"active": 0, "completed": 0, "failed": 0,
		"queued": 0, "paused": 0, "canceled": 0,
		"total": int64(len(list)), "speed": 0,
	}
	for _, v := range list {
		switch v.Status {
		case StatusDownloading:
			out["active"]++
			out["speed"] += v.Speed
		case StatusCompleted:
			out["completed"]++
		case StatusFailed:
			out["failed"]++
		case StatusQueued:
			out["queued"]++
		case StatusPaused:
			out["paused"]++
		case StatusCanceled:
			out["canceled"]++
		}
	}
	return out
}

// saveTasks writes the task index so the list survives a restart.
//
// The engine's sidecar beside each partial file stays the source of truth for
// how many bytes arrived; this index only records what the user asked for.
func (m *Manager) saveTasks() error {
	path := filepath.Join(m.app.dataDir(), "tasks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(m.List(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// loadTasks restores the index written by saveTasks.
//
// Tasks that were mid-transfer when the app closed come back as paused: the
// bytes are still on disk, but nothing is running, so "paused" is the only
// honest status to report.
func (m *Manager) loadTasks() {
	b, err := os.ReadFile(filepath.Join(m.app.dataDir(), "tasks.json"))
	if err != nil {
		return
	}
	var list []TaskView
	if err := json.Unmarshal(b, &list); err != nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range list {
		v := list[i]
		if v.Status == StatusDownloading || v.Status == StatusQueued {
			v.Status = StatusPaused
		}
		v.Speed, v.ETA = 0, 0
		// A completed task whose file has since been moved or deleted must not
		// keep claiming to be complete.
		if v.Status == StatusCompleted {
			if _, statErr := os.Stat(v.Dest); statErr != nil {
				v.Status = StatusFailed
				v.Error = "文件已被移动或删除"
			}
		}
		m.tasks[v.ID] = &task{view: v}
		m.order = append(m.order, v.ID)
	}
}
