package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"vdownload/internal/engine"
)

// debugLog appends a diagnostic line to the app's log file. It exists so a
// failure that would otherwise be invisible — a panic swallowed by a
// goroutine — leaves a trace the user can hand back.
//
// It writes through the app's data directory rather than computing its own, so
// a test's override applies here too. When it did not, every test run appended
// to the real log and, worse, created the real directory as a side effect —
// which then defeated the migration that moves the folder after a rename.
func (a *App) debugLog(format string, args ...any) {
	dir := a.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "vkmdownload.log"),
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

	// planRunning is set while a plan is working through its tasks.
	planRunning bool
}

func NewManager(app *App) *Manager {
	return &Manager{
		app:   app,
		tasks: make(map[string]*task),
		pump:  make(chan TaskView, 512),
	}
}

// limiterFor returns the limiter shared by every transfer for the given rate.
//
// One limiter for all tasks is the point: a "2 MB/s" setting that each of five
// concurrent downloads obeyed separately would allow 10. The instance is only
// replaced when the setting actually changes, because a new limiter starts with
// a full bucket and would briefly let the speed spike.
func (m *Manager) limiterFor(kilobytesPerSecond int) *engine.Limiter {
	rate := int64(kilobytesPerSecond) * 1024

	m.limMu.Lock()
	defer m.limMu.Unlock()

	// Rebuild when the rate changed, or when switching from "unlimited" (a nil
	// limiter) to a real one.
	if m.limRate != rate || (rate > 0 && m.limiter == nil) {
		m.limiter = engine.NewLimiter(rate)
		m.limRate = rate
	}
	return m.limiter
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
	name := ""
	if fn := strings.TrimSpace(req.Filename); fn != "" {
		dest = filepath.Join(dest, fn)
		name = fn
	}

	conns := req.Conns
	if conns <= 0 {
		conns = cfg.Connections
	}

	t := &task{}
	t.view = TaskView{
		ID:      id,
		URL:     url,
		Dest:    dest,
		Name:    name,
		Status:  StatusQueued,
		Conns:   conns,
		Created: time.Now().Format(time.RFC3339Nano),
	}

	m.mu.Lock()
	m.tasks[id] = t
	m.order = append([]string{id}, m.order...)
	m.mu.Unlock()

	m.app.emit("task:added", snapshot(t))

	if req.Start {
		ctx, cancel := context.WithCancel(m.app.ctx)
		t.mu.Lock()
		t.cancel = cancel
		t.mu.Unlock()
		go m.run(t, ctx, cancel)
	}
	_ = m.saveTasks()
	return snapshot(t), nil
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
		t.mu.Unlock()
		return fmt.Errorf("当前状态无法继续")
	}

	// The cancel function is created here rather than inside the goroutine.
	// Otherwise a pause that arrives while the goroutine is still starting would
	// find the previous run's cancel — or none at all — and the transfer would
	// carry on after the user had stopped it.
	ctx, cancel := context.WithCancel(m.app.ctx)
	t.cancel = cancel
	// A fresh run has no stop to report yet.
	t.stopIntent = ""
	// Marked as running straight away rather than queued. The goroutine may not
	// be scheduled for a moment, and during that window a queued status made a
	// second StartAll pick the same task up again and start it twice.
	t.view.Status = StatusDownloading
	t.view.Error = ""
	t.mu.Unlock()

	go m.run(t, ctx, cancel)
	return nil
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
	t.stopIntent = StatusCanceled
	t.view.Status = StatusCanceled
	t.view.Speed = 0
	t.view.ETA = 0
	v := t.view
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Wait for the transfer goroutine to stop before deleting: cancelling only
	// asks it to stop, and a write in flight would recreate the partial file
	// immediately after it was removed.
	t.waitStopped(3 * time.Second)

	// A cancelled task should start clean, so the partial file and the sidecar
	// that would resume it are both removed.
	_ = os.Remove(dest + engine.PartSuffix)
	_ = os.Remove(dest + engine.StateSuffix)

	m.app.emit("task:progress", v)
	_ = m.saveTasks()
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
	wasComplete := t.view.Status == StatusCompleted
	t.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	// Same reasoning as Cancel: the goroutine may still be writing, so waiting
	// avoids a race between removal and a final flush.
	t.waitStopped(3 * time.Second)

	// Removing a task touches nothing on disk unless the caller asked for it.
	// The partial file and its sidecar are deliberately left too: they are what
	// a later attempt at the same URL resumes from, and the user's instruction
	// was explicit that deleting a row must not disturb any file.
	if deleteFiles {
		_ = os.Remove(dest + engine.PartSuffix)
		_ = os.Remove(dest + engine.StateSuffix)
		// Only delete the published file when the task actually produced one,
		// so a failed task cannot remove an unrelated file sharing the name.
		if wasComplete {
			_ = os.Remove(dest)
		}
	}

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
	return nil
}

// ------------------------------------------------------------- bulk verbs

// StartAll resumes every task that is waiting to run.
//
// Tasks already transferring are left alone, as are completed ones: "start all"
// means "get everything going", not "start everything over".
func (m *Manager) StartAll() int {
	n := 0
	for _, v := range m.List() {
		switch v.Status {
		case StatusPaused, StatusFailed, StatusCanceled, StatusQueued:
			if err := m.Resume(v.ID); err == nil {
				n++
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

	started := m.StartAll()
	if started == 0 {
		m.mu.Lock()
		m.planRunning = false
		m.mu.Unlock()
		return 0, fmt.Errorf("没有等待下载的任务")
	}

	go m.watchPlan()
	return started, nil
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
// ctx and cancel come from the caller: Resume creates them before launching the
// goroutine so that a pause arriving immediately still cancels this run.
//
// A panic in a task goroutine would otherwise take the whole app down without a
// word, which is exactly the kind of failure a user can only report as "it
// crashed". The recover turns it into a failed task plus a log line, so the
// cause survives.
func (m *Manager) run(t *task, ctx context.Context, cancel context.CancelFunc) {
	t.mu.Lock()
	t.generation++
	gen := t.generation
	t.view.Status = StatusDownloading
	t.view.Error = ""
	t.view.Speed = 0
	t.view.ETA = 0
	url, dest, conns := t.view.URL, t.view.Dest, t.view.Conns
	done := make(chan struct{})
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
	if cfg.Proxy != "" {
		opt.Transport.ProxyURL = cfg.Proxy
	}

	dl, err := engine.New(opt)
	if err != nil {
		m.fail(t, gen, err)
		return
	}
	defer dl.Close()

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
	t.view.Status = StatusCompleted
	t.view.Name = filepath.Base(res.Path)
	t.view.Dest = res.Path
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
	switch {
	case errors.Is(err, engine.ErrNotAFile):
		// The most common cause by far is pasting the page a file is shown on
		// rather than the file itself, so the message names the fix.
		return "这是网页地址，不是文件下载地址 · 请改用文件的直链（例如网盘的「直接下载」链接）"
	case errors.Is(err, engine.ErrSizeMismatch):
		return "文件长度与服务器声明不符 · 已阻止发布，避免留下损坏的文件"
	case strings.Contains(msg, "403"):
		return "服务器拒绝了请求（403）· 可能需要登录，或该链接已失效"
	case strings.Contains(msg, "404"):
		return "链接不存在（404）· 请检查地址是否写错"
	case strings.Contains(msg, "401"):
		return "需要登录（401）· 该资源要求身份验证"
	case strings.Contains(msg, "416"):
		return "服务器无法满足该范围请求（416）· 文件可能已变化"
	case strings.Contains(msg, "no such host"),
		strings.Contains(msg, "dial tcp"),
		strings.Contains(msg, "connection refused"):
		return "无法连接到服务器 · 请检查网络或链接是否有效"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "TLS 证书校验失败 · 站点证书有问题，或被中间环节拦截"
	case strings.Contains(msg, "announced size"):
		return "文件长度与服务器声明不符 · 已阻止发布，避免留下损坏的文件"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		return "连接超时 · 服务器响应过慢或网络不稳定"
	case strings.Contains(msg, "no space"):
		return "磁盘空间不足 · 请清理后重试"
	case strings.Contains(msg, "access is denied"), strings.Contains(msg, "permission denied"):
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
		"queued": 0, "paused": 0, "total": int64(len(list)), "speed": 0,
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
