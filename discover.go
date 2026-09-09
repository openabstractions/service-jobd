package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	download "github.com/openabstractions/abstraction-download/go"
	job "github.com/openabstractions/abstraction-job/go"

	"github.com/openabstractions/service-jobd/discovery"
)

// This file is jobd's half of local discovery: it publishes the endpoint, it
// serves it, and it applies the rule that decides whether the socket or the
// heartbeat file is the thing to believe.
//
// Both still exist and that is not a transition state. A local socket cannot
// cross to another machine, and when the supervisor is a NAS the only thing
// both ends share is the store — so the file is not legacy, it is the
// cross-machine mechanism, and removing it breaks the NAS path outright.

// services is what one jobd process publishes.
//
// Two names, one endpoint, because this process serves both. Split them into
// two processes tomorrow and it publishes two endpoints; no caller can tell
// which happened, and no caller may try. That the count of processes is absent
// from every signature here is the entire point of the topology contract.
var services = []string{discovery.ServiceJobs, discovery.ServiceDownloads}

// listenLocally claims the service names, binds the endpoint and starts
// answering.
//
// Failure is fatal, and deliberately. A supervisor that could not listen and
// carried on is a machine where every application quietly downloads in-process
// and nobody can find out why: the contract's "fail loudly at startup, never
// silently decline to listen" is the only behaviour that is debuggable.
func listenLocally(root, tier string, started time.Time) *discovery.Server {
	host, _ := os.Hostname()

	endpoint, err := discovery.Claim(root, "", services...)
	if err != nil {
		fatal(fmt.Errorf("cannot claim a local endpoint: %w", err))
	}

	srv, err := discovery.Serve(endpoint, discovery.Identity{
		Content:  []string{discovery.Base},
		Critical: []string{discovery.Base},
		Owner:    download.Owner(),
		Host:     host,
		Store:    root,
		// Advisory, and a client must work without understanding it. It is
		// here so a human can see the whole chain in one place, which is the
		// same reason the heartbeat carries it.
		DelegatesTo: tier,
		PID:         os.Getpid(),
		StartedAt:   started,
	})
	if err != nil {
		// Do not leave a registry entry pointing at an endpoint nobody serves
		// for longer than it takes to notice.
		discovery.Withdraw(root, endpoint, services...)
		fatal(fmt.Errorf("cannot serve local discovery: %w", err))
	}
	return srv
}

// stopListening is listenLocally's shutdown half: withdraw the registry entry
// before closing the socket, so a racing query never finds a name claimed by
// nobody.
func stopListening(srv *discovery.Server, root string) {
	discovery.Withdraw(root, srv.Endpoint(), services...)
	srv.Close()
}

// presence is what the machine knows about a supervisor for this store.
type presence struct {
	// state is the three-state answer when IPC decided; absent otherwise.
	state discovery.State
	// byIPC says which mechanism produced this. The two are never combined.
	byIPC bool
	resp  *discovery.Response
	// heartbeat is populated only when the file decided, i.e. the supervisor
	// is on another machine.
	heartbeat download.Supervisor
	live      bool
}

// locate answers "is a supervisor watching this store" by exactly one
// mechanism, chosen by where the supervisor is.
//
// If the heartbeat names THIS host, the socket is authoritative and the file is
// ignored entirely — including when the socket says absent. That is the whole
// value of the change: a heartbeat written thirty seconds ago by a process that
// has since been killed says "running", and everything that believed it handed
// work into a store nobody was watching.
//
// If it names another host, the freshness heuristic applies exactly as before,
// because a local socket does not reach a NAS.
//
// Never both. Combining them would reintroduce the lie in whichever direction
// the combination favoured.
func locate(store job.Store, root string) presence {
	sup, live := download.SupervisorOf(store)
	host, _ := os.Hostname()

	// No record at all: nobody has ever announced here. The socket is still
	// worth asking, because it is local and it cannot be stale. The contract
	// covers "names this host" and "names another host" and is silent on this
	// third case, which is the state every fresh store is in.
	if sup.Host == "" {
		a := discovery.Query(root)
		return presence{state: a.State, byIPC: true, resp: a.Supervisor}
	}
	if strings.EqualFold(sup.Host, host) {
		a := discovery.Query(root)
		return presence{state: a.State, byIPC: true, resp: a.Supervisor}
	}
	return presence{byIPC: false, heartbeat: sup, live: live}
}

// cmdDiscover prints the three-state answer, for a human and for conformance
// runs from another language against this server.
func cmdDiscover(args []string) {
	service := discovery.ServiceJobs
	ask := discovery.AskWho
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--service":
			if i+1 < len(args) {
				i++
				service = args[i]
			}
		case "--look":
			ask = discovery.AskLook
		}
	}
	root := storeRoot()
	start := time.Now()
	a := discovery.Ask(root, service, ask)
	took := time.Since(start)

	fmt.Printf("%s  (%s, %s)\n", a.State, service, took.Round(time.Millisecond))
	if a.Why != "" {
		fmt.Printf("  why          %s\n", a.Why)
	}
	if a.Supervisor != nil {
		fmt.Printf("  owner        %s\n", a.Supervisor.Owner)
		fmt.Printf("  host         %s\n", a.Supervisor.Host)
		fmt.Printf("  store        %s\n", a.Supervisor.Store)
		fmt.Printf("  pid          %d\n", a.Supervisor.PID)
		fmt.Printf("  started      %s\n", a.Supervisor.StartedAt)
		fmt.Printf("  delegates to %s\n", a.Supervisor.DelegatesTo)
		fmt.Printf("  content      %s\n", strings.Join(a.Supervisor.Content, " "))
		fmt.Printf("  critical     %s\n", strings.Join(a.Supervisor.Critical, " "))
	}
	// Absent is not a failure. Exit status says so, because a script that
	// treats "no supervisor" as an error is the thing this whole contract is
	// trying to stop people writing.
	if reg, err := discovery.LoadRegistry(root); err == nil && len(reg.Services) > 0 {
		fmt.Printf("  registry     %s\n", strings.Join(reg.ServiceNames(), " "))
	}
}
