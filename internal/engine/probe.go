package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
)

// ProbeResult is everything the engine learned about a resource before it
// commits to a download strategy.
type ProbeResult struct {
	URL      string
	FinalURL string

	// Size is the total resource size, or -1 when the server never disclosed
	// it. Without a size there is nothing to segment, so -1 forces a single
	// stream.
	Size int64

	// RangeSupported is the *measured* answer, obtained by issuing a real
	// ranged GET. It is the only value the engine trusts.
	RangeSupported bool
	// AdvertisedRange is what the Accept-Ranges response header claimed, kept
	// purely so the engine can report a server that lies.
	AdvertisedRange bool

	ETag               string
	LastModified       string
	ContentType        string
	ContentDisposition string

	// SuggestedName is derived from Content-Disposition first, then the URL
	// path.
	SuggestedName string

	// Note explains how RangeSupported was established, for logs and tests.
	Note string
}

// Validator returns the value to send in If-Range on resume, preferring ETag.
func (p *ProbeResult) Validator() string {
	if p.ETag != "" {
		return p.ETag
	}
	return p.LastModified
}

// probeBodyLimit is how much of a probe response body is drained before the
// connection is discarded. Draining a small body lets the connection return to
// the pool; a large one must be abandoned immediately, because a server that
// ignores Range would otherwise make the probe download the whole file.
const probeBodyLimit = 64 << 10

// Probe determines the resource's size and whether byte ranges actually work.
//
// The strategy is HEAD first, falling back to a ranged GET:
//
//  1. HEAD is cheap and yields the metadata, but many servers answer it badly
//     (405, or a different Content-Length than GET), and it says nothing about
//     whether a Range request will really be honoured.
//  2. A one-byte GET with "Range: bytes=0-0" is the only authoritative test.
//     A 206 with Content-Range proves ranges work and reveals the true total
//     size; a 200 proves the server ignored the range header entirely.
//
// Step 2 runs even when HEAD advertised "Accept-Ranges: bytes", because a
// server claiming range support and then answering 200 is exactly the failure
// that would silently corrupt a segmented download.
func (d *Downloader) Probe(ctx context.Context, rawURL string) (*ProbeResult, error) {
	res := &ProbeResult{URL: rawURL, FinalURL: rawURL, Size: -1}

	// --- Step 1: HEAD, best effort. ---
	headErr := d.probeHead(ctx, rawURL, res)
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if headErr != nil && !isRetryable(headErr) {
		// A definitive answer such as 404 will not change when asked again, so
		// there is no point spending another request on the ranged probe.
		return nil, headErr
	}

	// --- Step 2: authoritative ranged GET. ---
	rangeErr := d.probeRange(ctx, rawURL, res)
	if rangeErr != nil && res.Size <= 0 {
		// Nothing usable came back at all.
		return nil, rangeErr
	}
	if rangeErr != nil {
		// If the ranged GET is impossible but HEAD gave us a usable size, fall
		// back to a single stream rather than failing the whole download.
		res.RangeSupported = false
		res.Note = fmt.Sprintf("range probe failed (%v); single stream", rangeErr)
	}

	// A web page is not a file, whatever its length says. This has to be caught
	// before the transfer starts: the bytes would otherwise be downloaded and
	// saved under the page's name, failing much later on a length mismatch that
	// tells the user nothing about the actual mistake.
	if err := notAFileError(res); err != nil {
		return res, err
	}
	return res, nil
}

// ErrNotAFile means the address answered with a web page rather than the bytes
// of a file — a share link to a hosting page instead of a direct link.
var ErrNotAFile = errors.New("该地址返回的是网页，不是文件")

// notAFileError reports a probe result that describes a web page.
func notAFileError(res *ProbeResult) error {
	if !looksLikeHTML(res.ContentType) {
		return nil
	}
	return fmt.Errorf("%w (Content-Type: %s)", ErrNotAFile, res.ContentType)
}

// looksLikeHTML reports whether a Content-Type header describes a web page.
func looksLikeHTML(contentType string) bool {
	if contentType == "" {
		return false
	}
	// Drop any parameters: "text/html; charset=utf-8" -> "text/html".
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	switch strings.ToLower(strings.TrimSpace(contentType)) {
	case "text/html", "application/xhtml+xml":
		return true
	}
	return false
}

// errHeadUnavailable means the server declined the HEAD request. It is expected
// rather than fatal: servers answer 405, 501 or 403 to HEAD routinely, and the
// ranged GET that follows settles both the size and range support anyway.
var errHeadUnavailable = errors.New("server declined HEAD")

// probeHead issues the HEAD request and folds whatever it learns into res.
//
// The request is wrapped in the shared retry helper: a transient 503 or a
// dropped connection during discovery must not fail the download outright,
// because the per-segment backoff never gets a chance to run that early.
func (d *Downloader) probeHead(ctx context.Context, rawURL string, res *ProbeResult) error {
	// Snapshot the fields a HEAD may fill in, so a success that is later
	// invalidated cannot leave half-updated metadata behind.
	size, etag, lastMod := res.Size, res.ETag, res.LastModified
	name, ctype, cdisp := res.SuggestedName, res.ContentType, res.ContentDisposition
	adv := res.AdvertisedRange

	err := d.doWithRetry(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header = d.baseHeaders()

		resp, err := d.client.Do(req)
		if err != nil {
			// A failed HEAD is not fatal by itself; the ranged GET still gets
			// its chance. Returning the error lets the retry helper decide
			// whether replaying is worthwhile.
			return err
		}
		defer drainAndClose(resp.Body, resp.ContentLength, 0)

		res.FinalURL = resp.Request.URL.String()

		switch {
		case resp.StatusCode == http.StatusMethodNotAllowed,
			resp.StatusCode == http.StatusNotImplemented,
			resp.StatusCode == http.StatusForbidden:
			return errHeadUnavailable
		case resp.StatusCode >= 400:
			return &statusError{resp.StatusCode, "HEAD " + rawURL}
		}

		res.AdvertisedRange = strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
		if resp.ContentLength >= 0 {
			res.Size = resp.ContentLength
		}
		res.ETag = resp.Header.Get("ETag")
		res.LastModified = resp.Header.Get("Last-Modified")
		res.ContentType = resp.Header.Get("Content-Type")
		res.ContentDisposition = resp.Header.Get("Content-Disposition")
		res.SuggestedName = suggestName(res.ContentDisposition, res.FinalURL)
		return nil
	})

	if err == nil {
		return nil
	}
	if errors.Is(err, errHeadUnavailable) {
		// Expected: leave everything the HEAD may have touched as it was.
		res.Size, res.ETag, res.LastModified = size, etag, lastMod
		res.SuggestedName, res.ContentType, res.ContentDisposition = name, ctype, cdisp
		res.AdvertisedRange = adv
		return nil
	}
	// Any other failure is left for probeRange to work around; Probe only gives
	// up when the ranged GET fails too.
	res.Size, res.ETag, res.LastModified = size, etag, lastMod
	res.SuggestedName, res.ContentType, res.ContentDisposition = name, ctype, cdisp
	res.AdvertisedRange = adv
	return err
}

// probeRange issues GET with "Range: bytes=0-0" and derives the truth about
// range support and total size from the response.
//
// The request is retried on transient failures, but the status-code handling is
// deliberately outside the retry loop: a 200 or a 416 is a *successful* probe
// that returns information, not an error to replay.
func (d *Downloader) probeRange(ctx context.Context, rawURL string, res *ProbeResult) error {
	resp, err := d.probeRangeFetch(ctx, rawURL)
	if err != nil {
		return err
	}
	if resp == nil {
		return errors.New("range probe produced no response")
	}
	// Only now is it safe to release the body: doing this inside the retry loop
	// would close the response the retry helper is about to read.
	defer drainAndClose(resp.Body, resp.ContentLength, 1)

	res.FinalURL = resp.Request.URL.String()
	if res.ETag == "" {
		res.ETag = resp.Header.Get("ETag")
	}
	if res.LastModified == "" {
		res.LastModified = resp.Header.Get("Last-Modified")
	}
	if res.ContentType == "" {
		res.ContentType = resp.Header.Get("Content-Type")
	}
	if res.ContentDisposition == "" {
		res.ContentDisposition = resp.Header.Get("Content-Disposition")
	}
	if res.SuggestedName == "" {
		res.SuggestedName = suggestName(res.ContentDisposition, res.FinalURL)
	}
	if !res.AdvertisedRange {
		res.AdvertisedRange = strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes")
	}

	switch resp.StatusCode {
	case http.StatusPartialContent:
		start, end, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			// A 206 without a parsable Content-Range is unusable; treat the
			// resource as unsegmentable rather than guess at offsets.
			res.RangeSupported = false
			res.Note = "206 response without a parsable Content-Range"
			return nil
		}
		if start != 0 {
			res.RangeSupported = false
			res.Note = fmt.Sprintf("206 response started at %d, want 0", start)
			return nil
		}
		if total >= 0 {
			res.Size = total
		}
		res.RangeSupported = true
		res.Note = fmt.Sprintf("verified with 206 Content-Range bytes %d-%d/%d", start, end, total)
		return nil

	case http.StatusOK:
		// The server ignored Range. Ranges are unsupported, whatever
		// Accept-Ranges claimed.
		res.RangeSupported = false
		if resp.ContentLength >= 0 {
			res.Size = resp.ContentLength
		}
		if res.AdvertisedRange {
			res.Note = "server advertised Accept-Ranges: bytes but ignored Range and answered 200"
		} else {
			res.Note = "server answered 200 to a ranged GET; no range support"
		}
		return nil

	case http.StatusRequestedRangeNotSatisfiable:
		// Usually means an empty resource, or a server that rejects any range.
		if total, ok := parseUnsatisfiedTotal(resp.Header.Get("Content-Range")); ok {
			res.Size = total
		}
		res.RangeSupported = false
		res.Note = "416 to a ranged GET"
		return nil

	default:
		return &statusError{resp.StatusCode, "GET with Range " + rawURL}
	}
}

// probeRangeFetch performs the ranged probe request, retrying transient
// failures. The returned body is the caller's to drain and close.
func (d *Downloader) probeRangeFetch(ctx context.Context, rawURL string) (*http.Response, error) {
	var out *http.Response

	err := d.doWithRetry(ctx, func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return err
		}
		req.Header = d.baseHeaders()
		req.Header.Set("Range", "bytes=0-0")

		resp, err := d.client.Do(req)
		if err != nil {
			return err
		}
		// A 5xx is transient and worth replaying; the body is released here
		// because this attempt is being discarded. Everything else is a real
		// answer and travels back to the caller untouched.
		if resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
			_ = resp.Body.Close()
			return &statusError{resp.StatusCode, "GET with Range " + rawURL}
		}
		out = resp
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// baseHeaders returns the headers applied to every request the engine makes.
func (d *Downloader) baseHeaders() http.Header {
	h := make(http.Header, len(d.opt.Headers)+2)
	for k, vs := range d.opt.Headers {
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	if h.Get("User-Agent") == "" {
		h.Set("User-Agent", d.userAgent)
	}
	if h.Get("Accept") == "" {
		h.Set("Accept", "*/*")
	}
	return h
}

// drainAndClose reads at most limit bytes from body before closing it.
//
// This exists for one reason: when a server ignores Range, the "probe"
// response body is the entire file. Draining it would turn a one-byte probe
// into a full download, so the body is abandoned and the connection is
// discarded instead.
func drainAndClose(body io.ReadCloser, contentLength, limit int64) {
	if limit > 0 {
		if contentLength < 0 || contentLength <= limit {
			_, _ = io.Copy(io.Discard, io.LimitReader(body, limit))
		}
	}
	_ = body.Close()
}

// parseContentRange parses "bytes START-END/TOTAL". TOTAL may be "*".
func parseContentRange(v string) (start, end, total int64, ok bool) {
	s := strings.TrimSpace(v)
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "bytes") {
		return 0, 0, 0, false
	}
	s = strings.TrimSpace(s[len("bytes"):])

	slash := strings.IndexByte(s, '/')
	if slash < 0 {
		return 0, 0, 0, false
	}
	rangePart := strings.TrimSpace(s[:slash])
	totalPart := strings.TrimSpace(s[slash+1:])

	dash := strings.IndexByte(rangePart, '-')
	if dash < 0 {
		return 0, 0, 0, false
	}
	start, err := strconv.ParseInt(strings.TrimSpace(rangePart[:dash]), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}
	end, err = strconv.ParseInt(strings.TrimSpace(rangePart[dash+1:]), 10, 64)
	if err != nil {
		return 0, 0, 0, false
	}

	if totalPart == "*" {
		total = -1
	} else {
		total, err = strconv.ParseInt(totalPart, 10, 64)
		if err != nil {
			return 0, 0, 0, false
		}
	}
	return start, end, total, true
}

// parseUnsatisfiedTotal parses the "bytes */TOTAL" form sent with a 416.
func parseUnsatisfiedTotal(v string) (int64, bool) {
	s := strings.TrimSpace(v)
	low := strings.ToLower(s)
	if !strings.HasPrefix(low, "bytes") {
		return 0, false
	}
	s = strings.TrimSpace(s[len("bytes"):])
	if !strings.HasPrefix(s, "*/") {
		return 0, false
	}
	total, err := strconv.ParseInt(strings.TrimSpace(s[2:]), 10, 64)
	if err != nil {
		return 0, false
	}
	return total, true
}

// suggestName picks a filename from Content-Disposition, then the URL path.
func suggestName(disposition, rawURL string) string {
	if name := filenameFromDisposition(disposition); name != "" {
		return name
	}
	return filenameFromURL(rawURL)
}

// filenameFromDisposition extracts a filename, honouring RFC 6266 filename*.
// Go's mime.ParseMediaType decodes the RFC 2231/5987 extended form into the
// plain "filename" parameter for us.
func filenameFromDisposition(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(v); err == nil {
		if fn := params["filename"]; fn != "" {
			return sanitizeFilename(fn)
		}
		return ""
	}
	// Some servers emit a bare "filename=foo.zip" with no disposition type,
	// which mime.ParseMediaType rejects. Scan for it manually.
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		if len(part) < len("filename=") {
			continue
		}
		if !strings.EqualFold(part[:len("filename=")], "filename=") {
			continue
		}
		val := strings.Trim(strings.TrimSpace(part[len("filename="):]), `"`)
		if val != "" {
			return sanitizeFilename(val)
		}
	}
	return ""
}

// filenameFromURL takes the last path segment of a URL.
func filenameFromURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	p := u.Path
	if p == "" || strings.HasSuffix(p, "/") {
		return ""
	}
	base := path.Base(p)
	if base == "." || base == "/" || base == "" {
		return ""
	}
	if dec, err := url.PathUnescape(base); err == nil {
		base = dec
	}
	return sanitizeFilename(base)
}

// sanitizeFilename makes a server-supplied name safe to join onto a directory.
//
// Server-supplied filenames are untrusted input: "..\\..\\Windows\\System32"
// or a name containing a path separator must not be able to steer the write
// outside the chosen directory.
//
// The base name is extracted *before* any character folding. Replacing the
// separators first would flatten `..\..\evil.exe` into a single entirely
// legal-looking name that still carried the traversal, which is exactly the
// failure this function exists to prevent.
func sanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Trim(name, `"'`)
	name = strings.ReplaceAll(name, `\`, "/")
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		// Keep only the final path component, discarding "..", drive letters
		// and every other directory reference along with it.
		name = name[i+1:]
	}

	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		switch {
		case r < 0x20 || r == 0x7f:
			// control characters
		case strings.ContainsRune(`<>:"|?*`, r):
			b.WriteRune('_')
		default:
			b.WriteRune(r)
		}
	}

	// Windows silently strips trailing dots and spaces, so "a. " and "a" would
	// collide with a sibling file.
	out := strings.TrimRight(b.String(), ". ")
	// A name made only of dots is a traversal artefact, not a name.
	if strings.Trim(out, ".") == "" {
		return ""
	}
	// Windows device names cannot be used as file names at all, with or
	// without an extension: NUL, NUL.txt and CON.tar.gz are all the device.
	if isWindowsDeviceName(out) {
		out = "_" + out
	}
	if len(out) > 180 {
		ext := path.Ext(out)
		if len(ext) >= len(out) {
			ext = ""
		}
		out = out[:180-len(ext)] + ext
	}
	return out
}

// isWindowsDeviceName reports whether a name refers to a reserved DOS device.
func isWindowsDeviceName(name string) bool {
	stem := name
	if dot := strings.IndexByte(stem, '.'); dot >= 0 {
		stem = stem[:dot]
	}
	switch strings.ToUpper(stem) {
	case "CON", "PRN", "AUX", "NUL",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9":
		return true
	}
	return false
}
