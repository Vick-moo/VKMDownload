package main

import (
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// VerifyPeerProcess, when true, makes the loopback endpoint confirm that the
// process on the other end of each connection is a genuine signed browser
// before honouring the request. An Origin header is self-reported and costs
// an attacker nothing to forge; a process image path plus an Authenticode
// signature cannot be faked without owning the vendor's signing key or
// injecting into the browser itself.
//
// The check runs once per request (not per byte transferred) and reads the
// switch live, so flipping it applies to the very next download with no
// restart. Measured on the developer's machine: ~0.3ms per lookup, signature
// verified once per browser executable then cached, so the steady-state cost
// per download is under 1ms.

// allowedBrowserExes names the browser executables the endpoint talks to.
// A caller whose image is anything else is rejected and its exe name is
// logged, so a new legitimate browser only needs its name reported once.
var allowedBrowserExes = map[string]bool{
	"chrome.exe": true, "chrome_beta.exe": true, "chrome_dev.exe": true,
	"chrome_canary.exe": true, "chromium.exe": true,
	"msedge.exe": true, "msedge_beta.exe": true, "msedge_dev.exe": true,
	"msedge_canary.exe": true,
	"brave.exe":         true, "brave_beta.exe": true, "brave_nightly.exe": true,
	"opera.exe": true, "opera_beta.exe": true, "opera_developer.exe": true,
	"vivaldi.exe": true, "arc.exe": true,
}

// trustCache remembers signature verdicts per executable (plus its mtime, so
// a browser update re-verifies). Verifying once instead of per request is
// what keeps the steady-state cost under a millisecond.
type trustCache struct {
	mu   sync.Mutex
	hits map[string]cachedVerdict
}

type cachedVerdict struct {
	mtime int64
	size  int64
	ok    bool
}

var peerTrust = &trustCache{hits: make(map[string]cachedVerdict)}

// Injectables for tests: the table lookup and the trust primitive are the
// only OS-touching parts, so stubbing these two covers every policy branch
// without real processes or signatures. errPeerCheckUnsupported is returned
// by the non-Windows stubs, where the peer table and Authenticode APIs do
// not exist.
var errPeerCheckUnsupported = errors.New("peer check unsupported on this OS")
var (
	tcpOwnerOfPort = tcpOwnerPid
	processImageOf = processImagePath
	verifyTrustOf  = verifyFileTrust
)

// peerCheckRequired reports whether caller verification is enforced.
func (a *App) peerCheckRequired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.VerifyPeerProcess
}

// checkPeerProcess confirms the loopback peer is a genuine signed browser.
//
// Infrastructure failures (unparseable address, vanished connection row,
// exited process) fail OPEN with a log line: the connection is alive in the
// handler, so these mean a transient lookup race rather than an attacker.
// A known non-browser image or a bad signature fails CLOSED.
func (a *App) checkPeerProcess(r *http.Request) (bool, string) {
	_, portStr, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		a.debugLog("peer check: bad remote addr %q: %v", r.RemoteAddr, err)
		return true, ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		a.debugLog("peer check: bad remote port %q", r.RemoteAddr)
		return true, ""
	}
	pid, err := tcpOwnerOfPort(uint16(port))
	if err != nil {
		a.debugLog("peer check: owner lookup failed for %s: %v", r.RemoteAddr, err)
		return true, ""
	}
	exe, err := processImageOf(pid)
	if err != nil || exe == "" {
		a.debugLog("peer check: pid %d has no image: %v", pid, err)
		return true, ""
	}
	base := strings.ToLower(filepath.Base(exe))
	if !allowedBrowserExes[base] {
		a.debugLog("peer check: rejected non-browser caller %s (pid %d)", exe, pid)
		return false, exe
	}
	// One Stat shared by the cache lookup and the store below: if the file
	// changes during verification, the stored identity will not match next
	// time and the verdict is recomputed instead of trusted.
	mtime, size, stated := statOf(exe)
	if hit, found := peerTrust.getAt(exe, mtime, size, stated); found {
		if !hit {
			a.debugLog("peer check: rejected untrusted image %s", exe)
			return false, exe
		}
		return true, ""
	}
	ok := verifyTrustOf(exe)
	peerTrust.putAt(exe, mtime, size, stated, ok)
	if !ok {
		a.debugLog("peer check: rejected bad signature %s", exe)
		return false, exe
	}
	return true, ""
}

// statOf stats once so verification and caching share the same file identity:
// otherwise a swap between the check and the store would file a stale
// verdict under a fresh mtime.
func statOf(exe string) (mtime int64, size int64, ok bool) {
	st, err := os.Stat(exe)
	if err != nil {
		return 0, 0, false
	}
	return st.ModTime().UnixNano(), st.Size(), true
}

// getAt reports the cached verdict for exe when the file identity matches,
// as (verdict, found). Returning a pointer into the map entry was technically
// fine under the lock but needlessly delicate to read.
func (c *trustCache) getAt(exe string, mtime, size int64, ok bool) (bool, bool) {
	if !ok {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	v, found := c.hits[cacheKey(exe)]
	if !found || v.mtime != mtime || v.size != size {
		return false, false
	}
	return v.ok, true
}

func (c *trustCache) putAt(exe string, mtime, size int64, stated, ok bool) {
	if !stated {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.hits) >= maxTrustEntries {
		// Bounded: an attacker pointing at endless same-named binaries in
		// fresh directories must not grow this without limit. Evicting
		// everything is crude but safe — the next request re-verifies.
		c.hits = make(map[string]cachedVerdict)
	}
	c.hits[cacheKey(exe)] = cachedVerdict{mtime: mtime, size: size, ok: ok}
}

// maxTrustEntries bounds the verdict cache: one entry per distinct browser
// binary in practice, so this ceiling is never hit in normal use.
const maxTrustEntries = 64

// cacheKey normalises for Windows' case-insensitive filesystem so
// C:\X\Chrome.exe and c:\x\chrome.exe share one verdict.
func cacheKey(exe string) string {
	return strings.ToLower(filepath.Clean(exe))
}
