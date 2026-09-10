//go:build !windows

package main

import (
	"fmt"
	"os"
)

// A per-user service is a Windows facility. systemd and launchd already start a
// program per user and restart it, so there is nothing here for `jobd service`
// to be — the supervisor is what `jobd start` runs.
func cmdService([]string) {
	fmt.Fprintln(os.Stderr, "jobd: per-user services are a Windows facility; `jobd start` runs a supervisor here")
	os.Exit(2)
}
