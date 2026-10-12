package engine

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"vkmdownload/testsrv"
)

// TestSegmentedPauseResumeIsByteExact reproduces the sequence the desktop app
// produces when a user pauses and resumes: cancel a *segmented* download once
// data has arrived, then resume it and compare the result byte for byte.
//
// The existing resume tests cancel at a threshold derived from reported
// progress; this one cancels from inside the progress callback on the first
// report, which is the timing the GUI produces when the pause button is pressed
// during the first moments of a transfer.
func TestSegmentedPauseResumeIsByteExact(t *testing.T) {
	// startServer fixes the seed and size, so the expectation has to come from
	// the same source: a mismatched seed would make every byte differ and
	// "prove" a corruption that is not there.
	const size = testSize
	const seed = testSeed

	srv, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 2 << 20
	})
	_ = srv

	for attempt := 0; attempt < 6; attempt++ {
		dest := filepath.Join(t.TempDir(), "out.bin")

		opt := testOptions(2)
		d, err := New(opt)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		// Cancel on the very first progress report, which is the earliest
		// moment a user could press pause.
		var fired bool
		_, err = d.Download(ctx, Request{
			URL:  url,
			Dest: dest,
			Progress: func(p Progress) {
				if !fired && p.Downloaded > 0 {
					fired = true
					cancel()
				}
			},
		})
		cancel()
		if !fired {
			d.Close()
			continue // finished before it could be interrupted
		}

		st, lerr := loadState(dest + StateSuffix)
		if lerr != nil {
			d.Close()
			continue
		}
		fi, ferr := os.Stat(dest + PartSuffix)
		if ferr != nil {
			d.Close()
			continue
		}

		// What the sidecar believes versus what the file actually holds.
		var claimed int64
		for _, s := range st.Segments {
			if e := s.Start + s.Written; e > claimed {
				claimed = e
			}
		}
		t.Logf("attempt %d: paused file=%d sidecarMax=%d realSize=%d",
			attempt, fi.Size(), claimed, size)

		if _, err := d.Download(context.Background(), Request{
			URL: url, Dest: dest, Resume: true,
		}); err != nil {
			d.Close()
			t.Fatalf("resume: %v", err)
		}
		d.Close()

		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		want := testsrv.Expected(seed, size)
		if !bytes.Equal(got, want) {
			holes := diffRanges(got, want)
			t.Fatalf("attempt %d: resume produced %d differing ranges, first %v",
				attempt, len(holes), firstFew(holes, 5))
		}
	}
}

// diffRanges returns the byte ranges where got differs from want.
func diffRanges(got, want []byte) [][2]int64 {
	var out [][2]int64
	in := false
	var start int64
	n := len(got)
	if len(want) < n {
		n = len(want)
	}
	for i := 0; i < n; i++ {
		bad := got[i] != want[i]
		if bad && !in {
			in, start = true, int64(i)
		}
		if !bad && in {
			out = append(out, [2]int64{start, int64(i)})
			in = false
		}
	}
	if in {
		out = append(out, [2]int64{start, int64(n)})
	}
	if len(got) != len(want) {
		out = append(out, [2]int64{int64(n), int64(max(len(got), len(want)))})
	}
	return out
}

func firstFew(h [][2]int64, k int) [][2]int64 {
	if len(h) <= k {
		return h
	}
	return h[:k]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestPauseKeepsSidecarConsistentWithFile pins the invariant that makes resume
// safe: after a cancelled download, the sidecar may not claim more progress than
// the partial file actually holds.
//
// When it does, resume starts from the sidecar's offset, the bytes between the
// file's real end and that offset are never requested, and the finished file has
// a hole in it while its length is exactly right — invisible to every length
// check.
func TestPauseKeepsSidecarConsistentWithFile(t *testing.T) {
	const size = testSize

	_, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 2 << 20
	})

	for attempt := 0; attempt < 8; attempt++ {
		dest := filepath.Join(t.TempDir(), "out.bin")

		opt := testOptions(2)
		d, err := New(opt)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		var fired bool
		_, _ = d.Download(ctx, Request{
			URL:  url,
			Dest: dest,
			Progress: func(p Progress) {
				// Cancel on the first report, the earliest a user could press
				// pause, which is also the moment the most data is in flight.
				if !fired && p.Downloaded > 0 {
					fired = true
					cancel()
				}
			},
		})
		cancel()
		d.Close()

		st, err := loadState(dest + StateSuffix)
		if err != nil {
			continue // nothing resumable this round
		}
		fi, err := os.Stat(dest + PartSuffix)
		if err != nil {
			continue
		}

		for _, s := range st.Segments {
			claimed := s.Start + s.Written
			t.Logf("attempt %d: seg [%d,%d] written=%d claimedEnd=%d fileSize=%d segs=%d noRange=%v",
				attempt, s.Start, s.End, s.Written, claimed, fi.Size(), len(st.Segments), st.NoRange)
			if claimed > fi.Size() {
				t.Fatalf("attempt %d: sidecar claims %d bytes written but the file holds only %d (over by %d)",
					attempt, claimed, fi.Size(), claimed-fi.Size())
			}
		}
	}
}

// TestResumeFromTruncatedSidecar covers the crash window where the sidecar is
// newer than the data it describes: the sidecar claims progress the file does
// not hold. Resuming must not trust those bytes.
func TestResumeFromTruncatedSidecar(t *testing.T) {
	// Same reasoning as above: these must match what startServer serves.
	const size = testSize
	const seed = testSeed

	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	// A sidecar claiming half the file is written, and a partial file that is
	// shorter than that.
	fake := &State{
		Version:    StateVersion,
		URL:        url,
		FinalURL:   url,
		Size:       size,
		Segments:   []SegState{{Start: 0, End: size - 1, Written: size / 2}},
		TempPath:   dest + PartSuffix,
		TargetPath: dest,
		CreatedAt:  time.Now(),
	}
	if err := os.WriteFile(dest+PartSuffix, make([]byte, size/4), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := saveState(dest+StateSuffix, fake); err != nil {
		t.Fatal(err)
	}

	opt := testOptions(2)
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{
		URL: url, Dest: dest, Resume: true,
	}); err != nil {
		// A refusal is acceptable; publishing a corrupt file is not.
		if errors.Is(err, ErrSizeMismatch) {
			t.Skipf("resume refused the inconsistent sidecar: %v", err)
		}
		t.Fatalf("resume: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	want := testsrv.Expected(seed, size)
	if !bytes.Equal(got, want) {
		t.Fatalf("resume from a truncated sidecar produced a corrupt file: %v",
			firstFew(diffRanges(got, want), 5))
	}
}

// TestServerReachable guards the helper against a silently broken test server.
func TestServerReachable(t *testing.T) {
	_, url := startServer(t, nil)
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("test server unreachable: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("unexpected status %s", resp.Status)
	}
}
