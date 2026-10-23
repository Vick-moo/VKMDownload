package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestPairingInfoWithoutAKey covers a fresh install: nothing is paired, there is
// no fingerprint to show, and the path is still reported so the settings page can
// offer "open the folder".
func TestPairingInfoWithoutAKey(t *testing.T) {
	app := newTestApp(t)

	st := app.PairingInfo()
	if st.Paired {
		t.Error("Paired = true without a pair.key")
	}
	if st.Fingerprint != "" {
		t.Errorf("Fingerprint = %q, want empty", st.Fingerprint)
	}
	if want := filepath.Join(app.dataDir(), pairKeyFile); st.KeyPath != want {
		t.Errorf("KeyPath = %q, want %q", st.KeyPath, want)
	}
	if st.Port != 0 {
		t.Errorf("Port = %d, want 0 while the endpoint is not running", st.Port)
	}
}

// TestPairingInfoShowsOnlyAFingerprint pins the privacy rule: the status carries
// a short hash of the key, never the key itself.
func TestPairingInfoShowsOnlyAFingerprint(t *testing.T) {
	app := newTestApp(t)
	key, err := app.pairKeyForThisInstall()
	if err != nil || key == "" {
		t.Fatalf("pairKeyForThisInstall: %q, %v", key, err)
	}

	st := app.PairingInfo()
	if !st.Paired {
		t.Fatal("Paired = false although pair.key was just written")
	}
	if len(st.Fingerprint) != 16 {
		t.Errorf("Fingerprint = %q, want 16 hex characters", st.Fingerprint)
	}
	if want := keyFingerprint(key); st.Fingerprint != want {
		t.Errorf("Fingerprint = %q, want %q", st.Fingerprint, want)
	}
	// The key is 64 hex characters; the fingerprint is a different string and
	// nothing in the status may contain the key.
	for _, field := range []string{st.Fingerprint, st.KeyPath, fmt.Sprintf("%+v", st)} {
		if strings.Contains(field, key) {
			t.Fatalf("the status leaks the pairing key: %q", field)
		}
	}
}

// TestPairingInfoReportsTheListenPort covers the field the extension-facing UI
// shows: the port actually bound, read from the same place /ping reports.
func TestPairingInfoReportsTheListenPort(t *testing.T) {
	app := newTestApp(t)
	startAPI(t, app)

	st := app.PairingInfo()
	if st.Port == 0 {
		t.Fatal("Port = 0 while the local API is listening")
	}
	if st.Port != app.apiPort() {
		t.Errorf("Port = %d, apiPort() = %d", st.Port, app.apiPort())
	}
}

// TestResetPairingIsIdempotent covers the only unpair action there is.
//
// The key is per install, not per browser, so reset is all-or-nothing; a second
// call must succeed (the requested end state already holds), and the in-memory
// cache must be dropped so the next handover generates a genuinely new key.
func TestResetPairingIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	first, err := app.pairKeyForThisInstall()
	if err != nil || first == "" {
		t.Fatalf("pairKeyForThisInstall: %q, %v", first, err)
	}

	if err := app.ResetPairing(); err != nil {
		t.Fatalf("ResetPairing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.dataDir(), pairKeyFile)); !os.IsNotExist(err) {
		t.Errorf("pair.key still exists after reset: %v", err)
	}
	if st := app.PairingInfo(); st.Paired {
		t.Error("Paired = true after reset")
	}

	// Idempotent: no key is not an error.
	if err := app.ResetPairing(); err != nil {
		t.Errorf("a second ResetPairing failed: %v", err)
	}

	// The cache was dropped: the next caller gets a new key, not the deleted one.
	second, err := app.pairKeyForThisInstall()
	if err != nil || second == "" {
		t.Fatalf("pairKeyForThisInstall after reset: %q, %v", second, err)
	}
	if second == first {
		t.Error("the deleted key was served from the in-memory cache")
	}
	if st := app.PairingInfo(); !st.Paired || st.Fingerprint != keyFingerprint(second) {
		t.Errorf("status after re-pair = %+v, want it to describe the new key", st)
	}
}

// The vector key used by the proof tests: 32 bytes 0x00..0x1f, hex encoded. Its
// challenge is "abcdef". Both are fixed so the expected proof can be checked
// against an independent implementation.
const (
	proofVectorKey       = "000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f"
	proofVectorChallenge = "abcdef"
	// proofVectorWant is HMAC-SHA256(key = the 32 DECODED key bytes,
	// message = "abcdef"), hex encoded. Computed with Node's crypto (the same
	// HMAC-SHA256 the extension's hmacHex uses: key from key.match(/../g), i.e.
	// hex-decoded) and independently with .NET's HMACSHA256; both agree.
	proofVectorWant = "2867d85143fa9948833a5ec6f3c7d31068cc5a9ba587e08b1e8ad88e1a30acb3"
)

// TestProofForMatchesTheExtensionHMACVector pins the wire algorithm with a known
// answer.
//
// The old implementation HMAC'd the 64 hex characters as the key, which no
// extension could reproduce: /ping returned a proof the extension always
// rejected, so findApp gave up on a healthy app and every signed /add failed.
// The app's tests never noticed because the test signer used the same function.
func TestProofForMatchesTheExtensionHMACVector(t *testing.T) {
	if got := proofFor(proofVectorKey, proofVectorChallenge); got != proofVectorWant {
		t.Fatalf("proofFor = %s, want %s (the extension's hex-decoded HMAC)", got, proofVectorWant)
	}
}

// TestProofForRejectsTheKeyTextAsTheKey is the reverse guard: it fails if
// someone changes proofFor back to HMAC over the 64 ASCII characters.
func TestProofForRejectsTheKeyTextAsTheKey(t *testing.T) {
	mac := hmac.New(sha256.New, []byte(proofVectorKey))
	mac.Write([]byte(proofVectorChallenge))
	ascii := hex.EncodeToString(mac.Sum(nil))

	if got := proofFor(proofVectorKey, proofVectorChallenge); got == ascii {
		t.Fatalf("proofFor is HMAC-ing the 64 hex characters again: %s", got)
	}
}

// TestProofForRefusesAMalformedKey covers the guard: a key that is not hex must
// not silently sign with an empty or partial key.
func TestProofForRefusesAMalformedKey(t *testing.T) {
	if got := proofFor("not-hex", proofVectorChallenge); got != "" {
		t.Errorf("proofFor(invalid) = %q, want empty", got)
	}
	if got := proofFor("", proofVectorChallenge); got != "" {
		t.Errorf("proofFor(empty) = %q, want empty", got)
	}
}

// writeProofVectorKey puts the fixed key in the app's data directory, so the
// handlers read it the way a real install would.
func writeProofVectorKey(t *testing.T, app *App) {
	t.Helper()
	dir := app.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, pairKeyFile), proofVectorKey)
	forgetPairKey(dir)
}

// TestPingProofUsesTheDecodedKey is the end-to-end half: with the fixed key on
// disk, the header /ping returns is exactly the extension's known-answer HMAC.
func TestPingProofUsesTheDecodedKey(t *testing.T) {
	app := newTestApp(t)
	writeProofVectorKey(t, app)

	rec := httptest.NewRecorder()
	app.apiPing(rec, httptest.NewRequest(http.MethodGet, "/ping?challenge="+proofVectorChallenge, nil))

	if got := rec.Header().Get(proofHeader); got != proofVectorWant {
		t.Fatalf("%s = %s, want %s", proofHeader, got, proofVectorWant)
	}
}

// TestRequestAuthenticUsesTheExtensionProof covers the other proofFor caller:
// the signature the extension computes over method/path/stamp must pass, and the
// old ASCII-key signature must not.
func TestRequestAuthenticUsesTheExtensionProof(t *testing.T) {
	app := newTestApp(t)
	writeProofVectorKey(t, app)

	req := httptest.NewRequest(http.MethodPost, "/add", nil)
	req.Header.Set(authHeader, signedRequest(proofVectorKey, http.MethodPost, "/add", time.Now()))
	if !app.requestAuthentic(req) {
		t.Fatal("a signature made with the extension's HMAC was refused")
	}

	stamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, []byte(proofVectorKey))
	mac.Write([]byte(requestAuthValue(http.MethodPost, "/add", stamp)))
	req.Header.Set(authHeader, stamp+":"+hex.EncodeToString(mac.Sum(nil)))
	if app.requestAuthentic(req) {
		t.Error("the old ASCII-key signature was accepted")
	}
}
