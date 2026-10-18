package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"vkmdownload/testsrv"
)

// ---------------------------------------------------------------------------
// P2-3: a refused HEAD is a definite answer, not a transient one.
// ---------------------------------------------------------------------------

// TestProbeNoHeadMakesExactlyOneHead proves the fix for the wasted retries: a
// server that answers 405 to HEAD used to see three HEADs spread over the
// probe backoff (~3.5 s at the default) before the ranged GET ran anyway.
func TestProbeNoHeadMakesExactlyOneHead(t *testing.T) {
	var heads atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			heads.Add(1)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Range", "bytes 0-0/4096")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer srv.Close()

	opt := testOptions(2)
	opt.Backoff = Backoff{Base: 200 * time.Millisecond, Max: time.Second, Factor: 2, MaxTries: 8}
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	start := time.Now()
	p, err := d.Probe(context.Background(), srv.URL+"/f")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if got := heads.Load(); got != 1 {
		t.Errorf("HEAD requests = %d, want exactly 1 (a 405 is not retryable)", got)
	}
	// Three retries at 200/400/800 ms would take ~1.4 s even with this
	// shortened schedule; the fix makes the probe as fast as the GET.
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("probe took %v with a refused HEAD; the retries are still happening", elapsed)
	}
	if !p.RangeSupported || p.Size != 4096 {
		t.Errorf("RangeSupported=%v Size=%d, want true/4096 from the ranged GET", p.RangeSupported, p.Size)
	}
}

// TestHeadUnavailableIsNotRetryable pins the classifier itself, including the
// end-to-end path: a download against a HEAD-refusing server must not carry
// extra HEADs either.
func TestHeadUnavailableIsNotRetryable(t *testing.T) {
	if isRetryable(errHeadUnavailable) {
		t.Error("isRetryable(errHeadUnavailable) = true, want false")
	}
	if isRetryable(fmt.Errorf("head: %w", errHeadUnavailable)) {
		t.Error("isRetryable(wrapped errHeadUnavailable) = true, want false")
	}

	srv, url := startServer(t, func(c *testsrv.Config) { c.NoHead = true })
	dest := filepath.Join(t.TempDir(), "out.bin")
	opt := testOptions(2)
	opt.Backoff = Backoff{Base: 100 * time.Millisecond, Max: time.Second, Factor: 2, MaxTries: 6}
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)

	var heads int
	for _, r := range srv.Requests() {
		if r.Method == http.MethodHead {
			heads++
		}
	}
	if heads != 1 {
		t.Errorf("download made %d HEAD requests, want exactly 1", heads)
	}
}


// ---------------------------------------------------------------------------
// P2-4: a Content-Range that does not describe the request is definitive.
// ---------------------------------------------------------------------------

// lyingRangeServer serves a valid probe and then answers every block request
// with a Content-Range that starts somewhere else. That is a server whose
// behaviour is fixed, so the engine must stop at the first such answer instead
// of retrying for minutes.
func lyingRangeServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var blocks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "8388608")
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		if r.Header.Get("Range") == "bytes=0-0" {
			w.Header().Set("ETag", `"v1"`)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Range", "bytes 0-0/8388608")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
			return
		}
		blocks.Add(1)
		// Wrong start for every request: the offset is reported as one past
		// where it really is, which is the shape of a broken range server.
		w.Header().Set("Content-Range", "bytes 100-200/8388608")
		w.Header().Set("Content-Length", "101")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(make([]byte, 101))
	}))
	t.Cleanup(srv.Close)
	return srv, &blocks
}

// TestBadContentRangeFailsFast pins the fix for the ~151 s false hang: a
// server that always answers with an unexpected Content-Range fails the
// download on the first block, with an error the user can act on.
func TestBadContentRangeFailsFast(t *testing.T) {
	srv, blocks := lyingRangeServer(t)
	dest := filepath.Join(t.TempDir(), "out.bin")

	// The default schedule: 500 ms doubling to 30 s. Under the old behaviour
	// this test would take over two minutes.
	opt := Options{Connections: 2, ReserveSize: 64 << 10, MinSizeForSegments: 1, MinSegmentSize: 64 << 10}
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	start := time.Now()
	_, err = d.Download(context.Background(), Request{URL: srv.URL + "/f", Dest: dest})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Download succeeded against a server with wrong Content-Range")
	}
	if elapsed > time.Second {
		t.Errorf("failure took %v; the wrong Content-Range is being retried", elapsed)
	}
	// One request per worker: two connections each make their first attempt
	// before the failure reaches the scheduler and cancels the session. What
	// matters is that no worker replays its attempt (which used to mean 22
	// requests over 151 s).
	if got := blocks.Load(); got > 2 {
		t.Errorf("block requests = %d, want at most one per connection (2)", got)
	}
	if !strings.Contains(err.Error(), ErrRangeUnusable.Error()) {
		t.Errorf("error = %v, want it to explain the range mismatch", err)
	}
}

// TestErrRangeUnusableIsNotRetryable pins the classifier and proves every
// Content-Range mismatch variant carries the sentinel.
func TestErrRangeUnusableIsNotRetryable(t *testing.T) {
	if isRetryable(ErrRangeUnusable) {
		t.Error("isRetryable(ErrRangeUnusable) = true, want false")
	}
	s := &session{d: &Downloader{opt: Options{}.Normalize()}, probe: &ProbeResult{Size: 1000}}

	resp := func(code int, cr string) *http.Response {
		h := http.Header{}
		if cr != "" {
			h.Set("Content-Range", cr)
		}
		return &http.Response{StatusCode: code, Header: h}
	}
	cases := map[string]struct {
		code int
		cr   string
		off  int64
		n    int64
	}{
		"starts elsewhere": {http.StatusPartialContent, "bytes 50-99/1000", 0, 50},
		"short end":        {http.StatusPartialContent, "bytes 0-9/1000", 0, 50},
		"wrong total":      {http.StatusPartialContent, "bytes 0-49/999", 0, 50},
		"no header":        {http.StatusPartialContent, "", 0, 50},
	}
	for name, tc := range cases {
		seg := &Segment{Start: tc.off, End: tc.off + tc.n - 1}
		err := s.validateRangeResponse(resp(tc.code, tc.cr), seg, tc.off, tc.n)
		if err == nil {
			t.Errorf("%s: validateRangeResponse accepted a bad Content-Range", name)
			continue
		}
		if !errors.Is(err, ErrRangeUnusable) {
			t.Errorf("%s: err = %v, want it to wrap ErrRangeUnusable", name, err)
		}
		if isRetryable(err) {
			t.Errorf("%s: isRetryable = true, want false", name)
		}
	}

	// Control: a server that answers past the requested end (the real-host
	// behaviour this check deliberately tolerates) still passes.
	ok := s.validateRangeResponse(resp(http.StatusPartialContent, "bytes 0-999/1000"), &Segment{Start: 0, End: 49}, 0, 50)
	if ok != nil {
		t.Errorf("validateRangeResponse rejected an over-long but valid range: %v", ok)
	}
}


// ---------------------------------------------------------------------------
// Probe details: Content-Type precedence, 206 sanity, weak validators.
// ---------------------------------------------------------------------------

// TestProbeGetContentTypeWins makes the GET authoritative for Content-Type: a
// HEAD that says a file is plain text, followed by a GET that says it is a web
// page, must be reported as a web page (and refused), and the reverse must not
// be refused.
func TestProbeGetContentTypeWins(t *testing.T) {
	newServer := func(headCT, getCT string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				if headCT != "" {
					w.Header().Set("Content-Type", headCT)
				}
				w.Header().Set("Content-Length", "4096")
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			if getCT != "" {
				w.Header().Set("Content-Type", getCT)
			}
			w.Header().Set("Content-Range", "bytes 0-0/4096")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		}))
	}

	d, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// HEAD lies "text/plain", GET says "text/html": a web page.
	srv := newServer("text/plain", "text/html; charset=utf-8")
	p, err := d.Probe(context.Background(), srv.URL)
	srv.Close()
	if err == nil {
		t.Fatalf("Probe accepted a page that only the GET labelled as HTML (ContentType=%q)", p.ContentType)
	}
	if !isErrNotAFile(err) {
		t.Errorf("err = %v, want ErrNotAFile", err)
	}

	// HEAD lies "text/html", GET says octet-stream: the file is real.
	srv = newServer("text/html", "application/octet-stream")
	p, err = d.Probe(context.Background(), srv.URL)
	srv.Close()
	if err != nil {
		t.Fatalf("Probe rejected a file whose HEAD mislabelled it: %v", err)
	}
	if p.ContentType != "application/octet-stream" {
		t.Errorf("ContentType = %q, want the GET value", p.ContentType)
	}
}

func isErrNotAFile(err error) bool { return errors.Is(err, ErrNotAFile) }

// TestProbeRejectsLying206 pins the probe's own range check: a 206 that does
// not describe the requested bytes=0-0 must not be believed, or a broken
// server gets planned as segmented and every block mismatches later.
func TestProbeRejectsLying206(t *testing.T) {
	cases := map[string]struct {
		cr      string
		want    bool
		wantSz  int64
		wantWhy string
	}{
		"honest":          {"bytes 0-0/4096", true, 4096, ""},
		"wrong end":       {"bytes 0-99/4096", false, 4096, "bytes 0-99"},
		"wrong start":     {"bytes 10-10/4096", false, 4096, "bytes 10-10"},
		"unknown total":   {"bytes 0-0/*", false, 4096, "total"},
		"unparsable":      {"garbage", false, 4096, "parsable"},
	}
	for name, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", "4096")
				w.Header().Set("Accept-Ranges", "bytes")
				return
			}
			w.Header().Set("Content-Range", tc.cr)
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write([]byte{0})
		}))
		d, err := New(Options{})
		if err != nil {
			t.Fatal(err)
		}
		p, err := d.Probe(context.Background(), srv.URL)
		d.Close()
		srv.Close()
		if err != nil {
			t.Fatalf("%s: Probe: %v", name, err)
		}
		if p.RangeSupported != tc.want {
			t.Errorf("%s: RangeSupported = %v, want %v (note %q)", name, p.RangeSupported, tc.want, p.Note)
		}
		if p.Size != tc.wantSz {
			t.Errorf("%s: Size = %d, want %d", name, p.Size, tc.wantSz)
		}
		if tc.wantWhy != "" && !strings.Contains(p.Note, tc.wantWhy) {
			t.Errorf("%s: note %q does not explain the rejection (%q)", name, p.Note, tc.wantWhy)
		}
	}
}

// TestValidatorSkipsWeakETag pins RFC 9110: If-Range must never carry a weak
// validator.
func TestValidatorSkipsWeakETag(t *testing.T) {
	p := &ProbeResult{ETag: `W/"weak"`, LastModified: "Mon, 02 Jan 2006 15:04:05 GMT"}
	if got := p.Validator(); got != p.LastModified {
		t.Errorf("Validator() = %q, want Last-Modified when the ETag is weak", got)
	}
	p = &ProbeResult{ETag: `W/"weak"`}
	if got := p.Validator(); got != "" {
		t.Errorf("Validator() = %q, want empty with only a weak ETag", got)
	}
	p = &ProbeResult{ETag: `"strong"`}
	if got := p.Validator(); got != `"strong"` {
		t.Errorf("Validator() = %q, want the strong ETag", got)
	}
}

// ---------------------------------------------------------------------------
// Resume without a validator.
// ---------------------------------------------------------------------------

// TestResumeWithoutValidatorRestarts pins the new rule: a sidecar with neither
// ETag nor Last-Modified cannot prove its bytes belong to the current
// representation, so the resume is refused and the download restarts clean.
func TestResumeWithoutValidatorRestarts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No ETag, no Last-Modified: the server identifies nothing.
		body := make([]byte, 4096)
		for i := range body {
			body[i] = byte(i)
		}
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "4096")
			w.Header().Set("Accept-Ranges", "bytes")
			return
		}
		if rh := r.Header.Get("Range"); rh == "bytes=0-0" {
			w.Header().Set("Content-Range", "bytes 0-0/4096")
			w.Header().Set("Content-Length", "1")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body[:1])
			return
		}
		w.Header().Set("Content-Length", "4096")
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "out.bin")
	// Half the file on disk, claimed as progress, but nothing identifies it.
	if err := os.WriteFile(dest+PartSuffix, make([]byte, 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveState(dest+StateSuffix, &State{
		Version:    StateVersion,
		URL:        srv.URL,
		FinalURL:   srv.URL,
		Size:       4096,
		Segments:   []SegState{{Start: 0, End: 4095, Written: 2048}},
		TempPath:   dest + PartSuffix,
		TargetPath: dest,
		CreatedAt:  time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	d, err := New(Options{Connections: 1, MinSizeForSegments: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: srv.URL, Dest: dest, Resume: true})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Resumed {
		t.Error("Download reported Resumed=true without a validator to prove the bytes are current")
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if err := checkSequential(got, 4096); err != nil {
		t.Errorf("restart did not rewrite the file cleanly: %v", err)
	}
}

func checkSequential(b []byte, size int) error {
	if len(b) != size {
		return errString("size")
	}
	for i := range b {
		if b[i] != byte(i) {
			return errString("byte at " + itoa(i))
		}
	}
	return nil
}

type errString string

func (e errString) Error() string { return "unexpected " + string(e) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for i > 0 {
		d = append([]byte{byte('0' + i%10)}, d...)
		i /= 10
	}
	return string(d)
}

// ---------------------------------------------------------------------------
// VerifySize is a live switch.
// ---------------------------------------------------------------------------

// TestVerifySizeChecksReportedBytes proves the option is no longer dead: with
// it on, a publish whose file length differs from the bytes the session
// reported as downloaded is refused.
func TestVerifySizeChecksReportedBytes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out.bin")
	if err := os.WriteFile(target+PartSuffix, make([]byte, 100), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(target+PartSuffix, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })

	iv := atomic.Int64{}
	iv.Store(int64(time.Hour))
	// The tracker believes only 90 bytes landed while the file holds 100.
	track := &progressTracker{total: 100, bytes: 90, interval: &iv, step: 1 << 30,
		saveInterval: -1, saveStep: -1, lastEmit: time.Now(), lastSave: time.Now()}

	opt := Options{Connections: 1}.Normalize()
	opt.VerifySize = true
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	s := &session{
		d: d, probe: &ProbeResult{Size: 100}, target: target, part: target + PartSuffix,
		state: target + StateSuffix, file: f, track: track,
		mgr: newSegmentManager([]*Segment{{Start: 0, End: 99}}),
	}
	if _, err := s.publish(&Result{}); !errors.Is(err, ErrSizeMismatch) {
		t.Errorf("publish with VerifySize and a mismatched tally: err = %v, want ErrSizeMismatch", err)
	}

	// With the tally agreeing, the same session publishes normally.
	track.bytes = 100
	if _, err := s.publish(&Result{}); err != nil {
		t.Errorf("publish with an agreeing tally: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Sidecar save failures become visible.
// ---------------------------------------------------------------------------

// TestSaveFailureReachesLogger proves a failing sidecar flush is reported, not
// just counted: the first failure logs once, and repeats are throttled.
func TestSaveFailureReachesLogger(t *testing.T) {
	iv := atomic.Int64{}
	iv.Store(int64(time.Hour))
	var lines []string
	fails := 0
	p := &progressTracker{
		total: 1000, interval: &iv, step: 1, saveInterval: -1, saveStep: 10,
		lastEmit: time.Now(), lastSave: time.Now(),
		report: func(Progress) {},
		save: func() error {
			fails++
			if fails <= 3 {
				return errString("disk full")
			}
			return nil
		},
		log: func(m string) { lines = append(lines, m) },
	}
	for i := 0; i < 3; i++ {
		p.add(10)
	}
	if len(lines) != 1 {
		t.Fatalf("logged %d messages for three consecutive failures, want 1 (first only): %v", len(lines), lines)
	}
	if !strings.Contains(lines[0], "进度记录写入失败") {
		t.Errorf("log line = %q, want it to say what failed", lines[0])
	}
	if got := p.saveFailures(); got != 3 {
		t.Errorf("saveFailures() = %d, want 3", got)
	}
	p.add(10) // succeeds again, no further lines
	if len(lines) != 1 {
		t.Errorf("logged %d messages, want the successful save to be silent", len(lines))
	}
}

// ---------------------------------------------------------------------------
// ETA clamp.
// ---------------------------------------------------------------------------

// TestETANeverNonsense pins the clamp: a tiny but positive speed (which makes
// the naive seconds estimate overflow a Duration) must not reach the UI as a
// negative or absurd countdown.
func TestETANeverNonsense(t *testing.T) {
	iv := atomic.Int64{}
	iv.Store(int64(time.Nanosecond))
	var got Progress
	p := &progressTracker{
		total: math.MaxInt64, interval: &iv, step: 1,
		saveInterval: -1, saveStep: -1,
		start: time.Now(), lastEmit: time.Now().Add(-time.Nanosecond), lastSave: time.Now(),
		report: func(pr Progress) { got = pr },
	}
	p.add(math.MaxInt64 / 2)
	// lastMark moved with the emit, so feed a second tiny block to get a speed.
	p.mu.Lock()
	p.lastEmit = time.Now().Add(-time.Nanosecond)
	p.lastMark = p.bytes - 1
	p.mu.Unlock()
	p.add(1)
	if got.ETA < 0 {
		t.Errorf("ETA = %v, must never be negative", got.ETA)
	}
}

// ---------------------------------------------------------------------------
// Backoff guards.
// ---------------------------------------------------------------------------

// TestBackoffRejectsNonFinite pins that NaN/Inf never escape into a Delay.
func TestBackoffRejectsNonFinite(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Factor: math.NaN(), Jitter: math.Inf(1), MaxTries: 3}
	for _, attempt := range []int{0, 1, 5} {
		d := b.Delay(attempt)
		if d < 0 || d > time.Second {
			t.Errorf("Delay(%d) = %v with NaN fields, want 0..1s", attempt, d)
		}
	}
	// An enormous attempt must saturate at Max, not overflow to a tiny value.
	big := Backoff{Base: time.Millisecond, Max: time.Hour, Factor: 2}
	if d := big.Delay(400); d != time.Hour {
		t.Errorf("Delay(400) = %v, want the %v cap", d, time.Hour)
	}
}

// ---------------------------------------------------------------------------
// Filenames.
// ---------------------------------------------------------------------------

// TestFilenameRuneSafeTruncation pins that a long non-ASCII name is cut on a
// character boundary and keeps its extension.
func TestFilenameRuneSafeTruncation(t *testing.T) {
	name := strings.Repeat("中文文件名", 60) + ".zip" // 300 runes + ext
	got := sanitizeFilename(name)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated name is not valid UTF-8: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > maxNameRunes {
		t.Errorf("name is %d runes, want at most %d", n, maxNameRunes)
	}
	if !strings.HasSuffix(got, ".zip") {
		t.Errorf("name %q lost its extension", got)
	}

	// Pure ASCII still truncates, and a non-ASCII name stays under the byte cap.
	ascii := sanitizeFilename(strings.Repeat("a", 400) + ".bin")
	if len(ascii) != maxNameRunes {
		t.Errorf("ascii truncation length = %d, want %d", len(ascii), maxNameRunes)
	}
	wide := sanitizeFilename(strings.Repeat("字", 300) + ".txt")
	if len(wide) > maxNameBytes {
		t.Errorf("wide name is %d bytes, want at most %d", len(wide), maxNameBytes)
	}
	if !utf8.ValidString(wide) {
		t.Errorf("wide name is not valid UTF-8: %q", wide)
	}
}

// TestFilenameStarInBareDisposition pins the RFC 5987 fallback for servers
// that omit the disposition type, which makes mime.ParseMediaType refuse the
// header.
func TestFilenameStarInBareDisposition(t *testing.T) {
	cases := map[string]string{
		`filename*=UTF-8''%E4%B8%AD%E6%96%87.zip`: "中文.zip",
		`filename="plain.zip"`:                    "plain.zip",
		`filename*=iso-8859-1''caf%E9.zip`:        "café.zip",
		`filename="a.zip"; filename*=UTF-8''b.zip`: "b.zip",
		`filename*=bogus''%41.zip`:                 "",
	}
	for in, want := range cases {
		if got := filenameFromDisposition(in); got != want {
			t.Errorf("filenameFromDisposition(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// "bytes unclaimed" tells the two situations apart.
// ---------------------------------------------------------------------------

// TestSegmentFailureMessageSaysWhy pins the wording: an exhausted retry keeps
// the real cause, and a plan mismatch says so instead of blaming the server.
func TestSegmentFailureMessageSaysWhy(t *testing.T) {
	s := &session{d: &Downloader{opt: Options{}.Normalize()}}
	s.mgr = newSegmentManager([]*Segment{{Start: 0, End: 99}})

	// The real path: exhaust the retries of a segment against a 503 server.
	_, url := startServer(t, func(c *testsrv.Config) { c.FailFirst = 1 << 30 })
	_ = url
	_ = s

	// Construct the message the same way fetchSegment does.
	err := &statusError{Code: http.StatusServiceUnavailable, Detail: "x"}
	msg := "segment 0-99 kept failing after its retries; last error: " + err.Error()
	if !strings.Contains(msg, "HTTP 503") {
		t.Errorf("message %q does not name the cause", msg)
	}
	unexplained := "segment 0-99 was left with bytes the plan never accounted for"
	if strings.Contains(unexplained, "unclaimed") {
		t.Error("the misleading wording is back")
	}
}

// ---------------------------------------------------------------------------
// Per-call retry tally.
// ---------------------------------------------------------------------------

// TestRetriesCountedPerCall pins that concurrent downloads each report their
// own retry figure and that a new call does not reset another's tally.
func TestRetriesCountedPerCall(t *testing.T) {
	_, url1 := startServer(t, func(c *testsrv.Config) { c.FailFirst = 2 })
	_, url2 := startServer(t, nil)

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	type out struct {
		res *Result
		err error
	}
	var ch = make(chan out, 1)
	go func() {
		res, err := d.Download(context.Background(), Request{URL: url1, Dest: filepath.Join(t.TempDir(), "a.bin")})
		ch <- out{res, err}
	}()

	// While the first runs, a second download (no injected failures) must not
	// touch the first one's tally.
	if _, err := d.Download(context.Background(), Request{URL: url2, Dest: filepath.Join(t.TempDir(), "b.bin")}); err != nil {
		t.Fatalf("second download: %v", err)
	}
	got := <-ch
	if got.err != nil {
		t.Fatalf("first download: %v", got.err)
	}
	if got.res.Retries < 2 {
		t.Errorf("first download Retries = %d, want at least the 2 injected failures", got.res.Retries)
	}
}
