package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"vkmdownload/testsrv"
)

// TestSaveStateRefusesToPersistUnsyncedProgress is the crash-safety invariant at
// the heart of the "preallocated all-zero file" bug.
//
// The sidecar may only ever describe bytes an fsync confirmed. When the flush
// cannot be confirmed, the flush must fail and leave the previous sidecar
// untouched, rather than advancing Written to an in-memory count that a power
// loss would erase. A closed handle makes Sync fail exactly as a real I/O error
// would.
func TestSaveStateRefusesToPersistUnsyncedProgress(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	part := target + PartSuffix
	state := target + StateSuffix
	const size = int64(1 << 20)

	// A durable sidecar from earlier, recording a quarter of the resource.
	prior := &State{
		Version:    StateVersion,
		URL:        "http://example.com/f",
		FinalURL:   "http://example.com/f",
		Size:       size,
		Segments:   []SegState{{Start: 0, End: size - 1, Written: size / 4}},
		TempPath:   part,
		TargetPath: target,
	}
	if err := saveState(state, prior); err != nil {
		t.Fatal(err)
	}

	f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		t.Fatal(err)
	}

	s := &session{
		req:     Request{URL: "http://example.com/f"},
		probe:   &ProbeResult{Size: size, RangeSupported: true, FinalURL: "http://example.com/f"},
		target:  target,
		part:    part,
		state:   state,
		file:    f,
		mgr:     newSegmentManager([]*Segment{{Start: 0, End: size - 1}}),
		created: time.Now(),
	}
	// The plan believes the whole file is written; only a quarter is durable.
	s.mgr.segs[0].Written = size

	// The medium refuses the flush.
	_ = f.Close()

	if err := s.saveState(); err == nil {
		t.Fatal("saveState succeeded even though the partial file could not be synced")
	}

	got, err := loadState(state)
	if err != nil {
		t.Fatalf("sidecar became unreadable after a failed sync: %v", err)
	}
	if got.Segments[0].Written != size/4 {
		t.Fatalf("sidecar claims %d written bytes after a failed sync, want the previous %d: "+
			"unsynced progress was persisted and a resume would publish zeros",
			got.Segments[0].Written, size/4)
	}

	// This download never completed, so the partial and sidecar are expected to
	// remain. What must not remain is a staging file abandoned by the failed
	// flush.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".vkmdownload-") {
			t.Errorf("failed flush left a staging file behind: %s", name)
		}
	}
}

// TestResumeRedownloadsUnsyncedRegion builds the on-disk state a power loss
// leaves behind — a preallocated all-zero partial file with only a durable
// prefix, plus a sidecar describing exactly that prefix — and checks that a
// resume fills the rest in rather than publishing the zeros.
func TestResumeRedownloadsUnsyncedRegion(t *testing.T) {
	const real = testSize / 4

	for _, conns := range []int{1, 2} {
		t.Run(fmt.Sprintf("conns=%d", conns), func(t *testing.T) {
			_, url := startServer(t, nil)

			dir := t.TempDir()
			target := filepath.Join(dir, "out.bin")
			part := target + PartSuffix
			state := target + StateSuffix

			d, err := New(testOptions(conns))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			p, err := d.Probe(context.Background(), url)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}

			// Crash artifacts: the partial was preallocated to the full length
			// (so its size says "complete") but only the first quarter of real
			// data was durable.
			f, err := os.OpenFile(part, os.O_RDWR|os.O_CREATE, 0o644)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Truncate(testSize); err != nil {
				_ = f.Close()
				t.Fatal(err)
			}
			if _, err := f.WriteAt(testsrv.Expected(testSeed, real), 0); err != nil {
				_ = f.Close()
				t.Fatal(err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}

			st := &State{
				Version:      StateVersion,
				URL:          url,
				FinalURL:     p.FinalURL,
				Size:         testSize,
				ETag:         p.ETag,
				LastModified: p.LastModified,
				Segments:     []SegState{{Start: 0, End: testSize - 1, Written: real}},
				TempPath:     part,
				TargetPath:   target,
				CreatedAt:    time.Now(),
			}
			if err := saveState(state, st); err != nil {
				t.Fatal(err)
			}

			res, err := d.Download(context.Background(), Request{URL: url, Dest: target, Resume: true})
			if err != nil {
				t.Fatalf("resume after power loss: %v", err)
			}
			if !res.Resumed {
				t.Error("Resumed = false; the durable prefix was not used")
			}
			// The whole point: the untouched (zero) region must have been
			// fetched, never published as data.
			assertContentMatches(t, target, testSeed, testSize)
			assertNoWorkingSetResidue(t, dir, target)
		})
	}
}
