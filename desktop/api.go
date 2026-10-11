package main

import (
	"context"
	"encoding/json"
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
func (a *App) apiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")

		// An empty Origin means the request did not come from a page at all
		// (curl, a test). Anything else has to be an extension.
		if origin != "" &&
			!strings.HasPrefix(origin, "chrome-extension://") &&
			!strings.HasPrefix(origin, "moz-extension://") {
			http.Error(w, "this endpoint only accepts requests from the browser extension",
				http.StatusForbidden)
			return
		}

		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
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

	cfg := a.Config()
	task, err := a.mgr.Start(StartRequest{
		URL:      req.URL,
		Filename: sanitizeSuggestedName(req.Filename),
		Dest:     cfg.DownloadDir,
		Conns:    cfg.Connections,
		Start:    cfg.AutoStart,
		Referer:  req.Referer,
	})
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// The caller is recorded, not just the URL. A burst of tasks arrived once
	// with no extension installed in any browser, and nothing in the log could
	// say where they came from — which made it impossible to tell a bug from
	// something else on the machine calling the endpoint.
	a.debugLog("extension added %s (origin=%q ua=%q)",
		req.URL, r.Header.Get("Origin"), r.Header.Get("User-Agent"))
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
	return name
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
