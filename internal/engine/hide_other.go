//go:build !windows

package engine

// Hidden helper files are a Windows Explorer concept; elsewhere these are
// no-ops so the engine stays portable without new dependencies.
func hideFile(path string)   {}
func unhideFile(path string) {}

// isHidden is only meaningful on Windows; tests there assert the real flag.
func isHidden(path string) bool { return false }
