package engine

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"vdownload/testsrv"
)

// The sizes here are deliberately modest: the tests must exercise real
// segmentation and real connection handling, but they also have to stay fast.
// MinSegmentSize is lowered so that a few megabytes still produce several
// segments.
const (
	testSize = 4 << 20 // 4 MiB
	testSeed = 7
)

// startServer launches a testsrv with the given tweaks and registers cleanup.
func startServer(t *testing.T, tweak func(*testsrv.Config)) (*testsrv.Server, string) {
	t.Helper()

	cfg := testsrv.DefaultConfig(testSize)
	cfg.Seed = testSeed
	if tweak != nil {
		tweak(&cfg)
	}

	srv := testsrv.New(cfg)
	base, err := srv.Start("127.0.0.1:0")
	if err != nil {
		t.Fatalf("start testsrv: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv, base + "/file"
}

// testOptions returns options that force real segmentation on small resources
// while keeping retry penalties short.
func testOptions(conns int) Options {
	o := Options{}.Normalize()
	o.Connections = conns
	o.MinSizeForSegments = 1
	o.MinSegmentSize = 64 << 10
	o.ReserveSize = 64 << 10
	o.VerifySize = true
	o.Backoff = Backoff{
		Base:     5 * time.Millisecond,
		Max:      20 * time.Millisecond,
		Factor:   2,
		Jitter:   0,
		MaxTries: 8,
	}
	o.ProgressInterval = 50 * time.Millisecond
	return o
}

// assertContentMatches re-derives the deterministic stream the server serves and
// compares it against the finished file.
//
// Comparing bytes, not just the length, is the whole point: a segmented
// download that writes ranges at the wrong offsets, or that stops at the first
// reserve block of every segment, produces a file of exactly the right length
// with the wrong contents.
func assertContentMatches(t *testing.T, path string, seed, size int64) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != size {
		t.Fatalf("%s holds %d bytes, want %d", path, st.Size(), size)
	}

	const chunk = 256 << 10
	got := make([]byte, chunk)
	want := make([]byte, chunk)

	for off := int64(0); off < size; {
		n := int64(chunk)
		if r := size - off; r < n {
			n = r
		}
		if _, err := f.ReadAt(got[:n], off); err != nil {
			t.Fatalf("read %d bytes at %d: %v", n, off, err)
		}
		testsrv.Fill(seed, off, want[:n])
		if !bytes.Equal(got[:n], want[:n]) {
			for i := int64(0); i < n; i++ {
				if got[i] != want[i] {
					t.Fatalf("content mismatch at offset %d: got %#02x, want %#02x",
						off+i, got[i], want[i])
				}
			}
			t.Fatalf("content mismatch in [%d,%d)", off, off+n)
		}
		off += n
	}
}

// rangeStart returns the first offset of a "bytes=lo-hi" header, or -1.
func rangeStart(header string) int64 {
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return -1
	}
	lo, _, ok := strings.Cut(spec, "-")
	if !ok || lo == "" {
		return -1
	}
	n, err := strconv.ParseInt(lo, 10, 64)
	if err != nil {
		return -1
	}
	return n
}

// countRanged counts the recorded requests that carried a Range header.
func countRanged(srv *testsrv.Server) int {
	var n int
	for _, r := range srv.Requests() {
		if r.Range != "" {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------------------
// The core guarantee: segmentation must not change the bytes
// ---------------------------------------------------------------------------

// TestSegmentedDownloadIsByteExact is the regression test for the bug where
// fetchSegment treated "one reserve block written" as "segment finished". Every
// segment kept only its first block, so a 4 MiB download silently published
// about a quarter of the data with a plausible file length.
func TestSegmentedDownloadIsByteExact(t *testing.T) {
	for _, conns := range []int{1, 2, 4, 8} {
		t.Run(fmt.Sprintf("conns=%d", conns), func(t *testing.T) {
			srv, url := startServer(t, nil)
			dest := filepath.Join(t.TempDir(), "out.bin")

			d, err := New(testOptions(conns))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()

			res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
			if err != nil {
				t.Fatalf("Download: %v", err)
			}
			if res.Size != testSize {
				t.Errorf("reported size = %d, want %d", res.Size, testSize)
			}
			assertContentMatches(t, dest, testSeed, testSize)

			// The working set must be gone once the download is published;
			// leaving it behind would make the next run resume a finished file.
			for _, suffix := range []string{PartSuffix, StateSuffix} {
				if _, err := os.Stat(dest + suffix); !os.IsNotExist(err) {
					t.Errorf("%s still exists after a successful download", dest+suffix)
				}
			}

			// A segmented run must really have issued several ranged requests,
			// otherwise the test would pass on the single-stream path and prove
			// nothing about segmentation.
			if conns > 1 {
				if n := countRanged(srv); n < 2 {
					t.Errorf("only %d ranged requests with conns=%d; segmentation did not happen", n, conns)
				}
			}
		})
	}
}

// TestSegmentedDownloadLargeReserveIsByteExact guards the interaction between
// the reserve block size and the segment length: with a reserve larger than a
// whole segment, the very first block finishes the segment, which is the case
// that hides a "one block only" bug.
func TestSegmentedDownloadLargeReserveIsByteExact(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	opt := testOptions(4)
	opt.ReserveSize = testSize * 4 // every segment fits in one reservation
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestSingleStreamDownloadIsByteExact covers the path taken when the server
// offers no ranges at all.
func TestSingleStreamDownloadIsByteExact(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.AdvertiseRange = false
		c.HonorRange = false
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(8))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.RangeSupported {
		t.Error("RangeSupported = true against a server that refuses ranges")
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestFakeRangeServerFallsBack proves that a server which advertises
// Accept-Ranges but answers 200 anyway is detected, rather than having its whole
// response written at the first segment's offset.
func TestFakeRangeServerFallsBack(t *testing.T) {
	srv, url := startServer(t, func(c *testsrv.Config) {
		c.AdvertiseRange = true
		c.HonorRange = false
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(8))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.RangeSupported {
		t.Error("RangeSupported = true, but the server ignored every Range request")
	}
	assertContentMatches(t, dest, testSeed, testSize)

	if n := countRanged(srv); n == 0 {
		t.Error("no ranged request was attempted, so range support was never measured")
	}
}

// ---------------------------------------------------------------------------
// Resume
// ---------------------------------------------------------------------------

// TestResumeContinuesFromPartialFile stops a download part way through, then
// finishes it, and checks both the bytes and that the second attempt really
// asked for a non-zero offset instead of starting over.
func TestResumeContinuesFromPartialFile(t *testing.T) {
	srv, url := startServer(t, func(c *testsrv.Config) {
		// Throttle so the download is still in flight when cancellation lands.
		c.PerConnRate = 2 << 20
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var observed int64
	progress := func(p Progress) {
		if observed == 0 && p.Downloaded > testSize/8 && p.Downloaded < testSize {
			observed = p.Downloaded
			cancel()
		}
	}

	_, err = d.Download(ctx, Request{URL: url, Dest: dest, Progress: progress})
	if err == nil {
		t.Skip("download finished before it could be interrupted")
	}
	if observed == 0 {
		t.Skip("no usable progress was observed before cancellation")
	}

	part := dest + PartSuffix
	state := dest + StateSuffix
	if _, err := os.Stat(part); err != nil {
		t.Fatalf("partial file missing after cancellation: %v", err)
	}
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("sidecar missing after cancellation: %v", err)
	}

	// Clear the log so resume requests can be told apart from the first run.
	srv.Reset()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest, Resume: true})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if !res.Resumed {
		t.Error("Resumed = false on the second attempt")
	}
	assertContentMatches(t, dest, testSeed, testSize)

	resumedFrom := int64(-1)
	for _, r := range srv.Requests() {
		if lo := rangeStart(r.Range); lo > 0 {
			resumedFrom = lo
			break
		}
	}
	if resumedFrom <= 0 {
		t.Errorf("no range request started beyond offset 0; the resume restarted from scratch (requests: %+v)",
			srv.Requests())
	}
}

// TestResumeWithoutSidecarStartsOver checks that asking to resume when nothing
// is resumable is harmless.
func TestResumeWithoutSidecarStartsOver(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest, Resume: true})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Resumed {
		t.Error("Resumed = true even though no partial data existed")
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestChangedResourceInvalidatesPartial rewrites the sidecar's validator so the
// resource looks changed on the server, then checks that the stale partial data
// is discarded rather than mixed with new bytes.
func TestChangedResourceInvalidatesPartial(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// Fabricate a plausible-looking partial state that claims a different
	// validator and a different size than the server actually serves.
	part := dest + PartSuffix
	if err := os.WriteFile(part, make([]byte, 512<<10), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := &State{
		Version:    StateVersion,
		URL:        url,
		FinalURL:   url,
		Size:       testSize,
		ETag:       `"a-different-validator"`,
		NoRange:    false,
		Segments:   []SegState{{Start: 0, End: testSize - 1, Written: 0}},
		TempPath:   part,
		TargetPath: dest,
		CreatedAt:  time.Now(),
	}
	if err := saveState(dest+StateSuffix, stale); err != nil {
		t.Fatal(err)
	}

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest, Resume: true})
	if err != nil {
		t.Fatalf("Download with a stale sidecar: %v", err)
	}
	if res.Resumed {
		t.Error("Resumed = true, but the sidecar described a different resource")
	}
	// The crucial part: the published file must match the server exactly, not
	// be a blend of the fabricated 512 KiB and freshly fetched bytes.
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestTruncatedPartialFileIsNotTrusted covers the crash window where the
// sidecar is newer than the data it describes. The clamps in
// segmentsFromStates must prevent the engine from skipping bytes it never
// actually received.
func TestTruncatedPartialFileIsNotTrusted(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// The sidecar claims the whole resource is already downloaded, but the
	// partial file on disk is short.
	part := dest + PartSuffix
	if err := os.WriteFile(part, make([]byte, 1024), 0o644); err != nil {
		t.Fatal(err)
	}
	lying := &State{
		Version:    StateVersion,
		URL:        url,
		FinalURL:   url,
		Size:       testSize,
		Segments:   []SegState{{Start: 0, End: testSize - 1, Written: testSize}},
		TempPath:   part,
		TargetPath: dest,
		CreatedAt:  time.Now(),
	}
	if err := saveState(dest+StateSuffix, lying); err != nil {
		t.Fatal(err)
	}

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest, Resume: true}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// ---------------------------------------------------------------------------
// Hostile servers
// ---------------------------------------------------------------------------

// TestDropAfterBytesFailsInsteadOfPublishing covers a connection that is
// hard-closed mid-body on every single response. The download cannot complete,
// and the engine must report an error rather than publish a truncated file.
func TestDropAfterBytesFailsInsteadOfPublishing(t *testing.T) {
	srv, url := startServer(t, func(c *testsrv.Config) {
		// The drop must land *inside* one reserve block, otherwise the server
		// conveniently finishes each 64 KiB block before cutting the
		// connection and the download completes normally.
		c.DropAfterBytes = 16 << 10
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	opt := testOptions(2)
	opt.Backoff.MaxTries = 3
	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := d.Download(ctx, Request{URL: url, Dest: dest}); err == nil {
		t.Fatal("Download succeeded even though the server truncates every response")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a truncated download was published under its final name")
	}
	if n := len(srv.Requests()); n < 2 {
		t.Errorf("only %d requests were made; the engine did not retry", n)
	}
}

// TestFailFirstRetriesWithBackoff exercises the 503 path, which is the one
// retryable status a server commonly returns.
func TestFailFirstRetriesWithBackoff(t *testing.T) {
	srv, url := startServer(t, func(c *testsrv.Config) {
		c.FailFirst = 3
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	res, err := d.Download(ctx, Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Retries == 0 {
		t.Error("Retries = 0, but the server answered 503 to the first three requests")
	}
	assertContentMatches(t, dest, testSeed, testSize)
	if n := len(srv.Requests()); n < 4 {
		t.Errorf("got %d requests, want at least 4 (three failures plus a success)", n)
	}
}

// TestNoHeadServerStillProbes covers a server that answers 405 to HEAD, which
// must not stop the engine from measuring range support.
func TestNoHeadServerStillProbes(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.NoHead = true
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	p, err := d.Probe(context.Background(), url)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if p.Size != testSize {
		t.Errorf("Size = %d, want %d", p.Size, testSize)
	}
	if !p.RangeSupported {
		t.Errorf("RangeSupported = false against a server that honours ranges (note: %s)", p.Note)
	}

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestChunkedResponseWithoutContentLength covers a server that omits
// Content-Length and streams with chunked framing instead.
func TestChunkedResponseWithoutContentLength(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.NoContentLength = true
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(1))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Size != testSize {
		t.Errorf("Size = %d, want %d", res.Size, testSize)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestCookieProtectedResource covers header propagation into the probe and
// every segment request.
func TestCookieProtectedResource(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.RequireCookie = "s3cret"
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	opt := testOptions(4)
	opt.Headers = map[string][]string{"Cookie": {"session=s3cret"}}

	d, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// TestMissingCookieFails checks the negative case, so the test above cannot pass
// merely because the server stopped enforcing the cookie.
func TestMissingCookieFails(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.RequireCookie = "s3cret"
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{URL: url, Dest: dest}); err == nil {
		t.Fatal("Download succeeded without the required cookie")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a forbidden download was published as a file")
	}
}

// ---------------------------------------------------------------------------
// Naming and destination handling
// ---------------------------------------------------------------------------

// TestDestinationDirectoryKeepsSuggestedName checks that pointing Dest at a
// directory joins the server-suggested name instead of treating the directory
// as the output file.
func TestDestinationDirectoryKeepsSuggestedName(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.ContentDisposition = `attachment; filename="payload.bin"`
	})
	dir := t.TempDir()

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dir})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	want := filepath.Join(dir, "payload.bin")
	if res.Path != want {
		t.Errorf("Path = %q, want %q", res.Path, want)
	}
	assertContentMatches(t, want, testSeed, testSize)
}

// TestTraversalFilenameStaysInsideDestination is the security regression test:
// a hostile Content-Disposition must not be able to write outside the chosen
// directory.
func TestTraversalFilenameStaysInsideDestination(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.ContentDisposition = `attachment; filename="../../escaped.bin"`
	})
	root := t.TempDir()
	dir := filepath.Join(root, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dir})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	rel, err := filepath.Rel(dir, res.Path)
	if err != nil {
		t.Fatal(err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.IsAbs(rel) {
		t.Fatalf("download escaped the destination directory: %q is outside %q", res.Path, dir)
	}

	if _, err := os.Stat(filepath.Join(root, "escaped.bin")); err == nil {
		t.Fatalf("traversal succeeded: %s was created in the parent directory", filepath.Join(root, "escaped.bin"))
	}
	assertContentMatches(t, res.Path, testSeed, testSize)
}

// TestExplicitDestWins checks that an explicit file name overrides whatever the
// server suggests.
func TestExplicitDestWins(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.ContentDisposition = `attachment; filename="server-choice.bin"`
	})
	dir := t.TempDir()
	dest := filepath.Join(dir, "mine.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if res.Path != dest {
		t.Errorf("Path = %q, want %q", res.Path, dest)
	}
	if _, err := os.Stat(filepath.Join(dir, "server-choice.bin")); err == nil {
		t.Error("the server-suggested name was used even though an explicit destination was given")
	}
	assertContentMatches(t, dest, testSeed, testSize)
}

// ---------------------------------------------------------------------------
// Cancellation
// ---------------------------------------------------------------------------

// TestCancellationStopsPromptly checks that cancelling the context aborts the
// transfer rather than letting it run to completion.
func TestCancellationStopsPromptly(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 1 << 20
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	start := time.Now()
	var triggered bool
	_, err = d.Download(ctx, Request{
		URL:  url,
		Dest: dest,
		Progress: func(p Progress) {
			if !triggered && p.Downloaded > 0 {
				triggered = true
				cancel()
			}
		},
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Download returned nil after its context was cancelled")
	}
	if !triggered {
		t.Skip("cancellation was never triggered; the download was too fast")
	}
	// An unthrottled 4 MiB download over loopback takes milliseconds; the
	// throttle means a completed transfer would take seconds. Anything close to
	// that means cancellation was ignored.
	if elapsed > 3*time.Second {
		t.Errorf("cancellation took %v to take effect", elapsed)
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a cancelled download was published under its final name")
	}
}

// TestCancellationKeepsWorkingSet checks that a cancelled download leaves the
// partial file and sidecar behind so a later run can resume.
func TestCancellationKeepsWorkingSet(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 1 << 20
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var triggered bool
	_, _ = d.Download(ctx, Request{
		URL:  url,
		Dest: dest,
		Progress: func(p Progress) {
			if !triggered && p.Downloaded > 0 {
				triggered = true
				cancel()
			}
		},
	})
	if !triggered {
		t.Skip("the download finished before cancellation could be triggered")
	}

	if _, err := os.Stat(dest + PartSuffix); err != nil {
		t.Errorf("partial file was removed on cancellation: %v", err)
	}
	if _, err := os.Stat(dest + StateSuffix); err != nil {
		t.Errorf("sidecar was removed on cancellation: %v", err)
	}
}

// TestPermanentFailureRemovesWorkingSet checks the opposite: a resource that
// cannot be fetched at all should not leave debris behind.
func TestPermanentFailureRemovesWorkingSet(t *testing.T) {
	srv, _ := startServer(t, nil)
	_ = srv
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	// A URL that will never resolve to a resource.
	if _, err := d.Download(context.Background(), Request{
		URL:  "http://127.0.0.1:1/nothing",
		Dest: dest,
	}); err == nil {
		t.Fatal("Download succeeded against a closed port")
	}
	if _, err := os.Stat(dest); err == nil {
		t.Error("a failed download was published as a file")
	}
}

// ---------------------------------------------------------------------------
// Result reporting
// ---------------------------------------------------------------------------

// TestResultReportsSegmentation checks the numbers a UI would display.
func TestResultReportsSegmentation(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	res, err := d.Download(context.Background(), Request{URL: url, Dest: dest})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	if res.Connections != 4 {
		t.Errorf("Connections = %d, want 4", res.Connections)
	}
	if res.InitialSegments != 4 {
		t.Errorf("InitialSegments = %d, want 4", res.InitialSegments)
	}
	if res.Segments < res.InitialSegments {
		t.Errorf("Segments = %d, want at least the initial %d", res.Segments, res.InitialSegments)
	}
	if res.Duration <= 0 {
		t.Error("Duration = 0")
	}
	if res.BytesPerSec <= 0 {
		t.Errorf("BytesPerSec = %v, want positive", res.BytesPerSec)
	}
	if !res.RangeSupported {
		t.Error("RangeSupported = false against a range-capable server")
	}
	if res.Probe == nil {
		t.Error("Probe = nil")
	}
}

// TestProgressIsMonotonic checks that reported progress never goes backwards and
// ends at the full size.
func TestProgressIsMonotonic(t *testing.T) {
	_, url := startServer(t, nil)
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(4))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var (
		last  int64
		calls int
		final int64
	)
	_, err = d.Download(context.Background(), Request{
		URL:  url,
		Dest: dest,
		Progress: func(p Progress) {
			calls++
			if p.Downloaded < last {
				t.Errorf("progress went backwards: %d after %d", p.Downloaded, last)
			}
			last = p.Downloaded
			final = p.Downloaded
		},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if calls == 0 {
		t.Fatal("the progress callback was never called")
	}
	if final != testSize {
		t.Errorf("final progress = %d, want %d", final, testSize)
	}
}

// TestProgressSpeedAndETAAreSane guards the units of the derived values.
func TestProgressSpeedAndETAAreSane(t *testing.T) {
	_, url := startServer(t, func(c *testsrv.Config) {
		c.PerConnRate = 8 << 20 // slow enough to produce several reports
	})
	dest := filepath.Join(t.TempDir(), "out.bin")

	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	var maxETA time.Duration
	_, err = d.Download(context.Background(), Request{
		URL:  url,
		Dest: dest,
		Progress: func(p Progress) {
			if p.Speed < 0 {
				t.Errorf("negative speed %v", p.Speed)
			}
			if p.ETA > maxETA {
				maxETA = p.ETA
			}
		},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if maxETA > 10*time.Minute {
		t.Errorf("largest reported ETA = %v; the seconds-to-nanoseconds conversion looks wrong", maxETA)
	}
}

// TestDownloadRejectsEmptyURL is a small guard on argument handling.
func TestDownloadRejectsEmptyURL(t *testing.T) {
	d, err := New(testOptions(2))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	if _, err := d.Download(context.Background(), Request{Dest: filepath.Join(t.TempDir(), "x")}); err == nil {
		t.Error("Download accepted an empty URL")
	} else if !strings.Contains(err.Error(), "no URL") {
		t.Errorf("error = %v, want it to complain about the missing URL", err)
	}
}
