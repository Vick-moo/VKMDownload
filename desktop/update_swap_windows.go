//go:build windows

package main

import (
	"fmt"
	"os/exec"
	"syscall"
	"time"
	"unsafe"
)

// createNoWindow is CREATE_NO_WINDOW: the process gets no console at all.
// HideWindow alone still makes Windows create one and merely hides it, which is
// why the console window used to flash on screen during an update.
const createNoWindow = 0x08000000

var (
	procMoveFileExW         = kernel32.NewProc("MoveFileExW")
	procCreateHardLinkW     = kernel32.NewProc("CreateHardLinkW")
	procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")
	procTerminateProcess    = kernel32.NewProc("TerminateProcess")
	procGetDiskFreeSpaceExW = kernel32.NewProc("GetDiskFreeSpaceExW")
	procBeep                = kernel32.NewProc("Beep")
	user32                  = syscall.NewLazyDLL("user32.dll")
	procEnumWindows         = user32.NewProc("EnumWindows")
	procGetWindowThreadPid  = user32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible     = user32.NewProc("IsWindowVisible")
)

const (
	movefileReplaceExisting = 0x1
	movefileWriteThrough    = 0x8

	synchronize      = 0x00100000
	processTerminate = 0x0001
	waitObject0      = 0x00000000
	waitTimeout      = 0x00000102
)

// hiddenCmd builds a command that runs with no console window. Every helper
// process this app launches (the updater, the shutdown command, "cmd /c start")
// goes through here: a console application started without it flashes a black
// window on screen, which is the single most-reported symptom of this update
// path.
func hiddenCmd(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createNoWindow,
	}
	return cmd
}

// hardLink creates a second name for the same file. Same volume only; the
// staged and live executables always live in one directory, so this is the
// normal path and the copy is the exception.
func (realUpdateSys) hardLink(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	r1, _, errno := procCreateHardLinkW.Call(
		uintptr(unsafe.Pointer(to)), uintptr(unsafe.Pointer(from)), 0)
	if r1 == 0 {
		return fmt.Errorf("CreateHardLink(%s): %w", dst, errno)
	}
	return nil
}

// replaceFile is the atomic swap: MoveFileEx either replaces the destination in
// one step or leaves it exactly as it was. There is no interval in which the
// destination path does not exist, which is the property the old
// rename-then-move script lacked.
//
// MOVEFILE_WRITE_THROUGH asks the filesystem to flush before returning. It is a
// request, not a guarantee: NTFS honours it for the rename itself, but it says
// nothing about data the caller wrote earlier through another handle — which is
// why the staged file is fsynced by its writer before the swap, and why the
// comment here does not claim the call makes the file durable. GetLastError is
// folded into the error because the difference between "sharing violation,
// retry" and "access denied, stop" is otherwise invisible.
func (s realUpdateSys) replaceFile(src, dst string) error {
	from, err := syscall.UTF16PtrFromString(src)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dst)
	if err != nil {
		return err
	}
	r1, _, errno := procMoveFileExW.Call(
		uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)),
		uintptr(movefileReplaceExisting|movefileWriteThrough))
	if r1 == 0 {
		return fmt.Errorf("MoveFileEx(%s -> %s): %w", src, dst, errno)
	}
	return nil
}

// startProcess launches a detached process without a console window and hands
// back its pid. The child outlives the caller on purpose: the updater helper
// must keep running after this process exits.
func (realUpdateSys) startProcess(exe string, args ...string) (int, error) {
	cmd := hiddenCmd(exe, args...)
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	pid := cmd.Process.Pid
	// Release, not Wait: this process is about to exit, and the helper must
	// not be killed with it.
	_ = cmd.Process.Release()
	return pid, nil
}

// processAlive reports whether pid is still running. A pid that cannot be
// opened at all is gone (or belongs to another user, which from here is the
// same thing).
func (realUpdateSys) processAlive(pid int) bool {
	h, err := openProcess(pid, synchronize)
	if err != nil {
		return false
	}
	defer procCloseHandle.Call(h)
	r1, _, _ := procWaitForSingleObject.Call(h, 0)
	return r1 == waitTimeout
}

// processHasWindow reports whether pid owns a visible top-level window. It is
// the second opinion used when the handshake never arrives: a process that is
// alive *and* on screen is running the new build.
func (realUpdateSys) processHasWindow(pid int) bool {
	found := false
	cb := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		procGetWindowThreadPid.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) != pid {
			return 1 // continue
		}
		if vis, _, _ := procIsWindowVisible.Call(hwnd); vis != 0 {
			found = true
			return 0 // stop
		}
		return 1
	})
	procEnumWindows.Call(cb, 0)
	return found
}

// waitProcess blocks until pid exits or the timeout passes.
func (s realUpdateSys) waitProcess(pid int, timeout time.Duration) error {
	h, err := openProcess(pid, synchronize)
	if err != nil {
		// The process went away between the liveness check and this call; that
		// is the outcome we wanted.
		if !s.processAlive(pid) {
			return nil
		}
		return fmt.Errorf("等待进程 %d 退出: %w", pid, err)
	}
	defer procCloseHandle.Call(h)
	r1, _, _ := procWaitForSingleObject.Call(h, uintptr(timeout/time.Millisecond))
	switch r1 {
	case waitObject0:
		return nil
	case waitTimeout:
		return fmt.Errorf("进程 %d 在 %s 内没有退出", pid, timeout)
	default:
		return fmt.Errorf("WaitForSingleObject(%d) = %#x", pid, r1)
	}
}

// killProcess terminates one specific pid. The helper passes the pid it started
// itself, never a pid looked up by name, so a copy the user launched by hand is
// never the victim.
func (realUpdateSys) killProcess(pid int) error {
	h, err := openProcess(pid, processTerminate|synchronize)
	if err != nil {
		return err
	}
	defer procCloseHandle.Call(h)
	r1, _, errno := procTerminateProcess.Call(h, 1)
	if r1 == 0 {
		return fmt.Errorf("TerminateProcess(%d): %v", pid, errno)
	}
	return nil
}

func openProcess(pid, access int) (uintptr, error) {
	h, _, errno := procOpenProcess.Call(uintptr(access), 0, uintptr(pid))
	if h == 0 {
		return 0, fmt.Errorf("OpenProcess(%d): %v", pid, errno)
	}
	return h, nil
}

// freeSpaceBytes reports the space available to the user on the volume holding
// path, or 0 when it cannot be determined.
func freeSpaceBytes(path string) int64 {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0
	}
	var free, total, avail uint64
	r1, _, _ := procGetDiskFreeSpaceExW.Call(
		uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&free)),
		uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&avail)))
	if r1 == 0 {
		return 0
	}
	return int64(free)
}

// beepAlert plays the two rising tones that mark the end of a plan. It calls
// kernel32!Beep directly: the previous implementation launched PowerShell for
// this, which flashed a console window for a sound.
func beepAlert() {
	go func() {
		procBeep.Call(880, 250)
		procBeep.Call(1175, 400)
	}()
}
