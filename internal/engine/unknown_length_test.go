package engine

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"vkmdownload/testsrv"
)

// dropThenNotFoundServer models the one sequence the fault-injecting test
// server cannot express on the unknown-length path: a single-stream transfer
// that lands bytes, loses the connection, and then meets a 404 before a later
// attempt finally succeeds.
//
// testsrv's chunked writer never drops a connection — DropAfterBytes and
// DropProbability apply only to fixed-length bodies — so a 404 that arrives
// *after* bytes were written cannot be produced there. That is exactly the
// blind spot the audit found: the unknown-length path had zero coverage of the
// post-progress 404 that drives the engine's retry.
//
// HEAD is refused (NoHead semantics) and, in the unknown mode, no response ever
// carries Content-Length (NoContentLength semantics), so the engine must take
// the single-stream-to-EOF path for the whole download.
type dropThenNotFoundServer struct {
	size        int64
	knownLength bool

	mu       sync.Mutex
	attempts int
	notFound int
}

func (s *dropThenNotFoundServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}

	if r.Header.Get("Range") == "bytes=0-0" {
		// The probe. A range-capable server proves it with a 206 and reveals
		// the total; a server without ranges answers 200 and, when it also
		// omits Content-Length, leaves the size unknown.
		w.Header().Set("ETag", `"abc"`)
		if s.knownLength {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-0/%d", s.size))
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
			return
		}
		w.Header().Set("Accept-Ranges", "none")
		w.WriteHeader(http.StatusOK)
		// Flush before writing so net/http commits chunked framing instead of
		// buffering the tiny body and inventing a Content-Length for it.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write([]byte{0})
		return
	}

	s.mu.Lock()
	s.attempts++
	attempt := s.attempts
	s.mu.Unlock()

	if attempt == 1 {
		// Serve a real prefix, then abort the connection mid-body.
		prefix := testsrv.Expected(testSeed, 64<<10)
		w.Header().Set("ETag", `"abc"`)
		if s.knownLength {
			w.Header().Set("Content-Length", fmt.Sprint(s.size))
		}
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		_, _ = w.Write(prefix)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	if attempt == 2 {
		s.mu.Lock()
		s.notFound++
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.serve(w, r)
}

func (s *dropThenNotFoundServer) serve(w http.ResponseWriter, r *http.Request) {
	body := testsrv.Expected(testSeed, s.size)

	offset := int64(0)
	if rg := r.Header.Get("Range"); rg != "" {
		var lo int64
		if _, err := fmt.Sscanf(rg, "bytes=%d-", &lo); err == nil && lo > 0 && lo < s.size {
			offset = lo
		}
	}

	w.Header().Set("ETag", `"abc"`)
	w.Header().Set("Accept-Ranges", "bytes")
	switch {
	case offset > 0:
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, s.size-1, s.size))
		w.Header().Set("Content-Length", fmt.Sprint(s.size-offset))
		w.WriteHeader(http.StatusPartialContent)
	case s.knownLength:
		w.Header().Set("Content-Length", fmt.Sprint(s.size))
		w.WriteHeader(http.StatusOK)
	default:
		// Unknown length: stream to EOF with no Content-Length.
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	_, _ = w.Write(body[offset:])
}

func (s *dropThenNotFoundServer) attemptsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *dropThenNotFoundServer) notFoundServed() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notFound
}

// TestPostProgressNotFoundUnknownAndKnownLength is the A/B the audit asked for.
//
// The same sequence — some bytes land, the connection drops, the next request
// is a 404 — is run twice: once against a server that discloses a length and
// supports ranges, and once against a server that discloses nothing. The known
// length case always recovered; the unknown length case did not, because the
// unbounded segment's Length() overflowed and drove Written negative, so
// progressed() was false and the 404 was classified as permanent.
func TestPostProgressNotFoundUnknownAndKnownLength(t *testing.T) {
	const size = 1 << 20

	for _, tc := range []struct {
		name        string
		knownLength bool
	}{
		{"known length", true},
		{"unknown length", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler := &dropThenNotFoundServer{size: size, knownLength: tc.knownLength}
			srv := httptest.NewServer(handler)
			defer srv.Close()

			dest := filepath.Join(t.TempDir(), "out.bin")

			// One connection in both arms, so the comparison is of the single
			// stream semantics rather than of the segment scheduler.
			d, err := New(testOptions(1))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			res, err := d.Download(context.Background(), Request{URL: srv.URL + "/file", Dest: dest})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if res.RangeSupported != tc.knownLength {
				t.Errorf("RangeSupported = %v, want %v", res.RangeSupported, tc.knownLength)
			}

			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatalf("read %s: %v", dest, err)
			}
			if want := testsrv.Expected(testSeed, size); !bytes.Equal(got, want) {
				t.Fatalf("downloaded %d bytes but contents differ", len(got))
			}

			// The 404 arrived only after bytes had been written, so it had to
			// be retried rather than reported as a dead link.
			if n := handler.attemptsMade(); n < 3 {
				t.Errorf("the server saw %d download attempts, want at least 3 (drop, 404, success) — the post-progress 404 was not retried", n)
			}
			if n := handler.notFoundServed(); n != 1 {
				t.Errorf("server served %d 404s, want exactly 1", n)
			}
		})
	}
}
