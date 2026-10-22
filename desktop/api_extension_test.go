package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// extRequest sends a GET carrying an Origin header, the way the extension's own
// fetch arrives at the loopback endpoint.
func extRequest(t *testing.T, url, origin string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", origin)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return resp
}

// addWithVersion posts a handover with the optional extension-version header,
// signed the way the extension signs its requests.
func addWithVersion(t *testing.T, base, origin, version, body string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, base+"/add", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", origin)
	if version != "" {
		req.Header.Set(extVersionHeader, version)
	}
	if h := signForApp(t, req.Method, req.URL.Path); h != "" {
		req.Header.Set(authHeader, h)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /add: %v", err)
	}
	return resp
}

// TestPingAdvertisesTheProtocolHandshake covers the new /ping fields: the old
// app/version/port stay, and the extension can now compare its own version
// against what this build expects.
func TestPingAdvertisesTheProtocolHandshake(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		App                 string `json:"app"`
		Version             string `json:"version"`
		Port                int    `json:"port"`
		Protocol            int    `json:"protocol"`
		MinExtensionVersion string `json:"minExtensionVersion"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.App != "VKMDownload" || body.Version == "" || body.Port == 0 {
		t.Errorf("existing fields changed: %+v", body)
	}
	if body.Protocol != extensionProtocol {
		t.Errorf("protocol = %d, want %d", body.Protocol, extensionProtocol)
	}
	if body.MinExtensionVersion != minExtensionVersion {
		t.Errorf("minExtensionVersion = %q, want %q", body.MinExtensionVersion, minExtensionVersion)
	}
}

// TestOriginLockOffAcceptsAnyExtensionOrigin pins the default: with no lock any
// browser extension origin still gets through, which is what the extension
// shipped against today expects.
func TestOriginLockOffAcceptsAnyExtensionOrigin(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	for _, origin := range []string{
		"chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh",
		"moz-extension://11111111-2222-3333-4444-555555555555",
	} {
		resp := extRequest(t, base+"/settings", origin)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("origin %s: status = %d, want 200", origin, resp.StatusCode)
		}
	}
}

// TestOriginLockAcceptsOnlyTheStoredOrigin covers the opt-in lock: an exact
// string match is admitted and every other extension is refused, with the
// refusal recorded for the settings page's diagnostics.
func TestOriginLockAcceptsOnlyTheStoredOrigin(t *testing.T) {
	const locked = "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh"

	app := newTestApp(t)
	cfg := app.Config()
	cfg.ExtensionOriginLock = locked
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	base := startAPI(t, app)

	cases := []struct {
		origin string
		want   int
	}{
		{locked, http.StatusOK},
		{"chrome-extension://aaaabbbbccccddddeeeeffffggggiiii", http.StatusForbidden},
		// Exact comparison: a trailing slash is a different string, and the
		// browser never sends one.
		{locked + "/", http.StatusForbidden},
		{"moz-extension://11111111-2222-3333-4444-555555555555", http.StatusForbidden},
		// A web page is refused by the older prefix rule, not the lock.
		{"https://evil.example", http.StatusForbidden},
	}
	for _, c := range cases {
		resp := extRequest(t, base+"/settings", c.origin)
		resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("origin %q: status = %d, want %d", c.origin, resp.StatusCode, c.want)
		}
	}

	// A locked-out extension cannot hand over a download even with a valid
	// pairing proof, and the attempt is counted against it.
	resp := postJSON(t, base+"/add", "chrome-extension://aaaabbbbccccddddeeeeffffggggiiii",
		`{"url":"http://127.0.0.1:1/x.bin","filename":"x.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("locked-out /add status = %d, want 403", resp.StatusCode)
	}
	if n := len(app.mgr.List()); n != 0 {
		t.Errorf("%d tasks created by a locked-out extension", n)
	}
	if st := app.ExtensionStatus(); st.Accepted != 0 || st.Rejected != 1 {
		t.Errorf("counters = accepted %d rejected %d, want 0/1", st.Accepted, st.Rejected)
	}

	diags := app.PeerDiagnostics()
	if len(diags) != 4 {
		t.Fatalf("diagnostics = %+v, want one per locked-out request (4)", diags)
	}
	for _, d := range diags {
		if d.Reason != peerReasonOriginLocked {
			t.Errorf("diagnostic reason = %q, want %q", d.Reason, peerReasonOriginLocked)
		}
		if d.Exe != "" {
			t.Errorf("diagnostic exe = %q, want empty (no executable was involved)", d.Exe)
		}
	}
}

// TestExtensionStatusCountsAcceptedAndRejected covers the counters and the last
// origin: reads are seen but not counted, while /add and /pair outcomes are.
func TestExtensionStatusCountsAcceptedAndRejected(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	base := startAPI(t, app)
	const origin = "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh"

	if st := app.ExtensionStatus(); !st.LastSeen.IsZero() || st.Accepted != 0 || st.Rejected != 0 {
		t.Fatalf("fresh app already has extension stats: %+v", st)
	}

	// A read records who is talking but is not an attempt.
	resp := extRequest(t, base+"/settings", origin)
	resp.Body.Close()
	st := app.ExtensionStatus()
	if st.LastOrigin != origin {
		t.Errorf("LastOrigin = %q, want %q", st.LastOrigin, origin)
	}
	if st.LastSeen.IsZero() {
		t.Error("LastSeen was not recorded")
	}
	if st.Accepted != 0 || st.Rejected != 0 {
		t.Errorf("a read was counted as an attempt: %+v", st)
	}

	// Accepted handover.
	resp = postJSON(t, base+"/add", origin, `{"url":"http://127.0.0.1:1/a.bin","filename":"a.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/add status = %d, want 200", resp.StatusCode)
	}

	// Rejected handover (malformed body, so the handler answers 400).
	resp = postJSON(t, base+"/add", origin, `{not json`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("/add malformed status = %d, want 400", resp.StatusCode)
	}

	// Accepted pairing, then a refused one.
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, true, nil)
	resp = extRequest(t, base+"/pair", origin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/pair status = %d, want 200", resp.StatusCode)
	}
	stubPeer(t, `C:\fakepair-vkm\curl.exe`, true, nil)
	resp = extRequest(t, base+"/pair", origin)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("/pair refused status = %d, want 403", resp.StatusCode)
	}

	st = app.ExtensionStatus()
	if st.Accepted != 2 || st.Rejected != 2 {
		t.Errorf("counters = accepted %d rejected %d, want 2/2", st.Accepted, st.Rejected)
	}
	if st.LastOrigin != origin {
		t.Errorf("LastOrigin = %q, want %q", st.LastOrigin, origin)
	}
}

// TestExtensionVersionMismatchIsAdvisory covers the version handshake: an older
// extension is flagged on the status but never refused a download, and an
// extension that sends no header at all keeps working.
func TestExtensionVersionMismatchIsAdvisory(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	base := startAPI(t, app)
	const origin = "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh"

	resp := addWithVersion(t, base, origin, "1.0.2", `{"url":"http://127.0.0.1:1/a.bin","filename":"a.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an older extension was refused: status = %d, want 200", resp.StatusCode)
	}
	st := app.ExtensionStatus()
	if st.ExtVersion != "1.0.2" {
		t.Errorf("ExtVersion = %q, want 1.0.2", st.ExtVersion)
	}
	if !st.VersionMismatch {
		t.Error("VersionMismatch = false for 1.0.2, which is older than " + minExtensionVersion)
	}

	resp = addWithVersion(t, base, origin, "1.1.0", `{"url":"http://127.0.0.1:1/b.bin","filename":"b.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	st = app.ExtensionStatus()
	if st.ExtVersion != "1.1.0" || st.VersionMismatch {
		t.Errorf("status = %q mismatch %v, want 1.1.0/false", st.ExtVersion, st.VersionMismatch)
	}

	// No header: an extension from before this handshake. Accepted, and the
	// last known version is left alone rather than cleared.
	resp = addWithVersion(t, base, origin, "", `{"url":"http://127.0.0.1:1/c.bin","filename":"c.bin"}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("an extension without a version header was refused: %d", resp.StatusCode)
	}
	if st := app.ExtensionStatus(); st.ExtVersion != "1.1.0" || st.VersionMismatch {
		t.Errorf("status = %q mismatch %v, want the previous verdict kept", st.ExtVersion, st.VersionMismatch)
	}
}

// TestCompareVersions pins the comparator the mismatch verdict rests on.
func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.2", "1.0.3", -1},
		{"1.0.3", "1.0.3", 0},
		{"1.0.10", "1.0.3", 1},
		{"1.0", "1.0.3", -1},
		{"2", "1.9.9", 1},
		{"1.0.3-beta", "1.0.3", 0},
		{"", "1.0.3", -1},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// TestStatusRecorderIsTransparent pins the wrapper the guard puts around /add
// and /pair: headers and body must reach the real writer unchanged, and the
// status a handler wrote (or the implicit 200 when it wrote only a body) must be
// what the counters see. No other path is wrapped, so CORS headers, statuses and
// body writes on reads keep going straight to the connection.
func TestStatusRecorderIsTransparent(t *testing.T) {
	t.Run("explicit status", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sr := &statusRecorder{ResponseWriter: rec}

		sr.Header().Set("Access-Control-Allow-Origin", "chrome-extension://abc")
		sr.WriteHeader(http.StatusForbidden)
		if _, err := sr.Write([]byte("no")); err != nil {
			t.Fatal(err)
		}

		if got := sr.Status(); got != http.StatusForbidden {
			t.Errorf("Status() = %d, want 403", got)
		}
		if rec.Code != http.StatusForbidden {
			t.Errorf("underlying status = %d, want 403", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "chrome-extension://abc" {
			t.Errorf("headers were not passed through: %q", got)
		}
		if got := rec.Body.String(); got != "no" {
			t.Errorf("body = %q, want %q", got, "no")
		}
	})

	t.Run("implicit 200", func(t *testing.T) {
		rec := httptest.NewRecorder()
		sr := &statusRecorder{ResponseWriter: rec}

		if _, err := sr.Write([]byte("{}")); err != nil {
			t.Fatal(err)
		}
		if got := sr.Status(); got != http.StatusOK {
			t.Errorf("Status() = %d, want 200 for a body-only handler", got)
		}
		if rec.Code != http.StatusOK {
			t.Errorf("underlying status = %d, want 200", rec.Code)
		}
	})
}
