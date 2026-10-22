//go:build windows

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows/registry"
)

// autostartKeyPath is the per-user Run key.
//
// HKCU rather than HKLM: starting with the user's session needs no
// administrator rights, and the entry disappears with the profile instead of
// leaving a machine-wide value pointing at a deleted executable.
const autostartKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// isNotExist maps the two "there is nothing here" errors to one answer. A
// missing key and a missing value are both normal states, not failures.
func isNotExist(err error) bool {
	return errors.Is(err, syscall.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, syscall.ERROR_PATH_NOT_FOUND)
}

// readAutostartValue returns the Run value, and whether it exists at all.
func readAutostartValue(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKeyPath, registry.QUERY_VALUE)
	if err != nil {
		if isNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	defer k.Close()

	value, _, err := k.GetStringValue(name)
	if err != nil {
		if isNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return value, true, nil
}

// writeAutostartValue creates the Run key if it is somehow absent and stores
// the value. CreateKey opens an existing key, so this is safe on every call.
func writeAutostartValue(name, value string) error {
	k, _, err := registry.CreateKey(registry.CURRENT_USER, autostartKeyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, value)
}

// deleteAutostartValue removes the value, treating "already gone" as success so
// the caller can retry the sync without a spurious error.
func deleteAutostartValue(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, autostartKeyPath, registry.SET_VALUE)
	if err != nil {
		if isNotExist(err) {
			return nil
		}
		return err
	}
	defer k.Close()

	if err := k.DeleteValue(name); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}
