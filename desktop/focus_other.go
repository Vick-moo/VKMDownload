//go:build !windows

package main

// raiseWindow is unavailable off Windows: the Win32 foreground lock, the
// AttachThreadInput trick and FlashWindowEx have no equivalent here, and this
// build is for development only (the app ships on Windows). False means "the
// window was not raised"; BringToFront still restores and pins it.
func raiseWindow() bool { return false }
