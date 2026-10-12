package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// startAPI launches the loopback endpoint and waits for it to accept requests.
func startAPI(t *testing.T, app *App) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	app.startLocalAPI(ctx)

	port := 0
	waitFor(t, "the local api to listen", 5*time.Second, func() bool {
		port = app.apiPort()
		return port > 0
	})
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// postJSON sends a request with the Origin a browser extension would use.
func postJSON(t *testing.T, url, origin, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	return resp
}

// TestLocalAPIPingIdentifiesTheApp is what the extension uses to find the app
// without guessing whether it is running.
func TestLocalAPIPingIdentifiesTheApp(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatalf("GET /ping: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		App     string `json:"app"`
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.App != "VKMDownload" {
		t.Errorf("app = %q, want VKMDownload", body.App)
	}
	if body.Version == "" {
		t.Error("version is empty")
	}
}

// TestLocalAPIAddCreatesTask covers the extension's main call.
func TestLocalAPIAddCreatesTask(t *testing.T) {
	app := newTestApp(t)
	// Ask-location would hold the arrival for placement instead of creating.
	disableAskLocation(t, app)
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop",
		`{"url":"http://127.0.0.1:1/from-extension.bin","filename":"from-extension.bin"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID == "" {
		t.Fatal("no task id returned")
	}
	if body.Name != "from-extension.bin" {
		t.Errorf("name = %q, want from-extension.bin", body.Name)
	}

	// It has to be in the list, not merely acknowledged.
	v, ok := find(app.mgr, body.ID)
	if !ok {
		t.Fatal("the task is not in the manager")
	}
	if v.URL != "http://127.0.0.1:1/from-extension.bin" {
		t.Errorf("url = %q", v.URL)
	}
}

// disableAskLocation pins the pre-ask behaviour for tests that cover direct
// creation: with the default on, arrivals wait for placement instead.
func disableAskLocation(t *testing.T, app *App) {
	t.Helper()
	cfg := app.Config()
	cfg.AskLocation = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
}

// TestLocalAPIAddAsksForLocation covers the ask-location flow: with the
// default on, the extension's arrival creates nothing — the front-end opens
// the placement dialog, and closing that dialog abandons the download.
func TestLocalAPIAddAsksForLocation(t *testing.T) {
	app := newTestApp(t)

	var mu sync.Mutex
	var events []string
	app.emit = func(name string, _ ...any) {
		mu.Lock()
		events = append(events, name)
		mu.Unlock()
	}
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop",
		`{"url":"http://127.0.0.1:1/wait.bin","filename":"wait.bin"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Pending bool `json:"pending"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.Pending {
		t.Error("pending = false, want the arrival held for placement")
	}
	if n := len(app.mgr.List()); n != 0 {
		t.Errorf("%d tasks created, want none before placement", n)
	}

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, name := range events {
		if name == "location:ask" {
			found = true
		}
	}
	if !found {
		t.Errorf("events = %v, want a location:ask", events)
	}
}

// TestLocalAPIRejectsAPage is the security boundary. A cross-origin POST from a
// web page is delivered even though the page cannot read the reply, so without
// this check any site the user visits could fill their download queue.
func TestLocalAPIRejectsAPage(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "https://evil.example",
		`{"url":"http://127.0.0.1:1/planted.bin"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
	if len(app.mgr.List()) != 0 {
		t.Error("a task was created for a request from a web page")
	}
}

// TestLocalAPIAcceptsAnyExtensionOrigin checks the origin test is not hard-coded
// to one browser: the same extension runs under Brave, Chrome, Edge and Opera,
// and each reports its own scheme.
func TestLocalAPIAcceptsAnyExtensionOrigin(t *testing.T) {
	for _, origin := range []string{
		"chrome-extension://aaaabbbbccccddddeeeeffffgggghhhh",
		"moz-extension://11111111-2222-3333-4444-555555555555",
	} {
		app := newTestApp(t)
		base := startAPI(t, app)

		resp := postJSON(t, base+"/add", origin, `{"url":"http://127.0.0.1:1/a.bin"}`)
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("origin %s: status = %d, want 200", origin, resp.StatusCode)
		}
	}
}

// TestLocalAPIRejectsMissingURL covers the empty case.
func TestLocalAPIRejectsMissingURL(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abc", `{"filename":"x.bin"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestLocalAPIPreflightAnswers checks the CORS preflight, without which the
// browser never sends the real request.
func TestLocalAPIPreflightAnswers(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	req, err := http.NewRequest(http.MethodOptions, base+"/add", bytes.NewReader(nil))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "chrome-extension://abc")
	req.Header.Set("Access-Control-Request-Method", "POST")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("OPTIONS: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}

// TestSanitizeSuggestedName covers the name a remote server can influence.
// Taking it at face value would let a server choose where the file lands.
func TestSanitizeSuggestedName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"report.pdf", "report.pdf"},
		{"  spaced.bin  ", "spaced.bin"},
		{"", ""},
		{"   ", ""},
		{"..", ""},
		{".", ""},
		{`..\..\evil.exe`, "evil.exe"},
		{"../../etc/passwd", "passwd"},
		{`C:\Windows\System32\drivers\etc\hosts`, "hosts"},
		{"/absolute/path/file.zip", "file.zip"},
		{`a\b/c\d.bin`, "d.bin"},
	}
	for _, c := range cases {
		if got := sanitizeSuggestedName(c.in); got != c.want {
			t.Errorf("sanitizeSuggestedName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLocalAPISettingsRoundTrip covers the switch living in the app: the
// extension's popup reads it and writes it back, so both directions have to work
// or the popup and the settings page would disagree.
func TestLocalAPISettingsRoundTrip(t *testing.T) {
	app := newTestApp(t)
	base := startAPI(t, app)

	type policy struct {
		Enabled   bool     `json:"enabled"`
		MinSizeKB int      `json:"minSizeKb"`
		SkipHosts []string `json:"skipHosts"`
	}

	read := func() policy {
		t.Helper()
		resp, err := http.Get(base + "/settings")
		if err != nil {
			t.Fatalf("GET /settings: %v", err)
		}
		defer resp.Body.Close()
		var p policy
		if err := json.NewDecoder(resp.Body).Decode(&p); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return p
	}

	// A fresh install takes over downloads, but only ones worth a separate
	// program.
	got := read()
	if !got.Enabled {
		t.Error("interception should default to on")
	}
	if got.MinSizeKB != 1024 {
		t.Errorf("minSizeKb = %d, want 1024", got.MinSizeKB)
	}

	// Turning it off must persist, and must not disturb the size limit.
	resp := postJSON(t, base+"/settings", "chrome-extension://abc", `{"enabled":false}`)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST status = %d, want 200", resp.StatusCode)
	}

	got = read()
	if got.Enabled {
		t.Error("enabled is still true after being switched off")
	}
	if got.MinSizeKB != 1024 {
		t.Errorf("minSizeKb = %d, want it left alone at 1024", got.MinSizeKB)
	}

	// And the change survives a reload from disk, which is what makes it a
	// setting rather than a session flag.
	if loadConfig(app.configPath()).ExtensionEnabled {
		t.Error("the switch did not survive a reload")
	}
}

// TestPolicyViewSplitsSkipHosts covers the parsing rules, which live in the app
// precisely so they are defined once and testable.
func TestPolicyViewSplitsSkipHosts(t *testing.T) {
	app := newTestApp(t)

	cfg := app.Config()
	cfg.ExtensionSkipHosts = "example.com, WWW.Another.org\n\n  third.net  ;fourth.io\r\n"
	if err := app.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}

	view := app.policyView()
	hosts, ok := view["skipHosts"].([]string)
	if !ok {
		t.Fatalf("skipHosts = %T, want []string", view["skipHosts"])
	}

	want := map[string]bool{
		"example.com": true, "another.org": true, "third.net": true, "fourth.io": true,
	}
	if len(hosts) != len(want) {
		t.Fatalf("hosts = %v, want %d entries", hosts, len(want))
	}
	for _, host := range hosts {
		if !want[host] {
			t.Errorf("unexpected host %q", host)
		}
	}
}

func TestLocalAPIAddKeepsTheFilenameInTheDownloadFolder(t *testing.T) {
	app := newTestApp(t)
	// Ask-location would hold the arrival for placement instead of creating.
	disableAskLocation(t, app)
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abc",
		`{"url":"http://127.0.0.1:1/x.bin","filename":"../../escaped.bin"}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)

	v, ok := find(app.mgr, body.ID)
	if !ok {
		t.Fatal("task missing")
	}
	dir := app.Config().DownloadDir
	if !strings.HasPrefix(v.Dest, dir) {
		t.Errorf("dest = %q, want it inside %q", v.Dest, dir)
	}
	if strings.Contains(v.Dest, "..") {
		t.Errorf("dest = %q still contains a traversal", v.Dest)
	}
}
