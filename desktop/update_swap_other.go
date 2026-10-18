//go:build !windows

package main

import (
	"errors"
	"os/exec"
	"time"
)

// Non-Windows builds are development-only (the app ships on Windows). The
// update path is implemented far enough to compile and to keep its tests
// meaningful: the file-level pieces are shared, only the kernel calls below are
// unavailable, and every one of them refuses loudly rather than pretending.

var errUpdateUnsupported = errors.New("self-update 只在 Windows 上受支持")

func (realUpdateSys) hardLink(src, dst string) error { return errUpdateUnsupported }

func (realUpdateSys) replaceFile(src, dst string) error { return errUpdateUnsupported }

func (realUpdateSys) startProcess(exe string, args ...string) (int, error) {
	return 0, errUpdateUnsupported
}

func (realUpdateSys) processAlive(pid int) bool { return false }

func (realUpdateSys) processHasWindow(pid int) bool { return false }

func (realUpdateSys) waitProcess(pid int, timeout time.Duration) error {
	return errUpdateUnsupported
}

func (realUpdateSys) killProcess(pid int) error { return errUpdateUnsupported }

func freeSpaceBytes(path string) int64 { return 0 }

// hiddenCmd keeps the same shape as the Windows one so callers do not need a
// build tag of their own.
func hiddenCmd(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

// beepAlert is unavailable without the kernel call; a no-op keeps plan
// completion working.
func beepAlert() {}
