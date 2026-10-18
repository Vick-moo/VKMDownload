//go:build windows

package main

// Encrypting the one credential this app must keep.
//
// A paused or failed download has to replay the browser's cookies to resume: a
// private Drive file or a signed classroom resource rejects a client that
// arrives without them. So the cookie cannot simply be dropped — but a plaintext
// cookie in tasks.json is a copy of the user's login sitting in a file that
// backup tools, sync clients and any program running as the same user can read.
//
// DPAPI is the Windows answer to exactly this: the OS derives a key from the
// user's logon and keeps the plaintext out of the file. What it buys and what it
// does not is worth stating precisely, because a vague "it is encrypted" is
// worse than no claim:
//
//   - It DOES protect against the realistic leak paths: copying tasks.json
//     elsewhere, a sync or backup tool shipping it off the machine, and another
//     Windows account on the same box.
//   - It does NOT protect against malicious code running as this user: that code
//     can call CryptUnprotectData too. The bar rises from "any file reader" to
//     "code that already has the user's session", which is a real change but not
//     a vault.
//
// The blob is user-bound and machine-bound (we deliberately pass no
// CRYPTPROTECT_LOCAL_MACHINE), so a copied file is useless on another account or
// computer.

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	// kernel32 is declared in peercheck_windows.go; LocalFree lives there too.
	procLocalFree = kernel32.NewProc("LocalFree")
)

// cryptProtectUIForbidden keeps the call from ever showing a Windows prompt: this
// runs while saving a task, and a modal dialog there would freeze the app.
const cryptProtectUIForbidden = 0x1

// dataBlob mirrors DATA_BLOB from dpapi.h.
type dataBlob struct {
	cbData uint32
	pbData *byte
}

func newBlob(b []byte) dataBlob {
	if len(b) == 0 {
		return dataBlob{}
	}
	return dataBlob{cbData: uint32(len(b)), pbData: &b[0]}
}

// sealSecret encrypts plain with DPAPI and returns a self-describing string.
//
// The "dpapi:v1:" prefix is what lets a later version tell an encrypted value
// from one an older build wrote in plaintext, without guessing.
func sealSecret(plain string) (string, error) {
	if plain == "" {
		return "", nil
	}
	in := newBlob([]byte(plain))
	var out dataBlob
	r, _, errno := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, // description
		0, // optional entropy: none, so only this user on this machine can open it
		0, 0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		return "", fmt.Errorf("CryptProtectData: %w", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	blob := unsafe.Slice(out.pbData, out.cbData)
	return secretPrefix + base64.StdEncoding.EncodeToString(blob), nil
}

// openSecret reverses sealSecret. A value without the prefix is returned as-is:
// that is a record written by a build before encryption existed, and the caller
// re-seals it on the next save.
func openSecret(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if !hasSecretPrefix(stored) {
		return stored, nil
	}
	raw, err := base64.StdEncoding.DecodeString(stored[len(secretPrefix):])
	if err != nil {
		return "", fmt.Errorf("stored secret is not valid base64: %w", err)
	}
	if len(raw) == 0 {
		return "", errors.New("stored secret is empty")
	}
	in := newBlob(raw)
	var out dataBlob
	r, _, errno := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)),
		0, 0, 0, 0,
		uintptr(cryptProtectUIForbidden),
		uintptr(unsafe.Pointer(&out)),
	)
	if r == 0 {
		// Most often: the file was copied from another account or machine, or
		// the user's password was reset without a recovery path. Either way the
		// bytes are gone for us; the download restarts without them.
		return "", fmt.Errorf("CryptUnprotectData: %w", errno)
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.pbData)))
	return string(unsafe.Slice(out.pbData, out.cbData)), nil
}

// newSecretNonce exists so tests can prove two seals of the same input differ
// (DPAPI salts each call); it is not used by the production path.
func newSecretNonce() []byte {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return b
}
