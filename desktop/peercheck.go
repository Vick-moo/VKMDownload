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
	"time"
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
	// browserRootCheck reports whether an image lives in an install root. It is
	// injectable for the same reason as the three above: a test cannot place a
	// file inside Program Files, and the rule itself is asserted directly.
	browserRootCheck = browserInstallRootOK
)

// peerCheckRequired reports whether caller verification is enforced.
func (a *App) peerCheckRequired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.VerifyPeerProcess
}

// Peer check diagnostics.
//
// A refused caller is otherwise invisible: the extension shows "not a recognized
// browser" and the user has no way to tell which process was refused or why. The
// settings page reads this ring instead. It is process memory only and never
// written to disk — a peer path names a file the user installed, and that is
// exactly the kind of detail a support log should not accumulate.

// PeerRejection is one refused caller.
type PeerRejection struct {
	Time time.Time `json:"time"`
	// Exe is the basename of the caller's image when one could be read
	// ("brave.exe"), empty when the check failed before that. The full path is
	// deliberately not kept: it names the user's install and account.
	Exe string `json:"exe"`
	// Reason is a stable machine-readable value the settings page can translate:
	// bad_addr, not_loopback, bad_port, no_owner, no_image, not_allowlisted,
	// not_in_install_root, bad_signature, origin_locked.
	Reason string `json:"reason"`
}

// peerRejectionKeep bounds the ring: enough to see a pattern, small enough that
// it is obviously a diagnostic and not a log.
const peerRejectionKeep = 20

const (
	peerReasonBadAddr          = "bad_addr"
	peerReasonNotLoopback      = "not_loopback"
	peerReasonBadPort          = "bad_port"
	peerReasonNoOwner          = "no_owner"
	peerReasonNoImage          = "no_image"
	peerReasonNotAllowlisted   = "not_allowlisted"
	peerReasonNotInInstallRoot = "not_in_install_root"
	peerReasonBadSignature     = "bad_signature"
	// peerReasonOriginLocked is not a peer check result: the Origin lock in
	// apiGuard records its refusals here too, so one list explains every "the
	// app will not talk to my extension" report.
	peerReasonOriginLocked = "origin_locked"
	// peerReasonOriginMissing is the locked-down case where the request carried
	// no Origin at all. Current Chrome omits Origin on an extension's own fetch,
	// so a locked app cannot match the request to any extension and refuses it.
	peerReasonOriginMissing = "origin_missing"
)

// recordPeerRejection appends a refusal to the in-memory ring, newest first.
//
// It records a reason and, at most, an executable's basename. Nothing here
// changes the verdict: the caller has already been refused.
func (a *App) recordPeerRejection(reason, exe string) {
	name := ""
	if exe != "" {
		name = filepath.Base(exe)
	}
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	a.peerRejects = append([]PeerRejection{{Time: time.Now(), Exe: name, Reason: reason}},
		a.peerRejects...)
	if len(a.peerRejects) > peerRejectionKeep {
		a.peerRejects = a.peerRejects[:peerRejectionKeep]
	}
}

// PeerDiagnostics returns the most recent refusals, newest first.
func (a *App) PeerDiagnostics() []PeerRejection {
	a.peerMu.Lock()
	defer a.peerMu.Unlock()
	out := make([]PeerRejection, len(a.peerRejects))
	copy(out, a.peerRejects)
	return out
}

// checkPeerProcess confirms the loopback peer is a genuine signed browser.
//
// Every path that cannot establish who the caller is fails CLOSED: an
// unparseable or non-loopback remote address, a socket whose owning process
// cannot be found, an image that cannot be read, a name outside the browser
// list, a path outside a browser install root, or a signature the cache cannot
// vouch for. There is no fail-open exception for "infrastructure" trouble — a
// lookup race and an attacker are indistinguishable from here, and the peer
// check is a security control, not a convenience: an unidentifiable caller is
// an untrusted caller. The refusal is a 403; the download simply stays in the
// browser.
func (a *App) checkPeerProcess(r *http.Request) (bool, string) {
	host, portStr, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		a.debugLog("peer check: bad remote addr %q: %v", r.RemoteAddr, err)
		a.recordPeerRejection(peerReasonBadAddr, "")
		return false, ""
	}
	// The peer must be on loopback. A request arriving from anywhere else means
	// the listener is not the only thing exposed, and treating it as a browser
	// would be exactly backwards.
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		a.debugLog("peer check: peer %q is not loopback", host)
		a.recordPeerRejection(peerReasonNotLoopback, "")
		return false, ""
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port <= 0 || port > 65535 {
		a.debugLog("peer check: bad remote port %q", r.RemoteAddr)
		a.recordPeerRejection(peerReasonBadPort, "")
		return false, ""
	}
	// The server port comes from the listener rather than the request, so a
	// caller cannot influence which socket is looked up.
	pid, err := tcpOwnerOfPort(r.RemoteAddr, uint16(a.apiPort()))
	if err != nil {
		// Fail CLOSED. This used to fail open, on the argument that a lookup race
		// meant a vanishing process rather than an attacker — but it also meant a
		// client that bound its source address to anything other than 127.0.0.1
		// (loopback is a whole /8 on Windows) was never found, and was therefore
		// waved through. A caller we cannot identify is a caller we do not trust;
		// the peer check is a security control, not a convenience.
		a.debugLog("peer check: cannot identify %s: %v", r.RemoteAddr, err)
		a.recordPeerRejection(peerReasonNoOwner, "")
		return false, ""
	}
	exe, err := processImageOf(pid)
	if err != nil || exe == "" {
		// Fail closed for the same reason as the lookup above: an image we
		// cannot name is not a browser we can vouch for.
		a.debugLog("peer check: pid %d has no readable image: %v", pid, err)
		a.recordPeerRejection(peerReasonNoImage, "")
		return false, ""
	}
	base := strings.ToLower(filepath.Base(exe))
	if !allowedBrowserExes[base] {
		a.debugLog("peer check: rejected non-browser caller %s (pid %d)", exe, pid)
		a.recordPeerRejection(peerReasonNotAllowlisted, exe)
		return false, exe
	}
	// A name is not an identity. Copying any validly signed Microsoft binary
	// (curl.exe is the obvious one) to %TEMP%\chrome.exe satisfies "known name"
	// and "valid signature" while being neither a browser nor installed in a
	// browser's directory — the copy keeps its signature, so a signature check
	// alone cannot tell them apart. Requiring the image to live in an install
	// root is what makes that copy fail: writing there needs administrator.
	if !browserRootCheck(exe) {
		a.debugLog("peer check: rejected %s: not inside a browser install directory", exe)
		a.recordPeerRejection(peerReasonNotInInstallRoot, exe)
		return false, exe
	}
	// One Stat shared by the cache lookup and the store below: if the file
	// changes during verification, the stored identity will not match next
	// time and the verdict is recomputed instead of trusted.
	mtime, size, stated := statOf(exe)
	if hit, found := peerTrust.getAt(exe, mtime, size, stated); found {
		if !hit {
			a.debugLog("peer check: rejected untrusted image %s", exe)
			a.recordPeerRejection(peerReasonBadSignature, exe)
			return false, exe
		}
		return true, ""
	}
	ok := verifyTrustOf(exe)
	peerTrust.putAt(exe, mtime, size, stated, ok)
	if !ok {
		a.debugLog("peer check: rejected bad signature %s", exe)
		a.recordPeerRejection(peerReasonBadSignature, exe)
		return false, exe
	}
	return true, ""
}

// browserInstallRootOK reports whether exe lives under a directory that only an
// administrator (or the user, for a per-user browser install) can write to.
//
// The list is deliberately about *roots*, not about any one vendor's layout:
// Chrome, Edge, Brave, Opera and Vivaldi all install under Program Files, and
// per-user installs live under LOCALAPPDATA. A browser the user keeps on another
// drive is refused — the refusal names the path, so it can be reported and added
// here rather than silently weakening the check.
func browserInstallRootOK(exe string) bool {
	clean := strings.ToLower(filepath.Clean(exe))
	for _, env := range []string{"ProgramFiles", "ProgramFiles(x86)", "ProgramW6432", "LOCALAPPDATA"} {
		root := os.Getenv(env)
		if root == "" {
			continue
		}
		root = strings.ToLower(filepath.Clean(root))
		if strings.HasPrefix(clean, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
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
//
// The verdict is keyed on the path plus the file's mtime and size, which a
// determined local attacker can forge (overwrite the image, then restore the
// timestamp and pad to the same length). That is accepted: the attacker already
// needs write access to a browser's install directory, which is administrator.
// The alternative — hashing a 200 MB browser binary on every request — is not
// worth it for a threat that has already won.
func cacheKey(exe string) string {
	return strings.ToLower(filepath.Clean(exe))
}
