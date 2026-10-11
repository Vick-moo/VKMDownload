package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// defaultUserAgent identifies the client. It is deliberately not a browser
// string: honesty costs nothing here and a real UA would make debugging which
// agent touched a server harder.
const defaultUserAgent = "vdownload/0.1"

var (
	// ErrIfRangeIgnored means a server answered a validated range request with
	// a full 200 body. The bytes already on disk cannot be trusted to belong to
	// the same representation, so the download must restart.
	ErrIfRangeIgnored = errors.New("server ignored If-Range and returned a full 200 response")

	// ErrSizeMismatch means the finished file is not the announced length.
	// With a server that lies about Content-Length this is the only check that
	// catches a silently truncated download.
	ErrSizeMismatch = errors.New("completed file size does not match the announced size")
)

// statusError carries a non-2xx response through the retry classifier.
type statusError struct {
	Code   int
	Detail string
}

func (e *statusError) Error() string { return fmt.Sprintf("HTTP %d: %s", e.Code, e.Detail) }

// Progress is a throttled snapshot handed to the progress callback.
type Progress struct {
	Total        int64
	Downloaded   int64
	Speed        float64 // bytes/sec measured over the last reporting interval
	ETA          time.Duration
	Connections  int
	Segments     int
	SegmentsDone int
}

// Options configures a Downloader. The zero value is usable; Normalize fills in
// sensible defaults.
type Options struct {
	// Connections is the maximum number of parallel segment workers.
	Connections int

	// MinSizeForSegments forces a single stream for resources smaller than
	// this. Splitting a 200 KiB file into eight ranges costs more in round
	// trips than it can possibly recover.
	MinSizeForSegments int64

	// MinSegmentSize is the smallest a segment may become. It also bounds
	// dynamic splitting, so a fast worker cannot shatter the plan into
	// thousands of tiny requests.
	MinSegmentSize int64

	// ChunkSize is the read/write buffer per worker.
	ChunkSize int
	// ReserveSize is how many bytes a worker claims at once. It bounds how far
	// the split point can drift ahead of the bytes actually written.
	ReserveSize int64

	// Backoff controls retry pacing.
	Backoff Backoff

	// Headers are sent with every request: Cookie, Referer, Authorization...
	Headers http.Header

	// ProgressInterval and ProgressBytes throttle progress reporting and
	// state flushing. Both are checked; whichever fires first wins.
	ProgressInterval time.Duration
	ProgressBytes    int64

	// Transport configures the shared HTTP transport.
	Transport TransportOptions

	// VerifySize re-checks the finished length against the announced size
	// before publishing the file.
	VerifySize bool

	// Limiter caps throughput. Sharing one across downloaders limits their
	// combined speed; nil means unlimited.
	Limiter *Limiter
}

// Normalize returns o with unset fields replaced by defaults and out-of-range
// values clamped.
func (o Options) Normalize() Options {
	if o.Connections < 1 {
		o.Connections = 8
	}
	if o.Connections > 64 {
		o.Connections = 64
	}
	if o.MinSizeForSegments <= 0 {
		o.MinSizeForSegments = 2 << 20
	}
	if o.MinSegmentSize <= 0 {
		o.MinSegmentSize = 1 << 20
	}
	if o.ChunkSize <= 0 {
		o.ChunkSize = 128 << 10
	}
	if o.ReserveSize <= 0 {
		o.ReserveSize = 512 << 10
	}
	o.Backoff = o.Backoff.normalized()
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = 250 * time.Millisecond
	}
	if o.ProgressBytes <= 0 {
		o.ProgressBytes = 1 << 20
	}
	o.Transport = o.Transport.normalized()
	return o
}

func (t TransportOptions) normalized() TransportOptions {
	d := DefaultTransportOptions()
	if t.DialTimeout <= 0 {
		t.DialTimeout = d.DialTimeout
	}
	if t.KeepAlive <= 0 {
		t.KeepAlive = d.KeepAlive
	}
	if t.TLSHandshakeTimeout <= 0 {
		t.TLSHandshakeTimeout = d.TLSHandshakeTimeout
	}
	if t.ResponseHeaderTimeout <= 0 {
		t.ResponseHeaderTimeout = d.ResponseHeaderTimeout
	}
	if t.MaxIdlePerHost <= 0 {
		t.MaxIdlePerHost = d.MaxIdlePerHost
	}
	return t
}

// Request is one download job.
type Request struct {
	// URL is the resource to fetch.
	URL string
	// Dest is the destination file path, or a directory, or empty to use the
	// name suggested by the server.
	Dest string
	// Resume continues from an existing sidecar and partial file when they are
	// still consistent with the server's current representation.
	Resume bool
	// Progress, when non-nil, is called with throttled progress snapshots.
	Progress func(Progress)
}

// Result summarises a completed download.
type Result struct {
	Path string
	Size int64

	Probe *ProbeResult

	// Connections is the configured parallelism actually used.
	Connections int
	// InitialSegments is the plan size before dynamic splitting.
	InitialSegments int
	// Segments is the plan size at the end, after dynamic splitting.
	Segments int
	// Splits counts how many times a busy segment was halved for an idle
	// worker. A non-zero value is direct evidence that dynamic segmentation
	// engaged.
	Splits int

	RangeSupported bool
	Resumed        bool

	Duration    time.Duration
	BytesPerSec float64
	Retries     int64
}

// Downloader runs downloads over a shared HTTP/1.1 transport.
type Downloader struct {
	tr     *http.Transport
	client *http.Client
	opt    Options

	userAgent string
	retries   atomic.Int64
}

// New creates a Downloader.
func New(opt Options) (*Downloader, error) {
	opt = opt.Normalize()
	tr, err := NewTransport(opt.Transport)
	if err != nil {
		return nil, err
	}
	return &Downloader{
		tr: tr,
		client: &http.Client{
			Transport: tr,
			// Deliberately no Client.Timeout: a multi-gigabyte download
			// legitimately takes a long time. Phase timeouts live on the
			// transport, and cancellation flows through the context.
		},
		opt:       opt,
		userAgent: defaultUserAgent,
	}, nil
}

// Options returns the normalized options in effect.
func (d *Downloader) Options() Options { return d.opt }

// Transport exposes the underlying transport, mainly so tests can assert that
// HTTP/1.1 is really pinned.
func (d *Downloader) Transport() *http.Transport { return d.tr }

// Close releases idle connections.
func (d *Downloader) Close() { d.tr.CloseIdleConnections() }

// Download fetches req.URL into req.Dest.
//
// Strategy selection is a ladder, tried in order:
//
//  1. Segmented, with If-Range validation, when a resumable partial file
//     already exists. A server that answers 200 instead of 206 is refusing the
//     resume, so the partial data is discarded and the ladder advances.
//  2. Segmented from scratch. Ranges were proven to work during probe, so if
//     this also fails with a full 200 the server's behaviour changed and the
//     only safe option is a single stream.
//  3. Single stream, which needs no range support at all.
func (d *Downloader) Download(ctx context.Context, req Request) (*Result, error) {
	start := time.Now()
	d.retries.Store(0)

	if strings.TrimSpace(req.URL) == "" {
		return nil, errors.New("no URL given")
	}

	probe, err := d.Probe(ctx, req.URL)
	if err != nil {
		return nil, err
	}

	// A server that reports neither a length nor range support can still be
	// downloaded: one stream read to EOF is exactly what curl does with such a
	// resource. Refusing it — which this used to do — made every GitHub
	// archive/refs/tags/*.tar.gz link undownloadable, because codeload builds
	// those on the fly and answers chunked, with no Content-Length.
	//
	// The cost is that progress is reported in bytes rather than a percentage,
	// since there is no total to divide by, and the finished size cannot be
	// verified against an announced one.
	if probe.Size < 0 {
		probe.Note = strings.TrimSpace(probe.Note + " (length unknown; single stream to EOF)")
	}

	target, err := resolveTarget(req.Dest, probe.SuggestedName)
	if err != nil {
		return nil, err
	}

	s := &session{
		d:       d,
		req:     req,
		probe:   probe,
		target:  target,
		part:    target + PartSuffix,
		state:   target + StateSuffix,
		created: time.Now(),
	}

	canSegment := probe.RangeSupported &&
		probe.Size >= d.opt.MinSizeForSegments &&
		d.opt.Connections > 1

	// Load resumable state, if any, before choosing the ladder.
	if req.Resume {
		s.loadResumable()
	}

	type strategy struct {
		name string
		run  func(context.Context) (*Result, error)
	}
	var ladder []strategy
	if s.resumeState != nil {
		// Only worth trying If-Range when there is something to resume.
		s.useIfRange = true
		ladder = append(ladder, strategy{"segmented+if-range", s.runSegmented})
	}
	if canSegment {
		ladder = append(ladder, strategy{"segmented", s.runSegmented})
	}
	ladder = append(ladder, strategy{"single", s.runSingle})

	var lastErr error
	for i, st := range ladder {
		res, err := st.run(ctx)
		if err == nil {
			out, perr := s.publish(res)
			if perr != nil {
				s.cleanup()
				return nil, perr
			}
			out.Duration = time.Since(start)
			if secs := out.Duration.Seconds(); secs > 0 {
				out.BytesPerSec = float64(out.Size) / secs
			}
			out.Retries = d.retries.Load()
			return out, nil
		}

		lastErr = err
		if ctx.Err() != nil {
			s.cleanup()
			return nil, ctx.Err()
		}

		// ErrIfRangeIgnored is not a transport failure, it means this strategy
		// does not apply to this server. Discard the untrustworthy bytes and
		// move down the ladder.
		if errors.Is(err, ErrIfRangeIgnored) && i+1 < len(ladder) {
			s.resetForRetry()
			continue
		}

		s.cleanup()
		return nil, fmt.Errorf("%s download failed: %w", st.name, err)
	}

	s.cleanup()
	return nil, lastErr
}

// session holds the per-download mutable state.
type session struct {
	d      *Downloader
	req    Request
	probe  *ProbeResult
	target string
	part   string
	state  string

	file    *os.File
	mgr     *segmentManager
	track   *progressTracker
	created time.Time

	resumeState *State
	useIfRange  bool
	resumed     bool
	initialSegs int
	splits      int
}

// loadResumable attaches a usable sidecar to the session, or leaves it nil.
func (s *session) loadResumable() {
	st, err := loadState(s.state)
	if err != nil {
		return
	}
	if !st.matches(s.probe) {
		// The resource changed under us. The partial bytes describe a
		// different representation, so they must not be mixed with new ones.
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	if !s.probe.RangeSupported {
		// Without range support there is no way to continue mid-file.
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	s.resumeState = st
	s.created = st.CreatedAt
	s.resumed = true
}

// openPart opens (creating if needed) the partial file.
func (s *session) openPart() error {
	if s.file != nil {
		return nil
	}
	f, err := os.OpenFile(s.part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open partial file %s: %w", s.part, err)
	}
	s.file = f
	return nil
}

// effectiveAvailable reports how many bytes of the partial file can be trusted.
//
// This is the size of the file on disk, capped at the announced resource size.
// The sidecar can be newer than the data it describes if the process died
// between writing the sidecar and flushing the file, and trusting a segment
// that claims more progress than the file holds would silently skip bytes.
func (s *session) effectiveAvailable() int64 {
	if s.file == nil {
		return 0
	}
	st, err := s.file.Stat()
	if err != nil {
		return 0
	}
	n := st.Size()
	if s.probe.Size >= 0 && n > s.probe.Size {
		n = s.probe.Size
	}
	return n
}

// newTracker builds the throttled progress reporter for this session.
func (s *session) newTracker(initial, total int64) {
	s.track = &progressTracker{
		total:    total,
		bytes:    initial,
		lastMark: initial,
		start:    time.Now(),
		lastEmit: time.Now(),
		interval: s.d.opt.ProgressInterval,
		step:     s.d.opt.ProgressBytes,
		conns:    s.d.opt.Connections,
	}
	s.track.save = s.saveState
	if s.req.Progress != nil {
		s.track.report = s.req.Progress
	}
	if s.mgr != nil {
		s.track.segments = func() (int, int) {
			_, total, done := s.mgr.progress()
			return total, done
		}
	}
}

// saveState persists the sidecar. It is called from the throttled tracker, so
// it stays off the hot path.
func (s *session) saveState() error {
	if s.state == "" || s.mgr == nil {
		return nil
	}

	segs := s.mgr.segmentStates()

	// Clamp every segment's progress to what the file actually holds.
	//
	// A cancelled transfer can leave the in-memory counters ahead of the bytes
	// on disk: the claim for the block being written is settled on the way out,
	// and the last write may not have reached the file. Persisting the larger
	// number would make a later resume start past bytes that were never
	// written, leaving a hole in a file whose length is still exactly right —
	// the one failure mode no length check can detect.
	if s.file != nil {
		if fi, err := s.file.Stat(); err == nil {
			available := fi.Size()
			if s.probe.Size >= 0 && available > s.probe.Size {
				available = s.probe.Size
			}
			for i := range segs {
				if room := available - segs[i].Start; segs[i].Written > room {
					if room < 0 {
						room = 0
					}
					segs[i].Written = room
					if segs[i].Written < segs[i].End-segs[i].Start+1 {
						segs[i].Done = false
					}
				}
			}
		}
	}

	return saveState(s.state, &State{
		Version:      StateVersion,
		URL:          s.req.URL,
		FinalURL:     s.probe.FinalURL,
		Size:         s.probe.Size,
		ETag:         s.probe.ETag,
		LastModified: s.probe.LastModified,
		NoRange:      !s.probe.RangeSupported,
		Segments:     segs,
		TempPath:     s.part,
		TargetPath:   s.target,
		CreatedAt:    s.created,
	})
}

// runSegmented downloads with multiple range requests.
func (s *session) runSegmented(ctx context.Context) (*Result, error) {
	if err := s.openPart(); err != nil {
		return nil, err
	}

	var reserved int64
	if s.resumeState != nil {
		available := s.effectiveAvailable()
		mgr, got := segmentsFromStates(s.resumeState.Segments, s.probe.Size, available, false)
		if mgr != nil {
			s.mgr = mgr
			reserved = got
		}
	}
	if s.mgr == nil {
		s.mgr = newSegmentManager(planSegments(s.probe.Size, s.d.opt.Connections, s.d.opt.MinSegmentSize))
	}
	s.initialSegs = s.mgr.segmentCount()
	s.newTracker(reserved, s.probe.Size)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	n := s.d.opt.Connections
	if n > s.initialSegs {
		n = s.initialSegs
	}

	type jobResult struct {
		seg *Segment
		err error
	}
	results := make(chan jobResult, n+1)

	running := 0
	for running < n {
		seg := s.mgr.take()
		if seg == nil {
			break
		}
		running++
		go func(seg *Segment) {
			results <- jobResult{seg, s.fetchSegment(ctx, seg)}
		}(seg)
	}

	var firstErr error
	for running > 0 {
		r := <-results
		running--

		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
				cancel()
			}
			continue
		}
		if firstErr != nil {
			continue
		}

		s.mgr.complete(r.seg)

		// A worker that just finished should help finish the biggest remaining
		// segment rather than idle: halve it and hand the tail to the next
		// free worker.
		if s.mgr.splitLargest(s.d.opt.MinSegmentSize) {
			s.splits++
		}

		if seg := s.mgr.take(); seg != nil {
			running++
			go func(seg *Segment) {
				results <- jobResult{seg, s.fetchSegment(ctx, seg)}
			}(seg)
		}
	}

	if firstErr != nil {
		return nil, firstErr
	}
	if !s.mgr.allDone() {
		return nil, errors.New("scheduler finished with segments still outstanding")
	}

	// allDone already confirmed every segment completed above.
	return &Result{
		Path:            s.target,
		Size:            s.probe.Size,
		Probe:           s.probe,
		Connections:     n,
		InitialSegments: s.initialSegs,
		Segments:        s.mgr.segmentCount(),
		Splits:          s.splits,
		RangeSupported:  true,
		Resumed:         s.resumed,
	}, nil
}

// fetchSegment downloads one segment, retrying retryable failures with
// exponential backoff.
func (s *session) fetchSegment(ctx context.Context, seg *Segment) error {
	b := s.d.opt.Backoff.normalized()

	for attempt := 0; attempt <= b.MaxTries; attempt++ {
		if attempt > 0 {
			s.d.retries.Add(1)
			if err := b.Sleep(ctx, attempt-1); err != nil {
				return err
			}
		}

		err := s.fetchOnce(ctx, seg)
		switch {
		case err == nil:
			debugf("seg-done    seg=%s", s.mgr.describe(seg))
			return nil
		case errors.Is(err, errSegmentIncomplete):
			// One reserve block landed and the segment still has work left.
			// Nothing failed, so the attempt budget is not consumed: restart
			// the count so a long segment cannot exhaust MaxTries and fail
			// merely for being large.
			attempt = -1
		case !isRetryable(err):
			return err
		}
	}

	// The loop can only fall through when every attempt asked for more work
	// than the segment had left, which fetchOnce maps to errSegmentIncomplete,
	// and the last check found none. Reaching here means the plan accounted for
	// fewer bytes than the resource has.
	return fmt.Errorf("segment %s ended with bytes unclaimed", s.mgr.describe(seg))
}

// errSegmentIncomplete is a sentinel that drives the per-segment loop in
// fetchSegment. It is not a failure: it means "this reserve block is done, the
// segment still has work".
//
// The distinction matters because fetchOnce only ever fetches one ReserveSize
// block. Returning nil after a successful block would make fetchSegment treat
// the whole segment as finished, silently leaving every segment short by
// everything past its first block — a download that ends with a plausible file
// length and a hole in the middle.
var errSegmentIncomplete = errors.New("segment still has unreserved work")

// probeRetryBudget bounds how many times a probe request is replayed before the
// download is abandoned.
//
// Probes need their own retry path because a transient 503 during discovery
// would otherwise fail the whole download before a single segment was even
// planned: the per-segment backoff never gets a chance to run.
const probeRetryBudget = 3

// doWithRetry runs op, replaying it with exponential backoff while the error is
// classified as transient. The final error is returned unchanged so the caller
// keeps its status information.
func (d *Downloader) doWithRetry(ctx context.Context, op func(context.Context) error) error {
	b := d.opt.Backoff.normalized()
	var lastErr error

	for attempt := 0; attempt <= probeRetryBudget; attempt++ {
		if attempt > 0 {
			d.retries.Add(1)
			if err := b.Sleep(ctx, attempt-1); err != nil {
				return err
			}
		}
		err := op(ctx)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryable(err) {
			return err
		}
	}
	return lastErr
}

// debugf writes a diagnostic line when VDOWNLOAD_DEBUG is set.
//
// With VDOWNLOAD_DEBUG=1 the lines go to stderr; with VDOWNLOAD_DEBUG=<path>
// they are appended to that file, which makes a high-volume trace analysable
// without fighting the test runner's own output.
var debugMu sync.Mutex

func debugf(format string, args ...any) {
	dest := os.Getenv("VDOWNLOAD_DEBUG")
	if dest == "" {
		return
	}
	line := fmt.Sprintf("[dbg] "+format+"\n", args...)

	debugMu.Lock()
	defer debugMu.Unlock()

	if dest == "1" {
		fmt.Fprint(os.Stderr, line)
		return
	}
	f, err := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	_, _ = f.WriteString(line)
	_ = f.Close()
}

// fetchOnce performs a single ranged request for up to ReserveSize bytes of the
// segment and writes them with WriteAt.
//
// WriteAt is the whole reason many workers can share one file handle: it takes
// an explicit offset and never moves a shared file cursor, so no worker can
// clobber another's position. Sharing one *os.File and using Write is the
// classic way to corrupt a segmented download.
func (s *session) fetchOnce(ctx context.Context, seg *Segment) error {
	off, n, ok := s.mgr.reserve(seg, s.d.opt.ReserveSize)
	if !ok {
		debugf("reserve-done seg=%s -> no work left", s.mgr.describe(seg))
		return nil
	}
	debugf("reserve     seg=%s off=%d n=%d", s.mgr.describe(seg), off, n)

	written := int64(0)
	// committed tracks how much of the claim has already been turned into real
	// progress, so the deferred cleanup hands back exactly the unfulfilled part.
	committed := int64(0)
	defer func() {
		if remaining := n - committed; remaining > 0 {
			s.mgr.unreserve(seg, remaining)
			debugf("unreserve   off=%d refunded=%d", off, remaining)
		}
		debugf("fetch-return off=%d want=%d wrote=%d", off, n, written)
	}()

	h := s.d.baseHeaders()
	if !seg.NoRange {
		h.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
		if s.useIfRange {
			if v := s.probe.Validator(); v != "" {
				h.Set("If-Range", v)
			}
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.probe.FinalURL, nil)
	if err != nil {
		return err
	}
	req.Header = h

	resp, err := s.d.client.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		_ = resp.Body.Close()
	}()

	if err := s.validateRangeResponse(resp, seg, off, n); err != nil {
		return err
	}

	bufp := bufPool.Get().(*[]byte)
	buf := *bufp
	defer bufPool.Put(bufp)

	for written < n {
		want := int64(len(buf))
		if r := n - written; r < want {
			want = r
		}
		got, rerr := io.ReadFull(resp.Body, buf[:want])
		if got > 0 {
			// The returned count matters as much as the error. A writer may
			// consume fewer bytes than offered and still return a nil error, and
			// treating that as a full write would leave the segment short while
			// its bookkeeping claimed the range was covered — a hole that no
			// length check can catch.
			wn, werr := s.file.WriteAt(buf[:got], off+written)
			if wn > 0 {
				debugf("wrote       [%d,%d)", off+written, off+written+int64(wn))
				written += int64(wn)
				// Only now does the segment's progress advance, and it is told
				// the absolute end rather than a byte count: reporting a
				// cumulative total as an increment is what previously made the
				// offset jump past a block that was never written.
				s.mgr.commit(seg, n, off+written)
				committed += int64(wn)
				s.track.add(int64(wn))
				// Throttle after the write, so the ceiling applies to bytes
				// actually stored rather than to bytes merely read into the
				// buffer. The limiter is shared, so several workers pause
				// together instead of each getting the full allowance.
				if err := s.d.opt.Limiter.Wait(ctx, wn); err != nil {
					return err
				}
			}
			if werr != nil {
				return fmt.Errorf("write %d bytes at offset %d: %w", got, off+written, werr)
			}
			if wn < got {
				// Hand the unwritten tail back so the next round re-fetches it
				// instead of stepping over it.
				return fmt.Errorf("short write at offset %d: wrote %d of %d bytes: %w",
					off+written, wn, got, io.ErrShortWrite)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				rerr = io.ErrUnexpectedEOF
			}
			return fmt.Errorf("short read for %d-%d: got %d of %d bytes: %w",
				off, off+n-1, written, n, rerr)
		}
	}

	// The block is on disk, but the segment may not be finished. Reporting
	// that distinctly is what keeps fetchSegment looping instead of declaring
	// victory after the first reserve block.
	//
	// The reminder is read through the manager because splitting mutates a
	// segment's end concurrently; a stale read here would report "finished" and
	// silently drop the bytes the split moved into the tail.
	if s.mgr.remaining(seg) > 0 {
		return errSegmentIncomplete
	}
	return nil
}

// validateRangeResponse checks that the response really describes the byte
// range that was asked for. A 206 carrying an unexpected Content-Range would
// otherwise be written at the wrong offset, producing a corrupt file that
// passes a length check.
func (s *session) validateRangeResponse(resp *http.Response, seg *Segment, off, n int64) error {
	if seg.NoRange {
		if resp.StatusCode != http.StatusOK {
			return &statusError{resp.StatusCode, "expected 200 for a non-range segment"}
		}
		return nil
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			return errors.New("206 response without a parsable Content-Range")
		}
		if start != off {
			return fmt.Errorf("206 Content-Range starts at %d, but %d was requested", start, off)
		}
		if end != off+n-1 {
			return fmt.Errorf("206 Content-Range ends at %d, but %d was requested", end, off+n-1)
		}
		if total >= 0 && s.probe.Size >= 0 && total != s.probe.Size {
			return fmt.Errorf("resource changed size mid-download: Content-Range says %d, probe said %d", total, s.probe.Size)
		}
		return nil

	case http.StatusOK:
		// The server answered with the whole resource. For a validated resume
		// this is a refusal of If-Range, and the partial bytes on disk cannot
		// be trusted to belong to the same representation.
		return ErrIfRangeIgnored

	default:
		return &statusError{
			resp.StatusCode,
			fmt.Sprintf("unexpected status for range %d-%d", off, off+n-1),
		}
	}
}

// runSingle downloads over one connection, which is the only option when the
// server does not support ranges or the resource is too small to bother.
func (s *session) runSingle(ctx context.Context) (*Result, error) {
	if err := s.openPart(); err != nil {
		return nil, err
	}

	// Ranges must be proven for a resume to be possible at all.
	canResume := s.probe.RangeSupported

	offset := int64(0)
	if canResume {
		offset = s.effectiveAvailable()
		if s.probe.Size >= 0 && offset > s.probe.Size {
			offset = s.probe.Size
		}
	}
	if err := s.file.Truncate(offset); err != nil {
		return nil, err
	}
	s.resumed = s.resumed || offset > 0

	// Single stream is one segment by definition. When the server never reported
	// a length the stream simply runs to EOF, so the segment carries no upper
	// bound — Size-1 would be a negative length, which the accounting would then
	// treat as an already-satisfied segment.
	segEnd := s.probe.Size - 1
	if s.probe.Size < 0 {
		segEnd = math.MaxInt64
	}
	s.mgr = newSegmentManager([]*Segment{{Start: 0, End: segEnd, NoRange: !s.probe.RangeSupported}})
	s.initialSegs = 1
	s.newTracker(offset, s.probe.Size)

	b := s.d.opt.Backoff.normalized()
	var lastErr error

	for attempt := 0; attempt <= b.MaxTries; attempt++ {
		if attempt > 0 {
			s.d.retries.Add(1)
			if err := b.Sleep(ctx, attempt-1); err != nil {
				return nil, err
			}
		}

		next, err := s.singleAttempt(ctx, offset, canResume)
		offset = next
		if err == nil {
			break
		}
		lastErr = err
		if !isRetryable(err) {
			return nil, err
		}
		if !canResume {
			// Nothing to resume from: a retry has to start over.
			offset = 0
			if terr := s.file.Truncate(0); terr != nil {
				return nil, terr
			}
			s.track.reset()
		}
		if attempt == b.MaxTries {
			return nil, fmt.Errorf("single-stream download gave up after %d attempts: %w", attempt+1, lastErr)
		}
	}

	if s.probe.Size >= 0 && offset != s.probe.Size {
		return nil, fmt.Errorf("%w: received %d bytes, expected %d", ErrSizeMismatch, offset, s.probe.Size)
	}

	return &Result{
		Path:            s.target,
		Size:            offset,
		Probe:           s.probe,
		Connections:     1,
		InitialSegments: 1,
		Segments:        1,
		RangeSupported:  s.probe.RangeSupported,
		Resumed:         s.resumed,
	}, nil
}

// singleAttempt performs one streaming GET and returns the new offset.
func (s *session) singleAttempt(ctx context.Context, offset int64, canResume bool) (int64, error) {
	h := s.d.baseHeaders()
	if canResume && offset > 0 {
		h.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		if v := s.probe.Validator(); v != "" {
			h.Set("If-Range", v)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.probe.FinalURL, nil)
	if err != nil {
		return offset, err
	}
	req.Header = h

	resp, err := s.d.client.Do(req)
	if err != nil {
		return offset, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		if offset > 0 {
			// Either the server never supported ranges or it refused If-Range.
			// This response is the full resource, so restart from zero and
			// consume it rather than throwing the body away.
			if err := s.file.Truncate(0); err != nil {
				return 0, err
			}
			s.track.reset()
			offset = 0
		}
	case http.StatusPartialContent:
		start, _, _, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok || start != offset {
			return offset, fmt.Errorf("206 Content-Range does not match the requested offset %d", offset)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if s.probe.Size >= 0 && offset == s.probe.Size {
			return offset, nil
		}
		return offset, &statusError{resp.StatusCode, "range not satisfiable"}
	default:
		return offset, &statusError{resp.StatusCode, "unexpected status for a single stream"}
	}

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)

	buf := (*bufp)[:s.d.opt.ChunkSize]
	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			wn, werr := s.file.WriteAt(buf[:n], offset)
			if wn > 0 {
				offset += int64(wn)
				s.track.add(int64(wn))
				// Keep the segment table truthful on this path too: the
				// sidecar is what a later resume trusts, and a single-stream
				// download that reported zero progress would look unrestartable.
				if s.mgr != nil {
					s.mgr.commit(s.mgr.segs[0], 0, offset)
				}
				if err := s.d.opt.Limiter.Wait(ctx, wn); err != nil {
					return offset, err
				}
			}
			if werr != nil {
				return offset, fmt.Errorf("write %d bytes at offset %d: %w", n, offset-int64(wn), werr)
			}
			if wn < n {
				return offset, fmt.Errorf("short write at offset %d: wrote %d of %d bytes: %w",
					offset-int64(wn), wn, n, io.ErrShortWrite)
			}
		}
		if errors.Is(rerr, io.EOF) {
			return offset, nil
		}
		if rerr != nil {
			return offset, rerr
		}
	}
}

// publish validates the finished file and moves it into place.
//
// The move happens only after the data is complete and flushed. Publishing a
// partially written file under its final name is how a download manager ends up
// handing the user a truncated file that looks finished.
func (s *session) publish(res *Result) (*Result, error) {
	if s.file == nil {
		return nil, errors.New("publish called before any file was opened")
	}

	if s.d.opt.VerifySize && s.probe.Size >= 0 {
		st, err := s.file.Stat()
		if err != nil {
			return nil, err
		}
		if st.Size() != s.probe.Size {
			return nil, fmt.Errorf("%w: file holds %d bytes, expected %d", ErrSizeMismatch, st.Size(), s.probe.Size)
		}
	}

	// Flush before renaming: without the sync a crash right after the rename
	// could leave a correctly named file with incomplete contents.
	if err := s.file.Sync(); err != nil {
		return nil, fmt.Errorf("sync %s: %w", s.part, err)
	}
	if err := s.file.Close(); err != nil {
		return nil, err
	}
	s.file = nil

	if err := moveFile(s.part, s.target); err != nil {
		return nil, err
	}

	// Report 100% as the final event so a UI never stalls just short of done.
	// This must happen *before* the sidecar is removed: finish() emits through
	// the tracker, and the tracker re-saves the sidecar on every emit, so
	// removing the file first would immediately resurrect it.
	if s.track != nil {
		s.track.finish()
	}
	_ = os.Remove(s.state)

	res.Path = s.target
	res.Segments = s.mgr.segmentCount()
	res.Splits = s.splits
	return res, nil
}

// resetForRetry discards partial data so the next strategy starts clean.
func (s *session) resetForRetry() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
	_ = os.Remove(s.part)
	_ = os.Remove(s.state)

	s.mgr = nil
	s.resumeState = nil
	s.useIfRange = false
	s.resumed = false
	s.splits = 0
	if s.track != nil {
		s.track.reset()
	}
}

// cleanup releases resources after a failure.
func (s *session) cleanup() {
	if s.file != nil {
		_ = s.file.Close()
		s.file = nil
	}
}

// bufPool recycles per-request read buffers across workers.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 256<<10)
		return &b
	},
}

// isRetryable classifies an error for the backoff loop.
//
// Client errors are not retried: a 404 will still be a 404 in thirty seconds.
// The exceptions are 408 and 429, which explicitly ask for a retry. Everything
// else, including short reads and 5xx, is treated as transient.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	// A refused resume is a strategy problem, not a transient fault.
	if errors.Is(err, ErrIfRangeIgnored) {
		return false
	}
	if errors.Is(err, ErrSizeMismatch) {
		return false
	}

	var se *statusError
	if errors.As(err, &se) {
		if se.Code >= 400 && se.Code < 500 {
			return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests
		}
		return true
	}
	return true
}

// resolveTarget turns the caller's destination into an absolute file path,
// treating a value that names an existing directory, or that ends in a
// separator, as a directory.
func resolveTarget(dest, suggested string) (string, error) {
	if suggested == "" {
		suggested = "download.bin"
	}
	if strings.TrimSpace(dest) == "" {
		return filepath.Abs(suggested)
	}

	abs, err := filepath.Abs(dest)
	if err != nil {
		return "", err
	}
	if strings.HasSuffix(dest, string(os.PathSeparator)) ||
		strings.HasSuffix(dest, "/") ||
		strings.HasSuffix(dest, `\`) {
		return filepath.Join(abs, suggested), nil
	}
	if st, err := os.Stat(abs); err == nil && st.IsDir() {
		return filepath.Join(abs, suggested), nil
	}
	return abs, nil
}

// moveFile moves src onto dst atomically when both live on the same volume,
// and falls back to copy-then-delete across volumes, where os.Rename cannot
// work.
func moveFile(src, dst string) error {
	if dir := filepath.Dir(dst); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	if err := os.Rename(src, dst); err == nil {
		return nil
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("cross-volume move: open %s: %w", src, err)
	}
	defer in.Close()

	// Copy to a sibling of the destination so the final rename stays on one
	// volume and remains atomic.
	tmp := dst + ".vdownload.tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("cross-volume move: create %s: %w", tmp, err)
	}

	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("cross-volume move: copy: %w", err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("cross-volume move: publish %s: %w", tmp, err)
	}
	return os.Remove(src)
}

// progressTracker throttles progress reporting and sidecar flushing.
//
// Flushing on every write would turn a 100 MiB download into hundreds of
// thousands of sidecar rewrites; flushing never would lose all progress on a
// crash. The tracker fires on whichever of "every N bytes" or "every N
// milliseconds" happens first.
type progressTracker struct {
	mu       sync.Mutex
	emitMu   sync.Mutex
	bytes    int64
	total    int64
	start    time.Time
	lastEmit time.Time
	lastMark int64
	interval time.Duration
	step     int64
	conns    int

	report   func(Progress)
	save     func() error
	segments func() (total, done int)
}

func (p *progressTracker) add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.bytes += n
	now := time.Now()
	dt := now.Sub(p.lastEmit)
	fire := dt >= p.interval || p.bytes-p.lastMark >= p.step

	var speed float64
	if fire {
		if secs := dt.Seconds(); secs > 0 {
			speed = float64(p.bytes-p.lastMark) / secs
		}
		p.lastEmit = now
		p.lastMark = p.bytes
	}
	bytes, total := p.bytes, p.total
	p.mu.Unlock()

	if !fire {
		return
	}
	p.emit(bytes, total, speed)
}

// reset returns the tracker to zero after a restart from scratch.
func (p *progressTracker) reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.bytes = 0
	p.lastMark = 0
	p.start = time.Now()
	p.lastEmit = time.Now()
	p.mu.Unlock()
}

// finish forces a final 100% event.
func (p *progressTracker) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	bytes, total := p.bytes, p.total
	p.mu.Unlock()
	p.emit(bytes, total, 0)
}

func (p *progressTracker) emit(bytes, total int64, speed float64) {
	if p == nil {
		return
	}
	// Serialise emission: add() decides which goroutine fires, but a slow
	// callback must not be entered concurrently.
	p.emitMu.Lock()
	defer p.emitMu.Unlock()

	if p.save != nil {
		_ = p.save()
	}
	if p.report == nil {
		return
	}

	prog := Progress{
		Total:       total,
		Downloaded:  bytes,
		Speed:       speed,
		Connections: p.conns,
	}
	if p.segments != nil {
		prog.Segments, prog.SegmentsDone = p.segments()
	}
	if speed > 0 && total > 0 && bytes < total {
		// (total-bytes)/speed is a count of *seconds*; it must be scaled by
		// time.Second before the conversion to a Duration. Converting the
		// second count first and multiplying afterwards would interpret it as
		// nanoseconds and inflate the estimate by 1e9.
		prog.ETA = time.Duration((float64(total-bytes) / speed) * float64(time.Second))
	}
	p.report(prog)
}
