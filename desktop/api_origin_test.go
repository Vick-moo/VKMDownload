package main

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

// fetchWithoutOrigin builds the request shape Chrome 155 sends for an
// extension's own fetch: no Origin header, `Sec-Fetch-Dest: empty`,
// `Sec-Fetch-Site: none`.
//
// The parameters let a test describe the other shapes a browser produces — an
// address-bar navigation (`document`), an image, or a page's own fetch
// (`empty` + `cross-site`).
func fetchWithoutOrigin(t *testing.T, method, url, body, dest, site string) *http.Request {
	t.Helper()
	var req *http.Request
	var err error
	if body == "" {
		req, err = http.NewRequest(method, url, nil)
	} else {
		req, err = http.NewRequest(method, url, strings.NewReader(body))
	}
	if err != nil {
		t.Fatalf("build %s %s: %v", method, url, err)
	}
	if dest != "" {
		req.Header.Set(secFetchDest, dest)
	}
	if site != "" {
		req.Header.Set(secFetchSite, site)
	}
	return req
}

func doRequest(t *testing.T, req *http.Request) *http.Response {
	t.Helper()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return resp
}

// TestGuardAcceptsChromeExtensionFetchWithoutOrigin is the regression this fix
// exists for: current Chrome sends no Origin on the extension's own fetch (only
// `Sec-Fetch-Site: none` + `Sec-Fetch-Dest: empty`), and /pair and /add used to
// answer 403 to every one of them.
func TestGuardAcceptsChromeExtensionFetchWithoutOrigin(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	base := startAPI(t, app)
	// /pair always runs the peer check, regardless of VerifyPeerProcess.
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, true, nil)

	t.Run("/pair reaches the handler", func(t *testing.T) {
		req := fetchWithoutOrigin(t, http.MethodGet, base+"/pair", "", "empty", "none")
		req.Header.Set("Sec-Fetch-Mode", "cors")
		resp := doRequest(t, req)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a Chrome 155 extension fetch must reach /pair)", resp.StatusCode)
		}
		var body struct {
			Key string `json:"key"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(body.Key) != 64 {
			t.Errorf("key length = %d, want 64", len(body.Key))
		}
		// Nothing to echo: an extension fetch needs no CORS for a host-permission
		// request, and a page never reaches this point.
		if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.Errorf("Access-Control-Allow-Origin = %q for a request without Origin, want it unset", got)
		}
	})

	t.Run("/add reaches the HMAC check", func(t *testing.T) {
		req := fetchWithoutOrigin(t, http.MethodPost, base+"/add",
			`{"url":"http://127.0.0.1:1/no-origin.bin","filename":"no-origin.bin"}`, "empty", "none")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(authHeader, signForApp(t, http.MethodPost, "/add"))
		resp := doRequest(t, req)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200 (a signed extension fetch must be accepted)", resp.StatusCode)
		}
		var body struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, ok := find(app.mgr, body.ID); !ok {
			t.Errorf("task %q was not created", body.ID)
		}
	})

	t.Run("/add still demands the pairing proof", func(t *testing.T) {
		req := fetchWithoutOrigin(t, http.MethodPost, base+"/add",
			`{"url":"http://127.0.0.1:1/no-proof.bin"}`, "empty", "none")
		req.Header.Set("Content-Type", "application/json")
		resp := doRequest(t, req)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 without a proof", resp.StatusCode)
		}
		var body struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&body)
		if !strings.Contains(body.Error, "not authenticated") {
			t.Errorf("error = %q, want the HMAC refusal (the request must reach that check, not the shape gate)", body.Error)
		}
	})
}

// TestGuardRejectsNavigationAndSubresourcesWithoutOrigin keeps the browser-side
// distinction: with no Origin, only an extension fetch (empty destination, no
// initiating site) is admitted. An address-bar load, an iframe or an <img> is
// not an extension.
func TestGuardRejectsNavigationAndSubresourcesWithoutOrigin(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	cases := []struct {
		name string
		dest string
		site string
	}{
		{"address bar", "document", "none"},
		{"iframe", "iframe", "cross-site"},
		{"image", "image", "cross-site"},
		{"script", "script", "cross-site"},
		{"page fetch", "empty", "cross-site"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := fetchWithoutOrigin(t, http.MethodGet, base+"/pair", "", c.dest, c.site)
			resp := doRequest(t, req)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403 for dest=%q site=%q", resp.StatusCode, c.dest, c.site)
			}
		})
	}
}

// TestGuardStillRejectsAWebPage covers the unchanged branch: an Origin that is
// not an extension is refused, whatever the fetch metadata says.
func TestGuardStillRejectsAWebPage(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	for _, path := range []string{"/pair", "/settings", "/ping"} {
		req, err := http.NewRequest(http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set(secFetchDest, "empty")
		req.Header.Set(secFetchSite, "cross-site")
		resp := doRequest(t, req)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("GET %s from a page = %d, want 403", path, resp.StatusCode)
		}
	}
}

// TestGuardKeepsExtensionOriginBehaviour pins that a real extension Origin is
// still admitted (and echoed), so the fix only adds the missing shape.
func TestGuardKeepsExtensionOriginBehaviour(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	base := startAPI(t, app)
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, true, nil)

	resp := extRequest(t, base+"/settings", "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings with an extension Origin = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the extension origin echoed", got)
	}

	resp = extRequest(t, base+"/pair", "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /pair with an extension Origin = %d, want 200", resp.StatusCode)
	}
}

// TestGuardLockedAppRejectsRequestsWithoutOrigin covers the lock's new branch: a
// request with no Origin cannot name an extension, so a locked app refuses it and
// records why. With the lock removed the same request is accepted again, which
// proves the lock — not something else — was the reason.
func TestGuardLockedAppRejectsRequestsWithoutOrigin(t *testing.T) {
	const locked = "chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh"

	app := newTestApp(t)
	disableAskLocation(t, app)
	cfg := app.Config()
	cfg.ExtensionOriginLock = locked
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	base := startAPI(t, app)

	add := func(t *testing.T) *http.Response {
		t.Helper()
		req := fetchWithoutOrigin(t, http.MethodPost, base+"/add",
			`{"url":"http://127.0.0.1:1/locked.bin","filename":"locked.bin"}`, "empty", "none")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(authHeader, signForApp(t, http.MethodPost, "/add"))
		return doRequest(t, req)
	}

	resp := add(t)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 while locked and no Origin", resp.StatusCode)
	}
	if !strings.Contains(string(body), "Origin") {
		t.Errorf("error body = %q, want it to name the missing Origin header", body)
	}
	if n := len(app.mgr.List()); n != 0 {
		t.Errorf("%d task(s) created by a request the lock refused", n)
	}

	diags := app.PeerDiagnostics()
	if len(diags) != 1 {
		t.Fatalf("diagnostics = %+v, want one entry", diags)
	}
	if diags[0].Reason != peerReasonOriginMissing {
		t.Errorf("diagnostic reason = %q, want %q", diags[0].Reason, peerReasonOriginMissing)
	}

	// Unlock: the identical request now gets through, so the refusal above came
	// from the lock and not from the missing Origin alone.
	cfg = app.Config()
	cfg.ExtensionOriginLock = ""
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	resp = add(t)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d after unlocking, want 200", resp.StatusCode)
	}
}

// TestGuardLogsWhyARequestWasRejected captures the new diagnostics: every
// refusal leaves method, path, Origin (or <none>) and the fetch metadata in the
// debug log, so the next report can be answered from the log.
func TestGuardLogsWhyARequestWasRejected(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	req, err := http.NewRequest(http.MethodGet, base+"/pair", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set(secFetchDest, "empty")
	req.Header.Set(secFetchSite, "cross-site")
	resp := doRequest(t, req)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}

	logText := readFileString(t, filepath.Join(app.dataDir(), "vkmdownload.log"))
	for _, want := range []string{
		"local api rejected GET /pair",
		"not a browser-extension request",
		"origin=",
		"evil.example",
		`dest="empty"`,
		`site="cross-site"`,
	} {
		if !strings.Contains(logText, want) {
			t.Errorf("debug log is missing %q:\n%s", want, logText)
		}
	}
}
