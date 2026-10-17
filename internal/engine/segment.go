package engine

import (
	"fmt"
	"os"
	"sync"
)

// Segment is one contiguous inclusive byte range of the resource, together
// with how much of it has been written and how much has been claimed.
//
// Two counters are kept, and keeping them separate is what makes the plan
// immune to a lost reservation:
//
//   - Written is the number of bytes confirmed to be on disk. It is the only
//     thing Next derives the request offset from.
//   - Reserved is the number of bytes handed out to workers but not yet
//     confirmed. It only ever gates a *new* claim, so it can never push the
//     request offset past bytes that were never written.
//
// A single counter serving both purposes is how a segment ends up silently
// short: a claim that is never fulfilled still advances the offset, the gap is
// never requested again, and the finished file has exactly the right length
// with a hole in the middle.
//
// Segment fields are not safe for concurrent use on their own; every access
// goes through segmentManager, which holds the lock.
type Segment struct {
	Start int64
	End   int64 // inclusive
	// Written counts bytes of this segment confirmed to be on disk.
	Written int64
	// Reserved counts bytes claimed by workers but not yet committed.
	Reserved int64
	// writtenHigh is the highest offset (exclusive) this segment has had
	// actually written. It is only maintained for the accounting diagnostic.
	writtenHigh int64
	Done        bool
	// NoRange marks a segment that must be fetched with a plain GET because
	// the server does not support byte ranges at all. Such a segment cannot be
	// resumed from a non-zero offset.
	NoRange bool
}

// Length is the total size of the segment.
func (s *Segment) Length() int64 { return s.End - s.Start + 1 }

// Next is the absolute offset of the first byte not yet written to disk.
func (s *Segment) Next() int64 { return s.Start + s.Written }

// Remaining is how many bytes of the segment still need to be written.
func (s *Segment) Remaining() int64 {
	r := s.End - s.Next() + 1
	if r < 0 {
		return 0
	}
	return r
}

// claimable is how many further bytes may be claimed right now: whatever is
// neither written nor already reserved.
func (s *Segment) claimable() int64 {
	if c := s.Remaining() - s.Reserved; c > 0 {
		return c
	}
	return 0
}

// outstanding reports whether any claimed work has not been committed.
func (s *Segment) outstanding() int64 { return s.Reserved }

// segmentManager owns the segment plan and the queue of unclaimed segments.
//
// The subtle part is reserve. A worker claims a run of bytes before it fetches
// them, so the manager's idea of a segment's progress is always *ahead* of what
// has actually reached the file. That is what makes splitLargest safe: when a
// segment is halved, the split point is derived from reserved progress, so the
// tail range can never overlap bytes another worker has already been told to
// fetch. Without the reservation the tail could overlap an in-flight write and
// the segment's bookkeeping would overcount, showing more than 100% progress.
type segmentManager struct {
	mu    sync.Mutex
	segs  []*Segment
	queue []*Segment
}

func newSegmentManager(segs []*Segment) *segmentManager {
	m := &segmentManager{segs: segs}
	m.queue = append(m.queue, segs...)
	return m
}

// planSegments divides total into at most conns ranges, each at least minSeg
// bytes long.
//
// Splitting a small resource across many ranges is counter-productive: each
// range costs a fresh request, and when the whole file is smaller than the
// connection's bandwidth-delay product the extra requests buy nothing. That is
// the reasoning behind MinSizeForSegments upstream and minSeg here.
func planSegments(total int64, conns int, minSeg int64) []*Segment {
	if total <= 0 {
		return nil
	}
	if conns < 1 {
		conns = 1
	}
	if minSeg <= 0 {
		minSeg = 1
	}

	n := conns
	if bySize := total / minSeg; bySize < int64(n) {
		n = int(bySize)
	}
	if n < 1 {
		n = 1
	}

	segs := make([]*Segment, 0, n)
	base := total / int64(n)
	rem := total % int64(n)

	var start int64
	for i := 0; i < n; i++ {
		size := base
		if int64(i) < rem {
			size++
		}
		segs = append(segs, &Segment{Start: start, End: start + size - 1})
		start += size
	}
	return segs
}

// take pops the next unclaimed segment, or returns nil when the queue is
// drained.
func (m *segmentManager) take() *Segment {
	m.mu.Lock()
	defer m.mu.Unlock()
	for len(m.queue) > 0 {
		s := m.queue[0]
		m.queue = m.queue[1:]
		if !s.Done && s.claimable() > 0 {
			return s
		}
	}
	return nil
}

// legacyAccounting reproduces the pre-fix bookkeeping, where a segment had a
// single counter that a *claim* advanced and that Next() read as progress.
//
// It exists so the difference between the two models can be demonstrated with
// one set of tests rather than argued about. Set VKMDOWNLOAD_LEGACY_ACCOUNTING=1
// to enable it; it is a diagnostic, not a supported mode.
func legacyAccounting() bool {
	return os.Getenv("VKMDOWNLOAD_LEGACY_ACCOUNTING") != ""
}

// reserve claims up to max bytes of the segment and returns the absolute offset
// to fetch from and the byte count.
//
// The offset comes from Written, never from the claim itself: a claim only
// decides *who* fetches a run of bytes, not where the segment's progress ends.
// The claim is provisional, so a failed fetch must hand the bytes back with
// unreserve, otherwise no other worker can pick them up.
func (m *segmentManager) reserve(s *Segment, max int64) (off, n int64, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if max <= 0 {
		max = defaultReserveSize
	}
	off = s.Next()
	if off > s.End {
		return 0, 0, false
	}
	// Legacy: one counter, advanced by the claim itself, so Next() reports the
	// claimed end as though it were written progress.
	n = s.End - off + 1
	if legacyAccounting() {
		if n > max {
			n = max
		}
		s.Written += n
		return off, n, true
	}
	// Current: a claim is only a right to fetch; progress moves on commit.
	n = s.claimable()
	if n > max {
		n = max
	}
	if n <= 0 {
		return 0, 0, false
	}
	s.Reserved += n
	return off, n, true
}

// commit records that this segment has been written up to the absolute offset
// end (exclusive), settling n bytes of the caller's claim.
//
// The end offset is passed in rather than a byte count, and the advance is
// computed here as the distance from the segment's current position. That makes
// the update idempotent with respect to how often it is called: a caller that
// reports a cumulative total instead of an increment cannot double-count,
// because the same absolute end always produces the same Written.
//
// It has two independent effects, and keeping them apart is essential:
//
//   - it settles n bytes of the caller's claim (Reserved), because the claim is
//     done either way and must not leak;
//   - it advances Written, which drives the next request offset.
//
// Deriving one from the other would be wrong. Written only ever moves forward
// and is capped at the segment length, while a claim can be settled for fewer
// bytes than it advanced; coupling them lets Reserved drift upward until the
// segment can never be marked complete.
func (m *segmentManager) commit(s *Segment, claimed, end int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if legacyAccounting() {
		// Under that model the claim itself already advanced progress, and
		// there is no separate reservation to settle.
		if end > s.writtenHigh {
			s.writtenHigh = end
		}
		return
	}

	if end > s.writtenHigh {
		s.writtenHigh = end
	}
	if claimed > 0 {
		s.Reserved -= claimed
		if s.Reserved < 0 {
			s.Reserved = 0
		}
	}

	delta := end - s.Next()
	if delta <= 0 {
		return
	}
	s.Written += delta
	if s.Written > s.Length() {
		s.Written = s.Length()
	}
}

// unreserve returns claimed-but-unwritten bytes after a failed fetch.
func (m *segmentManager) unreserve(s *Segment, n int64) {
	if n <= 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if legacyAccounting() {
		s.Written -= n
		if s.Written < 0 {
			s.Written = 0
		}
		return
	}
	s.Reserved -= n
	if s.Reserved < 0 {
		s.Reserved = 0
	}
}

// complete marks a segment finished.
func (m *segmentManager) complete(s *Segment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s.Done = true
}

// splitLargest hands the splittable work of the segment with the most
// unclaimed bytes to a new tail segment, so an idle worker can pick it up. It
// reports whether a split happened.
//
// This is the dynamic-segmentation rule: a worker that finishes early does not
// sit idle while one unlucky segment (a slow server, a congested range) holds
// up the whole download; it takes half of the biggest remaining chunk instead.
//
// Only bytes past the highest claim are splittable. Outstanding claims cover
// exactly [Next, Next+Reserved): every reserve hands out a run starting at
// the written offset, so the unclaimed region always begins at Next+Reserved,
// and cutting there keeps every in-flight claim inside the head half. An
// older rule skipped claimed segments outright, which in practice disabled
// the splitter: while workers run, every segment holds a claim, so the
// scheduler's "halve the biggest" only ever fired in the instant between one
// claim settling and the next being taken.
func (m *segmentManager) splitLargest(minSplit int64) bool {
	if minSplit < 1 {
		minSplit = 1
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	var best *Segment
	var bestUnclaimed int64
	for _, s := range m.segs {
		if s.Done {
			continue
		}
		// Unclaimed = remaining minus in-flight claims: only that part can
		// move to the tail without cutting across a claim.
		if u := s.Remaining() - s.outstanding(); u > bestUnclaimed {
			best, bestUnclaimed = s, u
		}
	}
	if best == nil {
		return false
	}

	// The tail receives half of the *unclaimed* work, so it is the smaller
	// half and the one that decides whether the split is worthwhile.
	half := bestUnclaimed / 2
	if half < minSplit {
		return false
	}

	// The split point derives from claimed progress, not written progress:
	// the claim region [Next, Next+Reserved) stays in the head and the tail
	// starts past it, so neither half overlaps a fetch in flight.
	mid := best.Next() + best.outstanding() + half
	tail := &Segment{Start: mid, End: best.End, NoRange: best.NoRange}
	best.End = mid - 1

	debugf("split       [%d,%d] unclaimed=%d -> head end=%d tail=[%d,%d]",
		best.Start, tail.End, bestUnclaimed, best.End, tail.Start, tail.End)

	m.segs = append(m.segs, tail)
	m.queue = append(m.queue, tail)
	return true
}

// allDone reports whether every segment has been completed.
func (m *segmentManager) allDone() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.segs {
		if !s.Done {
			return false
		}
	}
	return true
}

// remaining reports how many bytes a segment still needs written.
//
// It exists because Remaining() reads segment fields, and splitting mutates
// them from the scheduler goroutine while workers read them. Calling it without
// the manager's lock is a data race, and the consequence is not theoretical: a
// worker that observes a stale, larger remainder concludes the segment is
// finished and the scheduler marks it done, leaving a hole of exactly the
// bytes that were never fetched.
func (m *segmentManager) remaining(s *Segment) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.Remaining()
}

// describe renders a segment for logging and error messages under the lock.
func (m *segmentManager) describe(s *Segment) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return fmt.Sprintf("[%d,%d] written=%d/%d reserved=%d done=%v",
		s.Start, s.End, s.Written, s.Length(), s.Reserved, s.Done)
}

// done reports whether a segment has been completed.
func (m *segmentManager) done(s *Segment) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return s.Done
}

// segmentCount returns the number of segments in the plan.
func (m *segmentManager) segmentCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.segs)
}

// progress returns the number of bytes reserved across all segments, plus the
// segment counts needed for status reporting.
func (m *segmentManager) progress() (reserved int64, total int, done int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.segs {
		reserved += s.Written
		total++
		if s.Done {
			done++
		}
	}
	return reserved, total, done
}

// segmentStates snapshots the plan for persistence.
func (m *segmentManager) segmentStates() []SegState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SegState, 0, len(m.segs))
	for _, s := range m.segs {
		out = append(out, SegState{
			Start:   s.Start,
			End:     s.End,
			Written: s.Written,
			Done:    s.Done,
		})
	}
	return out
}

// segmentsFromStates rebuilds a manager from persisted state.
//
// Two different bounds are applied, and conflating them would be a bug:
//
//   - size caps each segment's End, because the resource is that long and no
//     later segment may fall outside it;
//   - available caps Written, because only that many bytes are known to exist
//     in the partial file. A crash can leave the sidecar newer than the data it
//     describes, and a segment claiming more progress than the file holds would
//     cause the engine to skip those bytes and publish a file with a hole in
//     it that still has the right length.
func segmentsFromStates(states []SegState, size, available int64, noRange bool) (*segmentManager, int64) {
	segs := make([]*Segment, 0, len(states))
	for _, st := range states {
		start, end := st.Start, st.End
		if start < 0 {
			start = 0
		}
		if end > size-1 {
			end = size - 1
		}
		if end < start {
			continue
		}

		s := &Segment{Start: start, End: end, NoRange: noRange}

		written := st.Written
		if written < 0 {
			written = 0
		}
		if byFile := available - start; written > byFile {
			written = byFile
		}
		if written < 0 {
			written = 0
		}
		if written > s.Length() {
			written = s.Length()
		}
		s.Written = written

		if noRange && s.Remaining() > 0 {
			// A server without range support cannot resume mid-file, so any
			// partial progress on this segment is worthless.
			s.Written = 0
		}
		if s.Remaining() == 0 {
			s.Done = true
		}
		segs = append(segs, s)
	}
	if len(segs) == 0 {
		return nil, 0
	}

	m := newSegmentManager(segs)
	// Only incomplete segments should be queued.
	m.mu.Lock()
	m.queue = m.queue[:0]
	var reserved int64
	for _, s := range segs {
		reserved += s.Written
		if !s.Done {
			m.queue = append(m.queue, s)
		}
	}
	m.mu.Unlock()
	return m, reserved
}
