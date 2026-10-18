//go:build windows

package engine

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"unsafe"
)

// TestSetHiddenPreservesOtherAttributes pins the read-modify-write contract:
// hiding and unhiding a file must not disturb attributes it does not own. A
// read-only file that came back writable after a download would be a data
// hazard, and an archive bit silently cleared would confuse backup tools.
func TestSetHiddenPreservesOtherAttributes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, _, err := procSetFileAttributesW.Call(uintptrOf(t, path), syscall.FILE_ATTRIBUTE_READONLY); ok == 0 {
		t.Fatalf("could not make the file read-only: %v", err)
	}

	hideFile(path)
	if !isHidden(path) {
		t.Error("file is not hidden after hideFile")
	}
	if attrs(t, path)&syscall.FILE_ATTRIBUTE_READONLY == 0 {
		t.Error("hideFile cleared the read-only attribute")
	}

	unhideFile(path)
	if isHidden(path) {
		t.Error("file is still hidden after unhideFile")
	}
	if attrs(t, path)&syscall.FILE_ATTRIBUTE_READONLY == 0 {
		t.Error("unhideFile cleared the read-only attribute")
	}

	// Cleanup must succeed, which it cannot while the file is read-only.
	_, _, _ = procSetFileAttributesW.Call(uintptrOf(t, path), syscall.FILE_ATTRIBUTE_NORMAL)
}

// TestSetHiddenOnMissingFileIsQuiet proves the best-effort contract: hiding a
// file that does not exist must neither panic nor change any state, because
// the sidecar save path re-hides files that a publish may have just removed.
func TestSetHiddenOnMissingFileIsQuiet(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "nope.part")
	hideFile(missing)
	unhideFile(missing)
	if isHidden(missing) {
		t.Error("isHidden reports true for a file that does not exist")
	}
}

func uintptrOf(t *testing.T, path string) uintptr {
	t.Helper()
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	return uintptr(unsafe.Pointer(ptr))
}

func attrs(t *testing.T, path string) uint32 {
	t.Helper()
	get, _, _ := procGetFileAttributesW.Call(uintptrOf(t, path))
	return uint32(get)
}
