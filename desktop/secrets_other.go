//go:build !windows

package main

// Off Windows there is no DPAPI. The app does not ship there — this build exists
// so `GOOS=linux go vet` and the test suite can compile — so the value is stored
// with an explicit marker rather than silently looking encrypted.

import "fmt"

func sealSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	return plainPrefix + plain, nil
}

func openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if hasSecretPrefix(stored) {
		// A DPAPI blob cannot be opened here; the caller restarts the download
		// without credentials rather than failing.
		return "", fmt.Errorf("DPAPI secret cannot be opened on this platform")
	}
	if len(stored) > len(plainPrefix) && stored[:len(plainPrefix)] == plainPrefix {
		return stored[len(plainPrefix):], nil
	}
	// An older plaintext record.
	return stored, nil
}

func newSecretNonce() []byte { return nil }
