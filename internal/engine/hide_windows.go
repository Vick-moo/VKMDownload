//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

const fileAttributeHidden = 0x2

// invalidFileAttributes is what GetFileAttributesW returns on failure.
//
// It is genuinely 0xFFFFFFFF, not ^uintptr(0): on 64-bit builds the latter is
// 0xFFFFFFFFFFFFFFFF, so the comparison never matched, a missing file's
// "failure" value was read as a hidden flag, and it was then written back to
// SetFileAttributesW as if it were the real attribute set.
const invalidFileAttributes = uintptr(0xFFFFFFFF)

var (
	modKernel32            = syscall.NewLazyDLL("kernel32.dll")
	procSetFileAttributesW = modKernel32.NewProc("SetFileAttributesW")
	procGetFileAttributesW = modKernel32.NewProc("GetFileAttributesW")
)

// setHidden flips the hidden attribute on path, leaving every other attribute
// (read-only, archive, …) exactly as it was. Best effort: hiding a helper
// file must never fail a download.
//
// Read-modify-write is not atomic, so a concurrent change by another process
// between the two calls can be lost; that is harmless here because the only
// attribute written is the hidden bit, which no download path depends on for
// correctness. Failures are logged rather than swallowed: a helper file that
// stays visible in Explorer looks like a downloaded file to the user.
func setHidden(path string, hide bool) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		debugf("hide: bad path %q: %v", path, err)
		return
	}
	get, _, getErr := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(ptr)))
	if get == invalidFileAttributes {
		// The usual cause is a file that does not exist (yet) or was just
		// removed; that is expected and not worth a line per save.
		if hide {
			return
		}
		debugf("hide: GetFileAttributesW(%q) failed: %v", path, getErr)
		return
	}
	attrs := uint32(get)
	if hide {
		attrs |= fileAttributeHidden
	} else {
		attrs &^= fileAttributeHidden
	}
	if ok, _, setErr := procSetFileAttributesW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(attrs),
	); ok == 0 {
		debugf("hide: SetFileAttributesW(%q, hidden=%v) failed: %v", path, hide, setErr)
	}
}

// hideFile marks a helper file (.part, sidecar) hidden so the download folder
// shows only finished files, the way system files stay out of the way.
func hideFile(path string) { setHidden(path, true) }

// unhideFile clears the hidden flag. Rename preserves attributes on Windows,
// so publishing must call this or the finished download stays invisible.
func unhideFile(path string) { setHidden(path, false) }

// isHidden reports the hidden flag, for tests.
//
// The failure return of GetFileAttributesW is 0xFFFFFFFF, which also has the
// hidden bit set: reading the flag before checking for failure answered "yes,
// hidden" for every file that does not exist.
func isHidden(path string) bool {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	get, _, _ := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(ptr)))
	if get == invalidFileAttributes {
		return false
	}
	return uint32(get)&fileAttributeHidden != 0
}
