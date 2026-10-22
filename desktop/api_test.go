package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"vkmdownload/testsrv"
)

// startAPI launches the loopback endpoint and waits for it to accept requests.
// currentTestApp is the app instance whose local API the helpers are driving.
// It exists so postJSON can sign requests without every call site passing the
// app through; the package does not run tests in parallel.
var currentTestApp *App

// signForApp produces the auth header for a request to the app, using
// the app instance the test is driving. It reads the key through the same path
// production code does, so a broken key store shows up here too.
func signForApp(t *testing.T, method, path string) string {
	t.Helper()
	if currentTestApp == nil {
		return ""
	}
	key, err := currentTestApp.pairKeyForThisInstall()
	if err != nil || key == "" {
		return ""
	}
	return signedRequest(key, method, path, time.Now())
}

func startAPI(t *testing.T, app *App) string {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	currentTestApp = app
	app.startLocalAPI(ctx)

	port := 0
	waitFor(t, "the local api to listen", 5*time.Second, func() bool {
		port = app.apiPort()
		return port > 0
	})
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// postJSON sends a request with the Origin a browser extension would use.
// postJSON sends a POST the way the extension does: with an Origin and with the
// pairing proof. Mutations require both now, because an Origin alone says "some
// extension" and /add takes caller-supplied cookies and URLs.
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
	if h := signForApp(t, req.Method, req.URL.Path); h != "" {
		req.Header.Set(authHeader, h)
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

// TestLocalAPIAddForwardsBrowserCookies covers the login-session handover end
// to end: the extension posts the browser's Cookie value with /add, and the
// task replays it on every engine request. A cookie-gated testsrv stands in
// for a Google Classroom private file — without the cookie it answers 403,
// with it the download completes byte-complete.
func TestLocalAPIAddForwardsBrowserCookies(t *testing.T) {
	const size = 256 << 10

	cfg := testsrv.DefaultConfig(size)
	cfg.RequireCookie = "s3cret"
	srv := testsrv.New(cfg)
	srvBase, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start testsrv: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	fileURL := srvBase + "/file"

	// add hands the URL to the app the way the extension does and waits for
	// the task to settle, returning its final status and destination.
	add := func(t *testing.T, cookie string) (TaskStatus, string) {
		t.Helper()
		app := newTestApp(t)
		// Ask-location would hold the arrival for placement instead: that
		// path is covered by TestLocalAPIAddAsksForLocation.
		disableAskLocation(t, app)
		c := app.Config()
		c.DownloadDir = t.TempDir()
		if err := app.SetConfig(c); err != nil {
			t.Fatalf("SetConfig: %v", err)
		}
		base := startAPI(t, app)

		body, _ := json.Marshal(map[string]string{
			"url":      fileURL,
			"filename": "cookie.bin",
			"cookie":   cookie,
		})
		resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop", string(body))
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		var got struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got.ID == "" {
			t.Fatal("no task id returned")
		}
		waitFor(t, "task to settle", 60*time.Second, func() bool {
			v, ok := find(app.mgr, got.ID)
			return ok && (v.Status == StatusCompleted || v.Status == StatusFailed)
		})
		v, _ := find(app.mgr, got.ID)
		return v.Status, v.Dest
	}

	t.Run("with cookie completes", func(t *testing.T) {
		status, dest := add(t, "session=s3cret")
		if status != StatusCompleted {
			t.Fatalf("status = %s, want completed", status)
		}
		st, err := os.Stat(dest)
		if err != nil {
			t.Fatalf("stat downloaded file: %v", err)
		}
		if st.Size() != size {
			t.Errorf("size = %d, want %d", st.Size(), size)
		}
	})

	t.Run("without cookie fails", func(t *testing.T) {
		if status, _ := add(t, ""); status != StatusFailed {
			t.Fatalf("status = %s, want failed without the cookie", status)
		}
	})
}

// TestLocalAPIAddDropsBrowserCookiesWhenTheSwitchIsOff covers the app-side half
// of extensionSendCookies.
//
// The extension honours the switch too, but one installed before the switch
// existed does not — it keeps sending the browser's cookies. Dropping them here
// is what makes "do not hand over my login" true for every extension version.
// The download must still be created: it simply arrives without a login session,
// exactly as if the user were not signed in.
func TestLocalAPIAddDropsBrowserCookiesWhenTheSwitchIsOff(t *testing.T) {
	cases := []struct {
		name string
		send bool
		want string
	}{
		{"switch off drops the cookie", false, ""},
		{"switch on keeps the cookie", true, "session=s3cret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := newTestApp(t)
			// Ask-location would park the request instead of creating a task.
			disableAskLocation(t, app)
			cfg := app.Config()
			cfg.ExtensionSendCookies = c.send
			// No transfer: the task only has to exist so its stored cookie can
			// be inspected.
			cfg.AutoStart = false
			if err := app.SetConfig(cfg); err != nil {
				t.Fatalf("SetConfig: %v", err)
			}
			base := startAPI(t, app)

			resp := postJSON(t, base+"/add", "chrome-extension://abc",
				`{"url":"http://127.0.0.1:1/private.bin","filename":"private.bin","cookie":"session=s3cret"}`)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200 (the handover must not fail)", resp.StatusCode)
			}
			var body struct {
				ID string `json:"id"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				t.Fatalf("decode: %v", err)
			}

			tk := app.mgr.get(body.ID)
			if tk == nil {
				t.Fatalf("task %q was not created", body.ID)
			}
			tk.mu.Lock()
			got := tk.cookie
			tk.mu.Unlock()
			if got != c.want {
				t.Errorf("stored cookie = %q, want %q", got, c.want)
			}
		})
	}
}

// TestLocalAPIAddStillCapsTheCookie pins the existing guard: dropping the value
// when the switch is off must not turn /add into a place to post an arbitrarily
// large body.
func TestLocalAPIAddStillCapsTheCookie(t *testing.T) {
	app := newTestApp(t)
	disableAskLocation(t, app)
	cfg := app.Config()
	cfg.ExtensionSendCookies = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	base := startAPI(t, app)

	big := strings.Repeat("a", (32<<10)+1)
	resp := postJSON(t, base+"/add", "chrome-extension://abc",
		`{"url":"http://127.0.0.1:1/x.bin","cookie":"`+big+`"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an oversized cookie", resp.StatusCode)
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
	// The header echoes the caller's origin rather than "*". Only an extension
	// origin reaches this point, so * was not exploitable, but echoing keeps a
	// future guard regression from turning into a readable reply.
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "chrome-extension://abc" {
		t.Errorf("Access-Control-Allow-Origin = %q, want the caller's origin", got)
	}
	if got := resp.Header.Get("Vary"); got != "Origin" {
		t.Errorf("Vary = %q, want Origin", got)
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

// TestLocalAPIAddAskNamesAnUnnamedHandover covers the extension handing over a
// download with no filename. That is what Chrome does when the handover wins
// the race with the response headers, and it showed up as a blank filename field
// in the placement dialog from the second browser download onwards. The ask
// branch creates no task, so there is no probe to fill the name in later: it has
// to derive a placeholder from the URL itself.
func TestLocalAPIAddAskNamesAnUnnamedHandover(t *testing.T) {
	cases := []struct{ name, url, want string }{
		{"last path segment", "http://127.0.0.1:1/dl/from-url.bin", "from-url.bin"},
		{"query string is not part of the name", "http://127.0.0.1:1/dl/a.zip?token=abc", "a.zip"},
		{"no extension gets a placeholder", "http://127.0.0.1:1/api/file/123", "download"},
		{"trailing slash gets a placeholder", "http://127.0.0.1:1/", "download"},
		{"bare host gets a placeholder", "http://127.0.0.1:1", "download"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := newTestApp(t)

			var mu sync.Mutex
			var ask map[string]string
			app.emit = func(name string, data ...any) {
				if name != "location:ask" {
					return
				}
				mu.Lock()
				defer mu.Unlock()
				if len(data) > 0 {
					if payload, ok := data[0].(map[string]string); ok {
						ask = payload
					}
				}
			}
			base := startAPI(t, app)

			resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop",
				fmt.Sprintf(`{"url":%q,"filename":""}`, c.url))
			defer resp.Body.Close()

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, want 200", resp.StatusCode)
			}
			var out struct {
				Pending bool   `json:"pending"`
				Name    string `json:"name"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !out.Pending {
				t.Error("pending = false, want the arrival held for placement")
			}
			// The dialog reads the event payload, but the reply carries the
			// name too (the extension shows it in its notification). Both have
			// to be usable, or one of the two surfaces goes blank again.
			if out.Name != c.want {
				t.Errorf("reply name = %q, want %q", out.Name, c.want)
			}
			mu.Lock()
			defer mu.Unlock()
			if ask == nil {
				t.Fatal("no location:ask payload captured")
			}
			if got := ask["filename"]; got != c.want {
				t.Errorf("location:ask filename = %q, want %q", got, c.want)
			}
		})
	}
}

// TestDefaultDestFor pins the folder the placement dialog pre-fills when
// "organize by type" is on. The dialog has to ask for the group folder up front:
// a task created there is Manual, so the organizer leaves it alone afterwards,
// and pre-filling the download directory would silently lose the sorting the
// user asked for.
func TestDefaultDestFor(t *testing.T) {
	app := newTestApp(t)
	dl := filepath.Join(t.TempDir(), "Downloads")
	root := filepath.Join(t.TempDir(), "Sorted")
	abs := filepath.Join(t.TempDir(), "Papers")

	cfg := app.Config()
	cfg.DownloadDir = dl
	cfg.OrganizeEnabled = true
	cfg.OrganizeDir = root
	cfg.OrganizeGroups = []OrganizeGroup{
		{Name: "Archives", Exts: []string{"zip", "7z"}},
		{Name: "Papers", Exts: []string{"pdf"}, Dir: abs},
	}
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	if got, want := app.DefaultDestFor("backup.zip"), filepath.Join(root, "Archives"); got != want {
		t.Errorf("grouped extension: got %q, want %q", got, want)
	}
	// A group with an absolute Dir wins over the organize root.
	if got := app.DefaultDestFor("paper.pdf"); got != abs {
		t.Errorf("absolute group dir: got %q, want %q", got, abs)
	}
	// Unknown extension: no group claims it, so it stays in the download folder.
	if got := app.DefaultDestFor("notes.xyz"); got != dl {
		t.Errorf("unknown extension: got %q, want %q", got, dl)
	}
	// An extension-less name cannot match a group either.
	if got := app.DefaultDestFor("README"); got != dl {
		t.Errorf("no extension: got %q, want %q", got, dl)
	}
	// No name at all: nothing to classify, so no guessing.
	if got := app.DefaultDestFor(""); got != dl {
		t.Errorf("empty name: got %q, want %q", got, dl)
	}

	// The switch is what makes the group table relevant at all.
	cfg = app.Config()
	cfg.OrganizeEnabled = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if got := app.DefaultDestFor("backup.zip"); got != dl {
		t.Errorf("organizing off: got %q, want %q", got, dl)
	}
}

// TestLocalAPISettingsAreReadOnlyForTheExtension covers the access change: the
// extension may read the interception policy, but only the app itself may change
// it.
//
// The endpoint used to accept enabled/minSizeKb from the extension's popup. Any
// installed extension that cleared the origin and peer checks could therefore
// switch interception off — or change the size filter, which changes what gets
// collected — from a surface the user never opened.
func TestLocalAPISettingsAreReadOnlyForTheExtension(t *testing.T) {
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

	// The read side is unchanged: a fresh install takes over downloads, but only
	// ones worth a separate program.
	got := read()
	if !got.Enabled {
		t.Error("interception should default to on")
	}
	if got.MinSizeKB != 1024 {
		t.Errorf("minSizeKb = %d, want 1024", got.MinSizeKB)
	}

	// Writing is refused, with a reason the popup can show verbatim.
	resp := postJSON(t, base+"/settings", "chrome-extension://abc", `{"enabled":false}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d, want 405", resp.StatusCode)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !strings.Contains(body.Error, "设置只能由应用自身修改") {
		t.Errorf("error = %q, want the read-only explanation", body.Error)
	}

	// And the policy really is untouched, in memory and on disk.
	got = read()
	if !got.Enabled {
		t.Error("enabled flipped although the write was refused")
	}
	if got.MinSizeKB != 1024 {
		t.Errorf("minSizeKb = %d, want it left alone at 1024", got.MinSizeKB)
	}
	if !loadConfig(app.configPath(), nil).ExtensionEnabled {
		t.Error("the refused write still reached config.json")
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

// TestPolicyViewCarriesExtensionSwitches covers the two behavioural switches the
// extension reads. Both are sent even when false, so the extension can tell
// "switched off" from "an app too old to have the field" — the two have
// different defaults on the extension side.
func TestPolicyViewCarriesExtensionSwitches(t *testing.T) {
	app := newTestApp(t)

	view := app.policyView()
	if got, ok := view["sendCookies"].(bool); !ok || !got {
		t.Errorf("sendCookies = %v (present=%v), want the default true", view["sendCookies"], ok)
	}
	if got, ok := view["incognito"].(bool); !ok || got {
		t.Errorf("incognito = %v (present=%v), want the default false", view["incognito"], ok)
	}

	cfg := app.Config()
	cfg.ExtensionSendCookies = false
	cfg.ExtensionIncognito = true
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}

	view = app.policyView()
	if got, ok := view["sendCookies"].(bool); !ok || got {
		t.Errorf("sendCookies = %v (present=%v), want false", view["sendCookies"], ok)
	}
	if got, ok := view["incognito"].(bool); !ok || !got {
		t.Errorf("incognito = %v (present=%v), want true", view["incognito"], ok)
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

// TestLocalAPIAddCarriesSizeBeforeStarting covers the handover's size: the
// browser already read Content-Length, so the row shows the total while the
// task is still paused instead of reading "0 B" until a transfer begins.
func TestLocalAPIAddCarriesSizeBeforeStarting(t *testing.T) {
	const wantSize int64 = 3145728

	app := newTestApp(t)
	// Ask-location would hold the arrival for placement instead of creating.
	disableAskLocation(t, app)
	// The size is observable on the row before any transfer starts, so the
	// direct-add scenario is the one where nothing auto-starts: with auto-start
	// on, the task leaves paused before the reply is even read.
	cfg := app.Config()
	cfg.AutoStart = false
	if err := app.SetConfig(cfg); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop",
		`{"url":"http://127.0.0.1:1/sized.bin","filename":"sized.bin","size":3145728}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.ID == "" {
		t.Fatal("no task id returned")
	}

	v, ok := find(app.mgr, body.ID)
	if !ok {
		t.Fatal("the task is not in the manager")
	}
	// The point of the size handover: it is known before anything transfers.
	if v.Status != StatusPaused {
		t.Errorf("status = %s, want paused", v.Status)
	}
	if v.Size != wantSize {
		t.Errorf("size = %d, want %d", v.Size, wantSize)
	}
}

// TestLocalAPIAddAsksLocationWithSize covers the same handover on the
// ask-location path: no task is created, and the location:ask payload carries
// the size so the placement dialog can show it too.
func TestLocalAPIAddAsksLocationWithSize(t *testing.T) {
	const wantSize = "3145728"

	app := newTestApp(t)

	var mu sync.Mutex
	var ask map[string]string
	seen := 0
	app.emit = func(name string, data ...any) {
		if name != "location:ask" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		seen++
		if len(data) > 0 {
			if payload, ok := data[0].(map[string]string); ok {
				ask = payload
			}
		}
	}
	base := startAPI(t, app)

	resp := postJSON(t, base+"/add", "chrome-extension://abcdefghijklmnop",
		`{"url":"http://127.0.0.1:1/wait.bin","filename":"wait.bin","size":3145728}`)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	// Nothing exists yet: the arrival waits for the user to place it.
	if n := len(app.mgr.List()); n != 0 {
		t.Errorf("%d tasks created, want none before placement", n)
	}

	mu.Lock()
	defer mu.Unlock()
	if seen == 0 || ask == nil {
		t.Fatalf("no location:ask payload captured (events seen: %d)", seen)
	}
	if got := ask["size"]; got != wantSize {
		t.Errorf("location:ask size = %q, want %q", got, wantSize)
	}
}
