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
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
func proofFor(key, challenge string) string {
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(challenge))
	return hex.EncodeToString(mac.Sum(nil))
}

// pairKeyForThisInstall is the app's key, generating it when needed.
func (a *App) pairKeyForThisInstall() (string, error) {
	return loadOrCreatePairKey(a.dataDir())
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
