package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPairingInfoWithoutAKey covers a fresh install: nothing is paired, there is
// no fingerprint to show, and the path is still reported so the settings page can
// offer "open the folder".
func TestPairingInfoWithoutAKey(t *testing.T) {
	app := newTestApp(t)

	st := app.PairingInfo()
	if st.Paired {
		t.Error("Paired = true without a pair.key")
	}
	if st.Fingerprint != "" {
		t.Errorf("Fingerprint = %q, want empty", st.Fingerprint)
	}
	if want := filepath.Join(app.dataDir(), pairKeyFile); st.KeyPath != want {
		t.Errorf("KeyPath = %q, want %q", st.KeyPath, want)
	}
	if st.Port != 0 {
		t.Errorf("Port = %d, want 0 while the endpoint is not running", st.Port)
	}
}

// TestPairingInfoShowsOnlyAFingerprint pins the privacy rule: the status carries
// a short hash of the key, never the key itself.
func TestPairingInfoShowsOnlyAFingerprint(t *testing.T) {
	app := newTestApp(t)
	key, err := app.pairKeyForThisInstall()
	if err != nil || key == "" {
		t.Fatalf("pairKeyForThisInstall: %q, %v", key, err)
	}

	st := app.PairingInfo()
	if !st.Paired {
		t.Fatal("Paired = false although pair.key was just written")
	}
	if len(st.Fingerprint) != 16 {
		t.Errorf("Fingerprint = %q, want 16 hex characters", st.Fingerprint)
	}
	if want := keyFingerprint(key); st.Fingerprint != want {
		t.Errorf("Fingerprint = %q, want %q", st.Fingerprint, want)
	}
	// The key is 64 hex characters; the fingerprint is a different string and
	// nothing in the status may contain the key.
	for _, field := range []string{st.Fingerprint, st.KeyPath, fmt.Sprintf("%+v", st)} {
		if strings.Contains(field, key) {
			t.Fatalf("the status leaks the pairing key: %q", field)
		}
	}
}

// TestPairingInfoReportsTheListenPort covers the field the extension-facing UI
// shows: the port actually bound, read from the same place /ping reports.
func TestPairingInfoReportsTheListenPort(t *testing.T) {
	app := newTestApp(t)
	startAPI(t, app)

	st := app.PairingInfo()
	if st.Port == 0 {
		t.Fatal("Port = 0 while the local API is listening")
	}
	if st.Port != app.apiPort() {
		t.Errorf("Port = %d, apiPort() = %d", st.Port, app.apiPort())
	}
}

// TestResetPairingIsIdempotent covers the only unpair action there is.
//
// The key is per install, not per browser, so reset is all-or-nothing; a second
// call must succeed (the requested end state already holds), and the in-memory
// cache must be dropped so the next handover generates a genuinely new key.
func TestResetPairingIsIdempotent(t *testing.T) {
	app := newTestApp(t)
	first, err := app.pairKeyForThisInstall()
	if err != nil || first == "" {
		t.Fatalf("pairKeyForThisInstall: %q, %v", first, err)
	}

	if err := app.ResetPairing(); err != nil {
		t.Fatalf("ResetPairing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(app.dataDir(), pairKeyFile)); !os.IsNotExist(err) {
		t.Errorf("pair.key still exists after reset: %v", err)
	}
	if st := app.PairingInfo(); st.Paired {
		t.Error("Paired = true after reset")
	}

	// Idempotent: no key is not an error.
	if err := app.ResetPairing(); err != nil {
		t.Errorf("a second ResetPairing failed: %v", err)
	}

	// The cache was dropped: the next caller gets a new key, not the deleted one.
	second, err := app.pairKeyForThisInstall()
	if err != nil || second == "" {
		t.Fatalf("pairKeyForThisInstall after reset: %q, %v", second, err)
	}
	if second == first {
		t.Error("the deleted key was served from the in-memory cache")
	}
	if st := app.PairingInfo(); !st.Paired || st.Fingerprint != keyFingerprint(second) {
		t.Errorf("status after re-pair = %+v, want it to describe the new key", st)
	}
}
