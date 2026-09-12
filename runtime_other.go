//go:build !windows

package main

import (
	"context"
	"errors"
)

func runWithRuntime(context.Context, []string) error {
	return errors.New("jobd: --runtime activation requires Windows")
}

func checkRuntimeAvailable(context.Context) error {
	return errors.New("jobd: --runtime activation requires Windows")
}
