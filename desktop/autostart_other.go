//go:build !windows

package main

// The app ships on Windows; these stubs exist so the settings carry the value
// on every platform (a portable config keeps its choice) and the development
// builds still compile. There is no registry to write on the others, so the
// sync is a no-op that reports success.
func readAutostartValue(string) (string, bool, error) { return "", false, nil }

func writeAutostartValue(string, string) error { return nil }

func deleteAutostartValue(string) error { return nil }
