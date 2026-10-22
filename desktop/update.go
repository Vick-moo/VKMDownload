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

	// Through the injectable hook, like StartUpdate: one code path means the
	// startup check and the button cannot disagree about where releases come
	// from, and neither needs a real network round trip in a test.
	fetch := a.fetchRelease
	if fetch == nil {
		fetch = fetchLatestRelease
	}
	rel, err := fetch(ctx)
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

	// What was actually verified for the download this status describes. They
	// stay false until the download reaches "ready"; a failure leaves Verified
	// false and the reason in Error. Filled from the marker at startup too, so
	// the settings page can show the result of the last attempt after a
	// restart instead of just "the updater checks hashes".
	//
	// omitempty makes them wire-optional: a caller that builds a status by hand
	// (the settings page's "found an update" placeholder) does not have to
	// repeat six fields it has no verdict for, and absent means false.
	DigestPresent bool `json:"digestPresent,omitempty"`
	SizeMatched   bool `json:"sizeMatched,omitempty"`
	Verified      bool `json:"verified,omitempty"`

	// The restore point: a backup of the version the last update replaced.
	// UpdateBackupInfo is the authoritative answer (it checks the file);
	// these fields are the same facts carried on the polled status. Optional on
	// the wire for the same reason as the verification flags.
	BackupAvailable bool   `json:"backupAvailable,omitempty"`
	BackupVersion   string `json:"backupVersion,omitempty"`
	BackupSize      int64  `json:"backupSize,omitempty"`
}

// UpdateBackup describes the copy of the previous version that the updater
// keeps so the user can go back to it.
type UpdateBackup struct {
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Size      int64  `json:"size"`
	Path      string `json:"path"`
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

	// Carry the existing restore point into the polled status: the settings page
	// shows it while the new download runs.
	bk := a.UpdateBackupInfo()
	a.updMu.Lock()
	a.upd.BackupAvailable = bk.Available
	a.upd.BackupVersion = bk.Version
	a.upd.BackupSize = bk.Size
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
	// Record what was actually checked before the verdict, so a failed update
	// can say which check failed instead of only that it failed. SizeMatched
	// means "a length was known and the bytes matched it"; with no length there
	// is nothing to match, so it stays false rather than guessing.
	digestPresent := rel.digest != ""
	sizeMatched := total > 0 && written == total
	a.setUpdVerification(digestPresent, sizeMatched, false)

	if digestPresent {
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
	// Every check above passed; this is the "verified" the UI reports.
	a.upd.DigestPresent = digestPresent
	a.upd.SizeMatched = sizeMatched
	a.upd.Verified = true
	// Kept for the last pre-swap check, and for telling the next launch which
	// build is which after an update that was never confirmed.
	a.updStagedSHA, a.updStagedSize = sum, written
	a.updMu.Unlock()
}

// setUpdVerification records what the download checks actually saw, before the
// verdict is known. Under the status lock because the poller reads these fields.
func (a *App) setUpdVerification(digestPresent, sizeMatched, verified bool) {
	a.updMu.Lock()
	a.upd.DigestPresent = digestPresent
	a.upd.SizeMatched = sizeMatched
	a.upd.Verified = verified
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
	digestPresent, sizeMatched, verified := a.upd.DigestPresent, a.upd.SizeMatched, a.upd.Verified
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
	// The backup is the restore point from this moment on; the status carries
	// it so a poller sees "可以还原到 X" without a second call.
	bakSize := sys.size(bak)
	a.updMu.Lock()
	a.upd.BackupAvailable = true
	a.upd.BackupVersion = appVersion
	a.upd.BackupSize = bakSize
	a.updMu.Unlock()

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
		// What was verified for this download, and what the restore point is.
		// Written here because this is the first moment a marker exists; the
		// next launch reads them back instead of only being told that the
		// updater "does hash checks".
		DigestPresent:   digestPresent,
		SizeMatched:     sizeMatched,
		Verified:        verified,
		BackupAvailable: true,
		BackupVersion:   appVersion,
		BackupSize:      bakSize,
		Time:            sys.now().Format(time.RFC3339),
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
		// A confirmed update has nothing to report. The marker is kept because
		// it carries the restore point (the backup and the version it holds)
		// the settings page offers until the user deletes it.
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

// UpdateBackupInfo reports whether the previous version can still be restored.
//
// It reads the marker and then the file: a record can outlive its backup (the
// user deleted it, an antivirus removed it), and offering a rollback that cannot
// work is worse than saying there is nothing to restore.
func (a *App) UpdateBackupInfo() UpdateBackup {
	st, err := updateStateLoad(a.updateStatePath())
	if err != nil {
		a.debugLog("update backup: unreadable state: %v", err)
		return UpdateBackup{}
	}
	if st == nil || st.Bak == "" {
		return UpdateBackup{}
	}
	sys := a.syscalls()
	if !sys.exists(st.Bak) {
		return UpdateBackup{}
	}
	size := st.BackupSize
	if size <= 0 {
		// A record written before the size was stored: ask the file.
		size = sys.size(st.Bak)
	}
	return UpdateBackup{
		Available: true,
		Version:   st.BackupVersion,
		Size:      size,
		Path:      st.Bak,
	}
}

// DeleteUpdateBackup removes the backup of the previous version and clears the
// record's reference to it.
//
// It is deliberately the only thing that deletes the backup: after a confirmed
// update the copy is the restore point the settings page offers, so nothing
// sweeps it away automatically. A missing record (or one that names no backup)
// is a clear error the UI can show verbatim; a file that is already gone is not,
// because the end state the user asked for is already true.
func (a *App) DeleteUpdateBackup() error {
	path := a.updateStatePath()
	st, err := updateStateLoad(path)
	if err != nil {
		return fmt.Errorf("更新状态文件损坏，无法删除备份")
	}
	if st == nil || st.Bak == "" {
		return fmt.Errorf("没有可删除的旧版本备份")
	}

	sys := a.syscalls()
	if sys.exists(st.Bak) {
		if rerr := sys.remove(st.Bak); rerr != nil {
			return fmt.Errorf("删除旧版本备份失败: %w", rerr)
		}
	}

	// The file is gone; the record must stop claiming otherwise, or the settings
	// page would keep offering a rollback that cannot work.
	st.Bak = ""
	st.BackupAvailable = false
	st.BackupVersion = ""
	st.BackupSize = 0
	if serr := updateStateSave(path, *st); serr != nil {
		return fmt.Errorf("备份已删除，但更新记录无法写回: %w", serr)
	}

	a.updMu.Lock()
	a.upd.BackupAvailable = false
	a.upd.BackupVersion = ""
	a.upd.BackupSize = 0
	a.updMu.Unlock()
	return nil
}

// hydrateUpdateStatus fills the read-only fields of the polled status from the
// last recorded attempt, so the settings page can show what was verified and
// what backup exists after a restart.
//
// State is deliberately left empty: this process has not started an update, and
// the UI's idle behaviour must not change just because a record exists.
func (a *App) hydrateUpdateStatus() {
	bk := a.UpdateBackupInfo()
	st, err := updateStateLoad(a.updateStatePath())
	if err != nil {
		a.debugLog("update status: unreadable marker: %v", err)
		st = nil
	}

	a.updMu.Lock()
	defer a.updMu.Unlock()
	a.upd.BackupAvailable = bk.Available
	a.upd.BackupVersion = bk.Version
	a.upd.BackupSize = bk.Size
	if st == nil {
		return
	}
	a.upd.DigestPresent = st.DigestPresent
	a.upd.SizeMatched = st.SizeMatched
	a.upd.Verified = st.Verified
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
	// Confirming the record here is what turns the backup into the restore
	// point: this process cannot have its own directory entry edited into
	// place, and the helper it is talking to runs from that backup. Once the
	// stamp is in, older copies are swept — but the backup the record names is
	// kept for the settings page's "还原旧版本".
	//
	// The stamp is evidence, so it is only written by the build the record
	// names. Without this check any executable that happened to be launched
	// with the handshake flag could confirm somebody else's update and make the
	// next start treat a backup of a version that was never replaced as a
	// restore point.
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
	} else {
		// The update is confirmed and this build is the one it names, so the
		// older copies can go now rather than waiting for the next launch. The
		// backup this record names is kept: it is the restore point.
		a.cleanupUpdateArtifacts()
	}
}

// cleanupStaleUpdate is the startup sweep. Every file decision lives in
// cleanupUpdateArtifacts; this name is kept because it is what the startup path
// (and several tests) already call.
func (a *App) cleanupStaleUpdate() {
	a.cleanupUpdateArtifacts()
}

// updateArtifact is one file an update attempt left next to the executable.
type updateArtifact struct {
	path string
	mod  time.Time
}

// cleanupUpdateArtifacts is the single sweep for what update attempts leave in
// the program directory: the backup family (.bak, .bak.*), the failed-build
// copies (.failed-*), the staged download (.new) and handshake files.
//
// It runs at startup and right after an update is confirmed. It never returns an
// error and never aborts halfway: a leftover file is a nuisance, while a startup
// that refuses to run (or a sweep that stops at the first locked file) is not.
//
// The rules are ordered by what must not go wrong:
//
//   - The record's own backup (st.Bak) is never deleted here. It is the restore
//     point the settings page offers and, while an update is settling, the
//     helper's own image. Only DeleteUpdateBackup removes it.
//   - Extra .bak copies go only when the record proves a confirmed update; one
//     backup stays. An unreadable or unfinished record never licenses deleting
//     a possible working copy.
//   - .failed-* copies are rollback evidence: they go when a confirmed newer
//     build is running (all but the newest), or when they are older than a day.
//     A corrupt marker keeps them all.
//   - .new goes when it is older than a day, or when no record names it (an
//     orphan from an attempt that never wrote a marker). A download this process
//     is running is never touched.
//   - Handshake files older than a day go (the existing rule).
func (a *App) cleanupUpdateArtifacts() {
	exe, staged, err := a.executablePaths()()
	if err != nil {
		a.debugLog("update cleanup: cannot locate the executable: %v", err)
		return
	}
	sys := a.syscalls()
	dir := filepath.Dir(exe)
	base := filepath.Base(exe)

	st, serr := updateStateLoad(a.updateStatePath())
	corrupt := serr != nil
	if corrupt {
		// A marker that cannot be parsed says nothing about which copy is the
		// only working one, so every .bak / .failed-* is left alone. Only files
		// that are unambiguously junk (a day-old staged download) still go.
		a.debugLog("update cleanup: state unreadable, keeping every backup: %v", serr)
		st = nil
	}
	confirmed := st != nil && st.Stage == stageConfirmed && confirmedBuildRunning(sys, st)

	var baks, failed []updateArtifact
	if entries, rerr := os.ReadDir(dir); rerr != nil {
		a.debugLog("update cleanup: cannot list %s: %v", dir, rerr)
	} else {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			info, ierr := e.Info()
			if ierr != nil {
				continue
			}
			switch {
			case name == base+".bak" ||
				strings.HasPrefix(name, base+".bak.") ||
				strings.HasPrefix(name, base+".bak-"):
				baks = append(baks, updateArtifact{filepath.Join(dir, name), info.ModTime()})
			case strings.HasPrefix(name, base+".failed-"):
				failed = append(failed, updateArtifact{filepath.Join(dir, name), info.ModTime()})
			}
		}
	}

	// One backup. The record's own copy always survives; when the record names
	// none (or it is gone already) the newest copy is kept instead of deleting
	// every possible way back.
	if st != nil && st.Stage == stageConfirmed {
		keep := st.Bak
		if keep == "" || !sys.exists(keep) {
			keep = newestArtifact(baks).path
		}
		for _, b := range baks {
			if samePathText(b.path, keep) {
				continue
			}
			a.removeUpdateArtifact(sys, b.path, "old update backup")
		}
	}

	// .failed-* copies: rollback evidence, dropped by age or once a confirmed
	// newer build is running. The newest is the last thing to go.
	cutoff := time.Now().Add(-24 * time.Hour)
	newestFailed := newestArtifact(failed).path
	for _, f := range failed {
		if corrupt {
			break
		}
		if f.mod.Before(cutoff) || (confirmed && !samePathText(f.path, newestFailed)) {
			a.removeUpdateArtifact(sys, f.path, "failed-build copy")
		}
	}

	// The staged download: an orphan has no record to belong to, and one that
	// does belong to a record is kept for a retry in the same session, but not
	// forever.
	if !a.updateDownloadInFlight() {
		if info, serr := os.Stat(staged); serr == nil && !info.IsDir() {
			named := st != nil && st.Staged != "" && samePathText(st.Staged, staged)
			old := time.Since(info.ModTime()) >= 24*time.Hour
			if old || (!named && !corrupt) {
				a.removeUpdateArtifact(sys, staged, "staged update")
			}
		}
	}

	a.cleanupHandshakeFiles()
}

// newestArtifact returns the most recently modified artifact, or the zero value
// for an empty list.
func newestArtifact(list []updateArtifact) updateArtifact {
	var best updateArtifact
	for _, a := range list {
		if best.path == "" || a.mod.After(best.mod) {
			best = a
		}
	}
	return best
}

// removeUpdateArtifact deletes one leftover and logs either outcome. A file an
// antivirus still holds is retried on the next start; failing the sweep over it
// would leave every other leftover in place.
func (a *App) removeUpdateArtifact(sys updateSys, path, what string) {
	if err := sys.remove(path); err != nil {
		a.debugLog("update cleanup: cannot remove %s %s: %v", what, path, err)
		return
	}
	a.debugLog("update cleanup: removed %s %s", what, path)
}

// updateDownloadInFlight reports whether this process is writing or holding the
// staged file. The cleanup must not delete the file a downloader has open.
func (a *App) updateDownloadInFlight() bool {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	switch a.upd.State {
	case "downloading", "ready", "applying":
		return true
	}
	return false
}

// confirmedBuildRunning reports whether the record describes a confirmed update
// and the executable on disk is the one that record names. It is the evidence
// that lets the sweep drop rollback copies: without it, "an update succeeded"
// would be a claim nobody checked.
func confirmedBuildRunning(sys updateSys, st *updateState) bool {
	if st.Exe == "" || st.NewSHA == "" {
		return false
	}
	sha, err := sys.sha256File(st.Exe)
	return err == nil && strings.EqualFold(sha, st.NewSHA)
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
