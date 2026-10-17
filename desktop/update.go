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
	"os/exec"
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
	// State is one of idle, downloading, ready, failed.
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
	a.updMu.Lock()
	if a.upd.State == "downloading" {
		st := a.upd
		a.updMu.Unlock()
		return st, nil
	}
	a.updMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rel, err := fetchLatestRelease(ctx)
	if err != nil {
		return a.updateFailed(err), err
	}
	if !isNewerVersion(appVersion, rel.tag) {
		err := fmt.Errorf("已是最新版本")
		return a.updateFailed(err), err
	}

	_, staged, err := stagedUpdatePath()
	if err != nil {
		return a.updateFailed(err), err
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

	h := sha256.New()
	total := resp.ContentLength
	var written int64
	buf := make([]byte, 128<<10)
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				_ = out.Close()
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
			_ = out.Close()
			if rerr != io.EOF {
				fail(rerr)
				return
			}
			break
		}
	}

	if rel.digest != "" {
		want := strings.TrimPrefix(rel.digest, "sha256:")
		if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, want) {
			_ = os.Remove(staged)
			fail(fmt.Errorf("新版文件校验失败，已丢弃"))
			return
		}
	}

	a.updMu.Lock()
	a.upd.State = "ready"
	a.upd.Downloaded = written
	a.upd.Total = written
	a.updMu.Unlock()
}

// ApplyUpdate pauses every transfer, waits for them to settle, then hands off
// to a helper script that swaps the executable and restarts the app.
//
// Pausing (not killing) is what makes updating mid-download safe: partial
// files and sidecars stay on disk, and after the restart every task loads as
// paused, ready to resume. The updater script only moves files once this
// process has exited, so the running image is never replaced under itself.
func (a *App) ApplyUpdate() error {
	a.updMu.Lock()
	ready := a.upd.State == "ready"
	a.updMu.Unlock()
	if !ready {
		return fmt.Errorf("新版还没下载好")
	}

	// Freeze scheduling first, so pausing cannot wake queued work back up.
	a.mgr.mu.Lock()
	a.mgr.updating = true
	a.mgr.mu.Unlock()
	unfreeze := func() {
		a.mgr.mu.Lock()
		a.mgr.updating = false
		a.mgr.mu.Unlock()
	}

	a.mgr.StopAll()
	deadline := time.Now().Add(30 * time.Second)
	for a.mgr.activeCount() > 0 {
		if time.Now().After(deadline) {
			unfreeze()
			return fmt.Errorf("有任务停不下来，已取消更新")
		}
		time.Sleep(200 * time.Millisecond)
	}

	exe, staged, err := stagedUpdatePath()
	if err != nil {
		unfreeze()
		return err
	}
	if _, err := os.Stat(staged); err != nil {
		unfreeze()
		return fmt.Errorf("新版文件不在了，请重新下载")
	}

	script, err := writeUpdaterScript()
	if err != nil {
		unfreeze()
		return err
	}
	// #nosec G204 — the script path is created by this app in the temp dir,
	// not taken from any user input.
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive",
		"-ExecutionPolicy", "Bypass", "-File", script,
		"-TargetPid", strconv.Itoa(os.Getpid()),
		"-Exe", exe, "-Staged", staged)
	if err := cmd.Start(); err != nil {
		unfreeze()
		return err
	}
	// The updater waits for this process to exit before touching the files.
	runtime.Quit(a.ctx)
	return nil
}

// writeUpdaterScript renders the file-swap helper. It is ASCII-only on
// purpose: Windows PowerShell reads a .ps1 as ANSI unless it carries a BOM,
// and any non-ASCII in it would need that handling.
func writeUpdaterScript() (string, error) {
	const script = `param([int]$TargetPid, [string]$Exe, [string]$Staged)
$Bak = "$Exe.bak"
try { (Get-Process -Id $TargetPid -ErrorAction Stop).WaitForExit(30000) } catch {}
for ($i = 0; $i -lt 60; $i++) {
  if (!(Test-Path $Staged)) { break }
  try {
    if (Test-Path $Bak) { Remove-Item $Bak -Force -ErrorAction Stop }
    if (Test-Path $Exe) { Rename-Item $Exe $Bak -ErrorAction Stop }
    Move-Item $Staged $Exe -Force -ErrorAction Stop
    break
  } catch { Start-Sleep -Milliseconds 500 }
}
if (Test-Path $Exe) { Start-Process $Exe }
Remove-Item $Bak -Force -ErrorAction SilentlyContinue
Remove-Item $PSCommandPath -Force -ErrorAction SilentlyContinue
`
	path := filepath.Join(os.TempDir(), "vkmdownload-update.ps1")
	if err := os.WriteFile(path, []byte(script), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
