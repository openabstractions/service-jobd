package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/openabstractions/abstraction-download/go/serve"
	"github.com/openabstractions/abstraction-download/go/serve/runtimehost"
	"golang.org/x/sys/windows"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

func processElevated() (bool, error) {
	var elevation uint32
	var returned uint32
	err := windows.GetTokenInformation(windows.GetCurrentProcessToken(), windows.TokenElevation,
		(*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &returned)
	if err != nil {
		return false, err
	}
	if returned != uint32(unsafe.Sizeof(elevation)) {
		return false, errors.New("unexpected token elevation information length")
	}
	return elevation != 0, nil
}

func runRuntime(ctx context.Context) error {
	host, err := runtimehost.Start(ctx, runtimehost.Options{ShutdownTimeout: 5 * time.Second, ForceTimeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer host.Close()
	return host.Wait()
}

// Both children share the service lifetime. An unexpected completion cancels
// the sibling and produces a service failure so SCM recovery can restart it.
func runServiceWork(ctx context.Context, flags []string, withRuntime bool,
	jobs func(context.Context, []string) error, runtime func(context.Context) error) error {
	if !withRuntime {
		return jobs(ctx, flags)
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 2)
	go func() { done <- jobs(childCtx, flags) }()
	go func() { done <- runtime(childCtx) }()
	err := <-done
	stopping := ctx.Err() != nil
	cancel()
	select {
	case <-done:
	case <-time.After(stopWait):
	}
	if stopping {
		return nil
	}
	if err == nil {
		err = errors.New("supervised component exited unexpectedly")
	}
	return err
}

// Readiness precedes the worker's bus becoming visible to start callers.
func runWithRuntime(ctx context.Context, args []string) error {
	return runReadyRuntime(ctx, args, serve.JobsContext, func(ctx context.Context) (func() error, func() error, error) {
		host, err := runtimehost.Start(ctx, runtimehost.Options{ShutdownTimeout: 5 * time.Second, ForceTimeout: 2 * time.Second})
		if err != nil {
			return nil, nil, err
		}
		return host.Wait, host.Close, nil
	})
}

func runReadyRuntime(ctx context.Context, args []string, jobs func(context.Context, []string) error,
	start func(context.Context) (wait func() error, close func() error, err error)) error {
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	wait, stopHost, err := start(childCtx)
	if err != nil {
		return err
	}
	defer stopHost()
	return runServiceWork(childCtx, args, true, jobs, func(lifetime context.Context) error {
		finished := make(chan struct{})
		defer close(finished)
		go func() {
			select {
			case <-lifetime.Done():
				cancel()
			case <-finished:
			}
		}()
		return wait()
	})
}

func checkRuntimeAvailable(ctx context.Context) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	command := runtimeStatusCommand(ctx, self)
	if err := command.Run(); err != nil {
		return fmt.Errorf("installed runtime status: %w", err)
	}
	return nil
}

func runtimeStatusCommand(ctx context.Context, self string) *exec.Cmd {
	command := exec.CommandContext(ctx, filepath.Join(filepath.Dir(self), "openabstractions.exe"), "status", "--json", "--timeout", "2s")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	return command
}
