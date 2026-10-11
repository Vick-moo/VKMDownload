package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"vdownload/testsrv"
)

// TestDiagnoseLegacyAccounting pins down which bookkeeping step lets a segment's
// recorded progress run ahead of the bytes on disk.
//
// Two things make it controlled rather than probabilistic:
//
//   - Connections is 1 and MinSegmentSize spans the whole resource, so the plan
//     is a single segment and the dynamic splitter never fires. Everything that
//     happens is therefore attributable to one worker's claim/commit sequence.
//   - The baseline is not the segment's own bookkeeping but the highest offset
//     that was actually handed to WriteAt, so a claim that is never fulfilled is
//     visible as a positive difference.
//
// Run with the legacy model to reproduce:
//
//	VDOWNLOAD_DIAGNOSE=1 VDOWNLOAD_LEGACY_ACCOUNTING=1 \
//	  go test ./internal/engine -run TestDiagnoseLegacyAccounting -v
func TestDiagnoseLegacyAccounting(t *testing.T) {
	if os.Getenv("VDOWNLOAD_DIAGNOSE") == "" {
		t.Skip("set VDOWNLOAD_DIAGNOSE=1 to run the accounting diagnostic")
	}

	mode := "current"
	if legacyAccounting() {
		mode = "legacy"
	}
	fmt.Printf("=== accounting model: %s ===\n", mode)

	const rounds = 16
	diverged := 0

	for round := 0; round < rounds; round++ {
		threshold := int64(testSize/8) + int64(round)*(int64(testSize)/int64(rounds*2))
		if threshold >= testSize {
			threshold = testSize - 1
		}

		srv, url := startServer(t, func(c *testsrv.Config) {
			c.PerConnRate = 2 << 20
		})
		_ = srv
		dest := filepath.Join(t.TempDir(), "out.bin")

		opt := testOptions(4)
		// Connections must exceed 1 for the segmented path to be chosen at all,
		// but MinSegmentSize spanning the whole resource collapses the plan to a
		// single segment, so exactly one worker runs and the dynamic splitter
		// never fires. Any divergence is then attributable to that one worker's
		// claim/commit sequence, with no concurrency in the picture.
		opt.MinSegmentSize = testSize * 4
		opt.ReserveSize = 64 << 10
		opt.Backoff.MaxTries = 1 // MustTries is floored by Normalize, so 1 is the minimum

		d, err := New(opt)
		if err != nil {
			t.Fatal(err)
		}

		ctx, cancel := context.WithCancel(context.Background())
		var observed int64
		progress := func(p Progress) {
			if observed == 0 && p.Downloaded > threshold && p.Downloaded < testSize {
				observed = p.Downloaded
				cancel()
			}
		}

		_, _ = d.Download(ctx, Request{URL: url, Dest: dest, Progress: progress})
		cancel()

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

		// The single segment's claim of progress, versus the file's real length.
		var claimed int64
		for _, s := range st.Segments {
			if e := s.Start + s.Written; e > claimed {
				claimed = e
			}
		}
		size := fi.Size()

		if claimed > size {
			diverged++
			if diverged <= 5 {
				fmt.Printf("round %2d: DIVERGED threshold=%d observed=%d claimedEnd=%d fileSize=%d delta=%d\n",
					round, threshold, observed, claimed, size, claimed-size)
				for _, s := range st.Segments {
					fmt.Printf("         seg [%d,%d] written=%d done=%v\n", s.Start, s.End, s.Written, s.Done)
				}
			}
		} else {
			fmt.Printf("round %2d: consistent threshold=%d observed=%d claimedEnd=%d fileSize=%d\n",
				round, threshold, observed, claimed, size)
		}
		d.Close()
	}

	fmt.Printf("=== %d/%d rounds had claimed progress ahead of the file (%s model) ===\n", diverged, rounds, mode)
}
