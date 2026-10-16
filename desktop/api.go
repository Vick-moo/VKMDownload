package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The browser extension talks to the app over a loopback HTTP endpoint rather
// than through Wails' bindings, which only exist inside the webview.
//
// Loopback only: nothing outside this machine can reach it. The origin check in
// apiGuard is what stops a web page the user happens to visit from adding
// downloads to their queue — a cross-origin POST is delivered even though the
// page cannot read the reply, so "it cannot see the response" is not protection.
const (
	apiPortFirst = 18888
	apiPortLast  = 18898
)

// apiState holds what the endpoint needs to know about itself.
type apiState struct {
	mu   sync.Mutex
	port int
}

// Port reports the port the API is listening on, or 0 when it is not running.
// The extension scans the range, so this is mostly for diagnostics and the UI.
func (a *App) apiPort() int {
	a.api.mu.Lock()
	defer a.api.mu.Unlock()
	return a.api.port
}

// startLocalAPI serves the extension's requests until ctx is cancelled.
//
// The port is not fixed in practice: another copy of the app, or anything else
// on the machine, may hold the first choice. The extension scans the same range
// for whichever one answers, so a busy port is not a failure.
func (a *App) startLocalAPI(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", a.apiPing)
	mux.HandleFunc("/settings", a.apiSettings)
	mux.HandleFunc("/add", a.apiAdd)
	handler := a.apiGuard(mux)

	for port := apiPortFirst; port <= apiPortLast; port++ {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			continue
		}

		srv := &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
		}

		a.api.mu.Lock()
		a.api.port = port
		a.api.mu.Unlock()

		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = srv.Shutdown(shutdown)
		}()
		go func() {
			if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
				a.debugLog("local api stopped: %v", err)
			}
		}()

		a.debugLog("local api listening on %s", addr)
		return
	}

	a.debugLog("local api: no free port between %d and %d", apiPortFirst, apiPortLast)
}

// apiGuard rejects callers that are not browser extensions and answers the
// preflight requests that follow from that.
//
// When the loopback credential is enforced (ApiAuthRequired), every
// non-preflight request must also carry Authorization: Bearer <ApiToken>.
// The token is shown in the app's settings so the user can hand it to the
// extension; without it, anything else on the machine that can speak HTTP
// could drive the endpoint once past the Origin check.
func (a *App) apiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// Mutations require a browser-extension Origin. An empty Origin (curl,
		// a local test) may still use the read-only endpoints, but must not
		// enqueue downloads: without this, any process on the machine could
		// push arbitrary tasks into the queue with no credential at all.
		if r.Method != http.MethodOptions && (r.URL.Path == "/add" || r.Method == http.MethodPost) {
			if !strings.HasPrefix(origin, "chrome-extension://") &&
				!strings.HasPrefix(origin, "moz-extension://") {
				http.Error(w, "this endpoint only accepts requests from the browser extension",
					http.StatusForbidden)
				return
			}
		} else if origin != "" &&
			!strings.HasPrefix(origin, "chrome-extension://") &&
			!strings.HasPrefix(origin, "moz-extension://") {
			http.Error(w, "this endpoint only accepts requests from the browser extension",
				http.StatusForbidden)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// Bearer enforcement lives behind the switch: off keeps the
		// historical posture (loopback + Origin check), on requires the
		// token on every real request.
		if a.apiAuthRequired() && !a.checkApiToken(r) {
			http.Error(w, "missing or invalid api token", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// apiAuthRequired reports whether the loopback credential is enforced.
func (a *App) apiAuthRequired() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.ApiAuthRequired && a.cfg.ApiToken != ""
}

// checkApiToken compares the request's Authorization header against the
// configured token with a constant-time compare.
func (a *App) checkApiToken(r *http.Request) bool {
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if len(got) <= len(prefix) || got[:len(prefix)] != prefix {
		return false
	}
	a.mu.Lock()
	want := a.cfg.ApiToken
	a.mu.Unlock()
	if want == "" {
		return false
	}
	return subtleCompare(got[len(prefix):], want)
}

// subtleCompare is a length-revealing constant-time string comparison: a
// plain == bails on the first differing byte, which leaks the token
// prefix-by-prefix to timing.
func subtleCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// apiPing lets the extension find the app without guessing.
func (a *App) apiPing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"app":     "VKMDownload",
		"version": appVersion,
		"port":    a.apiPort(),
	})
}

// apiSettings is the interception policy: read by the extension before every
// interception, and written by its popup when the user flips the switch there.
//
// It lives in the app rather than in the extension on purpose — one place to
// change it, and a change applies to the next download with no reinstall.
func (a *App) apiSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		a.apiSettingsUpdate(w, r)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET or POST"})
		return
	}
	writeJSON(w, http.StatusOK, a.policyView())
}

// apiSettingsUpdate applies a partial change from the extension's popup.
//
// Pointers so an omitted field is left alone: the popup toggles the switch
// without knowing anything about the size limit, and sending a zero for it
// would silently mean "take everything".
func (a *App) apiSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var patch struct {
		Enabled   *bool `json:"enabled"`
		MinSizeKB *int  `json:"minSizeKb"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}

	cfg := a.Config()
	if patch.Enabled != nil {
		cfg.ExtensionEnabled = *patch.Enabled
	}
	if patch.MinSizeKB != nil {
		cfg.ExtensionMinSizeKB = *patch.MinSizeKB
	}
	if err := a.SetConfig(cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, a.policyView())
}

// policyView renders the settings the extension consumes.
func (a *App) policyView() map[string]any {
	cfg := a.Config()

	// Split here rather than in the extension, so the parsing rules — blank
	// lines, stray commas, surrounding spaces — are defined once and testable.
	var skip []string
	for _, line := range strings.FieldsFunc(cfg.ExtensionSkipHosts, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ',' || r == ';'
	}) {
		host := strings.ToLower(strings.TrimSpace(line))
		host = strings.TrimPrefix(host, "www.")
		if host != "" {
			skip = append(skip, host)
		}
	}

	return map[string]any{
		"enabled":   cfg.ExtensionEnabled,
		"minSizeKb": cfg.ExtensionMinSizeKB,
		"skipHosts": skip,
	}
}

// apiAddRequest is what the extension posts when it hands over a download.
type apiAddRequest struct {
	URL      string `json:"url"`
	Filename string `json:"filename"`
	Referer  string `json:"referer"`
	// Cookie is the browser's cookies for the URL, serialised as a Cookie
	// header value. Hosts that require a login (Google Classroom / Drive
	// private files) reject a client that arrives without them; the engine
	// sends this back verbatim with every request for the task.
	Cookie string `json:"cookie"`
	// UserAgent is the browser's UA. Some hosts/WAFs refuse the engine's
	// default non-browser UA, so the browser's own string is replayed.
	UserAgent string `json:"userAgent"`
}

// apiAdd creates a task from a download the extension intercepted.
func (a *App) apiAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}

	var req apiAddRequest
	// A limit rather than trusting the caller: this is reachable by anything on
	// the machine, and an unbounded body is a trivial way to exhaust memory.
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "malformed request"})
		return
	}
	if strings.TrimSpace(req.URL) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is required"})
		return
	}
	// Field caps: URL and Referer are attacker-influenced strings that would
	// otherwise live unbounded in task memory, the index file and the log;
	// Filename is capped in runes (255, the common filesystem limit) because
	// a longer name cannot be created on disk anyway.
	if len(req.URL) > 8<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "url is too large"})
		return
	}
	if len(req.Referer) > 8<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "referer is too large"})
		return
	}
	if len([]rune(req.Filename)) > 255 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "filename is too long"})
		return
	}
	// Bounds on the replayed credentials: a normal browser Cookie header is a
	// few KB. Without a cap a caller could stuff megabytes into task memory
	// (the /add body limit alone would allow 1 MiB of it).
	if len(req.Cookie) > 32<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "cookie is too large"})
		return
	}
	if len(req.UserAgent) > 4<<10 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "user agent is too large"})
		return
	}

	cfg := a.Config()
	if cfg.AskLocation {
		// Ask first, create later: the front-end opens the new-task dialog
		// prefilled with these values, and closing it abandons the download.
		// The reply stays 200 with no id so the extension does not fall back
		// to a browser download of something the user is about to place.
		fn := sanitizeSuggestedName(req.Filename)
		// The URL is logged desensitised (host+path): query strings carry
		// session tokens and single-use signatures that must not land on disk.
		a.debugLog("extension asked %s (origin=%q ua=%q cookie=%dB)",
			logURL(req.URL), r.Header.Get("Origin"), r.Header.Get("User-Agent"), len(req.Cookie))
		a.emit("location:ask", map[string]string{
			"url":       req.URL,
			"filename":  fn,
			"referer":   req.Referer,
			"cookie":    req.Cookie,
			"userAgent": req.UserAgent,
		})
		writeJSON(w, http.StatusOK, map[string]any{"pending": true, "name": fn})
		return
	}
	task, err := a.mgr.Start(StartRequest{
		URL:       req.URL,
		Filename:  sanitizeSuggestedName(req.Filename),
		Dest:      cfg.DownloadDir,
		Conns:     cfg.Connections,
		Start:     cfg.AutoStart,
		Referer:   req.Referer,
		Cookie:    req.Cookie,
		UserAgent: req.UserAgent,
	})
	if err != nil {
		// A duplicate must NOT fail the handover: the extension falls back
		// to a browser download on any error, which would download the file
		// twice. Answer 200 with the existing record instead.
		var dup *duplicateError
		if errors.As(err, &dup) {
			a.debugLog("extension duplicate %s (existing %s)", logURL(req.URL), dup.ID)
			writeJSON(w, http.StatusOK, map[string]any{
				"duplicate": true,
				"id":        task.ID,
				"name":      task.Name,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// The caller is recorded, not just the URL. A burst of tasks arrived once
	// with no extension installed in any browser, and nothing in the log could
	// say where they came from — which made it impossible to tell a bug from
	// something else on the machine calling the endpoint.
	a.debugLog("extension added %s (origin=%q ua=%q cookie=%dB)",
		logURL(req.URL), r.Header.Get("Origin"), r.Header.Get("User-Agent"), len(req.Cookie))
	writeJSON(w, http.StatusOK, map[string]any{"id": task.ID, "name": task.Name})
}

// sanitizeSuggestedName makes a name from the browser safe to join onto a path.
//
// The browser's suggestion comes from the server, so it is untrusted: a name
// containing a separator or ".." would let a remote server choose where the
// file lands, outside the user's download folder.
func sanitizeSuggestedName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Take the last segment only, so any directory part is discarded.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	// A drive-relative name such as "C:foo" has no separator but still escapes
	// the download folder on Windows; the colon filter below neutralises it.
	var b strings.Builder
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			b.WriteRune('_')
		case strings.ContainsRune(`:*?"<>|`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}
	name = b.String()
	// Windows ignores trailing dots and spaces, which makes "evil. . " land on
	// "evil" and can hide the real extension.
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "." || name == ".." {
		return ""
	}
	// Reserved device names (NUL, CON, COM1, ...) cannot be created and refer
	// to devices when used bare; neutralise them with a suffix.
	upper := strings.ToUpper(name)
	if i := strings.Index(upper, "."); i >= 0 {
		upper = upper[:i]
	}
	switch upper {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		name += "_"
	}
	return name
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
