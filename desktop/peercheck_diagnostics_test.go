package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestPeerDiagnosticsRingIsBounded covers the ring: it keeps the newest entries
// and drops the oldest, newest first.
func TestPeerDiagnosticsRingIsBounded(t *testing.T) {
	app := newTestApp(t)
	for i := 0; i < peerRejectionKeep+5; i++ {
		app.recordPeerRejection("r"+strconv.Itoa(i), "")
	}

	got := app.PeerDiagnostics()
	if len(got) != peerRejectionKeep {
		t.Fatalf("ring length = %d, want %d", len(got), peerRejectionKeep)
	}
	if got[0].Reason != "r"+strconv.Itoa(peerRejectionKeep+4) {
		t.Errorf("newest = %q, want the last recorded entry", got[0].Reason)
	}
	if last := got[len(got)-1].Reason; last != "r5" {
		t.Errorf("oldest kept = %q, want r5 (r0-r4 dropped)", last)
	}
	if got[0].Time.IsZero() {
		t.Error("entry has no timestamp")
	}
}

// TestPeerDiagnosticsRecordBasenameOnly pins the privacy rule: the full path
// names the user's install and is never kept.
func TestPeerDiagnosticsRecordBasenameOnly(t *testing.T) {
	app := newTestApp(t)
	app.recordPeerRejection(peerReasonNotAllowlisted, `C:\Temp\someone\curl.exe`)

	got := app.PeerDiagnostics()
	if len(got) != 1 {
		t.Fatalf("entries = %d, want 1", len(got))
	}
	if got[0].Exe != "curl.exe" {
		t.Errorf("Exe = %q, want the basename curl.exe", got[0].Exe)
	}
	if strings.Contains(got[0].Exe, `\`) || strings.Contains(got[0].Exe, "/") {
		t.Errorf("Exe = %q still looks like a path", got[0].Exe)
	}
	if got[0].Reason != peerReasonNotAllowlisted {
		t.Errorf("Reason = %q, want %q", got[0].Reason, peerReasonNotAllowlisted)
	}
}

// TestPeerDiagnosticsEmptyIsAnArray documents the shape the front-end receives:
// no refusals is an empty list, not null.
func TestPeerDiagnosticsEmptyIsAnArray(t *testing.T) {
	app := newTestApp(t)
	if got := app.PeerDiagnostics(); got == nil || len(got) != 0 {
		t.Errorf("PeerDiagnostics() = %#v, want a non-nil empty slice", got)
	}
}

// TestPeerDiagnosticsReasons walks every refusal branch and asserts the stable
// reason value recorded for it, plus the exe name when one was readable.
func TestPeerDiagnosticsReasons(t *testing.T) {
	cases := []struct {
		name    string
		req     *http.Request
		stub    func(t *testing.T)
		want    string
		wantExe string
	}{
		{
			name: "unparseable remote address",
			req:  &http.Request{RemoteAddr: "not-an-address"},
			want: peerReasonBadAddr,
		},
		{
			name: "peer is not loopback",
			req:  &http.Request{RemoteAddr: "8.8.8.8:1234"},
			want: peerReasonNotLoopback,
		},
		{
			name: "unparseable port",
			req:  &http.Request{RemoteAddr: "127.0.0.1:not-a-port"},
			want: peerReasonBadPort,
		},
		{
			name: "socket owner cannot be found",
			stub: func(t *testing.T) { stubPeer(t, "", false, errPeerCheckUnsupported) },
			want: peerReasonNoOwner,
		},
		{
			name: "process has no readable image",
			stub: func(t *testing.T) { stubPeer(t, "", false, nil) },
			want: peerReasonNoImage,
		},
		{
			name:    "image is not an allowed browser",
			stub:    func(t *testing.T) { stubPeer(t, `C:\fakepair-vkm\curl.exe`, true, nil) },
			want:    peerReasonNotAllowlisted,
			wantExe: "curl.exe",
		},
		{
			name: "image is outside a browser install root",
			stub: func(t *testing.T) {
				stubPeer(t, `C:\Temp\chrome.exe`, true, nil)
				browserRootCheck = func(string) bool { return false }
			},
			want:    peerReasonNotInInstallRoot,
			wantExe: "chrome.exe",
		},
		{
			name:    "signature is not trusted",
			stub:    func(t *testing.T) { stubPeer(t, `C:\fakepair-vkm\brave.exe`, false, nil) },
			want:    peerReasonBadSignature,
			wantExe: "brave.exe",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			app := newTestApp(t)
			if c.stub != nil {
				c.stub(t)
			}
			req := c.req
			if req == nil {
				req = peerReq()
			}

			if ok, _ := app.checkPeerProcess(req); ok {
				t.Fatal("checkPeerProcess admitted a caller it must refuse")
			}

			got := app.PeerDiagnostics()
			if len(got) != 1 {
				t.Fatalf("diagnostics = %+v, want exactly one entry", got)
			}
			if got[0].Reason != c.want {
				t.Errorf("Reason = %q, want %q", got[0].Reason, c.want)
			}
			if got[0].Exe != c.wantExe {
				t.Errorf("Exe = %q, want %q", got[0].Exe, c.wantExe)
			}
		})
	}
}

// TestPeerDiagnosticsStayEmptyOnAdmission is the other half: the ring is a record
// of refusals, so an admitted browser adds nothing.
func TestPeerDiagnosticsStayEmptyOnAdmission(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, `C:\fakepair-vkm\brave.exe`, true, nil)

	if ok, detail := app.checkPeerProcess(peerReq()); !ok {
		t.Fatalf("signed brave.exe was refused (%q)", detail)
	}
	if got := app.PeerDiagnostics(); len(got) != 0 {
		t.Errorf("an admitted caller was recorded as a refusal: %+v", got)
	}
}

// TestPeerDiagnosticsDoNotChangeTheVerdict pins the "diagnostic only" promise: a
// refused caller is still refused with the same exe detail as before.
func TestPeerDiagnosticsDoNotChangeTheVerdict(t *testing.T) {
	app := newTestApp(t)
	stubPeer(t, `C:\fakepair-vkm\curl.exe`, true, nil)

	ok, exe := app.checkPeerProcess(peerReq())
	if ok || !strings.HasSuffix(exe, "curl.exe") {
		t.Errorf("checkPeerProcess = %v %q, want false with the rejected exe", ok, exe)
	}
	// And the error the guard reports still names it.
	if got := exeSuffix(exe); got != " (curl.exe)" {
		t.Errorf("exeSuffix = %q", got)
	}
}
