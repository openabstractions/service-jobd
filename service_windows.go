package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"

	"github.com/openabstractions/abstraction-download/go/serve"
)

const (
	serviceName    = "OpenAbstractionsSupervisor"
	serviceDisplay = "Abstraction supervisor"

	// SERVICE_USER_OWN_PROCESS. sc.exe offers it as `type= userown` and the
	// CreateServiceW reference does not list it, so there is nothing to import
	// it from. VISION.md 2026-09-10 "Host S is unnecessary" is the run that
	// proved the SCM accepts it for a third-party binary.
	serviceUserOwnProcess = 0x50

	windowlessName = "jobdw.exe"

	// The SCM kills a service that has not answered a stop within its wait hint,
	// and serve.JobsContext only looks at the context between sweeps — one sweep
	// can be a multi-gigabyte transfer. So the stop is bounded and this process
	// exits rather than being killed: an interrupted sweep loses nothing, the
	// lease lapses, and the next supervisor adopts the work.
	stopWait = 10 * time.Second
)

func cmdService(args []string) {
	request, err := serviceArguments(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobd:", err)
		os.Exit(2)
	}
	switch request.command {
	case "install":
		err = serviceInstall(request.runtime)
	case "uninstall":
		err = serviceUninstall()
	case "stop":
		if request.userFolder != "" {
			err = serviceStopUser(request.userFolder)
		} else {
			err = serviceStop()
		}
	case "run":
		err = serviceRun(request.runtime)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobd:", err)
		os.Exit(1)
	}
}

// windowlessImage is what gets registered, and it is never the running
// executable.
//
// A per-user service instance runs inside the interactive session, so the
// loader gives a console image a console window at every sign-in — measured,
// measured on Windows 11, and watched happening by the owner. jobd
// is that image; jobdw is the same package linked -H=windowsgui. Falling back
// to os.Executable() would register the window this whole mechanism exists to
// remove, so a missing twin is a refusal, and it is checked before the SCM is
// opened so the refusal is what an unprivileged run reports.
func windowlessImage() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	w := filepath.Join(filepath.Dir(exe), windowlessName)
	if _, err := os.Stat(w); err != nil {
		return "", fmt.Errorf("%s is not beside %s: a console image registered as a per-user service opens a window at every sign-in, so it will not be registered", windowlessName, filepath.Base(exe))
	}
	return w, nil
}

func serviceInstall(withRuntime bool) error {
	exe, err := windowlessImage()
	if err != nil {
		return err
	}
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_CREATE_SERVICE)
	if err != nil {
		return fmt.Errorf("registering a per-user service needs an administrator token: %w", err)
	}
	defer windows.CloseServiceHandle(m)

	name, err := windows.UTF16PtrFromString(serviceName)
	if err != nil {
		return err
	}
	disp, err := windows.UTF16PtrFromString(serviceDisplay)
	if err != nil {
		return err
	}
	bin, err := windows.UTF16PtrFromString(serviceCommand(exe, withRuntime))
	if err != nil {
		return err
	}

	// serviceStartName and password nil: a per-user service takes its account
	// from whoever signs in, which is why nothing is stored here to leak.
	h, existed, err := registrationHandle(
		func(access uint32) (windows.Handle, error) {
			return windows.CreateService(m, name, disp, access,
				serviceUserOwnProcess, windows.SERVICE_AUTO_START, windows.SERVICE_ERROR_NORMAL,
				bin, nil, nil, nil, nil, nil)
		},
		func(access uint32) (windows.Handle, error) { return windows.OpenService(m, name, access) },
	)
	if err != nil {
		return fmt.Errorf("registering %s: %w", serviceName, err)
	}
	defer windows.CloseServiceHandle(h)
	// A repair re-runs this action, and an older package may have registered a
	// different path — including the console image this function now refuses.
	// Reporting success without rewriting the configuration is how a service
	// stays pointed at an image that has moved.
	if existed {
		if err := windows.ChangeServiceConfig(h, serviceUserOwnProcess, windows.SERVICE_AUTO_START,
			windows.SERVICE_ERROR_NORMAL, bin, nil, nil, nil, nil, nil, disp); err != nil {
			return fmt.Errorf("rewriting %s, which was already registered: %w", serviceName, err)
		}
	}
	if err := setRecovery(h); err != nil {
		return fmt.Errorf("recovery policy for %s: %w", serviceName, err)
	}
	fmt.Printf("registered %s -> %s\n", serviceName, exe)
	return nil
}

// registrationHandle acquires the rights needed by both configuration and
// restart recovery, including on repair. ChangeServiceConfig2 requires
// SERVICE_START when the recovery actions contain SC_ACTION_RESTART:
// https://learn.microsoft.com/en-us/windows/win32/api/winsvc/nf-winsvc-changeserviceconfig2w
func registrationHandle(create, open func(uint32) (windows.Handle, error)) (windows.Handle, bool, error) {
	access := uint32(windows.SERVICE_CHANGE_CONFIG | windows.SERVICE_START)
	h, err := create(access)
	existed := errors.Is(err, windows.ERROR_SERVICE_EXISTS)
	if existed {
		h, err = open(access)
	}
	return h, existed, err
}

// setRecovery configures restart delays of 3s, 10s, then 30s. The documented SCM
// policy repeats the final action on subsequent failures; this list sets no
// three-restart limit. The failure count resets after an hour without failures.
// See https://learn.microsoft.com/en-us/windows/win32/api/winsvc/ns-winsvc-service_failure_actionsw
func setRecovery(h windows.Handle) error {
	s := &mgr.Service{Name: serviceName, Handle: h}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 3 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, uint32(time.Hour/time.Second)); err != nil {
		return err
	}
	// Without this the actions above fire only when the process dies without
	// reporting SERVICE_STOPPED. A supervisor that reports a tidy stop after
	// failing to open its store is the case we most want restarted, and it has
	// to be asked for.
	return s.SetRecoveryActionsOnNonCrashFailures(true)
}

func serviceUninstall() error {
	return serviceQuiesce(true)
}
func serviceStop() error { return serviceQuiesce(false) }
func serviceQuiesce(remove bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT|windows.SC_MANAGER_ENUMERATE_SERVICE)
	if err != nil {
		return fmt.Errorf("controlling registered per-user services needs an administrator token: %w", err)
	}
	defer windows.CloseServiceHandle(m)

	gone, err := uninstallServices(ctx, func() ([]string, error) { return instances(m) }, func(ctx context.Context, name string) (bool, error) { return checkedService(ctx, m, name, remove) })
	if err != nil {
		return err
	}
	if gone == 0 {
		fmt.Printf("no registered %s instances were present\n", serviceName)
	}
	return nil
}

func uninstallServices(ctx context.Context, list func() ([]string, error), remove func(context.Context, string) (bool, error)) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	names, err := list()
	if err != nil {
		return 0, fmt.Errorf("cannot enumerate supervisor instances: %w", err)
	}
	gone := 0
	for _, name := range append(names, serviceName) {
		if err := ctx.Err(); err != nil {
			return gone, err
		}
		removed, err := remove(ctx, name)
		if err != nil {
			return gone, fmt.Errorf("supervisor %s operation failed: %w", name, err)
		}
		if removed {
			gone++
		}
	}
	if err := ctx.Err(); err != nil {
		return gone, err
	}
	after, err := list()
	if err != nil {
		return gone, fmt.Errorf("cannot verify supervisor instance set after shutdown: %w", err)
	}
	if err = ctx.Err(); err != nil {
		return gone, err
	}
	known := map[string]bool{}
	for _, name := range names {
		known[name] = true
	}
	for _, name := range after {
		if !known[name] {
			return gone, fmt.Errorf("new supervisor instance appeared during shutdown: %s", name)
		}
	}
	return gone, nil
}

// instances names the per-session clones. Deleting the template does not delete
// them: the owner's probe outlived `sc delete` and stayed AUTO_START, so an
// uninstall that only deletes the template leaks a service per account that has
// signed in. The SCM cannot enumerate by template, but it enumerates every
// service and a clone is <template>_<luid>.
func instances(m windows.Handle) ([]string, error) {
	all, err := (&mgr.Mgr{Handle: m}).ListServices()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range all {
		if strings.HasPrefix(n, serviceName+"_") {
			out = append(out, n)
		}
	}
	return out, nil
}

func deleteService(ctx context.Context, m windows.Handle, name string) (bool, error) {
	return checkedService(ctx, m, name, true)
}
func checkedService(ctx context.Context, m windows.Handle, name string, remove bool) (bool, error) {
	p, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return false, err
	}
	access := uint32(windows.SERVICE_STOP | windows.SERVICE_QUERY_STATUS)
	if remove {
		access |= windows.DELETE
	}
	h, err := windows.OpenService(m, p, access)
	if err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
			return false, nil
		}
		return false, err
	}
	defer windows.CloseServiceHandle(h)
	ops := removalOps{
		query: func() (windows.SERVICE_STATUS_PROCESS, error) {
			var status windows.SERVICE_STATUS_PROCESS
			var needed uint32
			err := windows.QueryServiceStatusEx(h, windows.SC_STATUS_PROCESS_INFO, (*byte)(unsafe.Pointer(&status)), uint32(unsafe.Sizeof(status)), &needed)
			return status, err
		},
		stop: func() error {
			var status windows.SERVICE_STATUS
			return windows.ControlService(h, windows.SERVICE_CONTROL_STOP, &status)
		},
		pin: func(pid uint32) (removalProcess, error) {
			h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
			if err != nil {
				return nil, err
			}
			return retainedServiceProcess{h}, nil
		},
	}
	if remove {
		ops.remove = func() error { return windows.DeleteService(h) }
	}
	return stopAndDeleteService(ctx, ops)
}

type removalProcess interface {
	exited() (bool, error)
	close() error
}
type retainedServiceProcess struct{ handle windows.Handle }

func (p retainedServiceProcess) close() error { return windows.CloseHandle(p.handle) }
func (p retainedServiceProcess) exited() (bool, error) {
	state, err := windows.WaitForSingleObject(p.handle, 0)
	if err != nil {
		return false, err
	}
	switch state {
	case windows.WAIT_OBJECT_0:
		return true, nil
	case uint32(windows.WAIT_TIMEOUT):
		return false, nil
	default:
		return false, fmt.Errorf("unexpected process wait state %d", state)
	}
}

type removalOps struct {
	query  func() (windows.SERVICE_STATUS_PROCESS, error)
	stop   func() error
	pin    func(uint32) (removalProcess, error)
	remove func() error
}

// The SCM supplies the process identity. Pin and requery before requesting stop;
// keep that handle until both STOPPED and process exit are observed. Native SCM
// RPCs retain their OS blocking behavior; every subsequent action checks the
// shared deadline and no deletion occurs after it expires.
func stopAndDeleteService(ctx context.Context, ops removalOps) (bool, error) {
	var process removalProcess
	var pid uint32
	defer func() {
		if process != nil {
			process.close()
		}
	}()
	stopSent := false
	observedActive := false
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		status, err := ops.query()
		if err != nil {
			return false, err
		}
		if err = ctx.Err(); err != nil {
			return false, err
		}
		if status.CurrentState == windows.SERVICE_STOPPED {
			if process == nil && observedActive {
				return false, errors.New("stopped supervisor has no retained process identity to confirm exit")
			}
			if process != nil {
				exited, err := process.exited()
				if err != nil {
					return false, err
				}
				if !exited {
					if err = waitRemoval(ctx); err != nil {
						return false, err
					}
					continue
				}
			}
			if err = ctx.Err(); err != nil {
				return false, err
			}
			if ops.remove != nil {
				if err = ops.remove(); err != nil {
					return false, err
				}
			}
			return true, nil
		}
		observedActive = true
		if status.ServiceType&windows.SERVICE_WIN32_OWN_PROCESS == 0 || status.ServiceType&windows.SERVICE_WIN32_SHARE_PROCESS != 0 {
			return false, errors.New("refusing process-exit assumptions for a shared or unsupported service type")
		}
		if process == nil {
			// SCM does not guarantee a valid PID during START_PENDING or
			// STOP_PENDING. Wait for a stable running/paused identity.
			if status.CurrentState == windows.SERVICE_START_PENDING || status.CurrentState == windows.SERVICE_STOP_PENDING {
				if err = waitRemoval(ctx); err != nil {
					return false, err
				}
				continue
			}
			if status.ProcessId == 0 {
				return false, errors.New("active supervisor has no process identity")
			}
			pid = status.ProcessId
			process, err = ops.pin(pid)
			if err != nil {
				return false, fmt.Errorf("retain supervisor process: %w", err)
			}
			confirmed, err := ops.query()
			if err != nil {
				return false, err
			}
			if confirmed.ProcessId != pid || confirmed.CurrentState == windows.SERVICE_STOPPED {
				return false, errors.New("supervisor process changed while retaining identity")
			}
			continue
		}
		if status.ProcessId != 0 && status.ProcessId != pid {
			return false, errors.New("supervisor process changed during shutdown")
		}
		if !stopSent && status.CurrentState != windows.SERVICE_STOP_PENDING {
			if err = ctx.Err(); err != nil {
				return false, err
			}
			err = ops.stop()
			if err != nil && !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
				return false, fmt.Errorf("stop supervisor: %w", err)
			}
			stopSent = true
		}
		if err = waitRemoval(ctx); err != nil {
			return false, err
		}
	}
}
func waitRemoval(ctx context.Context) error {
	timer := time.NewTimer(25 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// serviceRun is what the SCM starts, and the dispatcher is the whole point of
// it. A per-user service instance is a service: it has ServicesPipeTimeout to
// reach StartServiceCtrlDispatcher, and a process that never does is recorded
// as a start failure, gets no recovery — a start failure is not what
// SERVICE_CONFIG_FAILURE_ACTIONS fires on — and keeps running unsupervised,
// which from outside looks exactly like it worked.
//
// Whether this process is a service is never asked before the dispatcher is
// called, only after it refuses. Every test for it is a heuristic about the
// parent process, and a heuristic that guesses wrong here would skip the
// dispatcher on the one path that must not skip it; guessing wrong about the
// wording of an error costs nothing.
func serviceRun(withRuntime bool) error {
	err := svc.Run(serviceName, supervisor{withRuntime: withRuntime})
	if errors.Is(err, windows.ERROR_FAILED_SERVICE_CONTROLLER_CONNECT) {
		return errors.New("`service run` is how the service manager starts this; `jobd run` is how you supervise here")
	}
	return err
}

type supervisor struct {
	withRuntime bool
	jobs        func(context.Context, []string) error
	runtime     func(context.Context) error
}

func (h supervisor) Execute(scmArgs []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	// scmArgs[0] is the service name; anything after it is what StartService was
	// given, which is the flag vector `jobd run` takes.
	flags := scmArgs
	if len(flags) > 0 {
		flags = flags[1:]
	}

	s <- svc.Status{State: svc.StartPending}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()

	done := make(chan error, 1)
	jobs := h.jobs
	if jobs == nil {
		jobs = serve.JobsContext
	}
	runtime := h.runtime
	if runtime == nil {
		runtime = runRuntime
	}
	go func() { done <- runServiceWork(ctx, flags, h.withRuntime, jobs, runtime) }()

	// Running is reported before the store is open and the bus is listening,
	// which is deliberate. Taking longer than ServicesPipeTimeout to say it is a
	// start failure, and recovery does not fire on those; a supervisor that
	// fails to start after saying Running exits non-zero instead, which with the
	// non-crash flag set is exactly what recovery does fire on.
	//
	// SessionChange is not accepted. A per-user service instance already lives
	// in the session, so the session ending is the SCM stopping this instance,
	// which arrives below as Stop.
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			return failed(err)
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending, WaitHint: uint32(stopWait / time.Millisecond)}
				stop()
				select {
				case err := <-done:
					return failed(err)
				case <-time.After(stopWait):
					return false, 0
				}
			}
		}
	}
}

// failed reports a supervisor that ended badly as a service-specific error
// rather than as a Win32 one, because it is not a Win32 error and the SCM
// prints whichever it is told.
func failed(err error) (bool, uint32) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobd:", err)
		return true, 1
	}
	return false, 0
}

// The registered binary arguments arrive through os.Args. scmArgs contains only
// the separate StartService argument vector, which remains download flags.
//
// `service stop --user <install folder>` is the per-user form of the checked
// upgrade stop. The verb is the same act; --user selects the account's own
// processes under that folder in place of registered SCM instances.
type serviceRequest struct {
	command    string
	runtime    bool
	userFolder string
}

func serviceArguments(args []string) (serviceRequest, error) {
	if len(args) >= 1 && len(args) <= 2 {
		command := args[0]
		enabled := len(args) == 2 && args[1] == "--runtime"
		if (command == "install" || command == "run") && (len(args) == 1 || enabled) {
			return serviceRequest{command: command, runtime: enabled}, nil
		}
		if (command == "uninstall" || command == "stop") && len(args) == 1 {
			return serviceRequest{command: command}, nil
		}
	}
	if len(args) == 3 && args[0] == "stop" && args[1] == "--user" && args[2] != "" {
		return serviceRequest{command: "stop", userFolder: args[2]}, nil
	}
	return serviceRequest{}, errors.New("service install|run [--runtime], service stop [--user <install folder>], or service uninstall")
}

func serviceCommand(exe string, withRuntime bool) string {
	command := windows.EscapeArg(exe) + " service run"
	if withRuntime {
		command += " --runtime"
	}
	return command
}
