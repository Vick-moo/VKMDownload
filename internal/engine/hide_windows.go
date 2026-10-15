//go:build windows

package engine

import (
	"syscall"
	"unsafe"
)

const fileAttributeHidden = 0x2

var (
	modKernel32            = syscall.NewLazyDLL("kernel32.dll")
	procSetFileAttributesW = modKernel32.NewProc("SetFileAttributesW")
	procGetFileAttributesW = modKernel32.NewProc("GetFileAttributesW")
)

// setHidden flips the hidden attribute on path, leaving every other attribute
// (read-only, archive, …) exactly as it was. Best effort: hiding a helper
// file must never fail a download.
func setHidden(path string, hide bool) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return
	}
	get, _, _ := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(ptr)))
	if get == ^uintptr(0) {
		return
	}
	attrs := uint32(get)
	if hide {
		attrs |= fileAttributeHidden
	} else {
		attrs &^= fileAttributeHidden
	}
	_, _, _ = procSetFileAttributesW.Call(
		uintptr(unsafe.Pointer(ptr)),
		uintptr(attrs),
	)
}

// hideFile marks a helper file (.part, sidecar) hidden so the download folder
// shows only finished files, the way system files stay out of the way.
func hideFile(path string) { setHidden(path, true) }

// unhideFile clears the hidden flag. Rename preserves attributes on Windows,
// so publishing must call this or the finished download stays invisible.
func unhideFile(path string) { setHidden(path, false) }

// isHidden reports the hidden flag, for tests.
func isHidden(path string) bool {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return false
	}
	get, _, _ := procGetFileAttributesW.Call(uintptr(unsafe.Pointer(ptr)))
	if get == ^uintptr(0) {
		return false
	}
	return uint32(get)&fileAttributeHidden != 0
}
