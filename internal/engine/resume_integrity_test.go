package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"vkmdownload/testsrv"
)

// TestDiagnoseResumeHoles is a temporary diagnostic. It reports every byte range
// that differs after a resume, plus the segment tables before and after, so the
// cause can be read off rather than guessed at.
func TestDiagnoseResumeHoles(t *testing.T) {
	if os.Getenv("VKMDOWNLOAD_DIAGNOSE") == "" {
		t.Skip("set VKMDOWNLOAD_DIAGNOSE=1 to run the resume diagnostic")
	}

	srv, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 2 << 20
	})
	_ = srv

	// The failure is timing dependent, so sweep several cancellation points.
	for _, frac := range []int64{8, 6, 4, 3, 2} {
		threshold := testSize / frac
		name := fmt.Sprintf("cancel>%d", threshold)
		dest := filepath.Join(t.TempDir(), "out.bin")

		opt := testOptions(2)
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

		_, err = d.Download(ctx, Request{URL: url, Dest: dest, Progress: progress})
		cancel()

		st, lerr := loadState(dest + StateSuffix)
		if lerr != nil {
			fmt.Printf("DIAG %-16s first attempt err=%v observed=%d -> no usable sidecar (%v)\n",
				name, err, observed, lerr)
			d.Close()
			continue
		}
		fi, _ := os.Stat(dest + PartSuffix)
		fileSize := int64(0)
		if fi != nil {
			fileSize = fi.Size()
		}

		// What the sidecar claims versus what is actually on disk.
		var claimed int64
		for _, s := range st.Segments {
			if e := s.Start + s.Written; e > claimed {
				claimed = e
			}
		}
		fileHoles := countZeroRuns(t, dest, st.Size)

		fmt.Printf("DIAG %-16s err=%v observed=%d fileSize=%d sidecarMax=%d zeroRuns=%d\n",
			name, err, observed, fileSize, claimed, fileHoles)
		for _, s := range st.Segments {
			fmt.Printf("DIAG     seg [%d,%d] written=%d done=%v\n", s.Start, s.End, s.Written, s.Done)
		}

		res, err2 := d.Download(context.Background(), Request{URL: url, Dest: dest, Resume: true})
		if err2 != nil {
			fmt.Printf("DIAG %-16s RESUME FAILED: %v\n", name, err2)
			d.Close()
			continue
		}
		holes := listHoles(t, dest, st.Size)
		var holeBytes int64
		for _, h := range holes {
			holeBytes += h[1] - h[0]
		}
		fmt.Printf("DIAG %-16s resume ok resumed=%v segs=%d splits=%d -> holes=%d bytes=%d\n",
			name, res.Resumed, res.Segments, res.Splits, len(holes), holeBytes)
		if len(holes) > 0 {
			// Distinguish "never written" (zeros) from "wrong bytes". A hole
			// full of zeros means the range was skipped; non-zero garbage means
			// content landed at the wrong offset.
			lo, hi := holes[0][0], holes[0][1]
			for _, h := range holes {
				if h[0] < lo {
					lo = h[0]
				}
				if h[1] > hi {
					hi = h[1]
				}
			}
			fmt.Printf("DIAG     hole span [%d,%d) len=%d\n", lo, hi, hi-lo)
			fmt.Printf("DIAG     first 3 holes: %v\n", holes[:min(3, len(holes))])

			// Sample the actual bytes inside the first hole.
			fh, err := os.Open(dest)
			if err == nil {
				sample := make([]byte, min64(48, int64(len(holes[0]))))
				_, _ = fh.ReadAt(sample, holes[0][0])
				exp := make([]byte, len(sample))
				testsrv.Fill(testSeed, holes[0][0], exp)
				fmt.Printf("DIAG     got=% x\n", sample)
				fmt.Printf("DIAG     want= % x\n", exp)
				fh.Close()
			}
			t.Errorf("%s: resume left %d holes (%d bytes)", name, len(holes), holeBytes)
		}
		d.Close()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// countZeroRuns counts runs of consecutive zero bytes, a cheap way to spot a
// hole without needing the expected stream.
func countZeroRuns(t *testing.T, path string, size int64) int {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return -1
	}
	defer f.Close()

	const chunk = 64 << 10
	buf := make([]byte, chunk)
	runs, inRun := 0, false
	for off := int64(0); off < size; {
		n := int64(chunk)
		if r := size - off; r < n {
			n = r
		}
		got, err := f.ReadAt(buf[:n], off)
		if got == 0 {
			break
		}
		if err != nil && got < int(n) {
			n = int64(got)
		}
		for i := int64(0); i < n; i++ {
			if buf[i] == 0 {
				if !inRun {
					inRun = true
					runs++
				}
			} else {
				inRun = false
			}
		}
		off += n
	}
	return runs
}

// listHoles returns every byte range that differs from the expected stream.
func listHoles(t *testing.T, path string, size int64) [][2]int64 {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return [][2]int64{{0, -1}}
	}
	defer f.Close()

	const chunk = 64 << 10
	got := make([]byte, chunk)
	want := make([]byte, chunk)

	var (
		holes     [][2]int64
		inHole    bool
		holeStart int64
	)
	for off := int64(0); off < size; {
		n := int64(chunk)
		if r := size - off; r < n {
			n = r
		}
		if _, err := f.ReadAt(got[:n], off); err != nil {
			break
		}
		testsrv.Fill(testSeed, off, want[:n])
		for i := int64(0); i < n; i++ {
			bad := got[i] != want[i]
			if bad && !inHole {
				inHole, holeStart = true, off+i
			}
			if !bad && inHole {
				holes = append(holes, [2]int64{holeStart, off + i})
				inHole = false
			}
		}
		off += n
	}
	if inHole {
		holes = append(holes, [2]int64{holeStart, size})
	}
	return holes
}
