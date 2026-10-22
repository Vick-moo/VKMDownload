//go:build windows

package main

import "testing"

// TestReadAutostartValueMapsMissingToAbsent covers the real registry backend's
// error mapping.
//
// It is deliberately read-only: the value name it asks about is one no build
// ever writes, so the test proves that "the key or value is not there" comes
// back as (absent, no error) without creating, changing or deleting anything in
// the user's Run key. All write behaviour is covered through the in-memory fake
// in autostart_test.go, which never opens a registry key.
func TestReadAutostartValueMapsMissingToAbsent(t *testing.T) {
	const name = "VKMDownloadValueThatMustNotExist"

	value, present, err := readAutostartValue(name)
	if err != nil {
		t.Fatalf("readAutostartValue(%q) on an absent value: %v", name, err)
	}
	if present {
		t.Errorf("present = true for a value this build never writes (value = %q)", value)
	}
	if value != "" {
		t.Errorf("value = %q, want empty", value)
	}
}
