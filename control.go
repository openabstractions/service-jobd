package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	download "github.com/openabstractions/abstraction-download/go"
	"github.com/openabstractions/abstraction-download/go/serve"
	job "github.com/openabstractions/abstraction-job/go"
)

// This file is jobd controlling jobd: start it in the background, stop it, and
// say whether one is running.
//
// It lives in the tool rather than in a shell script on purpose. A script would
// have to open supervisor.json and pick the pid out of the JSON — reaching
// through the abstraction into the file binding, which is the thing this project
// spends its time removing. jobd already owns that file. A script would also
// only run on the machine it was written for, and the other end of this chain is
// a Linux NAS.

// supervisorPID returns the process id of the supervisor watching this store,
// and whether it is alive.
//
// The heartbeat carries owner as "program@host:pid". Trusting the pid alone
// would be wrong across machines — a NAS supervisor's pid means nothing here —
// so the host has to match before the number is worth anything.
func supervisorPID(store job.Store) (int, bool) {
	sup, live := download.SupervisorOf(store)
	if !live {
		return 0, false
	}
	host, _ := os.Hostname()
	if !strings.EqualFold(sup.Host, host) {
		return 0, false
	}
	i := strings.LastIndex(sup.Owner, ":")
	if i < 0 {
		return 0, false
	}
	pid, err := strconv.Atoi(sup.Owner[i+1:])
	if err != nil {
		return 0, false
	}
	return pid, true
}

// cmdStop ends the supervisor watching this store. Nothing it was doing is lost:
// leases lapse, records stay, and the next supervisor adopts the work.
func cmdStop(args []string) {
	need(flag.NewFlagSet("stop", flag.ContinueOnError), args)
	_, store, _ := openRunner()
	sup, live := download.SupervisorOf(store)
	if !live {
		fmt.Println("no supervisor is running on this store")
		download.StopHeartbeat(store)
		return
	}
	pid, ok := supervisorPID(store)
	if !ok {
		fmt.Printf("a supervisor is announced as %s, but not on this machine — stop it there\n", sup.Owner)
		return
	}
	p, err := os.FindProcess(pid)
	if err == nil {
		_ = p.Kill()
	}
	// Remove the announcement even if the kill raced, so applications stop
	// handing work to something that has gone. A stale heartbeat is treated as
	// dead after a few intervals anyway; this just makes it immediate.
	download.StopHeartbeat(store)
	fmt.Printf("stopped %s\n", sup.Owner)
}

// cmdStart asks for a supervisor and says whether it made one.
//
// It used to stop whatever was running first and then sleep 300ms so the dying
// process could let go of its handles. With a fixed endpoint that sleep became
// load-bearing and wrong at once: the new supervisor asks for the name the old
// one may still hold, and losing that race degrades it to no bus at all. The
// answer is not a longer sleep. The endpoint admits one listener, so asking for
// a supervisor when one is already there is answered by the one that is there —
// `jobd stop` is still how you replace it, and a start no longer needs to know
// whether it is the first.
func cmdStart(args []string) {
	fs := flag.NewFlagSet("start", flag.ContinueOnError)
	withRuntime := fs.Bool("runtime", false, "require the installed sibling runtime")
	interval := fs.Duration("interval", 30*time.Second, "how often to sweep")
	endpoint := fs.String("endpoint", download.DefaultEndpoint(), "where applications connect")
	var without serve.Systems
	fs.Var(&without, "without", `run one tier lower by ignoring a system; repeatable, e.g. --without nas --without bits`)
	need(fs, args)

	self, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	childArgs := []string{"run"}
	if *withRuntime {
		childArgs = append(childArgs, "--runtime")
	}
	childArgs = append(childArgs, "--interval", interval.String(), "--endpoint", *endpoint)
	for _, w := range without {
		childArgs = append(childArgs, "--without", w)
	}
	logPath := filepath.Join(storeRoot(), LogName)

	got, err := startWithRuntime(context.Background(), download.Starting{
		Endpoint: *endpoint, Exe: self, Args: childArgs, Log: logPath}, *withRuntime, download.StartSupervisor, checkRuntimeAvailable)
	if err != nil {
		fmt.Fprintf(os.Stderr, "jobd: %v. See %s\n", err, logPath)
		os.Exit(1)
	}
	if *withRuntime {
		fmt.Printf("supervisor and runtime available at %s\n", got.Endpoint)
		return
	}
	if got.Already {
		fmt.Printf("already running: %s at %s\n", got.Owner, got.Endpoint)
		return
	}
	fmt.Printf("started %s\n", got.Owner)
	fmt.Printf("  store        %s\n", storeRoot())
	fmt.Printf("  listening at %s\n", got.Endpoint)
	fmt.Printf("  delegates to %s\n", got.Tier)
	fmt.Printf("  log          %s\n", got.Log)
}

// A bus response establishes supervisor availability. Runtime readiness is a
// separate capability check and does not establish ownership of that process.
func startWithRuntime(ctx context.Context, options download.Starting, required bool,
	start func(download.Starting) (download.Started, error), check func(context.Context) error) (download.Started, error) {
	if required {
		options.Within = 20 * time.Second
	}
	got, err := start(options)
	if err != nil {
		return got, err
	}
	if required {
		if err := check(ctx); err != nil {
			return got, fmt.Errorf("supervisor answered; runtime availability failed: %w", err)
		}
	}
	return got, nil
}
