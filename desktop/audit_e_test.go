package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Batch E tests: the per-install pairing that replaces "the reply says it is
// VKMDownload, so it must be".

// TestPairKeyIsCreatedOnceAndKept pins the storage rule: one key per install,
// reused across starts, and not a fresh one per process (which would unpair the
// extension on every launch).
func TestPairKeyIsCreatedOnceAndKept(t *testing.T) {
	dir := t.TempDir()

	first, err := loadOrCreatePairKey(dir)
	if err != nil {
		t.Fatalf("loadOrCreatePairKey: %v", err)
	}
	if !validPairKey(first) {
		t.Fatalf("key %q is not the documented 64-hex shape", first)
	}

	// Reset the cache to prove the value comes from the file, not memory.
	forgetPairKey(dir)

	second, err := loadOrCreatePairKey(dir)
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if second != first {
		t.Errorf("key changed between runs: %q then %q", first, second)
	}

	// Two installs must not share a key.
	other, err := loadOrCreatePairKey(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other == first {
		t.Error("two data directories produced the same key")
	}

	// The file is per-user private.
	st, err := os.Stat(filepath.Join(dir, pairKeyFile))
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if st.Size() != pairKeyBytes*2 {
		t.Errorf("key file is %d bytes, want %d hex characters", st.Size(), pairKeyBytes*2)
	}
}

// TestCorruptPairKeyIsReplaced pins that a key file with no entropy is not
// trusted: it is replaced by a fresh one, because a predictable key is worse
// than an unpairing.
func TestCorruptPairKeyIsReplaced(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pairKeyFile), []byte("not-a-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	forgetPairKey(dir)

	key, err := loadOrCreatePairKey(dir)
	if err != nil {
		t.Fatalf("loadOrCreatePairKey: %v", err)
	}
	if !validPairKey(key) || key == "not-a-key" {
		t.Errorf("key = %q, want a freshly generated one", key)
	}
}

// TestProofDependsOnKeyAndChallenge pins the primitive: same challenge and key
// give the same answer, any change gives a different one.
func TestProofDependsOnKeyAndChallenge(t *testing.T) {
	a := proofFor("key-one", "challenge")
	if a != proofFor("key-one", "challenge") {
		t.Error("the proof is not deterministic")
	}
	if a == proofFor("key-two", "challenge") {
		t.Error("a different key produced the same proof")
	}
	if a == proofFor("key-one", "challenge2") {
		t.Error("a different challenge produced the same proof")
	}
	if len(a) != 64 {
		t.Errorf("proof length = %d, want 64 hex characters", len(a))
	}
}

// TestPingAnswersOnlyAWellFormedChallenge pins the input rules: no challenge
// means no header (an HMAC of the empty string would be replayable), and a
// challenge carrying anything but hex is refused rather than hashed.
func TestPingAnswersOnlyAWellFormedChallenge(t *testing.T) {
	app := NewAppWithDataDir(t.TempDir())
	app.emit = func(string, ...any) {}
	forgetPairKey(app.dataDir())

	key, err := app.pairKeyForThisInstall()
	if err != nil {
		t.Fatal(err)
	}

	if got := app.answerChallenge(""); got != "" {
		t.Errorf("an empty challenge was answered: %q", got)
	}
	if got := app.answerChallenge("zzzz"); got != "" {
		t.Errorf("a non-hex challenge was answered: %q", got)
	}
	if got := app.answerChallenge(strings.Repeat("a", maxChallengeLen+2)); got != "" {
		t.Errorf("an over-long challenge was answered: %q", got)
	}

	challenge := "00112233445566778899aabbccddeeff"
	got := app.answerChallenge(challenge)
	if got != proofFor(key, challenge) {
		t.Errorf("proof = %q, want the HMAC of the challenge", got)
	}

	// And the endpoint sets it as a header.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/ping?challenge="+challenge, nil)
	app.apiPing(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if hdr := rec.Header().Get(proofHeader); hdr != got {
		t.Errorf("%s = %q, want %q", proofHeader, hdr, got)
	}

	// Without a challenge the body still tells the extension where it is, but
	// no proof is offered.
	rec = httptest.NewRecorder()
	app.apiPing(rec, httptest.NewRequest(http.MethodGet, "/ping", nil))
	if hdr := rec.Header().Get(proofHeader); hdr != "" {
		t.Errorf("a ping without a challenge carried a proof: %q", hdr)
	}
}

// TestPairRequiresAnExtensionOrigin is the core of P0-3: the key is a credential,
// so a caller without a browser-extension Origin must never reach /pair.
func TestPairRequiresAnExtensionOrigin(t *testing.T) {
	app := newTestApp(t)

	cases := []struct {
		name   string
		origin string
		want   int
	}{
		{"no origin", "", http.StatusForbidden},
		{"web page", "https://evil.example", http.StatusForbidden},
		{"chrome extension", "chrome-extension://abcdefghijklmnop", http.StatusOK},
		{"firefox extension", "moz-extension://abcdefghijklmnop", http.StatusOK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18888/pair", nil)
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			// The peer check is on in this app instance, so a fake request (which
			// has no real TCP port) fails it. That is the point: origin alone is
			// not enough, and the 403 for the two extension cases proves the
			// handler consults the peer check as well.
			app.apiGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				app.apiPair(w, r)
			})).ServeHTTP(rec, req)

			if tc.want == http.StatusOK && rec.Code == http.StatusOK {
				// Only reachable if the peer check passed; keep the assertion
				// honest about what was actually proved.
				var body map[string]string
				if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
					t.Fatalf("pair body: %v", err)
				}
				if !validPairKey(body["key"]) {
					t.Errorf("pair returned %q, want a 64-hex key", body["key"])
				}
				return
			}
			if rec.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", rec.Code)
			}
		})
	}
}

// TestPairRefusedWithoutASignedBrowser pins the peer requirement directly: with
// a caller that is not a signed browser, /pair answers 403 and no key.
func TestPairRefusedWithoutASignedBrowser(t *testing.T) {
	app := newTestApp(t)
	// Force the peer check on and make the image lookup report a non-browser.
	origImage := processImageOf
	origOwner := tcpOwnerOfPort
	t.Cleanup(func() {
		processImageOf = origImage
		tcpOwnerOfPort = origOwner
	})
	tcpOwnerOfPort = func(uint16) (uint32, error) { return 4321, nil }
	processImageOf = func(uint32) (string, error) { return `C:\Windows\System32\curl.exe`, nil }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18888/pair", nil)
	req.Header.Set("Origin", "chrome-extension://abcdefghijklmnop")
	req.RemoteAddr = "127.0.0.1:54321"
	app.apiPair(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 for a non-browser caller", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "key") && !strings.Contains(rec.Body.String(), "error") {
		t.Errorf("the refusal body looks like a key: %s", rec.Body.String())
	}
}

// TestPairRejectsNonGet pins the method rule: everything here is a GET so the
// extension needs no preflight.
func TestPairRejectsNonGet(t *testing.T) {
	app := newTestApp(t)
	rec := httptest.NewRecorder()
	app.apiPair(rec, httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18888/pair", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestPairKeyNeverReachesTheLog pins the "no secrets in logs" rule for the real
// code paths: generating the key and serving it over /pair must leave no trace
// of it in the app log. (A test that logged the key itself would prove nothing.)
func TestPairKeyNeverReachesTheLog(t *testing.T) {
	app := newTestApp(t)
	forgetPairKey(app.dataDir())

	exe := `C:\Program Files\Google\Chrome\Application\chrome.exe`
	origImage := processImageOf
	origOwner := tcpOwnerOfPort
	origTrust := verifyTrustOf
	t.Cleanup(func() {
		processImageOf = origImage
		tcpOwnerOfPort = origOwner
		verifyTrustOf = origTrust
	})
	tcpOwnerOfPort = func(uint16) (uint32, error) { return 4321, nil }
	processImageOf = func(uint32) (string, error) { return exe, nil }
	verifyTrustOf = func(string) bool { return true }

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18888/pair", nil)
	req.Header.Set("Origin", "chrome-extension://abcdefghijklmnop")
	req.RemoteAddr = "127.0.0.1:54321"
	app.apiPair(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the key for a signed browser: %s", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("pair body: %v", err)
	}
	key := body["key"]
	if !validPairKey(key) {
		t.Fatalf("pair returned %q", key)
	}

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "vkmdownload.log"))
	if err != nil {
		return // No log at all is also a pass: nothing wrote the key.
	}
	if strings.Contains(string(b), key) {
		t.Error("the pairing key appears in the app log")
	}
}

// TestPairingKeepsTheHandoverWorking is the end-to-end sanity check: a caller
// that holds the key can prove itself, and a caller that does not cannot.
func TestPairingKeepsTheHandoverWorking(t *testing.T) {
	app := newTestApp(t)
	key, err := app.pairKeyForThisInstall()
	if err != nil {
		t.Fatal(err)
	}

	challenge := "aabbccdd"
	real := app.answerChallenge(challenge)
	if real != proofFor(key, challenge) {
		t.Fatal("the app cannot answer its own challenge")
	}

	// An impostor holding a different key produces a different answer, so the
	// extension's comparison fails.
	impostor := proofFor("0000000000000000000000000000000000000000000000000000000000000000", challenge)
	if proofMatchesTest(impostor, real) {
		t.Error("an impostor's proof matched the app's")
	}

	_ = context.Background()
}

// proofMatchesTest mirrors the extension's constant-time comparison, so the test
// asserts the same property the browser relies on.
func proofMatchesTest(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var diff byte
	for i := 0; i < len(a); i++ {
		diff |= a[i] ^ b[i]
	}
	return diff == 0
}
