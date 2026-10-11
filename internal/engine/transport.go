package engine

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// TransportOptions configures the HTTP transport shared by probe requests and
// every segment worker.
type TransportOptions struct {
	// ProxyURL, when non-empty, routes every request through that proxy.
	// Empty means honour HTTP_PROXY / HTTPS_PROXY / NO_PROXY.
	ProxyURL string

	DialTimeout           time.Duration
	KeepAlive             time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	// MaxIdlePerHost bounds pooled idle connections per host. It should be at
	// least the segment count, otherwise a worker that finishes its segment
	// cannot hand its connection back and the next worker pays a fresh
	// handshake.
	MaxIdlePerHost int
	// InsecureSkipVerify disables certificate verification. Debugging only.
	InsecureSkipVerify bool
}

// DefaultTransportOptions returns the production defaults.
func DefaultTransportOptions() TransportOptions {
	return TransportOptions{
		DialTimeout:           20 * time.Second,
		KeepAlive:             30 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
		MaxIdlePerHost:        64,
	}
}

// NewTransport builds an http.Transport pinned to HTTP/1.1.
//
// Pinning is not a stylistic choice, it is the precondition for segmenting to
// do anything at all. HTTP/2 multiplexes every request for a host onto a
// single TCP connection, so a 16-segment download would push all 16 range
// requests through one TCP flow and immediately run back into that flow's
// congestion window: the apparent parallelism would be cosmetic, and worse, a
// server that throttles per connection would throttle all 16 "parallel"
// streams together.
//
// Over HTTP/1.1 a connection carries exactly one in-flight request, so N
// concurrent range requests necessarily occupy N distinct TCP connections.
//
// Two settings matter:
//
//   - TLSNextProto must be a non-nil empty map. That is what actually removes
//     the "h2" entry from the ALPN protocol list; ForceAttemptHTTP2 alone does
//     not fully disable HTTP/2 on every Go version.
//   - DisableCompression stops the transport from injecting
//     Accept-Encoding: gzip and transparently decompressing. Byte ranges and
//     Content-Length are only meaningful against the raw representation, so a
//     transparently decoded body would silently corrupt every offset.
func NewTransport(opt TransportOptions) (*http.Transport, error) {
	if opt.DialTimeout <= 0 {
		opt.DialTimeout = 20 * time.Second
	}
	if opt.KeepAlive <= 0 {
		opt.KeepAlive = 30 * time.Second
	}
	if opt.TLSHandshakeTimeout <= 0 {
		opt.TLSHandshakeTimeout = 15 * time.Second
	}
	if opt.ResponseHeaderTimeout <= 0 {
		opt.ResponseHeaderTimeout = 60 * time.Second
	}
	if opt.MaxIdlePerHost <= 0 {
		opt.MaxIdlePerHost = 64
	}

	proxyFn := http.ProxyFromEnvironment
	if opt.ProxyURL != "" {
		u, err := url.Parse(opt.ProxyURL)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL %q: %w", opt.ProxyURL, err)
		}
		if u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("invalid proxy URL %q: need scheme://host:port", opt.ProxyURL)
		}
		proxyFn = http.ProxyURL(u)
	}

	tr := &http.Transport{
		Proxy: proxyFn,
		DialContext: (&net.Dialer{
			Timeout:   opt.DialTimeout,
			KeepAlive: opt.KeepAlive,
		}).DialContext,

		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableCompression:  true,
		MaxIdleConns:        opt.MaxIdlePerHost * 2,
		MaxIdleConnsPerHost: opt.MaxIdlePerHost,
		MaxConnsPerHost:     0, // unlimited: the worker count already bounds this
		IdleConnTimeout:     90 * time.Second,

		TLSHandshakeTimeout:   opt.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opt.ResponseHeaderTimeout,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if opt.InsecureSkipVerify {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return tr, nil
}
