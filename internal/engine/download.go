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
const defaultUserAgent = "vkmdownload/0.1"

var (
	// ErrIfRangeIgnored means a server answered a validated range request with
	// a full 200 body. The bytes already on disk cannot be trusted to belong to
	// the same representation, so the download must restart.
	ErrIfRangeIgnored = errors.New("server ignored If-Range and returned a full 200 response")

	// ErrSizeMismatch means the finished file is not the announced length.
	// With a server that lies about Content-Length this is the only check that
	// catches a silently truncated download.
	ErrSizeMismatch = errors.New("completed file size does not match the announced size")

	// ErrRangeUnusable means the server answered a range request with a
	// Content-Range that does not describe the range asked for: a different
	// start, a short end, or a different total.
	//
	// It is a definitive answer, not a hiccup. The same request produced the
	// same wrong header, so replaying it only burns the attempt budget while
	// the download appears frozen: at the default backoff a stable such server
	// held a worker for about 151 s before the first error reached the user.
	ErrRangeUnusable = fmt.Errorf("%w: server returned bytes for a different range than requested", errNotRetryable)

	// errNotRetryable is embedded (via %w) in errors that are definite answers
	// rather than transient faults. isRetryable refuses them outright, which is
	// what keeps a refused HEAD from being replayed three times, and what marks
	// a server that ignores Range support as one to work around rather than one
	// to wait for.
	errNotRetryable = errors.New("not retryable")
)

// statusError carries a non-2xx response through the retry classifier.
type statusError struct {
	Code   int
	Detail string
	// PostProgress marks a status that is normally permanent but that this
	// session has evidence is temporary here: a 404 received after bytes had
	// already been written to disk. That is what a CDN whose links flap
	// between usable and not-found on a minute-scale cycle looks like — the
	// same request succeeded a moment ago, so the 404 is an outage, not a
	// dead link. Probe requests never set it, so a 404 during discovery still
	// fails fast.
	PostProgress bool
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
	// Suggested is the filename the server proposed (Content-Disposition
	// first, URL path second). It is known as soon as probing finishes, long
	// before the download completes, so the UI can show a real name instead
	// of a URL while transferring.
	Suggested string
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

	// ProgressInterval and ProgressBytes throttle progress reporting. Both
	// are checked; whichever fires first wins.
	ProgressInterval time.Duration
	ProgressBytes    int64

	// SaveInterval and SaveBytes throttle sidecar flushing independently of
	// reporting: crash safety (frequent saves) and disk churn (rare saves)
	// pull opposite ways, so they get their own knobs. Unset (<=0) follows
	// the progress thresholds above.
	SaveInterval time.Duration
	SaveBytes    int64
	// SavePercent adds a size-relative save trigger: flush at least every N%
	// of the total (0 disables). A 2 GiB file at 5% flushes every ~100 MiB
	// however SaveBytes is set. Unknown total disables it.
	SavePercent int

	// Transport configures the shared HTTP transport.
	Transport TransportOptions

	// VerifySize requests an extra consistency check at publish time: the
	// finished length must equal the announced size (the basic check, which
	// always runs) *and* the bytes reported as downloaded, so a file whose
	// writes were counted but never landed cannot slip through with the right
	// length. Unknown-size resources are exempt; there is nothing to compare.
	VerifySize bool

	// Limiter caps throughput. Sharing one across downloaders limits their
	// combined speed; nil means unlimited.
	Limiter *Limiter
}

// Reserve defaults. A claim bounds how far the split point may drift ahead of
// the written bytes and how often a worker pays a round trip; it does not
// bound memory — reads still move ChunkSize at a time.
const (
	defaultReserveSize = 16 << 20
	maxReserveSize     = 64 << 20
)

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
		// Match the pooled block exactly: a read then fills one whole buffer,
		// and the per-chunk tax (WriteAt, commit, limiter, progress) is paid
		// half as often as the old 128 KiB default.
		o.ChunkSize = bufPoolBlockSize
	}
	if o.ChunkSize > bufPoolBlockSize {
		o.ChunkSize = bufPoolBlockSize
	}
	if o.ReserveSize <= 0 {
		// One ranged GET per reserve. 512 KiB made a high-latency source
		// request-bound: one round trip per 512 KiB caps a single worker near
		// 10 MB/s on a 50 ms RTT no matter how many connections run. A claim
		// is bookkeeping, not buffering, so the default covers many round
		// trips of bandwidth.
		o.ReserveSize = defaultReserveSize
	}
	if o.ReserveSize > maxReserveSize {
		o.ReserveSize = maxReserveSize
	}
	o.Backoff = o.Backoff.normalized()
	if o.ProgressInterval <= 0 {
		o.ProgressInterval = 250 * time.Millisecond
	}
	if o.ProgressBytes <= 0 {
		o.ProgressBytes = 1 << 20
	}
	// Negative save thresholds mean disabled (the app exposes per-trigger
	// switches); only exact zero falls back to the progress thresholds.
	// Callers that never set them keep today's behaviour.
	if o.SaveInterval == 0 {
		o.SaveInterval = o.ProgressInterval
	}
	if o.SaveBytes == 0 {
		o.SaveBytes = o.ProgressBytes
	}
	if o.SavePercent < 0 {
		o.SavePercent = 0
	}
	if o.SavePercent > 100 {
		o.SavePercent = 100
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
	// Logger, when non-nil, receives engine warnings that are not fatal but
	// that the user deserves to know about: today, a sidecar flush that keeps
	// failing. Nil keeps them on the internal debug channel only. Callers must
	// expect it to be called from worker goroutines.
	Logger func(msg string)
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

	// StateSaveErrors counts sidecar flushes that failed during this download.
	// A non-zero value means the crash-recovery record is behind the data on
	// disk; it used to be invisible, so a persistently failing sidecar looked
	// like a download that merely saved rarely.
	StateSaveErrors int64
}

// Downloader runs downloads over a shared HTTP/1.1 transport.
type Downloader struct {
	tr     *http.Transport
	client *http.Client
	opt    Options

	// progressIntervalNs is the live progress-report cadence in nanoseconds.
	// Trackers read it on every block, so the UI refresh setting retunes
	// running transfers without restarting them.
	progressIntervalNs atomic.Int64

	userAgent string

	// activeRetries points at the retry counter of the Download call running
	// on this goroutine. It exists because the helpers that count retries
	// (probe, fetch loops) are methods on the shared Downloader, while the
	// tally belongs to one job: a single field would be reset by every new
	// job and read by every finished one.
	activeRetries atomic.Pointer[atomic.Int64]
}

// retryCounter returns the current call's retry tally, falling back to a
// throwaway counter when a helper is used outside Download (as tests do).
func (d *Downloader) retryCounter() *atomic.Int64 {
	if p := d.activeRetries.Load(); p != nil {
		return p
	}
	return new(atomic.Int64)
}

// New creates a Downloader.
func New(opt Options) (*Downloader, error) {
	opt = opt.Normalize()
	tr, err := NewTransport(opt.Transport)
	if err != nil {
		return nil, err
	}
	d := &Downloader{
		tr: tr,
		client: &http.Client{
			Transport: tr,
			// Deliberately no Client.Timeout: a multi-gigabyte download
			// legitimately takes a long time. Phase timeouts live on the
			// transport, and cancellation flows through the context.
		},
		opt:       opt,
		userAgent: defaultUserAgent,
	}
	d.progressIntervalNs.Store(int64(opt.ProgressInterval))
	return d, nil
}

// SetProgressInterval retunes progress reporting live; running trackers pick
// it up on their next block. Non-positive values restore the default.
func (d *Downloader) SetProgressInterval(iv time.Duration) {
	if iv <= 0 {
		iv = 250 * time.Millisecond
	}
	d.progressIntervalNs.Store(int64(iv))
}

// ProgressInterval reports the live cadence.
func (d *Downloader) ProgressInterval() time.Duration {
	return time.Duration(d.progressIntervalNs.Load())
}

// Options returns the normalized options in effect.
func (d *Downloader) Options() Options { return d.opt }

// Transport exposes the underlying transport, mainly so tests can assert that
// HTTP/1.1 is really pinned.
func (d *Downloader) Transport() *http.Transport { return d.tr }

// Close releases idle connections and the shared debug handle.
func (d *Downloader) Close() {
	d.tr.CloseIdleConnections()
	// The debug handle is process-global and shared by every Downloader, so it
	// cannot be left open here: a Downloader per benchmark repetition would
	// leak one descriptor each. It is closed instead, and the next debugf
	// reopens it on demand — which is why closing never loses later output.
	debugMu.Lock()
	if debugFile != nil {
		_ = debugFile.Close()
		debugFile = nil
	}
	debugMu.Unlock()
}

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

	if strings.TrimSpace(req.URL) == "" {
		return nil, errors.New("no URL given")
	}

	// Retries are counted per call, not on the Downloader. The Downloader is
	// shared by every job the app runs, so a process-wide counter reset here
	// would zero the tally of downloads that are still running and report
	// each job's figure against whatever else happened concurrently.
	var retries atomic.Int64
	d.activeRetries.Store(&retries)

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
	if s.resumeState != nil && canSegment {
		// Only worth trying If-Range when there is something to resume, and
		// only when the length is known: Size<0 can never be segmented, so a
		// resume of an unknown-length resource falls through to single stream.
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
			out.Retries = retries.Load()
			out.StateSaveErrors = s.track.saveFailures()
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

	// saveMu serialises sidecar flushes. The progress tracker calls save from
	// every worker's write path, so without it two flushes race on the same
	// temporary file and the same rename target.
	saveMu sync.Mutex

	// syncedHigh is the highest offset (exclusive) of the partial file that a
	// successful fsync has made durable. The sidecar's Written counters are
	// clamped to it, so the sidecar can only ever describe bytes that reached
	// the medium; the preallocated region past it is zeros that must be
	// re-fetched rather than trusted.
	syncedHigh int64

	resumeState *State
	useIfRange  bool
	resumed     bool
	initialSegs int
	splits      int
}

// loadResumable attaches a usable sidecar to the session, or leaves it nil.
//
// Every rejection path that finds something on disk removes it: a corrupt or
// version-mismatched sidecar, a partial whose bytes belong to a different
// representation, and a segment table that does not tile the resource are all
// unusable, and leaving them in place would make every later run pay for the
// same inspection without ever producing a download.
func (s *session) loadResumable() {
	st, err := loadState(s.state)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			debugf("resume: discarding unusable sidecar %s: %v", s.state, err)
			_ = os.Remove(s.state)
		}
		return
	}
	if !st.matches(s.probe) {
		// The resource changed under us. The partial bytes describe a
		// different representation, so they must not be mixed with new ones.
		debugf("resume: sidecar %s does not match the resource; discarding", s.state)
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	if !s.probe.RangeSupported {
		// Without range support there is no way to continue mid-file.
		debugf("resume: server does not support ranges; discarding %s", s.part)
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	if !validateSegmentCoverage(st.Segments, s.probe.Size) {
		// A segment table with a hole or an overlap would resume as though the
		// missing bytes were already present, publishing a correctly sized file
		// with a hole in it. Refuse the whole table instead.
		debugf("resume: sidecar %s does not tile [0,%d); discarding", s.state, s.probe.Size)
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	if st.Validator() == "" {
		// Nothing identifies the representation the partial bytes came from.
		// The length and URL can stay identical across a server-side rebuild
		// ("file replaced, same name, same size"), and stitching new bytes onto
		// the old prefix then yields a corrupt file with the right length.
		// Without a validator there is no safe resume, so the bytes are
		// discarded and the download restarts from zero.
		debugf("resume: sidecar %s has no ETag/Last-Modified; cannot prove the bytes are current, restarting", s.state)
		_ = os.Remove(s.part)
		_ = os.Remove(s.state)
		return
	}
	s.resumeState = st
	s.created = st.CreatedAt
	s.resumed = true
	// The persisted Written counters were clamped to a synced high-water mark
	// before they were written, so their extent is already durable and is the
	// basis this session measures further progress against.
	s.syncedHigh = stateExtent(st.Segments)
}

// stateExtent is the highest offset (exclusive) a persisted segment table
// claims to have written.
func stateExtent(states []SegState) int64 {
	var hi int64
	for _, st := range states {
		if e := st.Start + st.Written; e > hi {
			hi = e
		}
	}
	return hi
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
	// Helper files stay out of the user's way in Explorer; unhideFile on
	// publish (rename preserves attributes) makes the finished file visible.
	hideFile(s.part)
	return nil
}

// effectiveAvailable reports how many bytes of the partial file can be trusted.
//
// The basis is the synced high-water mark, not the file's size. The segmented
// path preallocates the partial file to the full resource length, so its size
// is the announced size from the first instant and a size-based clamp never
// removed anything — the preallocated zeros were indistinguishable from bytes
// that had really been written. The synced mark only ever covers bytes an
// fsync confirmed, so a crash that lost the tail cannot be mistaken for
// progress. The file's real size still bounds it, which catches a partial that
// was truncated after the sidecar was last written.
func (s *session) effectiveAvailable() int64 {
	n := s.syncedHigh
	if s.file != nil {
		if st, err := s.file.Stat(); err == nil && st.Size() < n {
			n = st.Size()
		}
	}
	if s.probe.Size >= 0 && n > s.probe.Size {
		n = s.probe.Size
	}
	if n < 0 {
		n = 0
	}
	return n
}

// progressed reports whether this session has already written bytes to disk.
//
// It reads the segment manager's committed bytes rather than the file size:
// the segmented path preallocates the part file to the announced size with
// Truncate before a single worker writes (runSegmented), so the file's size is
// non-zero from the first instant and would classify the very first 404 as
// "after progress". Written only advances on a real commit, and a resumed
// session starts with the sidecar's Written already restored, so progress from
// a previous attempt counts too.
//
// This session-level signal is the evidence that makes a 404 retryable. A
// per-attempt one cannot serve: a 404 is recognised in validateRangeResponse
// before the attempt that received it has written anything.
func (s *session) progressed() bool {
	if s.mgr == nil {
		return false
	}
	n, _, _ := s.mgr.progress()
	return n > 0
}

// newTracker builds the throttled progress reporter for this session.
func (s *session) newTracker(initial, total int64) {
	s.track = &progressTracker{
		total:        total,
		bytes:        initial,
		lastMark:     initial,
		lastSave:     time.Now(),
		lastSaveMark: initial,
		start:        time.Now(),
		lastEmit:     time.Now(),
		interval:     &s.d.progressIntervalNs,
		step:         s.d.opt.ProgressBytes,
		saveInterval: s.d.opt.SaveInterval,
		saveStep:     s.d.opt.SaveBytes,
		savePct:      int64(s.d.opt.SavePercent),
		conns:        s.d.opt.Connections,
		suggest:      s.probe.SuggestedName,
	}
	s.track.save = s.saveState
	s.track.log = s.req.Logger
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
//
// The partial file is fsynced before the sidecar is written. That ordering is
// what makes the sidecar's Written counters meaningful: a counter only ever
// advances after a successful WriteAt, and the fsync pushes those bytes to the
// medium, so a sidecar on disk can never claim more than the medium holds. It
// is also why the clamp below is based on the synced high-water mark rather
// than on the file's size: the size includes the preallocated tail, whose zeros
// would otherwise be published as if they were data.
func (s *session) saveState() error {
	if s.state == "" || s.mgr == nil {
		return nil
	}

	// One writer at a time. The tracker calls this from every worker's write
	// path, so concurrent flushes would otherwise race on the same temporary
	// file and the same rename.
	s.saveMu.Lock()
	defer s.saveMu.Unlock()

	// Read the candidate extent before the sync so every byte the sidecar will
	// claim was handed to the OS before fsync started; the sync then flushes
	// exactly those. The snapshot is taken afterwards, so a write that lands in
	// the gap is conservatively clamped back to the durable mark.
	extent := s.mgr.writtenExtent()
	segs := s.mgr.segmentStates()

	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			// Without a successful sync the counters may describe bytes that
			// never reached the medium. Persisting them would let a later
			// resume step over a hole that no length check can detect, so fail
			// and keep the previous sidecar instead.
			return fmt.Errorf("sync partial file %s before saving state: %w", s.part, err)
		}
		if extent > s.syncedHigh {
			s.syncedHigh = extent
		}

		available := s.syncedHigh
		if s.probe.Size >= 0 && available > s.probe.Size {
			available = s.probe.Size
		}
		for i := range segs {
			room := available - segs[i].Start
			if room < 0 {
				room = 0
			}
			if segs[i].Written > room {
				segs[i].Written = room
			}
			if segs[i].Written < segs[i].End-segs[i].Start+1 {
				segs[i].Done = false
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
	// Segmentation is only defined for a known length. An unknown length must
	// travel the single-stream path; planning ranges against it would produce
	// an empty plan that completes vacuously and publishes a false success.
	if s.probe.Size < 0 {
		return nil, fmt.Errorf("segmented download requires a known length, got %d", s.probe.Size)
	}
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
		plan := planSegments(s.probe.Size, s.d.opt.Connections, s.d.opt.MinSegmentSize)
		if len(plan) == 0 && s.probe.Size != 0 {
			// planSegments only returns empty for total<=0, and a negative
			// size was rejected above, so reaching here means the planner and
			// the probe disagree. An empty plan completes vacuously, which is
			// only a real success for an empty resource.
			return nil, fmt.Errorf("segment planner produced no segments for size %d", s.probe.Size)
		}
		s.mgr = newSegmentManager(plan)
	}

	// Extend the partial file to the announced size before any worker
	// writes. High segments otherwise land in a sparse file: the filesystem
	// zero-fills every gap on demand and the extent list fragments. One
	// Truncate up front replaces both. Extend-only, and best effort: a
	// failed preallocation costs only the fragmentation it would have
	// avoided, never the download itself.
	if s.probe.Size > 0 && s.file != nil {
		if fi, err := s.file.Stat(); err == nil && fi.Size() < s.probe.Size {
			if err := s.file.Truncate(s.probe.Size); err == nil {
				debugf("prealloc    size=%d", s.probe.Size)
			}
		}
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
	var lastErr error
	// notFound counts 404s that arrived after this session had already
	// written bytes. It is deliberately separate from attempt: the
	// errSegmentIncomplete branch below resets attempt to -1 every time a
	// block lands, and a server alternating between an open red window and a
	// green one would reset this budget forever. Bounding it on its own is
	// what keeps an oscillating server from retrying without end, while
	// still handing the failure to the session-level recovery instead of
	// declaring the link dead.
	notFound := 0

	for attempt := 0; attempt <= b.MaxTries; attempt++ {
		if attempt > 0 {
			s.d.retryCounter().Add(1)
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
		case isPostProgressNotFound(err):
			// The red window is still open. Spend this budget, never the
			// attempt counter, and let the last one through so the session
			// level can decide whether to replay the whole download.
			notFound++
			if notFound > transientNotFoundMax {
				return err
			}
			lastErr = err
		case !isRetryable(err):
			return err
		default:
			lastErr = err
		}
	}

	// The loop falls through in one of two ways, and the message says which.
	// When retries were exhausted, lastErr holds the real cause and is kept:
	// without it every such failure degraded into a generic "bytes unclaimed"
	// with 0 B on disk and nothing actionable. With no error at all the plan
	// itself accounted for fewer bytes than the segment needs.
	//
	// The last error is wrapped with %w so a status buried in it — in
	// particular the post-progress 404 that drives session-level recovery —
	// stays classified by errors.As at the caller.
	if lastErr != nil {
		return fmt.Errorf("segment %s kept failing after its retries; last error: %w", s.mgr.describe(seg), lastErr)
	}
	return fmt.Errorf("segment %s was left with bytes the plan never accounted for", s.mgr.describe(seg))
}

// transientNotFoundMax bounds how many post-progress 404s one segment absorbs
// before the failure is handed up to the session-level retry.
//
// It is small on purpose. A genuinely dead link (a consumed single-use token)
// must not stall for the whole segment attempt budget, and the long-horizon
// waiting belongs to the manager's recovery loop, which also re-probes and can
// therefore ride out an outage that outlives this segment.
const transientNotFoundMax = 5

// isPostProgressNotFound reports a 404 that arrived after this session had
// already written bytes: the signature of a CDN whose links flap.
func isPostProgressNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound && se.PostProgress
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
			d.retryCounter().Add(1)
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

// debugf writes a diagnostic line when VKMDOWNLOAD_DEBUG is set.
//
// With VKMDOWNLOAD_DEBUG=1 the lines go to stderr; with VKMDOWNLOAD_DEBUG=<path>
// they are appended to that file, which makes a high-volume trace analysable
// without fighting the test runner's own output.
var debugMu sync.Mutex
var debugFile *os.File
var debugDest string

func debugf(format string, args ...any) {
	dest := os.Getenv("VKMDOWNLOAD_DEBUG")
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
	// Reuse one append handle per destination instead of open-write-close per
	// line: the reserve/commit/write path calls debugf at high frequency.
	if debugFile == nil || debugDest != dest {
		if debugFile != nil {
			_ = debugFile.Close()
			debugFile = nil
		}
		f, err := os.OpenFile(dest, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return
		}
		debugFile = f
		debugDest = dest
	}
	_, _ = debugFile.WriteString(line)
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

	// Never read more than ChunkSize at once: the pooled buffer is shared at
	// bufPoolBlockSize, but a small ChunkSize is a deliberate brake on memory
	// and burst size. The same clamp lives on the single-stream path.
	chunk := s.d.opt.ChunkSize
	if chunk <= 0 || chunk > len(buf) {
		chunk = len(buf)
	}
	for written < n {
		want := int64(chunk)
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
				s.mgr.commit(seg, int64(wn), off+written)
				committed += int64(wn)
				s.track.add(int64(wn))
				// Throttle after the write, so the ceiling applies to bytes
				// actually stored rather than to bytes merely read into the
				// buffer. The limiter is shared, so several workers pause
				// together instead of each getting the full allowance.
				if err := s.d.opt.Limiter.Wait(ctx, int64(wn)); err != nil {
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
	// A non-range segment must never reach the segmented fetcher: such segments
	// are only built for the single-stream path (runSingle), which validates
	// its own responses. Arriving here means the strategy ladder misrouted, so
	// fail loudly instead of accepting a 200 body at a range offset.
	if seg.NoRange {
		return errors.New("non-range segment reached the segmented fetcher")
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			return fmt.Errorf("%w: 206 response without a parsable Content-Range", ErrRangeUnusable)
		}
		if start != off {
			return fmt.Errorf("%w: server sent bytes starting at %d, asked for %d", ErrRangeUnusable, start, off)
		}
		// Some servers (observed on a real host) ignore the requested range
		// end and always answer with start-EOF, e.g. asking for
		// bytes=0-524287 yields "Content-Range: bytes 0-2097151999/2097152000"
		// while the body still carries the requested prefix. Requiring an
		// exact end match turns every multi-connection block into a failure.
		// Accept any end at or past the requested end and read exactly the n
		// bytes asked for; a short end is still a definitive mismatch.
		if end < off+n-1 {
			return fmt.Errorf("%w: server stopped at %d, asked through %d", ErrRangeUnusable, end, off+n-1)
		}
		if total >= 0 && s.probe.Size >= 0 && total != s.probe.Size {
			return fmt.Errorf("%w: server now says the file is %d bytes, probe said %d", ErrRangeUnusable, total, s.probe.Size)
		}
		return nil

	case http.StatusOK:
		// The server answered with the whole resource. For a validated resume
		// this is a refusal of If-Range, and the partial bytes on disk cannot
		// be trusted to belong to the same representation.
		return ErrIfRangeIgnored

	default:
		return &statusError{
			Code:         resp.StatusCode,
			Detail:       fmt.Sprintf("unexpected status for range %d-%d", off, off+n-1),
			PostProgress: s.progressed(),
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
	// treat as an already-satisfied segment. unboundedSegmentEnd keeps Length()
	// inside int64 so commit's "past the end" clamp stays inert on this path.
	segEnd := s.probe.Size - 1
	if s.probe.Size < 0 {
		segEnd = unboundedSegmentEnd
	}
	s.mgr = newSegmentManager([]*Segment{{Start: 0, End: segEnd, NoRange: !s.probe.RangeSupported}})
	s.initialSegs = 1
	s.newTracker(offset, s.probe.Size)

	b := s.d.opt.Backoff.normalized()
	var lastErr error

	for attempt := 0; attempt <= b.MaxTries; attempt++ {
		if attempt > 0 {
			s.d.retryCounter().Add(1)
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
			return offset, fmt.Errorf("%w: 206 starts at %d, asked from %d", ErrRangeUnusable, start, offset)
		}
	case http.StatusRequestedRangeNotSatisfiable:
		if s.probe.Size >= 0 && offset == s.probe.Size {
			return offset, nil
		}
		return offset, &statusError{Code: resp.StatusCode, Detail: "range not satisfiable"}
	default:
		return offset, &statusError{
			Code:         resp.StatusCode,
			Detail:       "unexpected status for a single stream",
			PostProgress: s.progressed(),
		}
	}

	bufp := bufPool.Get().(*[]byte)
	defer bufPool.Put(bufp)

	// Defensive: ChunkSize comes from user options; Normalize clamps it, but a
	// Downloader built before this clamp (or a hand-filled Options) must not
	// be able to slice past the pooled buffer and panic.
	chunk := s.d.opt.ChunkSize
	if chunk <= 0 || chunk > len(*bufp) {
		chunk = len(*bufp)
	}
	buf := (*bufp)[:chunk]
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
				if err := s.d.opt.Limiter.Wait(ctx, int64(wn)); err != nil {
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

	// The finished length is always checked against the announced size when one
	// is known. A truncated download with plausible contents is otherwise
	// indistinguishable from a complete one. VerifySize adds the second arm:
	// the file must also hold exactly the bytes this session reports as
	// downloaded, which is where a mis-accounted write would show up (right
	// length, wrong provenance). Before this it was a switch nothing read: the
	// option, and the CLI flag that set it, changed no behaviour at all.
	if s.probe.Size >= 0 {
		st, err := s.file.Stat()
		if err != nil {
			return nil, err
		}
		if st.Size() != s.probe.Size {
			return nil, fmt.Errorf("%w: file holds %d bytes, expected %d", ErrSizeMismatch, st.Size(), s.probe.Size)
		}
		if s.d.opt.VerifySize {
			var reported int64
			if s.track != nil {
				reported = s.track.reported()
			}
			if reported != st.Size() {
				return nil, fmt.Errorf("%w: %d bytes were downloaded but the file holds %d",
					ErrSizeMismatch, reported, st.Size())
			}
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
	// Rename preserves the hidden flag on Windows: without this the finished
	// download would stay invisible in Explorer.
	unhideFile(s.target)

	// Report 100% as the final event so a UI never stalls just short of done.
	// This must happen *before* the sidecar is removed: finish() emits through
	// the tracker, and the tracker re-saves the sidecar on every emit, so
	// removing the file first would immediately resurrect it.
	if s.track != nil {
		s.track.finish()
	}
	if err := os.Remove(s.state); err != nil && !errors.Is(err, os.ErrNotExist) {
		// The download itself is complete; a stale sidecar is harmless but its
		// presence would make the next run inspect this target as resumable.
		debugf("publish: removing sidecar %s: %v", s.state, err)
	}

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

// bufPoolBlockSize is the single read-buffer size shared by all workers.
// Options.ChunkSize is clamped to it in Normalize, and every use site clamps
// again, so a custom ChunkSize can never slice past the pooled array.
const bufPoolBlockSize = 256 << 10

// bufPool recycles per-request read buffers across workers.
var bufPool = sync.Pool{
	New: func() any {
		b := make([]byte, bufPoolBlockSize)
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
	// Definite answers. Each of these will be the same answer when asked
	// again, so a retry only delays the report: a refused HEAD replayed three
	// times (about 3.5 s), or a server whose Content-Range never matches the
	// request retried for a couple of minutes.
	if errors.Is(err, errNotRetryable) {
		return false
	}
	// A refused resume is a strategy problem, not a transient fault.
	if errors.Is(err, ErrIfRangeIgnored) {
		return false
	}
	if errors.Is(err, ErrSizeMismatch) {
		return false
	}
	// Local write failures are never transient: retrying a full disk or a
	// permission error only burns the attempt budget to report the same error.
	if errors.Is(err, io.ErrShortWrite) {
		return false
	}
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		return false
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		return false
	}

	var se *statusError
	if errors.As(err, &se) {
		// A 4xx that arrived after this session had already written bytes is
		// not the permanent answer it normally is: the very same request
		// succeeded a moment ago, so a temporary outage has opened. Probe
		// paths never set the flag, which is what keeps a 404 during
		// discovery failing fast.
		if se.PostProgress {
			return true
		}
		if se.Code >= 400 && se.Code < 500 {
			return se.Code == http.StatusRequestTimeout || se.Code == http.StatusTooManyRequests
		}
		return true
	}
	return true
}

// IsNotFound reports whether err carries an HTTP 404 from the server.
//
// The manager uses it to keep one session-level recovery cycle going across
// failures of different shapes: the first attempt carries a post-progress 404
// from a segment fetch, while a later attempt can 404 at probe time, before
// any bytes have been written.
func IsNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.Code == http.StatusNotFound
}

// RecoverableNotFound reports whether a 404 is worth retrying as a whole
// session rather than failing the task permanently.
//
// It is true when the engine observed progress before the 404, or when a
// resume sidecar beside target records bytes already written. A link that
// 404s before either is true is treated as gone, so a mistyped address still
// errors out at once instead of burning a retry cycle.
func RecoverableNotFound(err error, target string) bool {
	if !IsNotFound(err) {
		return false
	}
	var se *statusError
	if errors.As(err, &se) && se.PostProgress {
		return true
	}
	return ResumableProgress(target)
}

// ResumableProgress reports whether the sidecar beside target records any
// durably written bytes, backed by a real partial file.
//
// The check is deliberately about identity as well as magnitude. A sidecar can
// outlive the partial file it describes, or describe a different download that
// happened to reuse the same destination name; counting either as "progress"
// drags a link that never worked into the manager's multi-minute recovery
// cycle before reporting a 404. So the sidecar must name this file, and the
// partial it points at must exist and hold at least one byte.
func ResumableProgress(target string) bool {
	if target == "" {
		return false
	}
	st, err := loadState(target + StateSuffix)
	if err != nil {
		return false
	}
	if st.TargetPath != "" && !samePath(st.TargetPath, target) {
		return false
	}
	if st.TempPath != "" && !samePath(st.TempPath, target+PartSuffix) {
		return false
	}
	fi, err := os.Stat(target + PartSuffix)
	if err != nil || fi.Size() <= 0 {
		return false
	}
	for _, seg := range st.Segments {
		if seg.Written > 0 {
			return true
		}
	}
	return false
}

// samePath compares two paths after cleaning, so an equivalent spelling of the
// same file is not treated as a different download.
func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
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

// tmpIDs numbers cross-volume staging files within this process.
var tmpIDs atomic.Uint64

func nextTmpID() uint64 { return tmpIDs.Add(1) }

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
	// volume and remains atomic. The name carries pid plus a counter so two
	// concurrent downloads of the same target never step on each other's
	// staging file.
	tmp := fmt.Sprintf("%s.vkmdownload-%d-%d.tmp", dst, os.Getpid(), nextTmpID())
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
// crash. Reporting and flushing have independent thresholds (report fires on
// UI cadence, save fires on crash-safety cadence); each fires on whichever
// of "every N bytes" or "every N milliseconds" happens first.
type progressTracker struct {
	mu           sync.Mutex
	emitMu       sync.Mutex
	bytes        int64
	total        int64
	start        time.Time
	lastEmit     time.Time
	lastMark     int64
	lastSave     time.Time
	lastSaveMark int64
	// delivered is the highest byte count handed to the callback so far.
	// Snapshots are taken under mu but delivered outside it, so two workers
	// can race: the one with the older snapshot must not rewind the UI.
	delivered int64
	// interval points at the downloader's live cadence: every block reads
	// the current value, so retuning needs no restart.
	interval *atomic.Int64
	step     int64
	// saveInterval/saveStep throttle the sidecar independently of reporting.
	saveInterval time.Duration
	saveStep     int64
	// saveFails counts sidecar flush failures. The watermark below only moves
	// on success, so this is what distinguishes "saved rarely" from "failing";
	// saveFailures() exposes it so a run can report it (Result.StateSaveErrors).
	saveFails int64
	// savePct is a size-relative save trigger (percent of total, 0 disables).
	savePct int64
	conns   int
	suggest string

	report   func(Progress)
	save     func() error
	log      func(string)
	segments func() (total, done int)
}

// reportEverySaveFailure is the cadence of user-visible warnings about a
// sidecar that keeps failing to save: the first one, then every Nth.
const reportEverySaveFailure = 100

func (p *progressTracker) add(n int64) {
	if n <= 0 {
		return
	}
	p.mu.Lock()
	p.bytes += n
	now := time.Now()
	interval := time.Duration(p.interval.Load())
	dt := now.Sub(p.lastEmit)
	fire := dt >= interval || p.bytes-p.lastMark >= p.step
	// The percent trigger joins the byte trigger, whichever is tighter; it
	// needs a known total, otherwise it stays out of the way.
	step := p.saveStep
	if p.savePct > 0 && p.total > 0 {
		if pct := p.total * p.savePct / 100; pct > 0 && (step < 0 || pct < step) {
			step = pct
		}
	}
	timeOK := p.saveInterval > 0 && now.Sub(p.lastSave) >= p.saveInterval
	byteOK := step >= 0 && p.bytes-p.lastSaveMark >= step
	save := timeOK || byteOK

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

	// Disk IO stays off every lock: a slow flush must serialise neither the
	// workers nor the callbacks.
	// A failed flush is reported to the caller's Logger (when one is set) on
	// the way to being counted. The count alone made the failure invisible in
	// the app: nothing read it, and the only trace was an opt-in debug line.
	if save && p.save != nil {
		if err := p.save(); err != nil {
			debugf("save-state failed: %v", err)
			p.mu.Lock()
			p.saveFails++
			fails := p.saveFails
			p.mu.Unlock()
			if p.log != nil && (fails == 1 || fails%reportEverySaveFailure == 0) {
				// The first failure and then every Nth: a failing target often
				// fails every few hundred milliseconds, and a line per flush
				// would bury everything else in the log.
				p.log(fmt.Sprintf("进度记录写入失败（第 %d 次），断点续传信息可能落后于已下载的数据: %v", fails, err))
			}
		} else {
			// Only a successful flush may advance the watermark: it describes
			// what is durable, not what was attempted. The snapshot is kept
			// (rather than the live counter) so a concurrent success that
			// already recorded newer progress is never rewound.
			p.mu.Lock()
			if bytes > p.lastSaveMark {
				p.lastSaveMark = bytes
			}
			if now.After(p.lastSave) {
				p.lastSave = now
			}
			p.mu.Unlock()
		}
	}
	if !fire {
		return
	}
	p.emit(bytes, total, speed)
}

// reported returns the byte count under the tracker lock. It is the snapshot
// publish compares against the finished file size for VerifySize.
func (p *progressTracker) reported() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.bytes
}

// saveFailures reports how many sidecar flushes have failed so far.
//
// Without a reader this counter was dead weight: a flush that kept failing
// (a full disk, a permissions problem, a target on a network share) looked
// exactly like a download that simply saved rarely, and the only trace was a
// debug line nobody had turned on.
func (p *progressTracker) saveFailures() int64 {
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.saveFails
}

// reset returns the tracker to zero after a restart from scratch.
func (p *progressTracker) reset() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.bytes = 0
	p.lastMark = 0
	p.lastSave = time.Now()
	p.lastSaveMark = 0
	p.start = time.Now()
	p.lastEmit = time.Now()
	p.mu.Unlock()
	// The delivery watermark belongs to the abandoned run: without this a
	// fresh run's early reports would all clamp to the old total.
	p.emitMu.Lock()
	p.delivered = 0
	p.emitMu.Unlock()
}

// finish forces a final 100% event. It reports only: persisting here would
// resurrect the sidecar that publish removes right after.
func (p *progressTracker) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	bytes, total := p.bytes, p.total
	p.mu.Unlock()
	p.emitFinal(bytes, total, 0)
}

// emit reports progress without waiting: a slow consumer must not stall the
// workers behind the callback. A frame that cannot take the emitter
// immediately is dropped — the next throttled tick carries fresher numbers,
// and emitFinal always waits, so the terminal frame is never lost.
func (p *progressTracker) emit(bytes, total int64, speed float64) {
	if p == nil {
		return
	}
	if !p.emitMu.TryLock() {
		return
	}
	defer p.emitMu.Unlock()
	p.emitLocked(bytes, total, speed)
}

// emitFinal reports the terminal frame and waits for the emitter, so the
// last progress event cannot be dropped.
func (p *progressTracker) emitFinal(bytes, total int64, speed float64) {
	if p == nil {
		return
	}
	p.emitMu.Lock()
	defer p.emitMu.Unlock()
	p.emitLocked(bytes, total, speed)
}

func (p *progressTracker) emitLocked(bytes, total int64, speed float64) {
	// The caller holds emitMu.

	if p.report == nil {
		return
	}

	// Snapshots are taken outside the counter lock, so a worker holding an
	// older snapshot can arrive here after a newer one. Clamp it: callbacks
	// must never see the count go backwards.
	if bytes < p.delivered {
		bytes = p.delivered
	} else {
		p.delivered = bytes
	}

	prog := Progress{
		Total:       total,
		Downloaded:  bytes,
		Speed:       speed,
		Connections: p.conns,
		Suggested:   p.suggest,
	}
	if p.segments != nil {
		prog.Segments, prog.SegmentsDone = p.segments()
	}
	if speed > 0 && total > 0 && bytes < total {
		// (total-bytes)/speed is a count of *seconds*; it must be scaled by
		// time.Second before the conversion to a Duration. Converting the
		// second count first and multiplying afterwards would interpret it as
		// nanoseconds and inflate the estimate by 1e9.
		//
		// The result is clamped: a speed measured over a sub-millisecond
		// window can be enormous or, with a NaN rounding, produce a conversion
		// whose result is not a valid Duration at all. Either one reached the
		// UI as a nonsense countdown.
		secs := float64(total-bytes) / speed
		if !math.IsNaN(secs) && !math.IsInf(secs, 0) && secs > 0 {
			const maxETA = float64(math.MaxInt64) / float64(time.Second)
			if secs > maxETA {
				secs = maxETA
			}
			prog.ETA = time.Duration(secs * float64(time.Second))
		}
	}
	p.report(prog)
}
