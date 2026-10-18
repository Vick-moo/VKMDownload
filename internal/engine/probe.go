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
	"unicode/utf8"
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
//
// A weak validator (ETag "W/...") is never returned: RFC 9110 forbids using it
// in If-Range, and the server would either reject the header or ignore it, so a
// resume built on one mixes bytes from two representations. Last-Modified is
// the standard fallback and is never the weak-syntax form.
func (p *ProbeResult) Validator() string {
	if v := strings.TrimSpace(p.ETag); v != "" && !isWeakValidator(v) {
		return v
	}
	return p.LastModified
}

// isWeakValidator reports whether v uses the weak entity-tag syntax, "W/"….
func isWeakValidator(v string) bool {
	v = strings.TrimSpace(v)
	if len(v) < 2 || v[0] != 'W' && v[0] != 'w' {
		return false
	}
	return v[1] == '/'
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

// ErrNeedsSignIn means the server answered with a sign-in page instead of the
// file, which is what a cloud-storage link to a private file does.
//
// It is separate from ErrNotAFile on purpose. Both end in an HTML body, but the
// advice differs completely: "this is a web page, find a direct link" sends
// someone hunting for a link that is already correct, when what is actually
// missing is their credentials.
var ErrNeedsSignIn = errors.New("这个文件需要登录才能下载")

// signInHosts are where a request lands when the server wants credentials
// rather than to serve the file.
var signInHosts = []string{
	"accounts.google.com",
	"login.live.com",
	"login.microsoftonline.com",
	"id.apple.com",
	"login.yahoo.com",
}

// needsSignIn reports whether the request finished at a sign-in page.
//
// It looks at where the request ended rather than at what came back: a sign-in
// page is HTML, but so is an error page, and only the host distinguishes them.
func needsSignIn(finalURL string) bool {
	if finalURL == "" {
		return false
	}
	u, err := url.Parse(finalURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	for _, signIn := range signInHosts {
		if host == signIn || strings.HasSuffix(host, "."+signIn) {
			return true
		}
	}
	return false
}

// notAFileError reports a probe result that describes a web page.
func notAFileError(res *ProbeResult) error {
	if needsSignIn(res.FinalURL) {
		return fmt.Errorf("%w (跳转到 %s)", ErrNeedsSignIn, hostOf(res.FinalURL))
	}
	if !looksLikeHTML(res.ContentType) {
		return nil
	}
	return fmt.Errorf("%w (Content-Type: %s)", ErrNotAFile, res.ContentType)
}

// hostOf returns just the host of a URL, for a message that names where the
// request was sent without pasting a very long sign-in URL into the UI.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
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
//
// It is deliberately unwrapped into the errNotRetryable family (see
// isRetryable): replaying a HEAD that was refused is pure waste. A server that
// answers 405 to HEAD answers 405 to the retry as well, and each retry also
// pays the backoff sleep, so a refusal that is an invitation to try the GET
// cost three extra HEADs and roughly 3.5 s before the same GET ran anyway.
var errHeadUnavailable = fmt.Errorf("%w: server declined HEAD", errNotRetryable)

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
			// The raw URL is left out on purpose: this error can reach the
			// user and the job file, and a signed link's query string is a
			// credential that must not be copied there.
			return &statusError{Code: resp.StatusCode, Detail: "HEAD"}
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
	// The GET is the transfer that will actually run, so its Content-Type wins
	// over whatever HEAD claimed. The two disagree often (a HEAD answered by a
	// generic handler that says text/html, or a GET that adds charset), and the
	// expensive question this field answers — "is this a web page instead of a
	// file?" — has to be answered about the bytes that will be stored.
	if ct := resp.Header.Get("Content-Type"); ct != "" {
		res.ContentType = ct
	}
	// A Content-Disposition learned here outranks a URL fallback assigned
	// earlier: probeHead fills SuggestedName from the URL token when HEAD
	// carries no disposition (observed: hosts that only send it on GET),
	// and without this the real filename is recorded yet never used.
	if cd := resp.Header.Get("Content-Disposition"); cd != "" {
		if res.ContentDisposition == "" {
			res.ContentDisposition = cd
		}
		if fn := filenameFromDisposition(cd); fn != "" {
			res.SuggestedName = fn
		}
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
		// A 206 whose Content-Range does not describe the requested one-byte
		// range is also a failure to honour Range, however confidently it was
		// labelled. Accepting it and taking Size from its total is what lets a
		// broken range server be planned as segmented, so every later block
		// mismatches and the download dies with an error that points at the
		// blocks rather than at the probe.
		if start != 0 || end != 0 {
			res.RangeSupported = false
			res.Note = fmt.Sprintf("206 claimed bytes %d-%d for a bytes=0-0 request", start, end)
			return nil
		}
		if total < 0 {
			// A 206 that does not disclose the total cannot be planned or
			// validated, whatever the response claimed about ranges.
			res.RangeSupported = false
			res.Note = "206 Content-Range did not disclose the total size"
			return nil
		}
		res.Size = total
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
		// The URL is deliberately left out: this error reaches the user and the
		// job file, and a signed link's query string is a credential.
		return &statusError{Code: resp.StatusCode, Detail: "GET with Range"}
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
		// answer and travels back to the caller untouched. The raw URL is
		// deliberately absent from the error: it can surface in the UI and the
		// job file, where a signed link's query string is a credential.
		if resp.StatusCode >= 500 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<12))
			_ = resp.Body.Close()
			return &statusError{Code: resp.StatusCode, Detail: "GET with Range"}
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
	// which mime.ParseMediaType rejects. Scan for it manually, preferring the
	// RFC 5987/6266 extended form ("filename*=UTF-8''..."), whose percent
	// escapes hold the real name for anything non-ASCII.
	var plain, extended string
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		before, _, hadEq := strings.Cut(part, "=")
		if !hadEq {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(before))
		var val string
		if key == "filename*" {
			val = strings.TrimSpace(part[len(before)+1:])
		} else {
			val = strings.Trim(strings.TrimSpace(part[len(before)+1:]), `"'`)
		}
		switch key {
		case "filename*":
			if dec := decodeExtValue(val); dec != "" && extended == "" {
				extended = sanitizeFilename(dec)
			}
		case "filename":
			if plain == "" {
				plain = sanitizeFilename(val)
			}
		}
	}
	if extended != "" {
		return extended
	}
	return plain
}

// decodeExtValue decodes an RFC 5987 ext-value: charset'language'percent-escaped.
// Only the UTF-8 and ISO-8859-1 charsets are honoured; anything else is left to
// the plain filename= parameter, which is what a browser does too.
func decodeExtValue(raw string) string {
	raw = strings.Trim(strings.TrimSpace(raw), `"'`)
	first := strings.IndexByte(raw, '\'')
	if first < 0 {
		return ""
	}
	second := strings.IndexByte(raw[first+1:], '\'')
	if second < 0 {
		return ""
	}
	charset := strings.ToLower(raw[:first])
	encoded := raw[first+1+second+1:]

	switch charset {
	case "utf-8", "utf8":
		dec, err := url.PathUnescape(encoded)
		if err != nil {
			return ""
		}
		return dec
	case "iso-8859-1", "latin1", "":
		// Each %XX is one Latin-1 byte; anything not escaped is literal ASCII.
		var b []byte
		for i := 0; i < len(encoded); i++ {
			if encoded[i] == '%' && i+2 < len(encoded) {
				if v, err := strconv.ParseUint(encoded[i+1:i+3], 16, 8); err == nil {
					b = append(b, byte(v))
					i += 2
					continue
				}
			}
			b = append(b, encoded[i])
		}
		out := make([]rune, len(b))
		for i, c := range b {
			out[i] = rune(c)
		}
		return string(out)
	default:
		return ""
	}
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
	// The cap is applied in characters, not bytes: slicing a UTF-8 string at
	// byte 180 used to cut a multi-byte character in half and leave a torn
	// sequence in the name.
	if utf8.RuneCountInString(out) > maxNameRunes {
		ext := path.Ext(out)
		if utf8.RuneCountInString(ext) >= maxNameRunes {
			ext = ""
		}
		runes := []rune(out)
		keep := maxNameRunes - utf8.RuneCountInString(ext)
		out = string(runes[:keep]) + ext
	}
	// Windows limits a file name to 255 UTF-16 units. Counting the UTF-8 form
	// in bytes is a conservative bound (UTF-8 is never shorter than UTF-16
	// units / 3), so staying under maxNameBytes keeps the name legal there
	// while never splitting a character.
	if len(out) > maxNameBytes {
		ext := path.Ext(out)
		runes := []rune(out)
		keep := len(runes) - utf8.RuneCountInString(ext)
		for keep > 1 && len(string(runes[:keep])+ext) > maxNameBytes {
			keep--
		}
		out = string(runes[:keep]) + ext
	}
	return out
}

// maxNameRunes is the character cap for a server-supplied name; maxNameBytes
// bounds the same name in UTF-8 bytes, a conservative bound on NTFS's 255
// UTF-16 unit limit.
const (
	maxNameRunes = 180
	maxNameBytes = 240
)

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
