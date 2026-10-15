package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// planSegments
// ---------------------------------------------------------------------------

// coveredBytes returns the set of offsets covered by segs, tolerating the nil
// slice. It is used to prove that a plan has neither holes nor overlaps, which
// is the property a wrong answer here would turn into a corrupt file.
func coveredBytes(segs []*Segment) map[int64]int {
	seen := make(map[int64]int)
	for _, s := range segs {
		for i := s.Start; i <= s.End; i++ {
			seen[i]++
		}
	}
	return seen
}

func TestPlanSegmentsCoversExactlyOnce(t *testing.T) {
	cases := []struct {
		name  string
		total int64
		conns int
		min   int64
	}{
		{"even split", 8 << 20, 4, 1 << 20},
		{"uneven split", (8 << 20) + 7, 3, 1},
		{"one byte", 1, 8, 1},
		{"below min segment", 100, 8, 1 << 20},
		{"conns larger than needed", 4 << 20, 64, 1 << 20},
		{"single conn", 8 << 20, 1, 1 << 20},
		{"prime size", 104729, 7, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			segs := planSegments(tc.total, tc.conns, tc.min)
			if len(segs) == 0 {
				t.Fatalf("planSegments(%d,%d,%d) returned no segments", tc.total, tc.conns, tc.min)
			}
			if len(segs) > tc.conns {
				t.Errorf("got %d segments, want at most conns=%d", len(segs), tc.conns)
			}

			covered := coveredBytes(segs)
			if int64(len(covered)) != tc.total {
				t.Errorf("plan covers %d distinct bytes, want %d", len(covered), tc.total)
			}
			for off, n := range covered {
				if n != 1 {
					t.Fatalf("offset %d covered %d times; plans must not overlap", off, n)
				}
			}
			for _, s := range segs {
				if s.Start > s.End {
					t.Errorf("segment [%d,%d] is empty", s.Start, s.End)
				}
				if got := s.Length(); tc.min > 0 && len(segs) > 1 && got < tc.min {
					// Only enforceable when the plan was not forced down to a
					// single segment by the total size.
					if tc.total >= tc.min*int64(len(segs)) {
						t.Errorf("segment [%d,%d] is %d bytes, below the %d minimum", s.Start, s.End, got, tc.min)
					}
				}
			}
		})
	}
}

func TestPlanSegmentsContiguous(t *testing.T) {
	segs := planSegments(1000, 7, 1)
	var next int64
	for i, s := range segs {
		if s.Start != next {
			t.Fatalf("segment %d starts at %d, want %d (plans must be contiguous)", i, s.Start, next)
		}
		next = s.End + 1
	}
	if next != 1000 {
		t.Errorf("plan ends at %d, want 1000", next)
	}
}

func TestPlanSegmentsZero(t *testing.T) {
	if segs := planSegments(0, 4, 1); segs != nil {
		t.Errorf("planSegments(0,...) = %v, want nil", segs)
	}
	if segs := planSegments(-5, 4, 1); segs != nil {
		t.Errorf("planSegments(-5,...) = %v, want nil", segs)
	}
}

// ---------------------------------------------------------------------------
// segmentManager
// ---------------------------------------------------------------------------

// TestSegmentManagerReserveCommitAndUnreserve pins the split between claiming
// bytes and confirming them, because conflating the two is what allowed a
// segment to keep an offset past bytes that were never written.
func TestSegmentManagerReserveCommitAndUnreserve(t *testing.T) {
	newMgr := func() (*segmentManager, *Segment) {
		m := newSegmentManager([]*Segment{{Start: 0, End: 999}})
		return m, m.segs[0]
	}

	t.Run("a claim alone never advances the request offset", func(t *testing.T) {
		m, s := newMgr()
		off, n, ok := m.reserve(s, 400)
		if !ok || off != 0 || n != 400 {
			t.Fatalf("reserve = (%d,%d,%v), want (0,400,true)", off, n, ok)
		}
		// The next claim may not skip ahead of bytes that were never confirmed.
		if off2, _, ok2 := m.reserve(s, 400); !ok2 || off2 != 0 {
			t.Fatalf("second reserve offset = %d, want 0: an uncommitted claim must not move progress", off2)
		}
		if got := s.Next(); got != 0 {
			t.Fatalf("Next() = %d, want 0", got)
		}
	})

	t.Run("commit advances, unreserve frees", func(t *testing.T) {
		m, s := newMgr()
		_, n, _ := m.reserve(s, 400)
		m.commit(s, n, n)
		if s.Next() != 400 {
			t.Fatalf("Next() = %d after committing 400, want 400", s.Next())
		}

		off, n, ok := m.reserve(s, 100)
		if !ok || off != 400 || n != 100 {
			t.Fatalf("reserve = (%d,%d,%v), want (400,100,true)", off, n, ok)
		}
		m.unreserve(s, n)
		if s.Next() != 400 {
			t.Fatalf("Next() = %d after unreserve, want 400: returning a claim must not move progress", s.Next())
		}
		// The freed bytes must be claimable again, or a failed fetch would
		// leave a permanent hole.
		if off, n, ok := m.reserve(s, 250); !ok || off != 400 || n != 250 {
			t.Fatalf("reserve after unreserve = (%d,%d,%v), want (400,250,true)", off, n, ok)
		}
	})

	t.Run("settling a claim and advancing progress are independent", func(t *testing.T) {
		m, s := newMgr()
		// Two claims are outstanding at once, which is the case that breaks if
		// one of commit's effects is derived from the other.
		if _, n1, _ := m.reserve(s, 300); n1 != 300 {
			t.Fatalf("first claim = %d, want 300", n1)
		}
		off2, n2, ok := m.reserve(s, 300)
		if !ok || off2 != 0 || n2 != 300 {
			t.Fatalf("second claim = (%d,%d,%v), want (0,300,true)", off2, n2, ok)
		}

		// Settle only the second claim. Progress still advances by its 300
		// bytes, and exactly 300 of the 600 reserved is released.
		m.commit(s, n2, n2)
		if s.Written != 300 {
			t.Fatalf("Written = %d, want 300", s.Written)
		}
		if s.Reserved != 300 {
			t.Fatalf("Reserved = %d, want 300 (the first claim is still outstanding)", s.Reserved)
		}

		// The first claim settles later; both effects must still land exactly.
		// The end offset is absolute, so settling the second claim brings the
		// segment's progress to the end of both runs.
		m.commit(s, 300, 600)
		if s.Reserved != 0 {
			t.Fatalf("Reserved = %d, want 0 once every claim is settled", s.Reserved)
		}
		if s.Written != 600 {
			t.Fatalf("Written = %d, want 600", s.Written)
		}
	})

	t.Run("claims are clamped to what is left", func(t *testing.T) {
		m, s := newMgr()
		_, n, _ := m.reserve(s, 800)
		m.commit(s, n, n)

		off, n, ok := m.reserve(s, 1<<20)
		if !ok || off != 800 || n != 200 {
			t.Fatalf("tail reserve = (%d,%d,%v), want (800,200,true)", off, n, ok)
		}
		// Everything after the committed 800 is claimed now, so nothing more
		// can be handed out until some of it is committed or returned.
		if _, _, ok := m.reserve(s, 100); ok {
			t.Error("reserve handed out bytes that were already claimed")
		}

		// Settle the tail claim through to the end of the segment. The end
		// offset is absolute, so this also covers the bytes committed earlier.
		m.commit(s, n, 1000)
		if s.Remaining() != 0 {
			t.Errorf("Remaining = %d, want 0", s.Remaining())
		}
		if s.Reserved != 0 {
			t.Errorf("Reserved = %d, want 0 once every claim is settled", s.Reserved)
		}
		if _, _, ok := m.reserve(s, 100); ok {
			t.Error("reserve succeeded past the end of the segment")
		}
	})
}

// TestSegmentManagerCommitIsIdempotentForOvercommit guards the counters against
// a commit that overshoots: Reserved must never go negative and Written must
// never exceed the segment length.
func TestSegmentManagerCommitIsIdempotentForOvercommit(t *testing.T) {
	m := newSegmentManager([]*Segment{{Start: 0, End: 99}})
	s := m.segs[0]

	m.commit(s, 0, 500) // far past the end on purpose
	if s.Written != 100 {
		t.Errorf("Written = %d after an oversized commit, want it clamped to 100", s.Written)
	}
	if s.Reserved < 0 {
		t.Errorf("Reserved = %d, want it never negative", s.Reserved)
	}
	if s.Remaining() != 0 {
		t.Errorf("Remaining = %d, want 0", s.Remaining())
	}
}

func TestSegmentManagerTakeSkipsCompleted(t *testing.T) {
	m := newSegmentManager([]*Segment{
		{Start: 0, End: 9},
		{Start: 10, End: 19},
		{Start: 20, End: 29},
	})
	first := m.take()
	if first == nil || first.Start != 0 {
		t.Fatalf("first take = %v, want the [0,9] segment", first)
	}
	m.complete(first)

	second := m.take()
	if second == nil || second.Start != 10 {
		t.Fatalf("second take = %v, want the [10,19] segment", second)
	}
	if third := m.take(); third == nil || third.Start != 20 {
		t.Fatalf("third take = %v, want the [20,29] segment", third)
	}
	if m.take() != nil {
		t.Error("take returned a segment from an empty queue")
	}
}

// TestSplitLargestPreservesCoverage is the regression test for the class of bug
// that made a segmented download silently short: the plan mutates while workers
// are running, so a split that loses or duplicates bytes corrupts the file
// without ever failing a length check.
func TestSplitLargestPreservesCoverage(t *testing.T) {
	total := int64(1 << 20)
	m := newSegmentManager(planSegments(total, 2, 1))

	var committed int64
	// Confirm a little of every segment so the split point has to account for
	// real progress, then split repeatedly.
	for _, s := range m.segs {
		if _, n, ok := m.reserve(s, 1000); ok {
			m.commit(s, n, n)
			committed += n
		}
	}

	splits := 0
	for m.splitLargest(1 << 10) {
		splits++
		if splits > 1000 {
			t.Fatal("splitLargest did not terminate")
		}
	}
	if splits == 0 {
		t.Fatal("no split happened; the test did not exercise the split path")
	}

	covered := coveredBytes(m.segs)
	if int64(len(covered)) != total {
		t.Errorf("after %d splits the plan covers %d bytes, want %d", splits, len(covered), total)
	}
	for off, n := range covered {
		if n != 1 {
			t.Fatalf("offset %d covered %d times after splitting", off, n)
		}
	}

	// No segment may claim to have written past its own end.
	for _, s := range m.segs {
		if s.Written > s.Length() {
			t.Errorf("segment [%d,%d] claims %d written bytes but is only %d long",
				s.Start, s.End, s.Written, s.Length())
		}
		if s.Reserved < 0 {
			t.Errorf("segment [%d,%d] has a negative reservation %d", s.Start, s.End, s.Reserved)
		}
	}
}

// TestSplitLargestSkipsOutstandingClaims documents the rule that keeps a split
// from cutting across an in-flight claim: a segment with uncommitted claimed
// bytes is left alone until the claim is committed or handed back.
func TestSplitLargestSkipsOutstandingClaims(t *testing.T) {
	m := newSegmentManager([]*Segment{{Start: 0, End: 1<<20 - 1}})

	// Claim a large run without committing it.
	_, n, ok := m.reserve(m.segs[0], 64<<10)
	if !ok || n != 64<<10 {
		t.Fatalf("reserve = (%d,%v), want 64 KiB claimed", n, ok)
	}
	if m.splitLargest(1 << 10) {
		t.Error("splitLargest split a segment with an outstanding claim")
	}

	// Once the claim is committed the segment becomes splittable again.
	m.commit(m.segs[0], n, n)
	if !m.splitLargest(1 << 10) {
		t.Error("splitLargest refused to split a segment with no outstanding claim")
	}
}

func TestSplitLargestRespectsMinimum(t *testing.T) {
	// A 1000-byte segment halves into two 500-byte pieces, so the minimum is
	// satisfied at exactly 500 and violated above it.
	m := newSegmentManager([]*Segment{{Start: 0, End: 999}})
	if !m.splitLargest(500) {
		t.Fatal("splitLargest refused a split that yields two 500-byte segments")
	}
	if len(m.segs) != 2 {
		t.Fatalf("got %d segments after the split, want 2", len(m.segs))
	}
	for _, s := range m.segs {
		if s.Length() < 500 {
			t.Errorf("segment [%d,%d] is %d bytes, below the 500 minimum", s.Start, s.End, s.Length())
		}
	}

	// The remaining halves are 500 bytes each, so a 501-byte minimum now
	// forbids any further split.
	if m.splitLargest(501) {
		t.Error("splitLargest split segments that would fall below the 501-byte minimum")
	}
	if len(m.segs) != 2 {
		t.Errorf("segment count changed to %d despite a refused split", len(m.segs))
	}
}

func TestSegmentsFromStatesClampsToFile(t *testing.T) {
	size := int64(1000)
	// The sidecar claims more progress than the file actually holds: a crash
	// between writing the sidecar and flushing the data produces exactly this.
	states := []SegState{
		{Start: 0, End: 499, Written: 400},
		{Start: 500, End: 999, Written: 500},
	}

	// Only 300 bytes exist on disk, so segment 0 may claim at most 300.
	m, reserved := segmentsFromStates(states, size, 300, false)
	if m == nil {
		t.Fatal("segmentsFromStates returned nil")
	}
	if reserved != 300 {
		t.Errorf("reserved = %d, want 300 (clamped to the bytes actually on disk)", reserved)
	}
	if got := m.segs[0].Written; got != 300 {
		t.Errorf("segment 0 Written = %d, want 300", got)
	}
	// Segment 1 starts at 500, past the end of the file, so it must claim 0.
	if got := m.segs[1].Written; got != 0 {
		t.Errorf("segment 1 Written = %d, want 0 (its start is past EOF)", got)
	}
}

func TestSegmentsFromStatesDropsResumeWhenNoRange(t *testing.T) {
	states := []SegState{{Start: 0, End: 999, Written: 500}}
	m, reserved := segmentsFromStates(states, 1000, 500, true)
	if m == nil {
		t.Fatal("segmentsFromStates returned nil")
	}
	if reserved != 0 {
		t.Errorf("reserved = %d, want 0: a server without range support cannot resume mid-file", reserved)
	}
	if got := m.segs[0].Written; got != 0 {
		t.Errorf("Written = %d, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Backoff
// ---------------------------------------------------------------------------

func TestBackoffDelayGrowsAndIsCapped(t *testing.T) {
	b := Backoff{Base: 100 * time.Millisecond, Max: time.Second, Factor: 2, Jitter: 0}

	want := []time.Duration{
		100 * time.Millisecond,
		200 * time.Millisecond,
		400 * time.Millisecond,
		800 * time.Millisecond,
		time.Second, // capped
		time.Second,
	}
	for i, w := range want {
		if got := b.Delay(i); got != w {
			t.Errorf("Delay(%d) = %v, want %v", i, got, w)
		}
	}
}

func TestBackoffJitterStaysWithinBounds(t *testing.T) {
	b := Backoff{Base: time.Second, Max: 10 * time.Second, Factor: 2, Jitter: 0.25}
	nominal := b.Delay(0) // jittered, so recompute the bounds from the config
	_ = nominal

	for i := 0; i < 200; i++ {
		d := b.Delay(1)
		lo := time.Duration(float64(2*time.Second) * 0.75)
		hi := time.Duration(float64(2*time.Second) * 1.25)
		if d < lo || d > hi {
			t.Fatalf("Delay(1) = %v, outside [%v,%v]", d, lo, hi)
		}
	}
}

func TestBackoffSleepRespectsContext(t *testing.T) {
	b := Backoff{Base: time.Hour, Max: time.Hour, Factor: 2}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := b.Sleep(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Errorf("Sleep on a cancelled context = %v, want context.Canceled", err)
	}
}

// ---------------------------------------------------------------------------
// Content-Range parsing
// ---------------------------------------------------------------------------

func TestParseContentRange(t *testing.T) {
	cases := []struct {
		in                string
		start, end, total int64
		ok                bool
	}{
		{"bytes 0-0/1000", 0, 0, 1000, true},
		{"bytes 100-199/1000", 100, 199, 1000, true},
		{"bytes 100-199/*", 100, 199, -1, true},
		{"BYTES 5-9/20", 5, 9, 20, true},
		{"  bytes 1-2/3  ", 1, 2, 3, true},
		{"", 0, 0, 0, false},
		{"bytes 1-2", 0, 0, 0, false},
		{"bytes */1000", 0, 0, 0, false}, // unsatisfied range: no start/end
		{"items 1-2/3", 0, 0, 0, false},
	}
	for _, tc := range cases {
		start, end, total, ok := parseContentRange(tc.in)
		if ok != tc.ok {
			t.Errorf("parseContentRange(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		if start != tc.start || end != tc.end || total != tc.total {
			t.Errorf("parseContentRange(%q) = (%d,%d,%d), want (%d,%d,%d)",
				tc.in, start, end, total, tc.start, tc.end, tc.total)
		}
	}
}

func TestParseUnsatisfiedTotal(t *testing.T) {
	if n, ok := parseUnsatisfiedTotal("bytes */1234"); !ok || n != 1234 {
		t.Errorf("parseUnsatisfiedTotal = (%d,%v), want (1234,true)", n, ok)
	}
	if _, ok := parseUnsatisfiedTotal("bytes 0-0/1234"); ok {
		t.Error("parseUnsatisfiedTotal accepted a satisfied range")
	}
}

// ---------------------------------------------------------------------------
// Filename handling
// ---------------------------------------------------------------------------

func TestSanitizeFilenameStripsTraversal(t *testing.T) {
	cases := []struct{ in, want string }{
		{`..\..\evil.exe`, "evil.exe"},
		{"../../evil.exe", "evil.exe"},
		{`C:\Windows\System32\evil.dll`, "evil.dll"},
		{"/etc/passwd", "passwd"},
		{`a<b>c:d"e|f?g*h.txt`, "a_b_c_d_e_f_g_h.txt"},
		{"  spaced.txt  ", "spaced.txt"},
		{"trailing.dot.", "trailing.dot"},
		{"", ""},
		{"..", ""},
		{"CON", "_CON"},
		{"nul.txt", "_nul.txt"},
	}
	for _, tc := range cases {
		if got := sanitizeFilename(tc.in); got != tc.want {
			t.Errorf("sanitizeFilename(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSuggestName(t *testing.T) {
	cases := []struct {
		disposition, url, want string
	}{
		{`attachment; filename="report.pdf"`, "http://x/y", "report.pdf"},
		{`attachment; filename*=UTF-8''r%C3%A9sum%C3%A9.pdf`, "http://x/y", "résumé.pdf"},
		{"", "http://example.com/path/file.zip", "file.zip"},
		{"", "http://example.com/path/file.zip?token=abc", "file.zip"},
		// A bare host contributes no name; Download falls back to
		// "download.bin" through resolveTarget.
		{"", "http://example.com/", ""},
		{"", "http://example.com", ""},
		{`attachment; filename="../../escape.bin"`, "http://example.com/a.bin", "escape.bin"},
	}
	for _, tc := range cases {
		if got := suggestName(tc.disposition, tc.url); got != tc.want {
			t.Errorf("suggestName(%q,%q) = %q, want %q", tc.disposition, tc.url, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Options and retry classification
// ---------------------------------------------------------------------------

func TestOptionsNormalize(t *testing.T) {
	var zero Options
	got := zero.Normalize()
	if got.Connections < 1 {
		t.Errorf("Connections = %d, want at least 1", got.Connections)
	}
	if got.ChunkSize <= 0 || got.ReserveSize <= 0 || got.MinSegmentSize <= 0 {
		t.Errorf("buffer and segment sizes must be positive, got chunk=%d reserve=%d minSeg=%d",
			got.ChunkSize, got.ReserveSize, got.MinSegmentSize)
	}
	if got.ProgressInterval <= 0 {
		t.Error("ProgressInterval must be positive")
	}

	huge := Options{Connections: 1000}.Normalize()
	if huge.Connections > 64 {
		t.Errorf("Connections = %d, want it clamped to 64", huge.Connections)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"context canceled", context.Canceled, false},
		{"deadline exceeded", context.DeadlineExceeded, false},
		{"if-range ignored", ErrIfRangeIgnored, false},
		{"size mismatch", ErrSizeMismatch, false},
		{"404", &statusError{http.StatusNotFound, "nope"}, false},
		{"403", &statusError{http.StatusForbidden, "nope"}, false},
		{"408", &statusError{http.StatusRequestTimeout, "slow"}, true},
		{"429", &statusError{http.StatusTooManyRequests, "slow down"}, true},
		{"500", &statusError{http.StatusInternalServerError, "boom"}, true},
		{"503", &statusError{http.StatusServiceUnavailable, "boom"}, true},
		{"wrapped 503", fmt.Errorf("segment failed: %w", &statusError{http.StatusServiceUnavailable, "boom"}), true},
		{"wrapped 404", fmt.Errorf("segment failed: %w", &statusError{http.StatusNotFound, "nope"}), false},
		{"plain io error", io.ErrUnexpectedEOF, true},
		{"segment incomplete", errSegmentIncomplete, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryable(tc.err); got != tc.want {
				t.Errorf("isRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestResolveTarget(t *testing.T) {
	dir := t.TempDir()

	if got, err := resolveTarget(dir, "f.bin"); err != nil || got != filepath.Join(dir, "f.bin") {
		t.Errorf("existing directory: got %q, err %v", got, err)
	}
	if got, err := resolveTarget(dir+string(os.PathSeparator), "f.bin"); err != nil || got != filepath.Join(dir, "f.bin") {
		t.Errorf("trailing separator: got %q, err %v", got, err)
	}

	explicit := filepath.Join(dir, "exact.bin")
	if got, err := resolveTarget(explicit, "f.bin"); err != nil || got != explicit {
		t.Errorf("explicit file: got %q, err %v", got, err)
	}

	if got, err := resolveTarget("", "f.bin"); err != nil || !filepath.IsAbs(got) || filepath.Base(got) != "f.bin" {
		t.Errorf("empty dest: got %q, err %v", got, err)
	}
}

// ---------------------------------------------------------------------------
// progressTracker
// ---------------------------------------------------------------------------

func TestProgressTrackerThrottles(t *testing.T) {
	var (
		reports int
		saves   int
	)
	p := &progressTracker{
		total:    1 << 30,
		interval: time.Hour, // only the byte threshold can fire
		step:     100,
		start:    time.Now(),
		lastEmit: time.Now(),
		report:   func(Progress) { reports++ },
		save:     func() error { saves++; return nil },
	}
	for i := 0; i < 1000; i++ {
		p.add(1)
	}
	if reports > 12 {
		t.Errorf("reported %d times for 1000 bytes with a 100-byte step, want about 10", reports)
	}
	if reports == 0 {
		t.Error("no progress was reported at all")
	}
	if saves != reports {
		t.Errorf("saves = %d, reports = %d; the sidecar should be flushed on each report", saves, reports)
	}
}

func TestProgressTrackerETAUsesSeconds(t *testing.T) {
	// Regression guard: (total-bytes)/speed is a count of seconds. Converting
	// it to a Duration without scaling by time.Second inflates the estimate by
	// a factor of 1e9, which previously produced absurd ETAs.
	var got Progress
	p := &progressTracker{
		total:    1000,
		interval: time.Nanosecond,
		step:     1,
		start:    time.Now().Add(-time.Second),
		lastEmit: time.Now().Add(-time.Second),
		report:   func(pr Progress) { got = pr },
	}
	p.add(500) // 500 bytes in ~1s => 500 B/s, so 500 bytes left => ~1s

	if got.Speed <= 0 {
		t.Fatalf("speed = %v, want positive", got.Speed)
	}
	if got.ETA <= 0 || got.ETA > time.Minute {
		t.Errorf("ETA = %v, want roughly one second", got.ETA)
	}
}

// ---------------------------------------------------------------------------
// resolveTarget / moveFile
// ---------------------------------------------------------------------------

func TestMoveFileSameVolume(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.part")
	dst := filepath.Join(dir, "a.bin")
	content := []byte("hello world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Errorf("content = %q, want %q", got, content)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source still exists after moveFile")
	}
}

func TestMoveFileCreatesParentDirectory(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "a.part")
	dst := filepath.Join(dir, "nested", "deeper", "a.bin")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFile(src, dst); err != nil {
		t.Fatalf("moveFile: %v", err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("destination missing: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Probe against a hand-written handler
// ---------------------------------------------------------------------------

func TestProbeReportsRangeSupportFromBehaviour(t *testing.T) {
	body := make([]byte, 4096)
	for i := range body {
		body[i] = byte(i * 7)
	}

	tests := []struct {
		name        string
		advertise   string
		honourRange bool
		wantRange   bool
		wantSize    int64
	}{
		{"honest server", "bytes", true, true, 4096},
		{"lying server", "bytes", false, false, 4096},
		{"silent about ranges", "", true, true, 4096},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.advertise != "" {
					w.Header().Set("Accept-Ranges", tc.advertise)
				}
				w.Header().Set("ETag", `"v1"`)
				if tc.honourRange && r.Header.Get("Range") != "" {
					w.Header().Set("Content-Range", "bytes 0-0/4096")
					w.Header().Set("Content-Length", "1")
					w.WriteHeader(http.StatusPartialContent)
					_, _ = w.Write(body[:1])
					return
				}
				w.Header().Set("Content-Length", fmt.Sprint(len(body)))
				w.WriteHeader(http.StatusOK)
				if r.Method != http.MethodHead {
					_, _ = w.Write(body)
				}
			}))
			defer srv.Close()

			d, err := New(Options{})
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			p, err := d.Probe(context.Background(), srv.URL)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if p.Size != tc.wantSize {
				t.Errorf("Size = %d, want %d", p.Size, tc.wantSize)
			}
			if p.RangeSupported != tc.wantRange {
				t.Errorf("RangeSupported = %v, want %v (note: %s)", p.RangeSupported, tc.wantRange, p.Note)
			}
		})
	}
}

// TestProbePrefersDispositionOverURLToken pins the filename fix: some hosts
// send no Content-Disposition on HEAD but do on the ranged GET (observed:
// dl.fuckingfast.co). The probe must promote the disposition name over the
// URL-token fallback it assigned from HEAD, or every such file lands with
// the token as its name.
func TestProbePrefersDispositionOverURLToken(t *testing.T) {
	const real = "Onimusha_Way_of_the_Sword_--_fitgirl-repacks.site_--_.part05.rar"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "2097152000")
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+real)
		w.Header().Set("Content-Range", "bytes 0-0/2097152000")
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer srv.Close()

	d, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	p, err := d.Probe(context.Background(), srv.URL+"/dl/TOKEN")
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.SuggestedName != real {
		t.Errorf("SuggestedName = %q, want the disposition name %q", p.SuggestedName, real)
	}
}

func TestProbeRejectsUnknownScheme(t *testing.T) {
	d, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Probe(context.Background(), "ftp://example.com/x"); err == nil {
		t.Error("Probe accepted an ftp URL")
	} else if !strings.Contains(err.Error(), "scheme") {
		t.Errorf("error = %v, want it to mention the scheme", err)
	}
}
