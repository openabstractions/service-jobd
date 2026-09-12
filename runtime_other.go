//go:build !windows

package main

import (
	"context"
	"errors"
)

func processElevated() (bool, error) {
	return false, errors.New("unelevated activation token check requires Windows")
}

func runWithRuntime(context.Context, []string) error {
	return errors.New("jobd: --runtime activation requires Windows")
}

func checkRuntimeAvailable(context.Context) error {
	return errors.New("jobd: --runtime activation requires Windows")
}
