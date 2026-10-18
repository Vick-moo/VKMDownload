package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stage-4 follow-up tests: the gaps the audits named after the first pass.

// TestLogsDoNotCarryUserAgentOrHandshakeToken pins the two things the log still
// leaked after URLs were sanitised: the full user agent (a browser/OS fingerprint
// attached to every bug report) and the update handshake path, whose filename is
// a credential for that attempt.
func TestLogsDoNotCarryUserAgentOrHandshakeToken(t *testing.T) {
	app := newTestApp(t)
	const ua = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/155.0.0.0"

	app.debugLog("extension added https://host.example/f.bin (origin=%q ua=%dB cookie=%dB)",
		"chrome-extension://abc", len(ua), 42)
	app.writeUpdateHandshake()

	b, err := os.ReadFile(filepath.Join(app.dataDir(), "vkmdownload.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	got := string(b)
	if strings.Contains(got, "Windows NT 10.0") || strings.Contains(got, "AppleWebKit") {
		t.Errorf("the log holds a full user agent: %s", got)
	}
	// A handshake token is 64 hex characters; the log must not contain one.
	if tok := app.updateToken; tok != "" && strings.Contains(got, tok) {
		t.Errorf("the log holds the handshake token: %s", got)
	}
}

// TestScrubRemovesCorruptAndTemporaryCopies pins the coverage gap the audit
// found: the corrupt copy loadTasks keeps aside and the .tmp a crash can strand
// hold the same records as the index, so "credentials were removed" was true
// only of the file the app opens.
func TestScrubRemovesCorruptAndTemporaryCopies(t *testing.T) {
	app := newTestApp(t)
	dir := app.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A corrupt copy with a cookie: removed outright, since a plaintext cookie
	// has no diagnostic value worth keeping.
	cookieCopy := filepath.Join(dir, "tasks.json.corrupt-20260101-000000.000")
	if err := os.WriteFile(cookieCopy, []byte(`[{"id":"a","url":"https://h/f?token=SECRET","cookie":"sid=SECRETCOOKIE"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A temporary copy without a cookie: its URL is still redacted.
	tmpCopy := filepath.Join(dir, "tasks.json.123.tmp")
	if err := os.WriteFile(tmpCopy, []byte(`[{"id":"b","url":"https://h/g?token=TMPTOKEN"}]`), 0o644); err != nil {
		t.Fatal(err)
	}

	app.scrubStoredSecrets()

	if _, err := os.Stat(cookieCopy); !os.IsNotExist(err) {
		t.Errorf("a stale copy holding a cookie survived the scrub")
	}
	b, err := os.ReadFile(tmpCopy)
	if err != nil {
		t.Fatalf("the temporary copy should be kept (redacted), not deleted: %v", err)
	}
	if strings.Contains(string(b), "TMPTOKEN") {
		t.Errorf("the temporary copy still holds a signed URL: %s", b)
	}
}

// TestClearHistoryRemovesCopiesToo pins the other half: "clear my data" has to
// mean every file the app wrote, not just the two it normally reads.
func TestClearHistoryRemovesCopiesToo(t *testing.T) {
	app := newTestApp(t)
	dir := app.dataDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	copies := []string{
		"tasks.json.corrupt-20260101-000000.000",
		"tasks.json.999.tmp",
		"tasks.backup-before-cleanup.json",
	}
	for _, name := range copies {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("[]"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := app.ClearDownloadHistory(); err != nil {
		t.Fatalf("ClearDownloadHistory: %v", err)
	}
	for _, name := range copies {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("%s survived the clear", name)
		}
	}
}

// TestSignedRequestIsBoundToMethodAndPath pins the auth primitive: a proof made
// for one request cannot be replayed onto another, and an old one expires.
func TestSignedRequestIsBoundToMethodAndPath(t *testing.T) {
	app := newTestApp(t)
	forgetPairKey(app.dataDir())
	key, err := app.pairKeyForThisInstall()
	if err != nil || key == "" {
		t.Fatalf("pair key: %v", err)
	}

	now := time.Now()
	good := signedRequest(key, http.MethodPost, "/add", now)

	mk := func(method, path, header string) *http.Request {
		r := httptest.NewRequest(method, "http://127.0.0.1:18888"+path, nil)
		r.Header.Set(authHeader, header)
		return r
	}
	if !app.requestAuthentic(mk(http.MethodPost, "/add", good)) {
		t.Error("a correctly signed request was refused")
	}
	if app.requestAuthentic(mk(http.MethodPost, "/settings", good)) {
		t.Error("a proof for /add was accepted on /settings")
	}
	if app.requestAuthentic(mk(http.MethodGet, "/add", good)) {
		t.Error("a proof for POST was accepted on GET")
	}

	// Two minutes and a second later it is stale.
	stale := signedRequest(key, http.MethodPost, "/add", now.Add(-3*time.Minute))
	if app.requestAuthentic(mk(http.MethodPost, "/add", stale)) {
		t.Error("an expired proof was accepted")
	}
	// A different key cannot sign for this app.
	other := signedRequest(strings.Repeat("ab", 32), http.MethodPost, "/add", now)
	if app.requestAuthentic(mk(http.MethodPost, "/add", other)) {
		t.Error("a proof from another key was accepted")
	}
	// And nothing at all is refused rather than treated as a local caller.
	if app.requestAuthentic(mk(http.MethodPost, "/add", "")) {
		t.Error("an unsigned request was accepted")
	}
}

// TestAddRejectsNonHTTPSchemes pins the door check: with AskLocation on, the URL
// reaches the new-task dialog before Start ever validates it.
func TestAddRejectsNonHTTPSchemes(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)
	app.cfg.VerifyPeerProcess = false

	for _, bad := range []string{"file:///C:/Windows/System32/calc.exe", "javascript:alert(1)", "ftp://h/f.bin"} {
		body := `{"url":"` + bad + `"}`
		resp := postJSON(t, base+"/add", "chrome-extension://abc", body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status = %d, want 400", bad, resp.StatusCode)
		}
	}
}

// TestHostHeaderMustNameThisMachine pins the DNS-rebinding guard: binding to
// 127.0.0.1 stops anything off-box from connecting, but a domain that resolves
// to loopback still arrives with its own name in Host.
func TestHostHeaderMustNameThisMachine(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	req, err := http.NewRequest(http.MethodGet, base+"/ping", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "rebind.evil.example:18888"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d with a foreign Host header, want 403", resp.StatusCode)
	}
}

// TestLoopbackHostAcceptsEverySpelling keeps the guard from breaking real
// callers, which spell the same machine several ways.
func TestLoopbackHostAcceptsEverySpelling(t *testing.T) {
	for _, h := range []string{"127.0.0.1:18888", "localhost:18888", "[::1]:18888", "127.0.0.1"} {
		if !loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = false, want true", h)
		}
	}
	for _, h := range []string{"", "evil.example", "1127.0.0.1:18888", "127.0.0.1.evil:18888"} {
		if loopbackHost(h) {
			t.Errorf("loopbackHost(%q) = true, want false", h)
		}
	}
}

// TestFileGoneTaskRefusesResume pins the interaction the audit spotted: a
// completed task whose file vanished is reported failed, but its URL was already
// redacted, so resuming it would fail for a reason unrelated to the real problem.
func TestFileGoneTaskRefusesResume(t *testing.T) {
	app := newTestApp(t)
	if err := os.MkdirAll(app.dataDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	list := []persistedTask{{
		TaskView: TaskView{
			ID: "gone-1", Status: StatusCompleted, Created: time.Now().Format(time.RFC3339Nano),
			URL:  "https://host.example/f.bin",
			Dest: filepath.Join(t.TempDir(), "already-moved.bin"),
		},
	}}
	b, _ := json.MarshalIndent(list, "", "  ")
	if err := os.WriteFile(filepath.Join(app.dataDir(), "tasks.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	app.mgr.loadTasks()

	err := app.mgr.Resume("gone-1")
	if err == nil {
		t.Fatal("Resume accepted a task whose file is gone and whose URL has no credentials")
	}
	if !strings.Contains(err.Error(), "重新添加") {
		t.Errorf("error = %v, want it to tell the user to re-add the link", err)
	}
}
