// Command idm is a multi-connection download manager: the command-line front
// end of the engine in internal/engine.
//
// Usage:
//
//	idm get <url> [flags]     download a resource
//	idm info <url> [flags]    probe a resource without downloading it
//	idm bench <url> [flags]   compare throughput across connection counts
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"vdownload/internal/engine"
	"vdownload/testsrv"
)

const usage = `idm - multi-connection download manager

usage:
  idm get   <url> [flags]    download a resource
  idm info  <url> [flags]    probe a resource without downloading
  idm bench <url> [flags]    compare throughput across connection counts

run "idm get -h" for the flags of a subcommand.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}

	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "get", "download":
		err = runGet(args)
	case "info", "probe":
		err = runInfo(args)
	case "bench":
		err = runBench(args)
	case "-h", "--help", "help":
		fmt.Print(usage)
		return
	default:
		fmt.Fprintf(os.Stderr, "idm: unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}

	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, "\ninterrupted; partial data and its sidecar were kept, rerun with -resume to continue")
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "idm: %v\n", err)
		os.Exit(1)
	}
}

// ---------------------------------------------------------------------------
// shared flags
// ---------------------------------------------------------------------------

// commonFlags holds the flags every subcommand shares.
type commonFlags struct {
	conns    int
	minSeg   int64
	chunk    int64
	reserve  int64
	headers  headerList
	proxy    string
	insecure bool
	timeout  time.Duration
}

func (c *commonFlags) register(fs *flag.FlagSet) {
	fs.IntVar(&c.conns, "conns", 8, "maximum number of parallel connections (1 disables segmentation)")
	fs.Int64Var(&c.minSeg, "min-segment", 1<<20, "smallest segment size in bytes")
	fs.Int64Var(&c.chunk, "chunk-size", 128<<10, "per-worker read/write buffer in bytes")
	fs.Int64Var(&c.reserve, "reserve", 512<<10, "bytes a worker claims from a segment at a time")
	fs.Var(&c.headers, "H", "extra request header, repeatable (e.g. -H 'Cookie: a=b')")
	fs.StringVar(&c.proxy, "proxy", "", "proxy URL, empty honours HTTP_PROXY/HTTPS_PROXY/NO_PROXY")
	fs.BoolVar(&c.insecure, "insecure", false, "skip TLS certificate verification (debugging only)")
	fs.DurationVar(&c.timeout, "timeout", 0, "overall timeout, 0 means no limit")
}

func (c *commonFlags) options() engine.Options {
	return engine.Options{
		Connections:    c.conns,
		MinSegmentSize: c.minSeg,
		ChunkSize:      int(c.chunk),
		ReserveSize:    c.reserve,
		Headers:        c.headers.Header(),
		Transport: engine.TransportOptions{
			ProxyURL:           c.proxy,
			InsecureSkipVerify: c.insecure,
		},
	}
}

// headerList collects repeated -H flags.
type headerList []string

func (h *headerList) String() string { return strings.Join(*h, ", ") }

func (h *headerList) Set(v string) error {
	if !strings.Contains(v, ":") {
		return fmt.Errorf("header %q must look like \"Name: value\"", v)
	}
	*h = append(*h, v)
	return nil
}

// Header converts the collected flags into a header set.
func (h headerList) Header() http.Header {
	if len(h) == 0 {
		return nil
	}
	out := make(http.Header, len(h))
	for _, raw := range h {
		name, value, _ := strings.Cut(raw, ":")
		name = strings.TrimSpace(name)
		value = strings.TrimSpace(value)
		if name == "" {
			continue
		}
		if prev := out.Get(name); prev != "" {
			// Repeated names accumulate, matching how curl treats -H.
			out.Set(name, prev+"; "+value)
			continue
		}
		out.Set(name, value)
	}
	return out
}

// newContext returns a context cancelled by Ctrl-C, plus the cancel func.
func newContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		select {
		case <-ch:
			fmt.Fprintln(os.Stderr, "\nstopping...")
			cancel()
		case <-ctx.Done():
		}
	}()
	if timeout > 0 {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, timeout)
		inner := cancel
		cancel = func() { tcancel(); inner() }
	}
	return ctx, cancel
}

// ---------------------------------------------------------------------------
// get
// ---------------------------------------------------------------------------

func runGet(args []string) error {
	fs := flag.NewFlagSet("get", flag.ExitOnError)
	var (
		cf       commonFlags
		out      = fs.String("o", "", "output file or directory (default: name suggested by the server)")
		resume   = fs.Bool("resume", true, "continue a previous partial download when possible")
		jsonOut  = fs.Bool("json", false, "print the result as JSON")
		quiet    = fs.Bool("quiet", false, "suppress the progress display")
		verify   = fs.Bool("verify-size", true, "re-check the finished length against the announced size")
		attempts = fs.Int("max-tries", 10, "maximum attempts per segment before giving up")
	)
	cf.register(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: idm get <url> [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	url, err := parseOneURL(fs, args)
	if err != nil {
		return err
	}

	ctx, cancel := newContext(cf.timeout)
	defer cancel()

	opt := cf.options()
	opt.VerifySize = *verify
	opt.Backoff.MaxTries = *attempts

	dl, err := engine.New(opt)
	if err != nil {
		return err
	}
	defer dl.Close()

	var progress func(engine.Progress)
	var stopProgress func()
	if !*quiet {
		progress, stopProgress = newProgressPrinter()
		defer stopProgress()
	}

	res, err := dl.Download(ctx, engine.Request{
		URL:      url,
		Dest:     *out,
		Resume:   *resume,
		Progress: progress,
	})
	if err != nil {
		return err
	}

	if stopProgress != nil {
		stopProgress()
	}
	if *jsonOut {
		return printJSON(res)
	}
	fmt.Printf("%s\n", res.Path)
	fmt.Printf("  %s in %s (%s/s)\n",
		humanBytes(res.Size), roundDuration(res.Duration), humanBytes(int64(res.BytesPerSec)))
	fmt.Printf("  connections=%d segments=%d (initial %d, splits %d) retries=%d range=%v resumed=%v\n",
		res.Connections, res.Segments, res.InitialSegments, res.Splits,
		res.Retries, res.RangeSupported, res.Resumed)
	return nil
}

// ---------------------------------------------------------------------------
// info
// ---------------------------------------------------------------------------

func runInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	var cf commonFlags
	cf.register(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: idm info <url> [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	url, err := parseOneURL(fs, args)
	if err != nil {
		return err
	}

	ctx, cancel := newContext(cf.timeout)
	defer cancel()

	dl, err := engine.New(cf.options())
	if err != nil {
		return err
	}
	defer dl.Close()

	p, err := dl.Probe(ctx, url)
	if err != nil {
		return err
	}

	fmt.Printf("url            %s\n", p.URL)
	if p.FinalURL != p.URL {
		fmt.Printf("final url      %s\n", p.FinalURL)
	}
	fmt.Printf("name           %s\n", p.SuggestedName)
	fmt.Printf("size           %d (%s)\n", p.Size, humanBytes(p.Size))
	fmt.Printf("range support  %v (measured)\n", p.RangeSupported)
	fmt.Printf("accept-ranges  %v (as advertised)\n", p.AdvertisedRange)
	if p.ContentType != "" {
		fmt.Printf("content-type   %s\n", p.ContentType)
	}
	if p.ETag != "" {
		fmt.Printf("etag           %s\n", p.ETag)
	}
	if p.LastModified != "" {
		fmt.Printf("last-modified  %s\n", p.LastModified)
	}
	if p.Note != "" {
		fmt.Printf("note           %s\n", p.Note)
	}
	if !p.RangeSupported {
		fmt.Println("\nthis server cannot be segmented; downloads will use a single connection")
	}
	return nil
}

// ---------------------------------------------------------------------------
// bench
// ---------------------------------------------------------------------------

func runBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	var (
		cf      commonFlags
		connArg = fs.String("conn-list", "1,2,4,8,16", "comma-separated connection counts to compare")
		reps    = fs.Int("reps", 1, "repetitions per connection count")
		dir     = fs.String("dir", "", "directory for the downloaded files (default: a temp dir, removed afterwards)")
		keep    = fs.Bool("keep", false, "keep the downloaded files")
		verify  = fs.Bool("verify", true, "compare the finished file against the server's deterministic content")
		seed    = fs.Int64("seed", 1, "content seed of the testsrv being benchmarked")
	)
	cf.register(fs)
	fs.Usage = func() {
		fmt.Fprintf(fs.Output(), "usage: idm bench <url> [flags]\n\nflags:\n")
		fs.PrintDefaults()
	}
	url, err := parseOneURL(fs, args)
	if err != nil {
		return err
	}

	counts, err := parseConns(*connArg)
	if err != nil {
		return err
	}

	ctx, cancel := newContext(cf.timeout)
	defer cancel()

	workDir := *dir
	if workDir == "" {
		workDir, err = os.MkdirTemp("", "vdownload-bench-")
		if err != nil {
			return err
		}
		if !*keep {
			defer os.RemoveAll(workDir)
		}
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return err
	}

	fmt.Printf("benchmarking %s\n", url)
	fmt.Printf("%-10s %-12s %-14s %-8s %-8s %-8s\n", "conns", "wall", "throughput", "segs", "splits", "retries")

	base := 0.0
	for _, n := range counts {
		best := 0.0
		var lastRes *engine.Result

		for rep := 0; rep < *reps; rep++ {
			opt := cf.options()
			opt.Connections = n
			opt.MinSizeForSegments = 0 // benchmark segmentation even on small resources
			opt.ProgressInterval = time.Hour

			dl, err := engine.New(opt)
			if err != nil {
				return err
			}

			dest := filepath.Join(workDir, fmt.Sprintf("bench-n%d-r%d.bin", n, rep))
			_ = os.Remove(dest)
			_ = os.Remove(dest + engine.PartSuffix)
			_ = os.Remove(dest + engine.StateSuffix)

			start := time.Now()
			res, err := dl.Download(ctx, engine.Request{URL: url, Dest: dest, Progress: nil})
			elapsed := time.Since(start)
			dl.Close()
			if err != nil {
				return fmt.Errorf("conns=%d: %w", n, err)
			}

			if *verify {
				if err := verifyContents(dest, *seed, res.Size); err != nil {
					return fmt.Errorf("conns=%d: %w", n, err)
				}
			}

			laps := elapsed.Seconds()
			if laps <= 0 {
				laps = 1e-9
			}
			rate := float64(res.Size) / laps
			if rate > best {
				best = rate
			}
			lastRes = res
			if n == counts[0] && rep == 0 {
				base = rate
			}
		}

		speedup := ""
		if base > 0 {
			speedup = fmt.Sprintf(" (%.2fx)", best/base)
		}
		fmt.Printf("%-10d %-12s %-14s %-8d %-8d %-8d%s\n",
			n,
			roundDuration(lastRes.Duration),
			humanBytes(int64(best))+"/s",
			lastRes.Segments,
			lastRes.Splits,
			lastRes.Retries,
			speedup,
		)
	}
	return nil
}

// verifyContents re-derives the deterministic content stream a testsrv would
// have served and compares it against the finished file, byte for byte.
//
// The comparison is done in chunks instead of materialising the whole resource,
// so verifying a multi-gigabyte download does not need that much RAM. This is
// the only check that catches a segment written at the wrong offset: a file
// stitched together from misplaced ranges still has exactly the right length.
func verifyContents(path string, seed, size int64) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return err
	}
	if size >= 0 && st.Size() != size {
		return fmt.Errorf("size mismatch: file holds %d bytes, server announced %d", st.Size(), size)
	}
	if size < 0 {
		size = st.Size()
	}

	const chunk = 1 << 20
	got := make([]byte, chunk)
	want := make([]byte, chunk)

	var off int64
	for off < size {
		n := int64(chunk)
		if r := size - off; r < n {
			n = r
		}
		if _, err := io.ReadFull(f, got[:n]); err != nil {
			return fmt.Errorf("read %s at %d: %w", path, off, err)
		}
		testsrv.Fill(seed, off, want[:n])
		if !bytes.Equal(got[:n], want[:n]) {
			for i := int64(0); i < n; i++ {
				if got[i] != want[i] {
					return fmt.Errorf("content mismatch at offset %d: got %#02x, want %#02x", off+i, got[i], want[i])
				}
			}
			return fmt.Errorf("content mismatch somewhere in [%d,%d)", off, off+n)
		}
		off += n
	}
	return nil
}

func parseConns(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, fmt.Errorf("bad connection count %q: %w", part, err)
		}
		if n < 1 {
			return nil, fmt.Errorf("connection count must be at least 1, got %d", n)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, errors.New("no connection counts given")
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// progress display
// ---------------------------------------------------------------------------

// newProgressPrinter returns a throttled callback and a stop func.
//
// The printer owns a mutex so that overlapping callbacks cannot interleave
// halves of two different lines, and it redraws with \r so the display stays on
// one line the way a download manager's window does.
func newProgressPrinter() (func(engine.Progress), func()) {
	var (
		mu     sync.Mutex
		last   = time.Now()
		closed bool
		width  int
	)
	stop := func() {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		closed = true
		if width > 0 {
			fmt.Printf("\r%s\r", strings.Repeat(" ", width))
		}
	}
	report := func(p engine.Progress) {
		mu.Lock()
		defer mu.Unlock()
		if closed {
			return
		}
		now := time.Now()
		// The engine already throttles, but a second gate keeps a chatty
		// caller from making the terminal flicker.
		if now.Sub(last) < 80*time.Millisecond && p.Downloaded != p.Total {
			return
		}
		last = now

		pct := "  ?  "
		if p.Total > 0 {
			pct = fmt.Sprintf("%5.1f%%", float64(p.Downloaded)/float64(p.Total)*100)
		}
		eta := "--:--"
		if p.ETA > 0 {
			eta = roundDuration(p.ETA)
		}
		line := fmt.Sprintf("%s %s / %s  %s/s  eta %s  seg %d/%d  conns %d",
			pct,
			humanBytes(p.Downloaded),
			humanBytes(p.Total),
			humanBytes(int64(p.Speed)),
			eta,
			p.SegmentsDone, p.Segments,
			p.Connections,
		)
		if pad := width - len(line); pad > 0 {
			line += strings.Repeat(" ", pad)
		}
		width = len(line)
		fmt.Printf("\r%s", line)
	}
	return report, stop
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

// parseOneURL extracts exactly one URL from args.
//
// Go's flag package stops parsing at the first non-flag argument, so the
// natural invocation "idm get <url> -o file" would leave the flags behind the
// URL unparsed. Rather than force callers to put flags first, the URL is
// located up front and the arguments are reordered so flags precede it.
func parseOneURL(fs *flag.FlagSet, args []string) (string, error) {
	flagArgs, posArgs := splitURLArg(fs, args)
	if len(posArgs) > 1 {
		return "", fmt.Errorf("expected exactly one URL, got %d: %v", len(posArgs), posArgs)
	}

	ordered := make([]string, 0, len(flagArgs)+2)
	ordered = append(ordered, flagArgs...)
	if len(posArgs) == 1 {
		// The terminator lets a URL that begins with "-" through unharmed.
		ordered = append(ordered, "--", posArgs[0])
	}
	if err := fs.Parse(ordered); err != nil {
		return "", err
	}

	if len(posArgs) == 0 {
		fs.Usage()
		return "", errors.New("no URL given")
	}
	return posArgs[0], nil
}

// splitURLArg separates flag arguments from positional ones.
//
// A flag written as "-name value" needs its value carried along; "-name=value"
// and boolean flags do not consume the next argument. Getting this wrong would
// silently turn a flag's value into a second "URL" and fail the count check.
func splitURLArg(fs *flag.FlagSet, args []string) (flagArgs, posArgs []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			posArgs = append(posArgs, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			posArgs = append(posArgs, a)
			continue
		}
		flagArgs = append(flagArgs, a)

		if strings.Contains(a, "=") {
			continue
		}
		name := strings.TrimLeft(a, "-")
		if name == "" {
			continue
		}
		f := fs.Lookup(name)
		if f == nil || isBoolFlag(f) {
			continue
		}
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, posArgs
}

// isBoolFlag reports whether a flag may be written without a value.
func isBoolFlag(f *flag.Flag) bool {
	type boolFlag interface{ IsBoolFlag() bool }
	bf, ok := f.Value.(boolFlag)
	return ok && bf.IsBoolFlag()
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// humanBytes formats a byte count with binary units.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// roundDuration formats a duration compactly: "1.4s", "2m13s", "1h02m".
func roundDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
