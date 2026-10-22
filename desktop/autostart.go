package main

import (
	"fmt"
	"os"
	"strings"
)

// Launch-at-login registration.
//
// The registry details live in autostart_windows.go (and the no-op stubs in
// autostart_other.go). This file owns the one decision the two share: what the
// Run value should say, and whether it already says it. Writing the value on
// every launch would be needless registry churn — and registry-change monitors
// would report the app every time it starts — so the sync is a
// read-compare-write.
//
// The entry point is SetLaunchAtLogin, called once from startup with the
// saved setting. The three backend functions and the executable path are
// variables, not direct calls, so a test can substitute an in-memory fake and
// the user's real Run key is never touched.

// autostartValueName is the Run value the app owns.
//
// A stable name rather than the executable's own name: if a future build renames
// the binary, the old entry is still found and updated instead of leaving a
// stale path behind.
var autostartValueName = "VKMDownload"

var (
	autostartRead   = readAutostartValue
	autostartWrite  = writeAutostartValue
	autostartDelete = deleteAutostartValue
	autostartExec   = os.Executable
)

// SetLaunchAtLogin makes the current user's Run entry match enabled.
//
// Enabled stores the quoted path of the running executable: Run entries may
// contain spaces, and an unquoted path with spaces is split by CreateProcess.
// It reads first and writes only when the stored value differs, so a normal
// launch leaves the registry alone and a moved executable repairs its entry.
//
// Disabled removes the value when it is there. An absent value is not an error:
// the setting and the registry are supposed to end up in the same state, and
// "it is already off" is that state. A read failure is returned rather than
// papered over — writing blindly could overwrite an entry the app could not
// inspect.
func SetLaunchAtLogin(enabled bool) error {
	if !enabled {
		have, present, err := autostartRead(autostartValueName)
		if err != nil {
			return err
		}
		if !present || have == "" {
			return nil
		}
		return autostartDelete(autostartValueName)
	}

	exe, err := autostartExec()
	if err != nil {
		return fmt.Errorf("无法确定程序路径: %w", err)
	}
	exe = strings.TrimSpace(exe)
	if exe == "" {
		return fmt.Errorf("无法确定程序路径")
	}
	want := `"` + exe + `"`

	have, present, err := autostartRead(autostartValueName)
	if err != nil {
		return err
	}
	if present && have == want {
		return nil
	}
	return autostartWrite(autostartValueName, want)
}
