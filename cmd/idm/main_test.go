package main

import (
	"flag"
	"strings"
	"sync"
	"testing"
	"time"

	"vkmdownload/internal/engine"
)

// ---------------------------------------------------------------------------
// flag parsing
// ---------------------------------------------------------------------------

// newGetFlagSet mirrors runGet's flag registration without running anything,
// so the parsing helpers can be exercised directly.
func newGetFlagSet() *flag.FlagSet {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	fs.SetOutput(discard{})
	fs.String("o", "", "output")
	fs.Bool("resume", true, "resume")
	fs.Bool("json", false, "json")
	var cf commonFlags
	cf.register(fs)
	return fs
}

// TestParseOneURLAcceptsFlagsAfterURL pins the reason parseOneURL exists: the
// Go flag package stops at the first positional argument, so "idm get <url>
// -o out.bin" would silently drop -o without the reordering.
func TestParseOneURLAcceptsFlagsAfterURL(t *testing.T) {
	fs := newGetFlagSet()
	url, err := parseOneURL(fs, []string{"http://example.com/a.bin", "-o", "out.bin", "-conns", "4"})
	if err != nil {
		t.Fatalf("parseOneURL: %v", err)
	}
	if url != "http://example.com/a.bin" {
		t.Errorf("url = %q", url)
	}
	if got := fs.Lookup("o").Value.String(); got != "out.bin" {
		t.Errorf("-o = %q, want out.bin (a flag after the URL must still be parsed)", got)
	}
	if got := fs.Lookup("conns").Value.String(); got != "4" {
		t.Errorf("-conns = %q, want 4", got)
	}
}

func TestParseOneURLRejectsTwoURLs(t *testing.T) {
	fs := newGetFlagSet()
	if _, err := parseOneURL(fs, []string{"http://a/x", "http://b/y"}); err == nil {
		t.Fatal("parseOneURL accepted two URLs")
	}
}

func TestParseOneURLRequiresOne(t *testing.T) {
	fs := newGetFlagSet()
	if _, err := parseOneURL(fs, []string{"-o", "x"}); err == nil {
		t.Fatal("parseOneURL accepted a run with no URL")
	}
}

// TestParseOneURLBooleanFlagDoesNotEatURL pins the subtlety splitURLArg
// encodes: a boolean flag written without a value must not consume the
// following argument as its value, which would turn the URL into a flag value
// and leave the run with no URL at all.
func TestParseOneURLBooleanFlagDoesNotEatURL(t *testing.T) {
	fs := newGetFlagSet()
	got, err := parseOneURL(fs, []string{"-json", "http://example.com/q.bin"})
	if err != nil {
		t.Fatalf("parseOneURL: %v", err)
	}
	if got != "http://example.com/q.bin" {
		t.Errorf("url = %q, want the URL to survive a boolean flag in front of it", got)
	}
}

func TestParseOneURLEscapedDashURL(t *testing.T) {
	fs := newGetFlagSet()
	got, err := parseOneURL(fs, []string{"--", "-weird.example/x"})
	if err != nil {
		t.Fatalf("parseOneURL: %v", err)
	}
	if got != "-weird.example/x" {
		t.Errorf("url = %q", got)
	}
}

func TestParseConns(t *testing.T) {
	got, err := parseConns("1, 2,4 ,16")
	if err != nil {
		t.Fatalf("parseConns: %v", err)
	}
	if len(got) != 4 || got[0] != 1 || got[3] != 16 {
		t.Errorf("parseConns = %v", got)
	}
	if _, err := parseConns("0"); err == nil {
		t.Error("parseConns accepted 0 connections")
	}
	if _, err := parseConns("abc"); err == nil {
		t.Error("parseConns accepted a non-number")
	}
	if _, err := parseConns(" , "); err == nil {
		t.Error("parseConns accepted an empty list")
	}
}

// ---------------------------------------------------------------------------
// repeated -H headers
// ---------------------------------------------------------------------------

// TestRepeatedHeadersStaySeparate pins the curl-compatible behaviour: two -H of
// the same name become two header lines, not one value joined with "; ". Two
// cookies joined that way form a single malformed cookie, and for every other
// header the second value simply disappears.
func TestRepeatedHeadersStaySeparate(t *testing.T) {
	var h headerList
	for _, v := range []string{"Cookie: a=1", "Cookie: b=2", "Accept-Language: zh", ": empty"} {
		if err := h.Set(v); err != nil {
			t.Fatalf("Set(%q): %v", v, err)
		}
	}
	if err := h.Set("no-colon"); err == nil {
		t.Error("Set accepted a header with no colon")
	}

	got := h.Header()
	if vs := got.Values("Cookie"); len(vs) != 2 || vs[0] != "a=1" || vs[1] != "b=2" {
		t.Errorf("Cookie values = %q, want two separate values", vs)
	}
	if v := got.Get("Accept-Language"); v != "zh" {
		t.Errorf("Accept-Language = %q", v)
	}
	if len(got) != 2 {
		t.Errorf("header set has %d names, want 2 (the empty name must be skipped): %v", len(got), got)
	}
}

// TestHeaderListNilIsNil keeps Options.Headers nil when no -H was given, so the
// engine applies its own defaults rather than seeing an empty map.
func TestHeaderListNilIsNil(t *testing.T) {
	var h headerList
	if h.Header() != nil {
		t.Error("Header() on an empty list returned a non-nil map")
	}
}

// ---------------------------------------------------------------------------
// defaults handed to the engine
// ---------------------------------------------------------------------------

// TestOptionsDeferToEngineDefaults pins the P2-16 fix: with no explicit flags
// the CLI must pass zero for chunk and reserve so engine.Normalize chooses,
// instead of re-pinning the old 128 KiB / 512 KiB values the engine rejected.
func TestOptionsDeferToEngineDefaults(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse(nil); err != nil {
		t.Fatal(err)
	}
	opt := cf.options()
	if opt.ChunkSize != 0 {
		t.Errorf("ChunkSize = %d, want 0 (engine default)", opt.ChunkSize)
	}
	if opt.ReserveSize != 0 {
		t.Errorf("ReserveSize = %d, want 0 (engine default)", opt.ReserveSize)
	}
	if opt.Limiter != nil {
		t.Error("Limiter is set with -limit unset; it must stay nil/unlimited")
	}

	normalized := opt.Normalize()
	if normalized.ChunkSize != 256<<10 {
		t.Errorf("engine default chunk = %d, want 256 KiB", normalized.ChunkSize)
	}
	if normalized.ReserveSize != 16<<20 {
		t.Errorf("engine default reserve = %d, want 16 MiB", normalized.ReserveSize)
	}
}

// TestOptionsNormalizeHandlesExplicitValues keeps an explicit flag working
// after the default changed to zero.
func TestOptionsNormalizeHandlesExplicitValues(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse([]string{"-chunk-size", "1024", "-reserve", "2048"}); err != nil {
		t.Fatal(err)
	}
	opt := cf.options().Normalize()
	if opt.ReserveSize != 2048 {
		t.Errorf("ReserveSize = %d, want 2048", opt.ReserveSize)
	}
	if opt.ChunkSize <= 0 {
		t.Errorf("ChunkSize = %d, want a positive value", opt.ChunkSize)
	}
}

// TestLimitFlagCreatesSharedLimiter pins the new -limit flag: KiB/s on the
// command line become a byte-per-second limiter that every connection shares,
// so "500" is the total ceiling rather than 500 per connection.
func TestLimitFlagCreatesSharedLimiter(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	var cf commonFlags
	cf.register(fs)
	if err := fs.Parse([]string{"-limit", "500"}); err != nil {
		t.Fatal(err)
	}
	lim := cf.options().Limiter
	if lim == nil {
		t.Fatal("-limit 500 produced no limiter")
	}
	if got := lim.Rate(); got != 500<<10 {
		t.Errorf("Rate = %d, want %d", got, 500<<10)
	}
}

// ---------------------------------------------------------------------------
// humanBytes / humanSize / roundDuration
// ---------------------------------------------------------------------------

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{1, "1 B"},
		{1023, "1023 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{1024 * 1024, "1.0 MiB"},
		{3 << 30, "3.0 GiB"},
		{1 << 40, "1.0 TiB"},
	}
	for _, tc := range cases {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHumanSizeUnknown pins the "-1 B" fix: a length the server never disclosed
// reads as unknown, not as a negative file size.
func TestHumanSizeUnknown(t *testing.T) {
	if got := humanSize(-1); !strings.Contains(got, "unknown") {
		t.Errorf("humanSize(-1) = %q, want an 'unknown' message", got)
	}
	if got := humanSize(2048); got != "2.0 KiB" {
		t.Errorf("humanSize(2048) = %q", got)
	}
}

func TestRoundDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{0, "0s"},
		{-5 * time.Second, "0s"},
		{1500 * time.Millisecond, "1.5s"},
		{90 * time.Second, "1m30s"},
		{time.Hour + 2*time.Minute, "1h02m"},
	}
	for _, tc := range cases {
		if got := roundDuration(tc.in); got != tc.want {
			t.Errorf("roundDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// progress printer
// ---------------------------------------------------------------------------

// TestProgressPrinterIsRaceFree drives overlapping callbacks and the stop func
// (run under -race): several workers report at once, and stopping must stay
// idempotent rather than panicking on a closed channel.
func TestProgressPrinterIsRaceFree(t *testing.T) {
	report, stop := newProgressPrinter()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := int64(0); n <= 4096; n += 512 {
				report(engine.Progress{Downloaded: n, Total: 4096, Speed: 1 << 20, Connections: 4})
			}
		}()
	}
	wg.Wait()
	stop()
	stop() // must not panic
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
