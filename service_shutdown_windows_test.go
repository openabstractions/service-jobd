package main

import (
	"context"
	"errors"
	"golang.org/x/sys/windows"
	"testing"
	"time"
)

type shutdownProcess struct {
	alive  bool
	closed bool
}

func (p *shutdownProcess) exited() (bool, error) { return !p.alive, nil }
func (p *shutdownProcess) close() error          { p.closed = true; return nil }

func TestCheckedServiceShutdown(t *testing.T) {
	for _, mode := range []string{"denied", "pending", "timeout", "alive-after-stopped", "changed-pid", "shared", "already-stopped", "stop-only", "unobserved-pending"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
			defer cancel()
			process := &shutdownProcess{alive: mode == "alive-after-stopped"}
			queries, stops, deletes, pins := 0, 0, 0, 0
			ops := removalOps{
				query: func() (windows.SERVICE_STATUS_PROCESS, error) {
					queries++
					state := uint32(windows.SERVICE_RUNNING)
					pid := uint32(17)
					kind := uint32(windows.SERVICE_WIN32_OWN_PROCESS)
					if mode == "shared" {
						kind = windows.SERVICE_WIN32_SHARE_PROCESS
					}
					if mode == "changed-pid" && queries > 1 {
						pid = 18
					}
					if mode == "already-stopped" {
						state = windows.SERVICE_STOPPED
						pid = 0
					}
					if mode == "unobserved-pending" {
						state = windows.SERVICE_STOP_PENDING
						if queries > 1 {
							state = windows.SERVICE_STOPPED
							pid = 0
						}
					}
					if stops > 0 && mode != "timeout" {
						state = windows.SERVICE_STOPPED
						pid = 0
						if mode == "pending" && queries == 4 {
							state = windows.SERVICE_STOP_PENDING
							pid = 17
						}
					}
					return windows.SERVICE_STATUS_PROCESS{CurrentState: state, ProcessId: pid, ServiceType: kind}, nil
				},
				stop: func() error {
					stops++
					if mode == "denied" {
						return windows.ERROR_ACCESS_DENIED
					}
					return nil
				},
				pin: func(pid uint32) (removalProcess, error) {
					pins++
					if pid != 17 {
						t.Fatal(pid)
					}
					return process, nil
				},
				remove: func() error { deletes++; return nil },
			}
			if mode == "stop-only" {
				ops.remove = nil
			}
			removed, err := stopAndDeleteService(ctx, ops)
			success := mode == "pending" || mode == "already-stopped" || mode == "stop-only"
			if success != (err == nil) || removed != success {
				t.Fatalf("removed=%v err=%v queries=%d", removed, err, queries)
			}
			if (!success || mode == "stop-only") && deletes != 0 {
				t.Fatal("deleted before confirmed shutdown")
			}
			if success && mode != "stop-only" && deletes != 1 {
				t.Fatal("no deletion")
			}
			if pins > 0 && !process.closed {
				t.Fatal("retained handle leaked")
			}
			if mode == "already-stopped" && (stops != 0 || pins != 0) {
				t.Fatal("stopped service controlled")
			}
		})
	}
}

func TestServiceUninstallSharedBudgetAndEnumeration(t *testing.T) {
	denied := errors.New("enumeration denied")
	if _, err := uninstallServices(context.Background(), func() ([]string, error) { return nil, denied }, func(context.Context, string) (bool, error) {
		t.Fatal("deleted after enumeration failure")
		return false, nil
	}); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	calls := 0
	_, err := uninstallServices(ctx, func() ([]string, error) { return []string{"first", "second"}, nil }, func(call context.Context, name string) (bool, error) {
		calls++
		if call != ctx {
			t.Fatal("new per-service budget")
		}
		if name != "first" {
			t.Fatal("continued after deadline", name)
		}
		<-call.Done()
		return false, call.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatal(calls, err)
	}
	if request, err := serviceArguments([]string{"stop"}); request.command != "stop" || request.runtime || request.userFolder != "" || err != nil {
		t.Fatal(request, err)
	}
	if _, err := serviceArguments([]string{"stop", "--runtime"}); err == nil {
		t.Fatal("accepted irrelevant stop flag")
	}
}

func TestServiceShutdownRefusesNewClone(t *testing.T) {
	lists := 0
	_, err := uninstallServices(context.Background(), func() ([]string, error) {
		lists++
		if lists == 1 {
			return []string{"existing"}, nil
		}
		return []string{"existing", "new-login"}, nil
	}, func(context.Context, string) (bool, error) { return true, nil })
	if err == nil || lists != 2 {
		t.Fatal("new clone was not detected", lists, err)
	}
}
