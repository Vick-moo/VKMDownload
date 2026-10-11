package testsrv

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// errDropped reports that the server intentionally severed the connection
// mid-body. It is a control-flow signal, never returned to a client.
var errDropped = errors.New("testsrv: connection dropped on purpose")

// Config describes a single test resource and every hostile behaviour the
// server should exhibit while serving it.
type Config struct {
	// Size is the number of bytes the resource really has.
	Size int64
	// Seed selects the deterministic content stream, see Fill.
	Seed int64

	// ETag is the validator advertised for the resource. Empty derives one
	// from Seed and Size.
	ETag string
	// ETagSwitchAfter, when >0, makes the server advertise ETagAfter once it
	// has served that many responses. This simulates a resource that changed
	// while the client was downloading it.
	ETagSwitchAfter int
	// ETagAfter is the replacement validator; empty means `"etag-swapped"`.
	ETagAfter string

	// LastModified is sent when non-zero.
	LastModified time.Time
	// ContentType and ContentDisposition are sent when non-empty.
	ContentType        string
	ContentDisposition string

	// AdvertiseRange controls the Accept-Ranges response header only.
	AdvertiseRange bool
	// HonorRange controls whether a Range request is actually answered with
	// 206. AdvertiseRange=true with HonorRange=false is the classic server
	// that lies about range support.
	HonorRange bool
	// IgnoreIfRange forces a full 200 response whenever the request carries an
	// If-Range header, i.e. the server does not honour resume validation.
	IgnoreIfRange bool

	// PerConnRate throttles each connection's body throughput to this many
	// bytes per second. 0 disables throttling. This is the knob that makes
	// multi-connection acceleration measurable on loopback, where RTT is
	// effectively zero.
	PerConnRate int64

	// LieContentLength, when non-zero, is advertised as Content-Length while
	// the server actually puts that many body bytes on the wire regardless of
	// the real remaining resource size. A larger value than the resource makes
	// clients hit an unexpected EOF; a smaller value silently truncates the
	// data and can only be caught by validating the finished file.
	LieContentLength int64

	// DropAfterBytes, when >0, hard-closes the connection after that many body
	// bytes without completing the declared Content-Length.
	DropAfterBytes int64
	// DropProbability, when >0, hard-closes the connection after each body
	// chunk with that probability.
	DropProbability float64

	// RequireCookie, when non-empty, makes the server answer 403 unless the
	// request carries session=<value>.
	RequireCookie string

	// FailFirst makes the first N requests answer 503 with Retry-After, which
	// exercises the engine's exponential backoff.
	FailFirst int

	// NoHead makes HEAD answer 405 Method Not Allowed, forcing the engine to
	// fall back to GET with Range: bytes=0-0.
	NoHead bool

	// NoContentLength makes responses omit Content-Length and stream with
	// chunked transfer encoding instead.
	NoContentLength bool
}

// DefaultConfig returns a well-behaved resource of the given size.
func DefaultConfig(size int64) Config {
	return Config{
		Size:           size,
		Seed:           1,
		AdvertiseRange: true,
		HonorRange:     true,
		LastModified:   time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
		ContentType:    "application/octet-stream",
	}
}

// Request is one recorded inbound request, used by tests to assert on the
// exact requests the engine made.
type Request struct {
	Method  string
	Range   string
	IfRange string
	Cookie  string
	Conn    string
	At      time.Time
}

// Server is the hostile test server.
type Server struct {
	cfg Config

	ln   net.Listener
	http *http.Server
	base string

	mu       sync.Mutex
	requests []Request
	served   int
	failed   int
}

// New creates a server for cfg. Call Start to listen.
func New(cfg Config) *Server {
	return &Server{cfg: cfg}
}

// ServeHTTP implements http.Handler. It is exported so the server can be
// driven by httptest as well as by Start.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.record(r)

	if s.cfg.RequireCookie != "" && cookieValue(r, "session") != s.cfg.RequireCookie {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusForbidden)
		return
	}

	if s.cfg.NoHead && r.Method == http.MethodHead {
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	s.mu.Lock()
	doFail := s.failed < s.cfg.FailFirst
	if doFail {
		s.failed++
	}
	s.mu.Unlock()
	if doFail {
		w.Header().Set("Retry-After", "1")
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}

	// Everything past this point needs byte-level control over the response,
	// so take the raw connection.
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "testsrv: ResponseWriter is not a Hijacker", http.StatusInternalServerError)
		return
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	s.handleConn(conn, brw.Reader, r)
}

// handleConn owns the connection from the first request onward and serves
// keep-alive requests itself.
func (s *Server) handleConn(conn net.Conn, br *bufio.Reader, first *http.Request) {
	lim := &rateLimiter{rate: s.cfg.PerConnRate, start: time.Now()}

	req := first
	for {
		if !s.writeResponse(conn, req, lim) {
			return
		}
		next, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		s.record(next)
		if s.cfg.RequireCookie != "" && cookieValue(next, "session") != s.cfg.RequireCookie {
			_, _ = conn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
			return
		}
		req = next
	}
}

// writeResponse writes exactly one response and reports whether the connection
// may be reused.
func (s *Server) writeResponse(conn net.Conn, req *http.Request, lim *rateLimiter) bool {
	cfg := s.cfg

	s.mu.Lock()
	s.served++
	served := s.served
	s.mu.Unlock()

	etag := cfg.ETag
	if etag == "" {
		etag = fmt.Sprintf(`"%016x-%016x"`, uint64(cfg.Seed), uint64(cfg.Size))
	}
	if cfg.ETagSwitchAfter > 0 && served > cfg.ETagSwitchAfter {
		etag = cfg.ETagAfter
		if etag == "" {
			etag = `"etag-swapped"`
		}
	}

	// Resolve the requested range. When HonorRange is false the server returns
	// a full 200 body no matter what Range the client asked for; that is
	// precisely the "pretends to support ranges" failure mode.
	start, end, partial := int64(0), cfg.Size-1, false
	if cfg.HonorRange {
		if rh := req.Header.Get("Range"); rh != "" {
			if lo, hi, ok := parseByteRange(rh, cfg.Size); ok {
				if !(cfg.IgnoreIfRange && req.Header.Get("If-Range") != "") {
					start, end, partial = lo, hi, true
				}
			}
		}
	}

	bodyLen := end - start + 1
	if bodyLen < 0 {
		bodyLen = 0
	}

	isHead := req.Method == http.MethodHead

	declareCL := bodyLen
	sendLen := bodyLen
	if cfg.LieContentLength != 0 {
		declareCL = cfg.LieContentLength
		sendLen = cfg.LieContentLength
	}

	status := "200 OK"
	if partial {
		status = "206 Partial Content"
	}

	// Chunked encoding is only usable on a non-partial GET: a 206 must carry
	// Content-Length, HEAD carries no body at all, and lying about the length
	// is mutually exclusive with chunked framing.
	useChunked := cfg.NoContentLength && !partial && cfg.LieContentLength == 0 && !isHead

	var h bytes.Buffer
	fmt.Fprintf(&h, "HTTP/1.1 %s\r\n", status)
	if useChunked {
		h.WriteString("Transfer-Encoding: chunked\r\n")
	} else {
		fmt.Fprintf(&h, "Content-Length: %d\r\n", declareCL)
	}
	if cfg.AdvertiseRange {
		h.WriteString("Accept-Ranges: bytes\r\n")
	} else {
		h.WriteString("Accept-Ranges: none\r\n")
	}
	fmt.Fprintf(&h, "ETag: %s\r\n", etag)
	if !cfg.LastModified.IsZero() {
		fmt.Fprintf(&h, "Last-Modified: %s\r\n", cfg.LastModified.UTC().Format(http.TimeFormat))
	}
	if cfg.ContentType != "" {
		fmt.Fprintf(&h, "Content-Type: %s\r\n", cfg.ContentType)
	}
	if cfg.ContentDisposition != "" {
		fmt.Fprintf(&h, "Content-Disposition: %s\r\n", cfg.ContentDisposition)
	}
	if partial {
		fmt.Fprintf(&h, "Content-Range: bytes %d-%d/%d\r\n", start, end, cfg.Size)
	}

	// A response is reusable only when nothing about it is deliberately broken.
	broken := cfg.LieContentLength != 0 || cfg.DropAfterBytes > 0 || cfg.DropProbability > 0
	if broken || cfg.NoContentLength {
		h.WriteString("Connection: close\r\n")
	} else {
		h.WriteString("Connection: keep-alive\r\n")
	}
	h.WriteString("\r\n")

	if _, err := conn.Write(h.Bytes()); err != nil {
		return false
	}
	if isHead {
		return !broken && !cfg.NoContentLength
	}

	if useChunked {
		return s.writeChunked(conn, start, bodyLen, lim) == nil
	}

	if err := s.writeBody(conn, start, sendLen, lim); err != nil {
		return false
	}
	return !broken
}

// writeBody streams sendLen bytes of resource content starting at start,
// applying throttling and the configured drop behaviours.
func (s *Server) writeBody(conn net.Conn, start, sendLen int64, lim *rateLimiter) error {
	const chunkSize = 32 << 10
	buf := make([]byte, chunkSize)

	var sent int64
	for sent < sendLen {
		n := int64(chunkSize)
		if r := sendLen - sent; r < n {
			n = r
		}

		if d := s.cfg.DropAfterBytes; d > 0 && sent+n > d {
			n = d - sent
			if n > 0 {
				Fill(s.cfg.Seed, start+sent, buf[:n])
				if _, err := conn.Write(buf[:n]); err != nil {
					return err
				}
				lim.throttle(int(n))
			}
			return errDropped
		}

		Fill(s.cfg.Seed, start+sent, buf[:n])
		if _, err := conn.Write(buf[:n]); err != nil {
			return err
		}
		lim.throttle(int(n))
		sent += n

		if s.cfg.DropProbability > 0 && rand.Float64() < s.cfg.DropProbability {
			return errDropped
		}
	}
	return nil
}

// writeChunked streams the whole resource with chunked transfer encoding.
func (s *Server) writeChunked(conn net.Conn, start, size int64, lim *rateLimiter) error {
	const chunkSize = 32 << 10
	buf := make([]byte, chunkSize)

	var sent int64
	for sent < size {
		n := int64(chunkSize)
		if r := size - sent; r < n {
			n = r
		}
		Fill(s.cfg.Seed, start+sent, buf[:n])

		var hdr [24]byte
		hs := strconv.AppendInt(hdr[:0], n, 16)
		hs = append(hs, '\r', '\n')
		if _, err := conn.Write(hs); err != nil {
			return err
		}
		if _, err := conn.Write(buf[:n]); err != nil {
			return err
		}
		if _, err := conn.Write([]byte("\r\n")); err != nil {
			return err
		}
		lim.throttle(int(n))
		sent += n
	}
	_, err := conn.Write([]byte("0\r\n\r\n"))
	return err
}

// Start listens on addr (use 127.0.0.1:0 for an ephemeral port) and serves in
// the background. It returns the base URL.
func (s *Server) Start(addr string) (string, error) {
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", err
	}
	s.ln = ln
	s.base = "http://" + ln.Addr().String()
	// Plain HTTP, so Go never negotiates h2 here; the engine under test is
	// HTTP/1.1-only by design as well. The hostile behaviours additionally
	// depend on raw connection control, which h2 would forbid anyway.
	s.http = &http.Server{Handler: s}
	go func() { _ = s.http.Serve(ln) }()
	return s.base, nil
}

// URL returns the URL of the test resource. Start must have been called.
func (s *Server) URL() string { return s.base + "/file" }

// Base returns the server's base URL. Start must have been called.
func (s *Server) Base() string { return s.base }

// Config returns the server's configuration.
func (s *Server) Config() Config { return s.cfg }

// Close stops the listener.
func (s *Server) Close() error {
	if s.http != nil {
		return s.http.Close()
	}
	return nil
}

// Requests returns a copy of the recorded request log.
func (s *Server) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Request, len(s.requests))
	copy(out, s.requests)
	return out
}

// Reset clears the request log and the failure counter.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = nil
	s.failed = 0
}

func (s *Server) record(r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, Request{
		Method:  r.Method,
		Range:   r.Header.Get("Range"),
		IfRange: r.Header.Get("If-Range"),
		Cookie:  r.Header.Get("Cookie"),
		Conn:    r.Header.Get("Connection"),
		At:      time.Now(),
	})
}

// rateLimiter enforces an average byte rate per connection. Because it is
// driven by cumulative bytes against wall-clock time since the connection
// started, it cannot drift the way a naive per-write sleep would.
type rateLimiter struct {
	rate  int64
	start time.Time
	sent  int64
}

func (l *rateLimiter) throttle(n int) {
	if l.rate <= 0 || n <= 0 {
		return
	}
	l.sent += int64(n)
	want := time.Duration(float64(l.sent) / float64(l.rate) * float64(time.Second))
	if elapsed := time.Since(l.start); want > elapsed {
		time.Sleep(want - elapsed)
	}
}

// parseByteRange parses a single-range "bytes=" header against a known size.
// Multi-range requests are rejected, as the engine never issues them.
func parseByteRange(h string, size int64) (int64, int64, bool) {
	const prefix = "bytes="
	if !strings.HasPrefix(h, prefix) {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	if spec == "" || strings.Contains(spec, ",") {
		return 0, 0, false
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	loRaw := strings.TrimSpace(spec[:dash])
	hiRaw := strings.TrimSpace(spec[dash+1:])

	if loRaw == "" {
		// Suffix range: the final N bytes.
		n, err := strconv.ParseInt(hiRaw, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		if n == 0 {
			return 0, 0, false
		}
		return size - n, size - 1, true
	}

	lo, err := strconv.ParseInt(loRaw, 10, 64)
	if err != nil || lo < 0 || lo >= size {
		return 0, 0, false
	}
	hi := size - 1
	if hiRaw != "" {
		hi, err = strconv.ParseInt(hiRaw, 10, 64)
		if err != nil || hi < lo {
			return 0, 0, false
		}
	}
	if hi >= size {
		hi = size - 1
	}
	return lo, hi, true
}

func cookieValue(r *http.Request, name string) string {
	c, err := r.Cookie(name)
	if err != nil {
		return ""
	}
	return c.Value
}
