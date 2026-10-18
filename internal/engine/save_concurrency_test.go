package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestConcurrentSaveStateIsSafe drives the sidecar flush from many goroutines at
// once, which is what the write path of a multi-connection download does: the
// progress tracker calls save from every worker, on whatever block happens to
// cross a threshold.
//
// Before the fix every call wrote to the same fixed "<state>.tmp" and renamed
// it, so writers interleaved, the rename raced, and most flushes failed or left
// a half-written file. The audit measured tens of thousands of failures at this
// scale. Run with -race: the counters and the temporary name must both be safe.
func TestConcurrentSaveStateIsSafe(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	part := target + PartSuffix
	state := target + StateSuffix
	const size = int64(1 << 20)

	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	s := &session{
		d:       &Downloader{},
		req:     Request{URL: "http://example.com/f"},
		probe:   &ProbeResult{Size: size, RangeSupported: true, FinalURL: "http://example.com/f"},
		target:  target,
		part:    part,
		state:   state,
		file:    f,
		mgr:     newSegmentManager([]*Segment{{Start: 0, End: size - 1}}),
		created: time.Now(),
	}
	s.mgr.segs[0].Written = size / 2

	const (
		writers = 16
		rounds  = 150 // 2400 flushes, the scale the audit saw fail by the thousand
	)

	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		failed []error
	)
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				if err := s.saveState(); err != nil {
					mu.Lock()
					failed = append(failed, err)
					mu.Unlock()
					return
				}
			}
		}()
	}
	wg.Wait()

	if len(failed) > 0 {
		t.Fatalf("%d of %d concurrent flushes failed, first: %v", len(failed), writers*rounds, failed[0])
	}

	// The surviving sidecar must be a complete, valid document.
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("sidecar missing after concurrent flushes: %v", err)
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("sidecar is not valid JSON: %v\n%s", err, b)
	}
	if st.Version != StateVersion {
		t.Errorf("sidecar version = %d, want %d", st.Version, StateVersion)
	}
	if len(st.Segments) != 1 || st.Segments[0].Written != size/2 {
		t.Errorf("sidecar segment table = %+v, want one segment with %d written", st.Segments, size/2)
	}

	// No temporary file may be left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("temporary file left behind: %s", e.Name())
		}
	}
}
