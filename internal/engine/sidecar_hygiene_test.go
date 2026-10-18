package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// assertNoWorkingSetResidue checks that a finished download left nothing but the
// published file behind: no partial, no sidecar, and no temporary staging file.
func assertNoWorkingSetResidue(t *testing.T, dir, target string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		switch {
		case strings.HasSuffix(name, ".tmp"),
			strings.Contains(name, ".vkmdownload-"),
			strings.HasSuffix(name, PartSuffix),
			strings.HasSuffix(name, StateSuffix):
			t.Errorf("working-set residue left behind: %s", name)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("finished file missing: %v", err)
	}
}

// TestCorruptSidecarIsDiscardedAndRedownloaded covers a sidecar that cannot be
// parsed. It must not linger: the engine has to remove it and start from
// scratch, leaving a correct file and no leftovers.
func TestCorruptSidecarIsDiscardedAndRedownloaded(t *testing.T) {
	_, url := startServer(t, nil)

	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(target+StateSuffix, []byte("{ not json at all"), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: target, Resume: true})
	if err != nil {
		t.Fatalf("Download with a corrupt sidecar: %v", err)
	}
	if res.Resumed {
		t.Error("Resumed = true with an unparsable sidecar")
	}
	assertContentMatches(t, target, testSeed, testSize)
	assertNoWorkingSetResidue(t, dir, target)
}

// TestVersionMismatchedSidecarIsDiscarded covers a well-formed sidecar written
// by an incompatible build. It is exactly as unusable as a corrupt one.
func TestVersionMismatchedSidecarIsDiscarded(t *testing.T) {
	_, url := startServer(t, nil)

	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	st := &State{
		Version:    StateVersion + 99,
		URL:        url,
		FinalURL:   url,
		Size:       testSize,
		Segments:   []SegState{{Start: 0, End: testSize - 1, Written: testSize / 4}},
		TempPath:   target + PartSuffix,
		TargetPath: target,
		CreatedAt:  time.Now(),
	}
	if err := saveState(target+StateSuffix, st); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target+PartSuffix, make([]byte, testSize/4), 0o644); err != nil {
		t.Fatal(err)
	}

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: target, Resume: true}); err != nil {
		t.Fatalf("Download with a version-mismatched sidecar: %v", err)
	}
	assertContentMatches(t, target, testSeed, testSize)
	assertNoWorkingSetResidue(t, dir, target)
}

// TestHoleySegmentTableRefusesResume covers a sidecar whose segment table does
// not tile the resource: a gap between two segments. Resuming it would leave the
// gap unfetched while still producing a file of the right length, so the whole
// resume must be refused and the download restarted.
func TestHoleySegmentTableRefusesResume(t *testing.T) {
	_, url := startServer(t, nil)

	dir := t.TempDir()
	target := filepath.Join(dir, "out.bin")
	part := target + PartSuffix
	state := target + StateSuffix

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	p, err := d.Probe(context.Background(), url)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}

	// A partial file exists, so the refusal is about the table, not absence.
	if err := os.WriteFile(part, make([]byte, testSize/4), 0o644); err != nil {
		t.Fatal(err)
	}

	const gap = 4096
	st := &State{
		Version:      StateVersion,
		URL:          url,
		FinalURL:     p.FinalURL,
		Size:         testSize,
		ETag:         p.ETag,
		LastModified: p.LastModified,
		Segments: []SegState{
			{Start: 0, End: testSize/2 - 1, Written: testSize / 4},
			{Start: testSize/2 + gap, End: testSize - 1, Written: 0},
		},
		TempPath:   part,
		TargetPath: target,
		CreatedAt:  time.Now(),
	}
	if err := saveState(state, st); err != nil {
		t.Fatal(err)
	}

	res, err := d.Download(context.Background(), Request{URL: url, Dest: target, Resume: true})
	if err != nil {
		t.Fatalf("Download with a holey sidecar: %v", err)
	}
	if res.Resumed {
		t.Error("Resumed = true for a segment table with a hole")
	}
	assertContentMatches(t, target, testSeed, testSize)
	assertNoWorkingSetResidue(t, dir, target)
}

// TestValidateSegmentCoverage pins the boundary cases of the tiling check.
func TestValidateSegmentCoverage(t *testing.T) {
	cases := []struct {
		name   string
		states []SegState
		size   int64
		want   bool
	}{
		{"single full segment", []SegState{{Start: 0, End: 999}}, 1000, true},
		{"contiguous pair", []SegState{{Start: 0, End: 499}, {Start: 500, End: 999}}, 1000, true},
		{"out of order but contiguous", []SegState{{Start: 500, End: 999}, {Start: 0, End: 499}}, 1000, true},
		{"starts late", []SegState{{Start: 1, End: 999}}, 1000, false},
		{"ends early", []SegState{{Start: 0, End: 998}}, 1000, false},
		{"hole in the middle", []SegState{{Start: 0, End: 499}, {Start: 600, End: 999}}, 1000, false},
		{"overlap", []SegState{{Start: 0, End: 600}, {Start: 500, End: 999}}, 1000, false},
		{"past the end", []SegState{{Start: 0, End: 1000}}, 1000, false},
		{"empty", nil, 1000, false},
		{"zero size", []SegState{{Start: 0, End: 0}}, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := validateSegmentCoverage(tc.states, tc.size); got != tc.want {
				t.Errorf("validateSegmentCoverage(%v, %d) = %v, want %v", tc.states, tc.size, got, tc.want)
			}
		})
	}
}
