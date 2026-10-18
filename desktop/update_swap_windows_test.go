//go:build windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestHiddenCmdSuppressesTheConsole pins the flag that stops the updater's
// helper — and the shutdown command, and "cmd /c start" — from flashing a black
// console window. HideWindow on its own is not enough: Windows still creates
// the console and merely hides it, which is the flash users actually see.
func TestHiddenCmdSuppressesTheConsole(t *testing.T) {
	cmd := hiddenCmd("cmd", "/c", "exit", "0")
	if cmd.SysProcAttr == nil {
		t.Fatal("hiddenCmd left SysProcAttr nil; a console window would appear")
	}
	if cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Errorf("CreationFlags = %#x, want CREATE_NO_WINDOW (%#x) set",
			cmd.SysProcAttr.CreationFlags, createNoWindow)
	}
	if !cmd.SysProcAttr.HideWindow {
		t.Error("HideWindow is not set")
	}
}

// errSharingViolation is ERROR_SHARING_VIOLATION (winerror.h). The syscall
// package does not export it, and a named constant reads better than a bare 32
// in the assertion below.
const errSharingViolation = syscall.Errno(32)

// TestReplaceFileAgainstARealLock is the one test here that uses the real
// kernel path rather than a fake: a file opened without FILE_SHARE_DELETE is
// exactly what an antivirus scanner, or the loader holding a running image,
// looks like to MoveFileEx. It is the case the retry loop exists for, and it is
// the case the old script turned into a lost program.
//
// The two files are ordinary files in t.TempDir(); nothing near the real
// executable is touched.
func TestReplaceFileAgainstARealLock(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "VKMDownload.exe")
	src := filepath.Join(dir, "VKMDownload.exe.new")
	if err := os.WriteFile(dst, []byte("OLD-VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("NEW-VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}

	path16, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	// Read access, sharing read only: no FILE_SHARE_DELETE, so the target
	// cannot be replaced while this handle is open.
	h, err := syscall.CreateFile(path16, syscall.GENERIC_READ,
		syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		t.Fatalf("open the target without FILE_SHARE_DELETE: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = syscall.CloseHandle(h)
		}
	}()

	sys := realUpdateSys{logPath: filepath.Join(dir, "update.log")}
	replaceErr := sys.replaceFile(src, dst)
	if replaceErr == nil {
		t.Fatal("replaceFile succeeded against a target locked without FILE_SHARE_DELETE")
	}
	// The failure must be the retryable one, not something that means the
	// update is doomed. The errno is wrapped rather than formatted into the
	// message, so this does not depend on the system language.
	if !errors.Is(replaceErr, errSharingViolation) &&
		!errors.Is(replaceErr, syscall.ERROR_ACCESS_DENIED) {
		t.Errorf("replaceFile error = %v, want a sharing violation", replaceErr)
	}
	if got, _ := os.ReadFile(dst); string(got) != "OLD-VERSION" {
		t.Errorf("the target changed despite the failure: %q", got)
	}
	if got, _ := os.ReadFile(src); string(got) != "NEW-VERSION" {
		t.Errorf("the source was consumed despite the failure: %q", got)
	}

	// Once the lock is gone the same call goes through, in one step.
	if err := syscall.CloseHandle(h); err != nil {
		t.Fatalf("close the target: %v", err)
	}
	closed = true

	if err := sys.replaceFile(src, dst); err != nil {
		t.Fatalf("replaceFile after the lock was released: %v", err)
	}
	if got, _ := os.ReadFile(dst); string(got) != "NEW-VERSION" {
		t.Errorf("target content = %q, want the replacement", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the source still exists after a successful replace")
	}
}

// TestRealHardLinkKeepsTheOldBytes pins the backup's contract: after a hard
// link the two names are the same file, so nothing can half-copy it.
func TestRealHardLinkKeepsTheOldBytes(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "VKMDownload.exe")
	bak := filepath.Join(dir, "VKMDownload.exe.bak")
	if err := os.WriteFile(exe, []byte("OLD-VERSION"), 0o755); err != nil {
		t.Fatal(err)
	}
	sys := realUpdateSys{logPath: filepath.Join(dir, "update.log")}
	if err := backupExe(sys, exe, bak); err != nil {
		t.Fatalf("backupExe: %v", err)
	}
	if got, _ := os.ReadFile(bak); string(got) != "OLD-VERSION" {
		t.Errorf("backup content = %q", got)
	}
	if sys.size(exe) != sys.size(bak) {
		t.Error("the backup and the original differ in size")
	}
}
