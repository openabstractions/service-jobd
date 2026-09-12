package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows/svc"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRuntimeRegistrationOptIn(t *testing.T) {
	for _, command := range []string{"install", "run"} {
		for _, enabled := range []bool{false, true} {
			args := []string{command}
			if enabled {
				args = append(args, "--runtime")
			}
			got, flag, err := serviceArguments(args)
			if err != nil || got != command || flag != enabled {
				t.Fatalf("parse %v = %q %v %v", args, got, flag, err)
			}
			line := serviceCommand(`C:\Program Files\OA\jobdw.exe`, flag)
			want := `"C:\Program Files\OA\jobdw.exe" service run`
			if enabled {
				want += " --runtime"
			}
			if line != want {
				t.Fatalf("registered command %q, want %q", line, want)
			}
		}
	}
	for _, args := range [][]string{nil, {"uninstall", "--runtime"}, {"run", "--unknown"}, {"run", "--runtime", "--runtime"}} {
		if _, _, err := serviceArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestWorkerOnlyDoesNotStartRuntime(t *testing.T) {
	want := []string{"--interval", "1h"}
	err := runServiceWork(context.Background(), want, false, func(_ context.Context, flags []string) error {
		if !reflect.DeepEqual(flags, want) {
			t.Errorf("worker flags %v", flags)
		}
		return nil
	}, func(context.Context) error { t.Error("runtime started without opt-in"); return nil })
	if err != nil {
		t.Fatal(err)
	}
}

func TestServiceComponentFailureCancelsSibling(t *testing.T) {
	for _, runtimeFails := range []bool{false, true} {
		t.Run(map[bool]string{true: "runtime", false: "download"}[runtimeFails], func(t *testing.T) {
			started := make(chan struct{})
			stopped := make(chan struct{})
			failure := errors.New("component failed")
			failing := func(context.Context) error { <-started; return failure }
			waiting := func(ctx context.Context) error { close(started); <-ctx.Done(); close(stopped); return ctx.Err() }
			worker, runtime := waiting, failing
			if !runtimeFails {
				worker, runtime = failing, waiting
			}
			err := runServiceWork(context.Background(), nil, true, func(ctx context.Context, _ []string) error { return worker(ctx) }, runtime)
			if !errors.Is(err, failure) {
				t.Fatalf("failure = %v", err)
			}
			select {
			case <-stopped:
			default:
				t.Fatal("sibling still running")
			}
		})
	}
}

func TestServiceStopCancelsBothComponents(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{}, 2)
	stopped := make(chan struct{}, 2)
	wait := func(ctx context.Context) error {
		started <- struct{}{}
		<-ctx.Done()
		stopped <- struct{}{}
		return ctx.Err()
	}
	done := make(chan error, 1)
	go func() {
		done <- runServiceWork(ctx, nil, true, func(ctx context.Context, _ []string) error { return wait(ctx) }, wait)
	}()
	<-started
	<-started
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("stop hung")
	}
	if len(stopped) != 2 {
		t.Fatal("both components must stop")
	}
}

func TestUnexpectedCleanRuntimeExitIsFailure(t *testing.T) {
	err := runServiceWork(context.Background(), nil, true, func(ctx context.Context, _ []string) error { <-ctx.Done(); return ctx.Err() }, func(context.Context) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "unexpectedly") {
		t.Fatalf("runtime exit = %v", err)
	}
}

func TestRuntimeStartupFailureReportsSCMRecoveryFailure(t *testing.T) {
	stopped := make(chan struct{})
	h := supervisor{withRuntime: true,
		jobs:    func(ctx context.Context, _ []string) error { <-ctx.Done(); close(stopped); return ctx.Err() },
		runtime: func(context.Context) error { return errors.New("missing sibling runtime") },
	}
	statuses := make(chan svc.Status)
	done := make(chan ended, 1)
	go func() {
		specific, code := h.Execute([]string{serviceName}, make(chan svc.ChangeRequest), statuses)
		done <- ended{specific, code}
	}()
	if (<-statuses).State != svc.StartPending || (<-statuses).State != svc.Running {
		t.Fatal("startup statuses")
	}
	select {
	case result := <-done:
		if !result.serviceSpecific || result.code == 0 {
			t.Fatalf("SCM result %+v", result)
		}
	case <-time.After(time.Second):
		t.Fatal("startup failure did not return")
	}
	select {
	case <-stopped:
	default:
		t.Fatal("worker not stopped")
	}
}

func TestUserRuntimeReadinessPrecedesWorker(t *testing.T) {
	ready := false
	stopped := false
	failure := errors.New("worker ended")
	err := runReadyRuntime(context.Background(), nil,
		func(context.Context, []string) error {
			if !ready {
				t.Error("worker before ready")
			}
			return failure
		},
		func(ctx context.Context) (func() error, func() error, error) {
			ready = true
			return func() error { <-ctx.Done(); return ctx.Err() }, func() error { stopped = true; return nil }, nil
		})
	if !errors.Is(err, failure) || !stopped {
		t.Fatalf("err=%v stopped=%v", err, stopped)
	}
}

func TestUserRuntimeStartupRefusalDoesNotStartWorker(t *testing.T) {
	failure := errors.New("missing runtime")
	err := runReadyRuntime(context.Background(), nil,
		func(context.Context, []string) error { t.Error("worker started"); return nil },
		func(context.Context) (func() error, func() error, error) { return nil, nil, failure })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}

func TestStatusUsesExactHiddenSibling(t *testing.T) {
	command := runtimeStatusCommand(context.Background(), `C:\Program Files\OA\jobdw.exe`)
	want := []string{`C:\Program Files\OA\openabstractions.exe`, "status", "--json", "--timeout", "2s"}
	if !reflect.DeepEqual(command.Args, want) {
		t.Fatalf("command %v", command.Args)
	}
	if command.SysProcAttr == nil || !command.SysProcAttr.HideWindow || command.SysProcAttr.CreationFlags&0x08000000 == 0 {
		t.Fatal("status must be windowless")
	}
}
