//go:build !windows

package main

// Non-Windows builds (dev only; the app ships on Windows): the peer table
// and Authenticode APIs do not exist, so verification is unavailable.
func tcpOwnerPid(port uint16) (uint32, error) {
	return 0, errPeerCheckUnsupported
}

func processImagePath(pid uint32) (string, error) {
	return "", errPeerCheckUnsupported
}

func verifyFileTrust(path string) bool { return false }
