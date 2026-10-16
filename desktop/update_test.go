package main

import (
	"errors"
	"net/http"
	"strings"
	"testing"
)

func TestIsNewerVersion(t *testing.T) {
	cases := []struct {
		current, tag string
		want         bool
	}{
		{"0.2.3", "v0.2.3", false},
		{"0.2.3", "0.2.3", false},
		{"0.2.2", "v0.2.3", true},
		{"0.2.3", "v0.2.4", true},
		{"0.2.3", "v0.3.0", true},
		{"0.2.3", "v1.0.0", true},
		{"0.2.3", "v0.2.2", false},
		{"0.2.3", "v0.2.30", true},
		{"0.2.3", "v0.2", false},   // shorter can never be newer
		{"0.2", "v0.2.1", true},    // longer wins only past a shared prefix
		{"0.2.3", "latest", false}, // unparsable is never newer
		{"", "v0.2.3", false},
		{"0.2.3", "", false},
		{"oops", "v0.2.3", false},
	}
	for _, c := range cases {
		if got := isNewerVersion(c.current, c.tag); got != c.want {
			t.Errorf("isNewerVersion(%q, %q) = %v, want %v", c.current, c.tag, got, c.want)
		}
	}
}

// errTransport fails every request: an offline machine.
type errTransport struct{ err error }

func (t errTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, t.err
}

// withOffline swaps the default transport for one that always fails and
// restores it afterwards. Callers must not run in parallel: the client is
// process-global.
func withOffline(t *testing.T) {
	t.Helper()
	old := http.DefaultClient.Transport
	http.DefaultClient.Transport = errTransport{err: errors.New("no network")}
	t.Cleanup(func() { http.DefaultClient.Transport = old })
}

func TestCheckUpdateOfflineReportsNoUpdate(t *testing.T) {
	withOffline(t)
	a := &App{}
	info, err := a.CheckUpdate()
	if err == nil {
		t.Fatal("CheckUpdate with no network = nil error, want an error")
	}
	if info.Available {
		t.Error("CheckUpdate with no network reports available, want silent no-update")
	}
}

func TestCheckUpdateOnceOfflineIsSilent(t *testing.T) {
	withOffline(t)
	var emitted []string
	a := &App{
		emit: func(name string, _ ...any) { emitted = append(emitted, name) },
	}
	// Must not panic, must not emit, must leave the updater state alone.
	a.checkUpdateOnce()
	if len(emitted) != 0 {
		t.Errorf("checkUpdateOnce emitted %v offline, want silence", emitted)
	}
	if st := a.UpdateProgress(); st != (UpdateStatus{}) {
		t.Errorf("checkUpdateOnce touched updater state offline: %+v", st)
	}
}

func TestOpenReleasePageRejectsForeignHosts(t *testing.T) {
	a := &App{}
	evil := []string{
		"https://evil.com/Vick-moo/VKMDownload/releases/tag/v0.2.5",
		"https://github.com.evil.com/Vick-moo/VKMDownload/releases/tag/v0.2.5",
		"https://github.com.evil.com.evil.com/x",
		"http://github.com/Vick-moo/VKMDownload/releases/tag/v0.2.5", // not https
		"https://github.com/ someone-else/VKMDownload/releases/tag/v0.2.5",
		"https://github.com/Vick-moo/OtherRepo/releases/tag/v0.2.5",
		"javascript:alert(1)",
		"",
		"   ",
	}
	for _, u := range evil {
		if err := a.OpenReleasePage(u); err == nil {
			t.Errorf("OpenReleasePage(%q) = nil, want rejection", u)
		} else if !strings.Contains(err.Error(), "发布页") {
			t.Errorf("OpenReleasePage(%q) error = %q, want the allowlist message", u, err)
		}
	}
}
