//go:build windows

package main

import (
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

// Window activation for BringToFront.
//
// Windows refuses to let a background process take the foreground: while the
// browser owns it, SetForegroundWindow returns 0 and nothing happens — no
// error, no flash, no hint. The new-task dialog therefore opened inside a
// window that stayed behind the browser, which looks exactly like the app
// ignoring the download.
//
// raiseWindow does two things about that:
//
//  1. Find this process's own top-level window and ask for the foreground the
//     way the foreground process itself would: attach to its input queue, then
//     SetForegroundWindow/SetFocus. The attach is what makes the call legal
//     from a background thread.
//  2. If Windows refuses anyway — the foreground lock is a deliberate
//     anti-focus-stealing feature and it can win — flash the taskbar button so
//     the user at least sees that the app wants attention.
//
// Everything here is best effort: with no window, no foreground thread, or a
// refused call, the function returns false and the caller carries on. The pin
// in BringToFront remains as the second line of defence.
//
// The user32/kernel32 handles and the enumeration procs are shared with the
// other Windows-only files (update_swap_windows.go, peercheck_windows.go); this
// file only adds the procedures it needs.

const (
	swRestore = 9 // SW_RESTORE: restore an iconic window and activate it

	gwOwner        = 4            // GetWindow(hwnd, GW_OWNER)
	gwlExStyle     = ^uintptr(19) // GWL_EXSTYLE (=-20), sign-extended to LONG_PTR
	wsExToolWindow = 0x00000080   // a tool window is not the app's main window

	asfwAny = 0xFFFFFFFF // AllowSetForegroundWindow(ASFW_ANY)

	flashwAll       = 0x00000003 // FLASHW_ALL: caption and taskbar button
	flashwTimerNoFG = 0x0000000C // FLASHW_TIMERNOFG: until it comes to the front
)

// flashWindowInfo mirrors FLASHWINFO from winuser.h.
type flashWindowInfo struct {
	cbSize    uint32
	hwnd      uintptr
	dwFlags   uint32
	uCount    uint32
	dwTimeout uint32
}

var (
	procGetForegroundWindow = user32.NewProc("GetForegroundWindow")
	procAttachThreadInput   = user32.NewProc("AttachThreadInput")
	procAllowSetForeground  = user32.NewProc("AllowSetForegroundWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSetFocus            = user32.NewProc("SetFocus")
	procFlashWindowEx       = user32.NewProc("FlashWindowEx")
	procShowWindow          = user32.NewProc("ShowWindow")
	procIsIconic            = user32.NewProc("IsIconic")
	procGetWindow           = user32.NewProc("GetWindow")
	procGetWindowLongW      = user32.NewProc("GetWindowLongW")
	procGetCurrentThreadID  = kernel32.NewProc("GetCurrentThreadId")
)

// EnumWindows has no context pointer, so the callback reads its target from
// these package variables. A mutex keeps two overlapping BringToFront calls
// from overwriting each other's target, and the callback is created once
// because syscall.NewCallback thunks live for the lifetime of the process —
// creating one per download would leak a thunk per download.
var (
	ownWindowMu    sync.Mutex
	ownWindowPID   uint32
	ownWindowFound uintptr
)

var enumOwnWindowCallback = syscall.NewCallback(func(hwnd, _ uintptr) uintptr {
	var pid uint32
	procGetWindowThreadPid.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	if pid != ownWindowPID {
		return 1 // another process's window: keep looking
	}
	// EnumWindows only walks top-level windows, but it includes hidden ones
	// (the tray helper, message-only windows): only a visible window can be
	// brought to the front.
	if vis, _, _ := procIsWindowVisible.Call(hwnd); vis == 0 {
		return 1
	}
	// Owned windows are dialogs, tooltips and popups parented to something
	// else; tool windows (WS_EX_TOOLWINDOW) are tray/message helpers. The main
	// window has neither, and picking a helper here would move the wrong
	// window.
	if owner, _, _ := procGetWindow.Call(hwnd, gwOwner); owner != 0 {
		return 1
	}
	if ex, _, _ := procGetWindowLongW.Call(hwnd, gwlExStyle); uint32(ex)&wsExToolWindow != 0 {
		return 1
	}
	ownWindowFound = hwnd
	return 0 // the first match in z-order is ours
})

// ownTopLevelWindow returns this process's main top-level window, or 0.
//
// The window is identified by owning pid, never by title: the title is
// user-language, may include a version, and changes with the task list, while
// the pid is a fact. Enumerating also means no dependency on Wails internals.
func ownTopLevelWindow() uintptr {
	ownWindowMu.Lock()
	defer ownWindowMu.Unlock()
	ownWindowPID = uint32(os.Getpid())
	ownWindowFound = 0
	procEnumWindows.Call(enumOwnWindowCallback, 0)
	return ownWindowFound
}

// raiseWindow forces this process's window to the foreground. It reports
// whether the window ended up there; on any refusal it flashes the taskbar
// instead and returns false. It never panics: it runs on the path that answers
// a browser download, and a panic there would take the whole app down (the
// syscall wrappers panic if a DLL lacks an export).
func raiseWindow() (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	// AttachThreadInput, SetForegroundWindow and the detach must all run on one
	// thread; Go would otherwise be free to move this goroutine to another OS
	// thread between calls, leaving the attach on a thread that is about to
	// stop running and the foreground call on a thread with no right to make
	// it. Wails' own window functions lock for the same reason.
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hwnd := ownTopLevelWindow()
	if hwnd == 0 {
		return false
	}

	// WindowUnminimise runs first in BringToFront, but a direct call (or a race
	// with the user minimising) can still find the window iconic, and Windows
	// will not foreground an iconic window without restoring it. SW_RESTORE
	// only when iconic: on a maximised window it would un-maximise it.
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, swRestore)
	}

	fg := foregroundWindow()
	ourThread, _, _ := procGetCurrentThreadID.Call()

	// Join the foreground thread's input queue. A thread may only set the
	// foreground window if it shares the queue with the one that owns it, so
	// this is what turns a refused call into an allowed one. The detach is
	// deferred: returning while still attached would leave this thread's input
	// queue joined to the browser's.
	if fg != 0 {
		fgThread, _, _ := procGetWindowThreadPid.Call(fg)
		if fgThread != 0 && fgThread != ourThread {
			if r, _, _ := procAttachThreadInput.Call(ourThread, fgThread, 1); r != 0 {
				defer procAttachThreadInput.Call(ourThread, fgThread, 0)
			}
		}
	}

	// ASFW_ANY: allow any process to take the foreground, which is what the
	// foreground process would normally grant. Harmless when Windows already
	// allows it; the call is advisory.
	procAllowSetForeground.Call(asfwAny)
	procSetForegroundWindow.Call(hwnd)
	// SetFocus alone only reaches the calling thread's queue; it works here
	// because of the attach above, and makes the dialog's input fields usable
	// without a second click.
	procSetFocus.Call(hwnd)

	if foregroundWindow() == hwnd {
		return true
	}

	// Refused. FLASHW_TIMERNOFG keeps flashing until the window is brought to
	// the front, which is the only signal Windows still allows a background
	// process to give.
	var fwi flashWindowInfo
	fwi.cbSize = uint32(unsafe.Sizeof(fwi))
	fwi.hwnd = hwnd
	fwi.dwFlags = flashwAll | flashwTimerNoFG
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fwi)))
	return false
}

// foregroundWindow returns the foreground window handle, or 0.
func foregroundWindow() uintptr {
	h, _, _ := procGetForegroundWindow.Call()
	return h
}
