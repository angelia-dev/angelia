//go:build !linux

package main

// setNotDumpable is Linux only: other platforms have no /proc/<pid>/environ
// to protect this way (ADR 0042 §3).
func setNotDumpable() error { return nil }
