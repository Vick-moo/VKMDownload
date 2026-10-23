package main

// Per-install pairing between the app and its browser extension.
//
// The endpoint is loopback and the Origin check kept web pages out, but neither
// proves *who* is asking: any program on the machine can put
// "Origin: chrome-extension://..." on a request and read the reply. That was
// enough to walk away with the browser's cookies (including HttpOnly ones, which
// the extension reads through the cookies API) and with signed download URLs
// that carry their own credentials.
//
// So the extension now has to prove it is talking to *this* app, and the app
// only hands out downloads to a caller that holds the per-install key:
//
//  1. On first use the extension asks GET /pair. That endpoint answers only when
//     the request comes from a browser process whose image is signed (the peer
//     check), so a random local program cannot obtain the key.
//  2. Every candidate port must answer /ping?challenge=<nonce> with
//     HMAC-SHA256(key, nonce). A program squatting on the port cannot compute
//     that, so the extension refuses the port instead of sending it cookies.
//  3. The key lives in chrome.storage.local for the extension, and in the user's
//     private app-data directory for the app. It is never logged.
//
// The key is a capability, not an identity: rotating it (deleting pair.key)
// simply unpairs the extension, which re-pairs on its next handover.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// pairKeyFile is the key's filename inside the app's data directory.
	pairKeyFile = "pair.key"
	// pairKeyBytes is the raw key length; 32 bytes makes the HMAC's security
	// margin the full strength of SHA-256.
	pairKeyBytes = 32
	// proofHeader carries the answer to the /ping challenge.
	proofHeader = "X-VKMDownload-Proof"
	// maxChallengeLen bounds the nonce a caller may send, so the query string
	// cannot be used to make the app hash megabytes.
	maxChallengeLen = 128
)

// pairMu guards the cache: the endpoint is served from several goroutines.
//
// The cache is keyed by data directory. A single package-level value was wrong
// as soon as two installations shared a process (tests, or a second profile):
// the second one silently reused the first one's key, which is exactly the
// cross-install sharing this whole scheme exists to prevent.
var (
	pairMu     sync.Mutex
	pairCached = map[string]string{}
)

// loadOrCreatePairKey returns the per-install key, generating it on first use.
//
// A key that cannot be written is returned for this run but not persisted: the
// extension then re-pairs on every start, which is worse for latency and better
// than refusing to download at all.
func loadOrCreatePairKey(dir string) (string, error) {
	pairMu.Lock()
	defer pairMu.Unlock()
	if k := pairCached[dir]; k != "" {
		return k, nil
	}

	path := filepath.Join(dir, pairKeyFile)
	if b, err := os.ReadFile(path); err == nil {
		if k := strings.TrimSpace(string(b)); validPairKey(k) {
			pairCached[dir] = k
			return k, nil
		}
		// A corrupt key file is replaced rather than trusted: a key with no
		// entropy is worse than a fresh one, because it looks valid.
	}

	raw := make([]byte, pairKeyBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	key := hex.EncodeToString(raw)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return key, err
	}
	// 0600: the key is a capability over this user's downloads. On Windows the
	// data directory is already inside the user's private profile, so this is
	// belt and braces rather than the only barrier.
	if err := os.WriteFile(path, []byte(key), 0o600); err != nil {
		return key, err
	}
	pairCached[dir] = key
	return key, nil
}

// forgetPairKey drops the cached key for dir. Only tests need it; production
// code never changes a key under itself.
func forgetPairKey(dir string) {
	pairMu.Lock()
	delete(pairCached, dir)
	pairMu.Unlock()
}

// validPairKey reports whether k has the shape this code writes: hex, exactly
// pairKeyBytes bytes long.
func validPairKey(k string) bool {
	if len(k) != pairKeyBytes*2 {
		return false
	}
	_, err := hex.DecodeString(k)
	return err == nil
}

// proofFor computes the answer the extension must see for a challenge.
//
// The key is the 64-character hex form of 32 random bytes, and the HMAC key is
// those decoded bytes — not the 64 ASCII characters. The extension decodes the
// hex before signing (crypto.subtle with the bytes of `key.match(/../g)`), so
// using the string bytes here produced a proof that never matched: /ping
// answered 200 with the wrong proof, findApp gave up on a healthy app, and every
// signed /add was refused, while the app's own tests still passed because the
// test signer used the same wrong function. Do not "simplify" this back to
// []byte(key).
func proofFor(key, challenge string) string {
	raw, err := hex.DecodeString(key)
	if err != nil || len(raw) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(challenge))
	return hex.EncodeToString(mac.Sum(nil))
}

// pairKeyForThisInstall is the app's key, generating it when needed.
func (a *App) pairKeyForThisInstall() (string, error) {
	return loadOrCreatePairKey(a.dataDir())
}

// Request authentication, app side.
//
// The peer check answers "is the caller a real browser process?" and the pairing
// key answers "is the responder really this app?". Neither answers "is this
// caller our extension?": any extension in the same browser passes the peer
// check, and the Origin header only says "some extension". Since /add enqueues a
// download with attacker-chosen cookies and URL — an SSRF primitive against the
// user's intranet — every mutation requires proof that the caller holds the
// per-install key. (The extension may no longer write the settings at all: POST
// /settings answers 405; the proof rule stays for the endpoints that really
// mutate, and for the contract a future extension build will still need.)
//
// The proof is HMAC(key, method + "\n" + path + "\n" + unix) with a two-minute
// window. A timestamp rather than a server-issued nonce keeps the endpoint
// stateless; replay inside the window needs the ability to read loopback traffic,
// which on Windows already means administrator.
const (
	authHeader = "X-VKMDownload-Auth"
	// authWindow bounds clock skew and replay together.
	authWindow = 2 * time.Minute
)

// requestAuthValue is the string that gets signed for this request.
func requestAuthValue(method, path, stamp string) string {
	return method + "\n" + path + "\n" + stamp
}

// signedRequest produces the header value for a request, given the key.
func signedRequest(key, method, path string, now time.Time) string {
	stamp := strconv.FormatInt(now.Unix(), 10)
	return stamp + ":" + proofFor(key, requestAuthValue(method, path, stamp))
}

// requestAuthentic reports whether the request carries a valid, fresh proof.
//
// A missing or malformed header is refused; so is one that signs a different
// method or path, which stops a proof captured for /ping from being replayed
// onto /add.
func (a *App) requestAuthentic(r *http.Request) bool {
	raw := strings.TrimSpace(r.Header.Get(authHeader))
	if raw == "" {
		return false
	}
	stamp, sig, ok := strings.Cut(raw, ":")
	if !ok || sig == "" {
		return false
	}
	when, err := strconv.ParseInt(stamp, 10, 64)
	if err != nil {
		return false
	}
	skew := time.Since(time.Unix(when, 0))
	if skew > authWindow || skew < -authWindow {
		return false
	}
	key, kerr := a.pairKeyForThisInstall()
	if kerr != nil || key == "" {
		return false
	}
	want := proofFor(key, requestAuthValue(r.Method, r.URL.Path, stamp))
	// Constant-time compare: a byte-at-a-time comparison leaks how much of a
	// forged proof was correct, which is enough to forge one byte at a time.
	return hmac.Equal([]byte(sig), []byte(want))
}

// apiPair hands the key to a caller that has already passed the origin and peer
// checks. It is a GET so the extension can use it without a preflight, and it
// answers 403 — not 200 with an empty body — when either check fails, because a
// caller that cannot pair must treat the app as absent rather than as paired.
func (a *App) apiPair(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	// The peer check is required here regardless of the VerifyPeerProcess
	// setting. That switch governs whether ordinary handovers are verified; this
	// endpoint hands out a credential, so "off" cannot mean "give the key to
	// anything that asks".
	ok, exe := a.checkPeerProcess(r)
	if !ok {
		a.debugLog("pair refused: caller is not a signed browser%s", exeSuffix(exe))
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": "caller is not a recognized browser" + exeSuffix(exe),
		})
		return
	}

	key, err := a.pairKeyForThisInstall()
	if err != nil {
		// Logged without the key itself: the path is enough to diagnose.
		a.debugLog("pair: cannot store the pairing key: %v", err)
	}
	if key == "" {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "pairing unavailable"})
		return
	}
	// Deliberately not logged: the key is a capability.
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"key": key})
}

// answerChallenge computes the proof for a /ping query, or "" when there is
// nothing to answer.
//
// It is separate from the handler so a test can assert that a request without a
// challenge gets no header at all — an empty challenge must not receive the HMAC
// of the empty string, which any squatter could replay.
func (a *App) answerChallenge(challenge string) string {
	challenge = strings.TrimSpace(challenge)
	if challenge == "" || len(challenge) > maxChallengeLen {
		return ""
	}
	// Hex only: anything else is either a mistake or an attempt to smuggle
	// characters into the HMAC input from a query string.
	if _, err := hex.DecodeString(challenge); err != nil {
		return ""
	}
	key, err := a.pairKeyForThisInstall()
	if err != nil || key == "" {
		return ""
	}
	return proofFor(key, challenge)
}

// ---------------------------------------------------------------------------
// status and reset, for the settings page

// PairingStatus describes the app<->extension pairing without revealing the key.
//
// The key is per install, not per browser: one key covers every browser and
// every extension instance that pairs with this app. There is therefore no
// "disconnect just this browser" — ResetPairing is all-or-nothing, and every
// extension re-pairs on its next handover.
type PairingStatus struct {
	// Paired reports that pair.key exists and holds a key this code recognises.
	Paired bool `json:"paired"`
	// Fingerprint is the first 8 bytes of sha256(key) in hex (16 characters).
	// It is stable for a key and cannot be turned back into the key; the
	// settings page shows it so two installations can be told apart.
	Fingerprint string `json:"fingerprint"`
	// Port is the loopback port the extension should find the app on, 0 when
	// the endpoint is not running.
	Port int `json:"port"`
	// KeyPath is where pair.key lives. It is for "open the folder", never for
	// showing the file's contents.
	KeyPath string `json:"keyPath"`
}

// keyFingerprint is a stable, non-secret identifier for a pairing key.
//
// 8 bytes of SHA-256: short enough to read out over the phone, long enough that
// two randomly generated keys will not collide in practice. The hash is of the
// key itself, so it changes exactly when the key does.
func keyFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:8])
}

// PairingInfo reports the pairing state for the settings page.
//
// It reads the key file rather than the cache: "paired" means the key is on
// disk and readable, which is what survives a restart. A key that exists only
// in memory (the write failed) is not paired, because the next start cannot use
// it either.
func (a *App) PairingInfo() PairingStatus {
	path := filepath.Join(a.dataDir(), pairKeyFile)
	out := PairingStatus{Port: a.apiPort(), KeyPath: path}

	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	key := strings.TrimSpace(string(b))
	if !validPairKey(key) {
		return out
	}
	out.Paired = true
	out.Fingerprint = keyFingerprint(key)
	return out
}

// ResetPairing deletes the pairing key so every extension has to pair again.
//
// It is all-or-nothing by design: the key is per install, not per browser, so
// "disconnect this browser only" cannot be expressed. A missing file counts as
// success — the requested end state (no key) already holds — which also makes
// the call safe to retry. The in-memory cache is dropped so the next request
// generates a genuinely new key instead of serving the deleted one.
func (a *App) ResetPairing() error {
	dir := a.dataDir()
	path := filepath.Join(dir, pairKeyFile)
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("无法删除配对密钥：%w", err)
	}
	forgetPairKey(dir)
	return nil
}
