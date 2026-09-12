package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type ended struct {
	serviceSpecific bool
	code            uint32
}

type driven struct {
	requests chan svc.ChangeRequest
	statuses chan svc.Status
	ended    chan ended
}

// drive starts the handler the way the service manager starts it: a channel of
// controls in, a channel of statuses out, the service name as the first
// argument. It closes the status channel once Execute has returned, which the
// SCM has no need to do and a test does.
func drive(t *testing.T) *driven {
	t.Helper()
	t.Setenv("ABSTRACTION_STORE", t.TempDir())
	d := &driven{
		requests: make(chan svc.ChangeRequest),
		statuses: make(chan svc.Status),
		ended:    make(chan ended, 1),
	}
	args := []string{serviceName, "--interval", "1h", "--endpoint", testEndpoint(t)}
	go func() {
		specific, code := supervisor{}.Execute(args, d.requests, d.statuses)
		d.ended <- ended{specific, code}
		close(d.statuses)
	}()
	return d
}

// A service stop is not a signal on Windows, so nothing in the supervisor loop
// would end it if JobsContext did not take a context. This is the failure the
// SCM reports as a hung service and then kills.
func TestExecuteAnswersServiceControlStop(t *testing.T) {
	d := drive(t)

	if got := (<-d.statuses).State; got != svc.StartPending {
		t.Fatalf("first status = %v, want StartPending", got)
	}
	// Running arrives before the store is open, deliberately: taking longer than
	// ServicesPipeTimeout to say it is a start failure, and a start failure is
	// the one failure the recovery policy does not fire on.
	if got := (<-d.statuses).State; got != svc.Running {
		t.Fatalf("second status = %v, want Running", got)
	}

	d.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	if got := (<-d.statuses).State; got != svc.StopPending {
		t.Fatalf("status after a stop = %v, want StopPending", got)
	}

	// Longer than stopWait so a handler that never returns fails here, where the
	// message says what went wrong, rather than at the test binary's timeout.
	select {
	case got := <-d.ended:
		if got.serviceSpecific || got.code != 0 {
			t.Fatalf("Execute = (%v, %d) after a stop, want a clean (false, 0)", got.serviceSpecific, got.code)
		}
	case <-time.After(2 * stopWait):
		t.Fatalf("Execute did not return within %v of a stop", 2*stopWait)
	}
	for range d.statuses {
	}
}

// Interrogate may arrive at any time, and answering it with anything but the
// status the SCM handed us is how a service reports a state it is not in.
func TestExecuteAnswersInterrogate(t *testing.T) {
	d := drive(t)
	<-d.statuses
	<-d.statuses

	want := svc.Status{State: svc.Running, Accepts: svc.AcceptStop, CheckPoint: 7}
	d.requests <- svc.ChangeRequest{Cmd: svc.Interrogate, CurrentStatus: want}
	if got := <-d.statuses; got != want {
		t.Fatalf("interrogate answered %+v, want %+v", got, want)
	}

	d.requests <- svc.ChangeRequest{Cmd: svc.Stop}
	for range d.statuses {
	}
	<-d.ended
}

// windowlessImage refuses rather than falling back, because the fallback is the
// console image and registering that is a console window at every sign-in. The
// test binary has no twin beside it, which is the case being asserted.
func TestWindowlessImageRefusesWhenTheTwinIsMissing(t *testing.T) {
	_, err := windowlessImage()
	if err == nil {
		t.Fatalf("windowlessImage found %s beside the test binary and returned it", windowlessName)
	}
	if !strings.Contains(err.Error(), windowlessName) {
		t.Fatalf("refusal does not name %s: %v", windowlessName, err)
	}
}

func testEndpoint(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf(`\\.\pipe\jobd-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
}

// This fake SCM grants exactly the requested handle rights. No services are
// created or configured on the test host.
func TestRegistrationHandleCanConfigureRestartRecovery(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair=%t", repair), func(t *testing.T) {
			var granted uint32
			opens := 0
			h, existed, err := registrationHandle(func(access uint32) (windows.Handle, error) {
				if repair {
					return 0, windows.ERROR_SERVICE_EXISTS
				}
				granted = access
				return 41, nil
			}, func(access uint32) (windows.Handle, error) {
				opens++
				granted = access
				return 41, nil
			})
			if err != nil || h != 41 || existed != repair || (opens == 1) != repair {
				t.Fatalf("handle=%v existed=%v opens=%d err=%v", h, existed, opens, err)
			}
			// Model the documented authorization check for the operation the
			// caller performs next, rather than comparing to a shared constant.
			configureRestart := func() error {
				if granted&windows.SERVICE_CHANGE_CONFIG == 0 || granted&windows.SERVICE_START == 0 {
					return windows.ERROR_ACCESS_DENIED
				}
				return nil
			}
			if err := configureRestart(); err != nil {
				t.Fatalf("restart recovery with granted rights %#x: %v", granted, err)
			}
		})
	}
}

func TestRegistrationHandleDoesNotHideSCMFailures(t *testing.T) {
	for _, repair := range []bool{false, true} {
		t.Run(fmt.Sprintf("repair=%t", repair), func(t *testing.T) {
			opens := 0
			h, existed, err := registrationHandle(func(uint32) (windows.Handle, error) {
				if repair {
					return 0, windows.ERROR_SERVICE_EXISTS
				}
				return 0, windows.ERROR_ACCESS_DENIED
			}, func(uint32) (windows.Handle, error) {
				opens++
				return 0, windows.ERROR_ACCESS_DENIED
			})
			if h != 0 || existed != repair || !errors.Is(err, windows.ERROR_ACCESS_DENIED) || (opens == 1) != repair {
				t.Fatalf("handle=%v existed=%v opens=%d err=%v", h, existed, opens, err)
			}
		})
	}
}
