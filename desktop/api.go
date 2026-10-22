package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
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

// Extension protocol handshake.
//
// /ping advertises both values; /add carries the extension's own version so the
// app can flag "your extension is older than this build expects" on the settings
// page. The check is advisory only: an older extension is never refused a
// download, because refusing would break users who cannot update instantly.
const (
	extensionProtocol   = 1
	minExtensionVersion = "1.0.3"
	// extVersionHeader carries the extension's version on /add. A header rather
	// than a query parameter: /add already posts a JSON body, and a query string
	// would be one more thing the pairing proof has to cover.
	extVersionHeader = "X-VKMDownload-Ext-Version"
)

// apiState holds what the endpoint needs to know about itself.
type apiState struct {
	mu   sync.Mutex
	port int
}

// ExtensionInfo is what the settings page shows about the extension that last
// talked to the app. It lives in process memory only: it is a diagnostic for
// this run, not history.
type ExtensionInfo struct {
	// LastSeen is when an extension-origin request last passed the guard.
	LastSeen time.Time `json:"lastSeen"`
	// LastOrigin is the last accepted extension origin, i.e. "the extension ID
	// currently talking to the app".
	LastOrigin string `json:"lastOrigin"`
	// ExtVersion is the version the last /add reported, empty when it did not
	// say (an older extension build).
	ExtVersion string `json:"extVersion"`
	// VersionMismatch reports that ExtVersion is older than minExtensionVersion.
	// It never changes whether /add is accepted.
	VersionMismatch bool `json:"versionMismatch"`
	// Accepted/Rejected count /add and /pair outcomes since the app started.
	Accepted int64 `json:"accepted"`
	Rejected int64 `json:"rejected"`
}

// noteExtensionSeen records that an extension-origin request passed the guard.
func (a *App) noteExtensionSeen(origin string) {
	a.extMu.Lock()
	defer a.extMu.Unlock()
	if origin != "" {
		a.extInfo.LastOrigin = origin
	}
	a.extInfo.LastSeen = time.Now()
}

// noteExtensionResult counts an /add or /pair request that reached its handler.
func (a *App) noteExtensionResult(origin string, accepted bool) {
	a.extMu.Lock()
	defer a.extMu.Unlock()
	if origin != "" {
		a.extInfo.LastOrigin = origin
	}
	a.extInfo.LastSeen = time.Now()
	if accepted {
		a.extInfo.Accepted++
	} else {
		a.extInfo.Rejected++
	}
}

// noteExtensionVersion records the version an /add carried, and whether it is
// older than this build expects.
func (a *App) noteExtensionVersion(v string) {
	v = strings.TrimSpace(v)
	if v == "" {
		// An older build does not send the header: nothing to compare, and the
		// previous verdict (if any) is left alone rather than cleared.
		return
	}
	a.extMu.Lock()
	defer a.extMu.Unlock()
	a.extInfo.ExtVersion = v
	a.extInfo.VersionMismatch = compareVersions(v, minExtensionVersion) < 0
}

// ExtensionStatus reports the last extension request and the counters.
func (a *App) ExtensionStatus() ExtensionInfo {
	a.extMu.Lock()
	defer a.extMu.Unlock()
	return a.extInfo
}

// isExtensionOrigin reports whether an Origin header has the shape a browser
// extension sends. This is the old guard rule, unchanged.
func isExtensionOrigin(origin string) bool {
	return strings.HasPrefix(origin, "chrome-extension://") ||
		strings.HasPrefix(origin, "moz-extension://")
}

// compareVersions compares two dotted numeric versions, returning -1, 0 or 1.
//
// Missing components are zero, so "1.0" < "1.0.3". A component is read up to its
// first non-digit, so "1.0.3-beta" compares as "1.0.3". This is a compatibility
// gate, not a package manager: it only has to order ordinary extension versions
// correctly.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		av, bv := 0, 0
		if i < len(as) {
			av = leadingNumber(as[i])
		}
		if i < len(bs) {
			bv = leadingNumber(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// leadingNumber reads the digits a version component starts with, clamped so an
// absurd input cannot overflow the comparison.
func leadingNumber(s string) int {
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
		if n > 1<<24 {
			return 1 << 24
		}
	}
	return n
}

// statusRecorder remembers the status a handler wrote, so the guard can count
// accepted/rejected /add and /pair requests without every handler reporting back.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Status returns the status the handler wrote (200 when it wrote only a body).
func (s *statusRecorder) Status() int {
	if s.status == 0 {
		return http.StatusOK
	}
	return s.status
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
	mux.HandleFunc("/pair", a.apiPair)
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
// When the loopback peer check is enforced (VerifyPeerProcess), every
// non-preflight request must come from a genuine signed browser process.
// The Origin header alone is self-reported and costs an attacker nothing to
// forge; without the peer check, anything else on the machine that can speak
// HTTP could drive the endpoint once past it.
//
// Two optional/diagnostic layers sit on top:
//
//   - ExtensionOriginLock, when set, admits exactly one extension origin and
//     refuses every other extension (still a 403). Empty keeps the historical
//     "any extension" rule.
//   - Accepted/rejected counters and the last origin are recorded in memory for
//     the settings page. They never affect a verdict.
func (a *App) apiGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		extOrigin := isExtensionOrigin(origin)
		// The counters describe the two verbs the extension uses to do work.
		// Preflights and reads are not attempts and are not counted.
		counted := r.Method != http.MethodOptions &&
			(r.URL.Path == "/add" || r.URL.Path == "/pair")

		// reject answers and records. A refused /add or /pair from an extension
		// is exactly the rejection the settings page should be able to show; the
		// verdict itself is unchanged.
		reject := func(body map[string]string) {
			if extOrigin && counted {
				a.noteExtensionResult(origin, false)
			}
			writeJSON(w, http.StatusForbidden, body)
		}

		// Mutations require a browser-extension Origin. An empty Origin (curl,
		// a local test) may still use the read-only endpoints, but must not
		// enqueue downloads: without this, any process on the machine could
		// push arbitrary tasks into the queue with no credential at all.
		//
		// /pair is held to the same rule even though it is a GET: it hands out
		// the pairing key, which is a credential, so a caller without a browser
		// Origin must never reach it.
		if r.Method != http.MethodOptions && (r.URL.Path == "/add" || r.URL.Path == "/pair" || r.Method == http.MethodPost) {
			if !extOrigin {
				// JSON, not plain text: the extension parses the reply body
				// and shows the error verbatim, and a plain-text 403 made it
				// fall back to "HTTP 403" every time.
				reject(map[string]string{
					"error": "this endpoint only accepts requests from the browser extension",
				})
				return
			}
		} else if origin != "" && !extOrigin {
			reject(map[string]string{
				"error": "this endpoint only accepts requests from the browser extension",
			})
			return
		}

		// Origin lock (opt-in). With a stored origin only that exact extension is
		// admitted; the comparison is exact because a browser Origin carries no
		// trailing slash and case matters to the extension ID. A refusal is a
		// peer diagnostic, so the settings page can explain "my other extension
		// cannot talk to the app".
		if extOrigin {
			if lock := a.Config().ExtensionOriginLock; lock != "" && origin != lock {
				a.recordPeerRejection(peerReasonOriginLocked, "")
				reject(map[string]string{
					"error": "this extension is not the one this app is locked to",
				})
				return
			}
		}

		// The Host header has to name this machine. Binding to 127.0.0.1 stops
		// anything off-box from connecting, but it does not stop DNS rebinding:
		// a page on http://rebind.evil/ whose domain resolves to 127.0.0.1 sends
		// requests that reach this listener and carry that name in Host. The
		// Origin check rejects its Origin, but a Host check is the layer that
		// makes the whole class impossible rather than merely blocked.
		if !loopbackHost(r.Host) {
			reject(map[string]string{
				"error": "unexpected Host header",
			})
			return
		}

		// Echo the caller's origin instead of "*". Only an extension origin gets
		// this far, so * was not exploitable — but a wildcard would let a future
		// regression that admits some page read the reply, and it is exactly the
		// kind of header nobody re-checks.
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
		}
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		// The extension sends its version and its pairing proof as headers; both
		// have to be named here for a preflighted request to carry them.
		w.Header().Set("Access-Control-Allow-Headers",
			"Content-Type, "+authHeader+", "+extVersionHeader)

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Mutations must prove they hold the pairing key. The Origin check says
		// "some browser extension" and the peer check says "a real browser", but
		// neither says "our extension": any installed extension passes both, and
		// /add enqueues a download with caller-supplied cookies and URL. The proof
		// makes the endpoint useless to anything that merely looks like a browser.
		if r.Method == http.MethodPost || r.URL.Path == "/add" {
			if !a.requestAuthentic(r) {
				reject(map[string]string{
					"error": "request is not authenticated by the VKMDownload extension",
				})
				return
			}
		}
		// Peer verification likewise reads its switch live, so toggling it
		// applies to the very next download with no restart.
		if a.peerCheckRequired() {
			if ok, exe := a.checkPeerProcess(r); !ok {
				// JSON so the extension can show which process was refused
				// verbatim; a plain-text body made every refusal read as a
				// bare "HTTP 403" in the notification.
				reject(map[string]string{
					"error": "caller is not a recognized browser" + exeSuffix(exe),
				})
				return
			}
		}

		// The guard accepted the request.
		//
		// Only /add and /pair go through a status recorder: their many 400/403
		// returns are counted from the handler's status without threading a
		// counter through every path. Every other request is passed to the
		// handler untouched — wrapping is only worth its (small) risk where the
		// status is actually needed — and then the origin is remembered for the
		// settings page.
		if !counted {
			next.ServeHTTP(w, r)
			if extOrigin {
				a.noteExtensionSeen(origin)
			}
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)
		if extOrigin {
			a.noteExtensionResult(origin, rec.Status() < http.StatusBadRequest)
		}
	})
}

// loopbackHost reports whether a Host header names this machine.
//
// The port is irrelevant (the listener only ever binds a loopback address), so
// only the host part is compared. An empty Host is refused: HTTP/1.1 requires
// it, and its absence means something hand-rolled is talking to the endpoint.
func loopbackHost(hostport string) bool {
	if hostport == "" {
		return false
	}
	host := hostport
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		host = h
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	switch strings.ToLower(host) {
	case "127.0.0.1", "localhost", "::1":
		return true
	}
	return false
}

// exeSuffix names the rejected caller for the user, so a legitimate new
// browser only needs its exe name reported once to earn an allowlist entry.
func exeSuffix(exe string) string {
	if exe == "" {
		return ""
	}
	return " (" + filepath.Base(exe) + ")"
}

// apiPing lets the extension find the app without guessing, and proves the app's
// identity when the caller asks for a challenge.
//
// The self-reported "app" field is not evidence: any program can answer with it.
// The extension therefore requires the proof header, which only a holder of the
// per-install key can produce for the nonce it just generated.
func (a *App) apiPing(w http.ResponseWriter, r *http.Request) {
	// Only the two read-only methods. The handler has no side effects, but a
	// service that answers PUT with 200 invites a caller to believe it can.
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if proof := a.answerChallenge(r.URL.Query().Get("challenge")); proof != "" {
		w.Header().Set(proofHeader, proof)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"app":     "VKMDownload",
		"version": appVersion,
		"port":    a.apiPort(),
		// The handshake: which contract this app speaks, and the oldest
		// extension it expects. An older extension can still work; this is how
		// it learns that it is old.
		"protocol":            extensionProtocol,
		"minExtensionVersion": minExtensionVersion,
	})
}

// apiSettings is the interception policy: read by the extension before every
// interception. It is read-only for the extension now.
//
// The policy lives in the app rather than in the extension so there is one place
// to change it, and a change applies to the next download with no reinstall.
// Writing it, however, is the app's own business: the endpoint used to accept
// enabled/minSizeKb from the extension's popup, which meant any installed
// extension that cleared the origin/peer checks could switch interception off
// (or change what gets collected) from a surface the user never opened.
func (a *App) apiSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{
			"error": "设置只能由应用自身修改",
		})
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use GET"})
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
		// The two behavioural switches the extension reads. They are sent even
		// when false, so a switched-off feature is distinguishable from an app
		// old enough not to have the field — the extension fills the historical
		// defaults only when the key is absent.
		"sendCookies": cfg.ExtensionSendCookies,
		"incognito":   cfg.ExtensionIncognito,
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
	// Size is the total size the browser already knew (Content-Length) when
	// it handed the download over. The row shows it without waiting for a
	// transfer to start, and the duplicate check uses it. 0 = unknown.
	Size int64 `json:"size"`
}

// apiAdd creates a task from a download the extension intercepted.
func (a *App) apiAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "use POST"})
		return
	}

	// The optional version handshake. An older extension does not send this
	// header and is still served: the value is recorded for the settings page,
	// never used to refuse a download.
	a.noteExtensionVersion(r.Header.Get(extVersionHeader))

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
	// Same rule as Start, applied at the door. With AskLocation on, the URL is
	// forwarded to the new-task dialog before Start ever sees it — so without
	// this a file:// or javascript: string reached the dialog and the user was
	// asked to place a download that could not exist.
	if u, uerr := url.Parse(strings.TrimSpace(req.URL)); uerr != nil ||
		(u.Scheme != "http" && u.Scheme != "https") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "only http and https URLs are accepted"})
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
	// Drop the browser's cookies when the user has turned cookie forwarding off.
	//
	// The switch is also enforced in the extension, but an extension installed
	// before it existed does not read it and keeps sending cookies. Dropping them
	// here is what makes the setting true for every extension version. The
	// download itself is unaffected: it simply arrives without a login session,
	// exactly as if the user were not signed in.
	if !cfg.ExtensionSendCookies {
		req.Cookie = ""
	}
	if cfg.AskLocation {
		// Ask first, create later: the front-end opens the new-task dialog
		// prefilled with these values, and closing it abandons the download.
		// The reply stays 200 with no id so the extension does not fall back
		// to a browser download of something the user is about to place.
		fn := sanitizeSuggestedName(req.Filename)
		if fn == "" {
			// The browser can hand over no name at all — Chrome fills the
			// filename in from the response headers, and a fast handover can
			// arrive before that. Unlike the direct-add path there is no probe
			// to fill the name in later, so the dialog would open blank; derive
			// a placeholder from the URL instead.
			fn = fallbackSuggestedName(req.URL)
		}
		// The URL is logged desensitised (host+path): query strings carry
		// session tokens and single-use signatures that must not land on disk.
		// The user agent is reduced to a length: it is a fingerprint that says
		// which browser, build and OS the user is on, and this log is the file a
		// user is asked to attach to a bug report.
		a.debugLog("extension asked %s (origin=%q ua=%dB cookie=%dB)",
			logURL(req.URL), r.Header.Get("Origin"), len(r.Header.Get("User-Agent")), len(req.Cookie))
		a.emit("location:ask", map[string]string{
			"url":       req.URL,
			"filename":  fn,
			"referer":   req.Referer,
			"cookie":    req.Cookie,
			"userAgent": req.UserAgent,
			"size":      fmt.Sprintf("%d", req.Size),
		})
		writeJSON(w, http.StatusOK, map[string]any{"pending": true, "name": fn})
		return
	}
	// Clamped here as well as in the engine: the setting is user-controlled and
	// every connection is a live socket plus a worker goroutine, so a bad value
	// should never reach the task at all.
	task, err := a.mgr.Start(StartRequest{
		URL:       req.URL,
		Filename:  sanitizeSuggestedName(req.Filename),
		Dest:      cfg.DownloadDir,
		Conns:     clampConns(cfg.Connections),
		Start:     cfg.AutoStart,
		Referer:   req.Referer,
		Cookie:    req.Cookie,
		UserAgent: req.UserAgent,
		Size:      req.Size,
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
	a.debugLog("extension added %s (origin=%q ua=%dB cookie=%dB)",
		logURL(req.URL), r.Header.Get("Origin"), len(r.Header.Get("User-Agent")), len(req.Cookie))
	writeJSON(w, http.StatusOK, map[string]any{"id": task.ID, "name": task.Name})
}

// maxConns is the ceiling on parallel connections per task. config.normalize
// enforces the same number, so a value that slipped past it (a hand-edited
// config.json, a request carrying its own count) still cannot open more than
// this many sockets per task.
const maxConns = 64

// clampConns bounds a connection count to [1, maxConns].
func clampConns(n int) int {
	if n < 1 {
		return 1
	}
	if n > maxConns {
		return maxConns
	}
	return n
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

// fallbackSuggestedName derives a placeholder name from the URL for an arrival
// the browser could not name.
//
// Only the ask path needs this: no task exists yet, so there is no probe to
// fill the name in later, and an empty name used to reach the placement dialog
// as an empty filename field (the browser's second and later handovers are fast
// enough to beat the response headers that carry the real name). The last path
// segment is used when it looks like a filename; anything else — a bare host, a
// trailing slash, a segment without an extension such as /api/file/123 — becomes
// "download". Both are placeholders the user can replace in the dialog, and the
// app's own sanitising still applies.
func fallbackSuggestedName(raw string) string {
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		name := sanitizeSuggestedName(path.Base(u.Path))
		if name != "" && filepath.Ext(name) != "" {
			return name
		}
	}
	return "download"
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
