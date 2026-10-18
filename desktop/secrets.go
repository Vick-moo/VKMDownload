package main

// The storage format around a sealed credential, plus the platform stubs.
//
// Only Windows has DPAPI, and the app only ships on Windows; the non-Windows
// build exists for `GOOS=linux go vet` and for tests, where it must not pretend
// to encrypt. It stores a plainly-marked value instead, so nothing in the code
// path can mistake "no encryption available" for "encrypted".

import "strings"

// secretPrefix marks a value sealed by sealSecret.
const secretPrefix = "dpapi:v1:"

// plainPrefix marks a value stored without encryption, used only off Windows.
const plainPrefix = "plain:v1:"

// hasSecretPrefix reports whether stored was written by sealSecret.
func hasSecretPrefix(stored string) bool {
	return strings.HasPrefix(stored, secretPrefix)
}

// sealCookie encrypts a cookie for tasks.json.
//
// A failure is reported, never silently downgraded to plaintext: writing the
// cookie out in the clear is exactly what this change exists to stop, and doing
// it because the OS call failed would be worse than losing the cookie — the
// download can be restarted, a leaked session cannot be unleaked.
func sealCookie(cookie string) (string, error) {
	return sealSecret(cookie)
}

// openCookie decrypts what sealCookie wrote, tolerating a plaintext record left
// by an older build (it is re-sealed on the next save).
func openCookie(stored string) (string, error) {
	return openSecret(stored)
}
