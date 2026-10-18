package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// updateRepo is the single place the app looks for new releases.
const updateRepo = "Vick-moo/VKMDownload"

// UpdateInfo describes the newest published release compared to this build.
type UpdateInfo struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	URL       string `json:"url"`
	Current   string `json:"current"`
}

// isNewerVersion reports whether tag (e.g. "v0.2.3") is newer than current
// (e.g. "0.2.2"). It compares dot-separated numeric parts only, so a tag the
// project never produced can never read as newer by accident.
func isNewerVersion(current, tag string) bool {
	num := func(s string) []int {
		s = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V"))
		var out []int
		for _, p := range strings.Split(s, ".") {
			digits := ""
			for _, r := range p {
				if r < '0' || r > '9' {
					break
				}
				digits += string(r)
			}
			if digits == "" {
				return nil
			}
			n, err := strconv.Atoi(digits)
			if err != nil {
				return nil
			}
			out = append(out, n)
		}
		return out
	}

	a, b := num(current), num(tag)
	if a == nil || b == nil {
		return false
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		if b[i] != a[i] {
			return b[i] > a[i]
		}
	}
	return len(b) > len(a)
}

// CheckUpdate asks GitHub for the latest release. It is a binding so the
// settings page can offer a manual "check" alongside the automatic one at
// startup. A missing network is not an error worth showing: it just reports
// no update.
func (a *App) CheckUpdate() (UpdateInfo, error) {
	info := UpdateInfo{Current: appVersion}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	rel, err := fetchLatestRelease(ctx)
	if err != nil {
		return info, err
	}
	if isNewerVersion(appVersion, rel.tag) {
		info.Available = true
		info.Version = strings.TrimPrefix(strings.TrimPrefix(rel.tag, "v"), "V")
		info.URL = rel.pageURL
	}
	return info, nil
}

// checkUpdateOnce runs at startup and announces a newer release, if any.
// Silence on failure: an offline machine must start exactly as fast as an
// online one, and the settings page offers a manual check anyway.
func (a *App) checkUpdateOnce() {
	info, err := a.CheckUpdate()
	if err != nil || !info.Available {
		return
	}
	// The payload mirrors UpdateInfo: the front-end gates the banner on
	// "available", and an event without it would be received yet never shown.
	a.emit("update:available", map[string]any{
		"available": true,
		"version":   info.Version,
		"url":       info.URL,
		"current":   info.Current,
	})
}

// OpenReleasePage opens the release page in the system browser. The URL is
// parsed and every component checked — not string-prefix matched — because a
// prefix test lets "releases/../../owner/repo" through: scheme and host are
// pinned, the path must sit under this project's releases, and ".." segments
// are refused outright.
func (a *App) OpenReleasePage(raw string) error {
	pu, err := url.Parse(strings.TrimSpace(raw))
	if err != nil ||
		pu.Scheme != "https" || pu.Host != "github.com" ||
		!strings.HasPrefix(pu.Path, "/"+updateRepo+"/releases/") ||
		strings.Contains(pu.Path, "..") {
		return fmt.Errorf("只允许打开本项目的发布页")
	}
	runtime.BrowserOpenURL(a.ctx, raw)
	return nil
}

// ReleaseNotes fetches the published notes for one version straight from the
// GitHub release, so the "what's new" dialog answers from the same source the
// update itself comes from — a release that ships without a notes file simply
// shows the fallback text, never an error page. The version is digits and
// dots only (validated below): it reaches a URL, so anything else is refused
// outright. The body is the release's markdown, rendered as text by the
// front-end.
func (a *App) ReleaseNotes(version string) (string, error) {
	v := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(version, "v"), "V"))
	if !safeVersion(v) {
		return "", fmt.Errorf("版本号不合法")
	}

	a.updMu.Lock()
	if a.notesVersion == v && a.notesBody != "" {
		body := a.notesBody
		a.updMu.Unlock()
		return body, nil
	}
	a.updMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+updateRepo+"/releases/tags/v"+v, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "VKMDownload/"+appVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("发布说明拉取失败：HTTP %d", resp.StatusCode)
	}

	var rel struct {
		Body string `json:"body"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", err
	}

	a.updMu.Lock()
	a.notesVersion, a.notesBody = v, rel.Body
	a.updMu.Unlock()
	return rel.Body, nil
}

// safeVersion reports whether v is a plain dotted number like 0.2.6 — the
// only shape allowed into a GitHub API URL.
func safeVersion(v string) bool {
	if v == "" {
		return false
	}
	for _, r := range v {
		if (r < '0' || r > '9') && r != '.' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- self-update

// UpdateStatus is the self-update state machine the front-end polls.
type UpdateStatus struct {
	// State is one of idle, downloading, ready, applying, failed. "applying"
	// is the window between a click on 立即更新 and the process exiting; it
	// exists so a second click cannot start a second helper.
	State      string `json:"state"`
	Version    string `json:"version"`
	Downloaded int64  `json:"downloaded"`
	Total      int64  `json:"total"`
	Error      string `json:"error"`
}

// releaseAsset is what the updater needs from the latest release.
type releaseAsset struct {
	tag     string
	pageURL string
	exeURL  string
	digest  string // "sha256:..." or "" when the API did not report one
}

// fetchLatestRelease reads the newest published release and locates the
// executable asset in it.
func fetchLatestRelease(ctx context.Context) (releaseAsset, error) {
	var out releaseAsset

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"https://api.github.com/repos/"+updateRepo+"/releases/latest", nil)
	if err != nil {
		return out, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// An explicit user agent instead of Go's default. A firewall log showing
	// "Go-http-client/1.1" reaching GitHub looks like something hiding; this says
	// which program it is and which build.
	req.Header.Set("User-Agent", "VKMDownload/"+appVersion)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return out, fmt.Errorf("releases API answered %d", resp.StatusCode)
	}

	var rel struct {
		TagName string `json:"tag_name"`
		HTMLURL string `json:"html_url"`
		Assets  []struct {
			Name   string `json:"name"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	// Bounded like every other remote body this app reads: a hijacked or
	// broken connection must not be able to stream unbounded bytes into
	// memory through the decoder.
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return out, err
	}
	out.tag, out.pageURL = rel.TagName, rel.HTMLURL
	for _, as := range rel.Assets {
		if as.Name == "VKMDownload.exe" {
			out.exeURL, out.digest = as.URL, as.Digest
		}
	}
	if out.exeURL == "" {
		return out, fmt.Errorf("最新版本里没有找到程序文件")
	}
	return out, nil
}

// stagedUpdatePath is where the downloaded replacement waits. It sits next to
// the running executable so the final move never crosses volumes.
func stagedUpdatePath() (exe, staged string, err error) {
	exe, err = os.Executable()
	if err != nil {
		return "", "", err
	}
	return exe, exe + ".new", nil
}

// StartUpdate downloads the newest executable in the background. Staging is
// safe at any moment: it uses its own plain HTTP client, shares nothing with
// the download engine, and touches neither the running file nor any task.
// Applying (ApplyUpdate) is what pauses everything first.
func (a *App) StartUpdate() (UpdateStatus, error) {
	// The claim and the check happen in one critical section. They used to be
	// separated by the whole network round trip, so two clicks in the same
	// instant both saw "not downloading", both fetched the release and both
	// opened the same staged file with O_TRUNC — interleaving two downloads
	// into one file that then failed every checksum, permanently, until the
	// app was restarted.
	a.updMu.Lock()
	switch a.upd.State {
	case "downloading":
		st := a.upd
		a.updMu.Unlock()
		return st, nil
	case "applying":
		// The executable is being replaced right now. A download started here
		// would write the staged file the helper is about to install.
		st := a.upd
		a.updMu.Unlock()
		return st, fmt.Errorf("更新正在进行中，请稍候")
	}
	a.upd = UpdateStatus{State: "downloading"}
	// Whatever was verified before belongs to the previous attempt.
	a.updStagedSHA, a.updStagedSize = "", 0
	a.updMu.Unlock()

	failAndClear := func(err error) (UpdateStatus, error) {
		return a.updateFailed(err), err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	fetch := a.fetchRelease
	if fetch == nil {
		fetch = fetchLatestRelease
	}
	rel, err := fetch(ctx)
	if err != nil {
		return failAndClear(err)
	}
	if !isNewerVersion(appVersion, rel.tag) {
		// "Already current" is not a failure. Recording it as failed painted a
		// red error row and a retry button for the normal, expected answer.
		a.updMu.Lock()
		a.upd = UpdateStatus{State: "current", Version: strings.TrimPrefix(strings.TrimPrefix(rel.tag, "v"), "V")}
		st := a.upd
		a.updMu.Unlock()
		return st, fmt.Errorf("已是最新版本")
	}

	_, staged, err := stagedUpdatePath()
	if err != nil {
		return failAndClear(err)
	}

	a.updMu.Lock()
	a.upd = UpdateStatus{State: "downloading", Version: strings.TrimPrefix(strings.TrimPrefix(rel.tag, "v"), "V")}
	st := a.upd
	a.updMu.Unlock()

	go a.downloadUpdate(rel, staged)
	return st, nil
}

// UpdateProgress reports the self-update state for the front-end poller.
func (a *App) UpdateProgress() UpdateStatus {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	return a.upd
}

func (a *App) updateFailed(err error) UpdateStatus {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	a.upd = UpdateStatus{State: "failed", Error: err.Error()}
	return a.upd
}

// downloadUpdate streams the replacement executable to disk, verifying its
// SHA-256 against the digest the releases API reported. A mismatch fails the
// update rather than staging a file the updater must not trust.
func (a *App) downloadUpdate(rel releaseAsset, staged string) {
	fail := func(err error) {
		a.updMu.Lock()
		a.upd.State = "failed"
		a.upd.Error = err.Error()
		a.updMu.Unlock()
	}

	// A hard ceiling on the whole transfer. The plain client has no read
	// timeout, so a server that stalls mid-stream would otherwise park the
	// updater in "downloading" forever — and StartUpdate refuses to reenter
	// in that state, so the user could not even retry without restarting
	// the app. Ten minutes is far more than an exe this size ever needs.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rel.exeURL, nil)
	if err != nil {
		fail(err)
		return
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fail(fmt.Errorf("下载新版失败：HTTP %d", resp.StatusCode))
		return
	}

	out, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o755)
	if err != nil {
		fail(err)
		return
	}
	// Every exit below has to release this handle. The success path used to
	// return with it still open, and the failure paths called os.Remove on a
	// file they were still writing: on Windows that delete fails with a sharing
	// violation, the error was discarded with `_ =`, and "the bad file was
	// discarded" became a lie — the file stayed, and the next attempt tripped
	// over it.
	closed := false
	closeOut := func() error {
		if closed {
			return nil
		}
		closed = true
		return out.Close()
	}
	defer closeOut()
	discardStaged := func() {
		if cerr := closeOut(); cerr != nil {
			a.debugLog("closing the staged update file failed: %v", cerr)
		}
		if rerr := os.Remove(staged); rerr != nil && !os.IsNotExist(rerr) {
			a.debugLog("cannot discard the staged update file: %v", rerr)
		}
	}

	h := sha256.New()
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				discardStaged()
				fail(werr)
				return
			}
			h.Write(buf[:n])
			written += int64(n)
			a.updMu.Lock()
			a.upd.Downloaded = written
			a.upd.Total = total
			a.updMu.Unlock()
		}
		if rerr != nil {
			if rerr != io.EOF {
				discardStaged()
				fail(rerr)
				return
			}
			break
		}
	}

	// Flushed before anything trusts the file: the checks below read it back,
	// and the helper re-reads it minutes later in another process.
	if serr := out.Sync(); serr != nil {
		discardStaged()
		fail(serr)
		return
	}
	if cerr := closeOut(); cerr != nil {
		discardStaged()
		fail(cerr)
		return
	}

	// The digest is the one check that ties the bytes on disk to the release
	// GitHub published. When the release carries none there is nothing to
	// compare against, so the local checks below have to carry the weight —
	// and the gap is written to the log rather than silently accepted.
	sum := hex.EncodeToString(h.Sum(nil))
	if rel.digest != "" {
		want := strings.TrimPrefix(rel.digest, "sha256:")
		if !strings.EqualFold(sum, want) {
			discardStaged()
			fail(fmt.Errorf("新版文件校验失败，已丢弃"))
			return
		}
	} else if total <= 0 {
		// No digest *and* no length: nothing at all ties the bytes on disk to
		// what the release published. A truncated download would satisfy the
		// remaining checks whenever its first megabyte survived, and the result
		// would be installed as the program.
		discardStaged()
		fail(fmt.Errorf("服务器没有提供文件长度，无法确认下载完整，已放弃本次更新"))
		return
	} else {
		a.debugLog("release %s reported no digest; relying on length and header checks", rel.tag)
	}

	// A truncated transfer that still returns 200 is the classic silent
	// failure here: the file exists, is large, and is not the program.
	if total > 0 && written != total {
		discardStaged()
		fail(fmt.Errorf("下载不完整：收到 %d 字节，应为 %d", written, total))
		return
	}
	if written < 1<<20 {
		discardStaged()
		fail(fmt.Errorf("下载到的新版文件过小（%d 字节）", written))
		return
	}
	if magic, err := readPrefix(staged, 2); err != nil || string(magic) != "MZ" {
		discardStaged()
		fail(fmt.Errorf("下载到的文件不是可执行文件"))
		return
	}

	a.updMu.Lock()
	a.upd.State = "ready"
	a.upd.Downloaded = written
	a.upd.Total = written
	// Kept for the last pre-swap check, and for telling the next launch which
	// build is which after an update that was never confirmed.
	a.updStagedSHA, a.updStagedSize = sum, written
	a.updMu.Unlock()
}

// ------------------------------------------------------- injected plumbing

// syscalls is the update path's view of the operating system. Tests replace it
// with a fake so no test can touch a real executable.
func (a *App) syscalls() updateSys {
	if a.sys == nil {
		return realUpdateSys{}
	}
	return a.sys
}

// quitApp is runtime.Quit, injectable for the same reason emit and hide are:
// tests must be able to observe the "quit was requested and refused" path.
func (a *App) quitApp() func(context.Context) {
	if a.quit == nil {
		return runtime.Quit
	}
	return a.quit
}

func (a *App) pauseFor(d time.Duration) {
	if a.sleepFor == nil {
		time.Sleep(d)
		return
	}
	a.sleepFor(d)
}

func (a *App) executablePaths() func() (string, string, error) {
	if a.exePaths == nil {
		return stagedUpdatePath
	}
	return a.exePaths
}

// runningExe is this process's own image. Injectable for the same reason
// executablePaths is: a test cannot make os.Executable describe a fixture, and
// the update confirmation has to be checked against a file a test can write.
func (a *App) runningExe() (string, error) {
	if a.selfPath == nil {
		return os.Executable()
	}
	return a.selfPath()
}

// updateStatePath is the marker the helper and the next launch share. It sits
// next to tasks.json in the data directory, never next to the executable: an
// install under Program Files is not writable for the user running the app.
func (a *App) updateStatePath() string {
	return filepath.Join(a.dataDir(), "update-state.json")
}

// updateDir holds the handshake files the new version drops for the helper.
func (a *App) updateDir() string { return filepath.Join(a.dataDir(), "update") }

// ------------------------------------------------------------ applying

// freezeForUpdate stops scheduling and pauses every transfer, so the executable
// is swapped under a still machine. Pausing rather than killing is what makes
// updating mid-download safe: partial files and sidecars stay on disk, and after
// the restart every task loads as paused, ready to resume.
func (a *App) freezeForUpdate() (func(), error) {
	a.mgr.mu.Lock()
	a.mgr.updating = true
	a.mgr.mu.Unlock()

	// What was transferring before the freeze. If the update is abandoned the
	// user expects those downloads to carry on: before this they came back
	// paused, which looks like the app quietly stopped their work.
	var wasRunning []string
	for _, v := range a.mgr.List() {
		if v.Status == StatusDownloading || v.Status == StatusQueued {
			wasRunning = append(wasRunning, v.ID)
		}
	}

	unfreeze := func() {
		a.mgr.mu.Lock()
		a.mgr.updating = false
		a.mgr.mu.Unlock()
		if len(wasRunning) == 0 {
			return
		}
		a.mgr.batch(func() {
			for _, id := range wasRunning {
				if v, ok := a.mgr.view(id); ok && v.Status == StatusPaused {
					_ = a.mgr.Resume(id)
				}
			}
		})
	}

	a.mgr.StopAll()
	deadline := time.Now().Add(30 * time.Second)
	for a.mgr.activeCount() > 0 {
		if time.Now().After(deadline) {
			unfreeze()
			return unfreeze, fmt.Errorf("有任务停不下来，已取消更新")
		}
		time.Sleep(200 * time.Millisecond)
	}
	return unfreeze, nil
}

// preflightExeDir proves the program directory is writable and has room for one
// more copy of the executable *before* anything is started. Failing here costs
// the user a click; failing after the helper was launched costs a restart with a
// frozen task list.
func preflightExeDir(exe string, need int64) error {
	// The updater appends ".bak", ".new" and ".failed-<stamp>" to this path,
	// and the Win32 file APIs used for the swap reject anything past MAX_PATH
	// unless it is explicitly long-path prefixed. Refusing here names the cause
	// and suggests the fix; discovering it after the process has quit leaves
	// the user with no running program and no explanation.
	const maxPlainPath = 259
	if len(exe)+len(".failed-20060102-150405") > maxPlainPath {
		return fmt.Errorf("程序路径太长（%d 个字符），更新需要在其后追加备份名；请把程序移到更短的目录（例如 C:\\VKMDownload\\）后再更新", len(exe))
	}
	dir := filepath.Dir(exe)
	f, err := os.CreateTemp(dir, ".vkmdownload-writable-*")
	if err != nil {
		return fmt.Errorf("程序目录不可写，请把程序移到可写目录或用管理员权限运行：%w", err)
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)

	if need > 0 {
		if free := freeSpaceBytes(dir); free > 0 && free < need+need/2 {
			return fmt.Errorf("磁盘空间不足：更新需要约 %d MB", (need+need/2)/(1<<20))
		}
	}
	return nil
}

// ApplyUpdate pauses every transfer, waits for them to settle, then hands off to
// a helper that swaps the executable and starts the new build.
//
// The helper is this same executable, started from a backup copy of itself
// (exe.bak). It has to be the backup: Windows lets a running image be renamed
// but never overwritten, so the process that performs the swap must not be the
// file being swapped.
func (a *App) ApplyUpdate() error {
	a.updMu.Lock()
	if a.upd.State != "ready" {
		state := a.upd.State
		a.updMu.Unlock()
		if state == "applying" {
			return fmt.Errorf("更新正在进行中")
		}
		return fmt.Errorf("新版还没下载好")
	}
	// ready -> applying inside the same lock the poller reads: the front-end
	// disables its button, but nothing stops a second binding call.
	a.upd.State = "applying"
	wantSize, wantSHA, version := a.updStagedSize, a.updStagedSHA, a.upd.Version
	a.updMu.Unlock()

	fail := func(err error) error {
		a.updMu.Lock()
		a.upd.State = "failed"
		a.upd.Error = err.Error()
		a.updMu.Unlock()
		return err
	}

	unfreeze := func() {}
	sys := a.syscalls()
	exe, staged, err := a.executablePaths()()
	if err != nil {
		return fail(err)
	}
	// Everything that can be checked without touching the running machine is
	// checked first: a refusal here costs the user a click, while a refusal
	// after the freeze costs them a paused download list.
	if err := validateStaged(sys, staged, wantSize, wantSHA); err != nil {
		return fail(err)
	}
	if err := preflightExeDir(exe, sys.size(staged)); err != nil {
		return fail(err)
	}

	unfreeze, err = a.freezeForUpdate()
	if err != nil {
		return fail(err)
	}

	bak := exe + ".bak"
	if err := backupExe(sys, exe, bak); err != nil {
		unfreeze()
		return fail(err)
	}

	args := helperArgs{
		Mode:   modeApply,
		Exe:    exe,
		Bak:    bak,
		Staged: staged,
		State:  a.updateStatePath(),
		OkDir:  a.updateDir(),
		Token:  newUpdateToken(),
		Parent: os.Getpid(),
	}
	oldSHA, _ := sys.sha256File(bak)
	newSHA, _ := sys.sha256File(staged)
	st := updateState{
		Stage:   stagePrepared,
		Version: version,
		Exe:     exe,
		Bak:     bak,
		Staged:  staged,
		OldSHA:  oldSHA,
		NewSHA:  newSHA,
		NewSize: wantSize,
		Time:    sys.now().Format(time.RFC3339),
	}
	if err := updateStateSave(args.State, st); err != nil {
		unfreeze()
		return fail(fmt.Errorf("写入更新状态失败：%w", err))
	}
	markStage(sys, args.State, stageBackedUp, "")

	if err := a.handoffToHelper(args, unfreeze); err != nil {
		return fail(err)
	}
	return nil
}

// handoffToHelper starts the helper and then gets this process out of the way.
//
// requestQuit first, then runtime.Quit: Wails' Windows Frontend.Quit consults
// OnBeforeClose before doing anything, and with "minimise to tray" on that hook
// hides the window and vetoes the quit. Without the explicit intent recorded
// here the process survives, the helper waits out its full timeout and the
// update is frozen in "updating" forever — which is exactly what was reported
// as "the update did nothing and the app had to be killed".
//
// The loop after the quit is a watchdog: on success this process is gone before
// the first tick. If it is still here the quit was refused, and the caller must
// be told rather than left with a frozen scheduler.
func (a *App) handoffToHelper(args helperArgs, unfreeze func()) error {
	sys := a.syscalls()
	pid, err := sys.startProcess(args.Bak, args.argv()...)
	if err != nil {
		if unfreeze != nil {
			unfreeze()
		}
		return fmt.Errorf("启动更新助手失败：%w", err)
	}
	a.debugLog("update helper started pid=%d mode=%s", pid, args.Mode)

	_ = a.mgr.saveTasks()
	a.requestQuit()
	a.quitApp()(a.ctx)

	const quitWatchdog = 10 * time.Second
	for waited := time.Duration(0); waited < quitWatchdog; waited += 500 * time.Millisecond {
		a.pauseFor(500 * time.Millisecond)
	}
	if unfreeze != nil {
		unfreeze()
	}
	a.debugLog("quit was refused; no update was started")
	return fmt.Errorf("程序没有退出，更新未开始")
}

// ------------------------------------------------------- the next launch

// ConsumeUpdateNotice reports what became of the last update attempt, once.
//
// The front-end pulls this instead of being told by an event: Wails buffers no
// events, so anything emitted from startup is gone before a listener exists.
// Handing it out only once is what keeps a repeated page load from showing a
// stale warning.
//
// Apart from the "already told the user" flag, this touches nothing. An update
// that was never confirmed must not be undone automatically: reaching this code
// at all means an executable started, and replacing it with the backup because a
// handshake file went missing would turn a working new version into a downgrade.
func (a *App) ConsumeUpdateNotice() string {
	a.updMu.Lock()
	defer a.updMu.Unlock()

	path := a.updateStatePath()
	st, err := updateStateLoad(path)
	if err != nil {
		// A marker that cannot be parsed is not a licence to delete anything;
		// the backup may be the only working copy on this machine.
		a.debugLog("update state unreadable (%v); leaving every file in place", err)
		return ""
	}
	if st == nil || st.Notified {
		return ""
	}
	msg := a.updateNotice(st)
	if msg == "" {
		return ""
	}
	st.Notified = true
	if err := updateStateSave(path, *st); err != nil {
		a.debugLog("cannot mark the update notice as shown: %v", err)
	}
	return msg
}

// updateNotice turns a marker into one sentence for the user, using the hash of
// the executable that is actually running to decide which of the two builds this
// is.
func (a *App) updateNotice(st *updateState) string {
	current := ""
	if st.Exe != "" {
		if sha, err := a.syscalls().sha256File(st.Exe); err == nil {
			current = sha
		}
	}
	runningNew := st.NewSHA != "" && strings.EqualFold(current, st.NewSHA)

	switch st.Stage {
	case stageRolledBack:
		reason := st.Reason
		if reason == "" {
			reason = "新版没有正常启动"
		}
		return fmt.Sprintf("上次更新失败，已自动恢复到旧版本（%s）。", reason)

	case stageAborted:
		msg := "上次更新没有完成，程序仍是旧版本。"
		if st.Reason != "" {
			msg += "（" + st.Reason + "）"
		}
		return msg

	case stagePrepared, stageBackedUp:
		return "上次更新没有开始，程序仍是旧版本。请重新检查更新。"

	case stageReplaced, stageLaunched:
		if runningNew {
			// The new build is demonstrably running; only the handshake went
			// missing. Say so, keep the backup and offer the manual restore
			// instead of guessing.
			return fmt.Sprintf("新版 %s 已在运行，但更新没有最终确认。旧版本备份仍保留在 %s，可在设置中手动还原。",
				st.Version, st.Bak)
		}
		return "上次更新没有生效，程序仍是旧版本，请重新下载更新。"

	case stageConfirmed:
		// A confirmed update has nothing to report; the marker is only kept so
		// the next start can clean up after it.
		return ""

	default:
		return "更新状态未知，未改动任何文件。"
	}
}

// RollBackUpdate puts the backup of an unconfirmed update back in place.
//
// It needs the same hand-off as an update: the running executable cannot be
// replaced under itself, so a helper started from the backup does the swap.
func (a *App) RollBackUpdate() error {
	path := a.updateStatePath()
	st, err := updateStateLoad(path)
	if err != nil {
		return fmt.Errorf("更新状态文件损坏，无法还原")
	}
	if st == nil {
		return fmt.Errorf("没有可还原的更新记录")
	}
	exe, _, err := a.executablePaths()()
	if err != nil {
		return err
	}
	if st.Bak == "" || !a.syscalls().exists(st.Bak) {
		return fmt.Errorf("旧版本备份已不在，无法还原")
	}

	unfreeze, err := a.freezeForUpdate()
	if err != nil {
		return err
	}
	args := helperArgs{
		Mode:   modeRollback,
		Exe:    exe,
		Bak:    st.Bak,
		State:  path,
		Parent: os.Getpid(),
	}
	return a.handoffToHelper(args, unfreeze)
}

// writeUpdateHandshake tells the helper this build is up and running.
//
// It runs at domReady rather than startup: the window existing is the evidence
// the helper needs, and startup emits are dropped by Wails anyway. The file
// names this pid, so a stale handshake from an earlier attempt can never pass
// for this one's.
func (a *App) writeUpdateHandshake() {
	tok := a.updateToken
	if !safeUpdateToken(tok) {
		return
	}
	path := filepath.Join(a.updateDir(), tok+".ok")
	if err := writeHandshake(path, os.Getpid()); err != nil {
		a.debugLog("cannot write the update handshake: %v", err)
		return
	}
	a.debugLog("update handshake written: %s", path)
	// Confirming the record here is what lets the *next* start clean up the
	// backup: this process cannot have its own directory entry edited into
	// place, and the helper it is talking to runs from that backup.
	//
	// The stamp is evidence, so it is only written by the build the record
	// names. Without this check any executable that happened to be launched
	// with the handshake flag confirmed somebody else's update, and the next
	// start then deleted the backup of a version that was never replaced.
	sys := a.syscalls()
	st, err := updateStateLoad(a.updateStatePath())
	if err != nil || st == nil {
		return
	}
	exe, eerr := a.runningExe()
	if eerr != nil {
		a.debugLog("cannot identify the running executable: %v", eerr)
		return
	}
	if st.Exe != "" && !samePathText(exe, st.Exe) {
		a.debugLog("not confirming the update record: running %s, record names %s", exe, st.Exe)
		return
	}
	if st.NewSHA == "" {
		a.debugLog("not confirming the update record: it carries no hash for the new build")
		return
	}
	if sha, herr := sys.sha256File(exe); herr != nil || !strings.EqualFold(sha, st.NewSHA) {
		a.debugLog("not confirming the update record: the running image is not the one it names")
		return
	}
	st.Stage = stageConfirmed
	st.Time = time.Now().Format(time.RFC3339)
	if serr := updateStateSave(a.updateStatePath(), *st); serr != nil {
		a.debugLog("cannot confirm the update record: %v", serr)
	}
}

// cleanupStaleUpdate removes what a finished update leaves behind: the backup
// copy of the previous version and the marker. It only acts when the record
// says the update was confirmed *and* the running executable is the one that
// record describes — the backup is the only other copy of a working program, so
// a wrong deletion here cannot be undone.
func (a *App) cleanupStaleUpdate() {
	path := a.updateStatePath()
	st, err := updateStateLoad(path)
	if err != nil {
		a.debugLog("update state unreadable: %v", err)
	} else if st != nil && st.Stage == stageConfirmed && st.Bak != "" {
		sys := a.syscalls()
		// A confirmed record with no hash proves nothing about which build is
		// running, and the backup may be the only copy of a working version.
		// The old code treated "no hash" as "matches", deleted the backup on
		// that basis, and dropped the marker whether or not the delete worked.
		if st.NewSHA == "" {
			a.debugLog("keeping the update backup: the record carries no hash to verify it against")
		} else if sha, herr := sys.sha256File(st.Exe); herr != nil || !strings.EqualFold(sha, st.NewSHA) {
			a.debugLog("keeping the update backup: the running image is not the one the record names")
		} else if rerr := sys.remove(st.Bak); rerr != nil {
			// The marker stays: it is the only thing that still says a backup
			// exists and is safe to remove later.
			a.debugLog("cannot remove the update backup %s: %v", st.Bak, rerr)
		} else {
			a.debugLog("removed the update backup %s", st.Bak)
			if merr := sys.remove(path); merr != nil {
				a.debugLog("cannot remove the update record: %v", merr)
			}
		}
	} else if st != nil && st.Stage != stageConfirmed {
		// An update that never reached "confirmed" leaves its staged file
		// behind. It is another full copy of the program and nothing will ever
		// use it once the helper is gone, so it is swept after a day — long
		// enough that a retry in the same session still finds it.
		a.cleanupStaleStaged()
	}
	a.cleanupHandshakeFiles()
}

// cleanupStaleStaged removes a downloaded replacement that no longer belongs to
// an update in progress. Only files older than a day go: a retry within the same
// session must still be able to reuse the download.
func (a *App) cleanupStaleStaged() {
	_, staged, err := a.executablePaths()()
	if err != nil {
		return
	}
	info, serr := os.Stat(staged)
	if serr != nil || time.Since(info.ModTime()) < 24*time.Hour {
		return
	}
	if rerr := os.Remove(staged); rerr != nil {
		a.debugLog("cannot remove the stale staged update %s: %v", staged, rerr)
	} else {
		a.debugLog("removed the stale staged update %s", staged)
	}
}

// cleanupHandshakeFiles drops handshake files left by earlier attempts. The one
// just written is minutes old at worst, so only old files go.
func (a *App) cleanupHandshakeFiles() {
	entries, err := os.ReadDir(a.updateDir())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-24 * time.Hour)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		_ = os.Remove(filepath.Join(a.updateDir(), e.Name()))
	}
}
