//go:build !windows

package main

// The upgrade exclusion is written by the Windows installer transaction. Linux
// and macOS packages stop their registered services before replacement and
// have no installer-held record for activation to honour.
func refuseDuringUpgrade() error { return nil }
