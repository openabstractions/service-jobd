// jobd is the supervisor: the thing that finishes work nobody is watching.
//
// It does not move bytes. BITS does that, or a NAS does, or the in-process
// fetchers do. jobd exists because every one of those leaves a gap that only
// something long-running can close:
//
//   - **Delivery.** A delegated transfer that finishes while no application is
//     open sits there. BITS will not release the file until someone calls
//     Complete(), and nothing verifies the digest until someone asks. Without a
//     supervisor that happens the next time a human runs a command, which may be
//     days later — the bytes arrived on Tuesday and the file appeared on Friday.
//   - **Orphans.** A machine that reboots mid-transfer leaves a job with an
//     expired lease and a half-written file. Something has to pick it up.
//   - **One answer to "what is downloading?"** Every tool keeps that in its own
//     memory today, which is why a console pull never shows up in an app's
//     download list and why restarting a server empties it while the partial
//     files are still on disk.
//
// It is deliberately small: a loop over the job store, plus the two calls the
// download layer already exposes. Everything hard lives below it.
//
// # Why a per-user service on Windows
//
// A scheduled task fires and exits, and nothing restarts it when it dies
// mid-transfer. A per-user service is the facility that both starts at sign-in
// and is restarted by the service manager when it fails, and it stores no
// account and no password because it runs as whoever signed in. `jobd service
// install` registers it and the installer's custom action is what calls that;
// `jobd install` still prints the scheduled task, which is the fallback for an
// account with no administrator token. VISION.md 2026-09-10 "Host S is
// unnecessary" is the run that decided this.
//
// # Two images, one program
//
// A per-user service instance runs inside the interactive session, so the
// loader gives a console image a console window at every sign-in — the
// subsystem is a bit in the image header, and minimized by a shortcut is still
// a taskbar button. That is why this package is linked twice. jobd.exe is the
// console program a person types at; jobdw.exe is the same source linked
// -H=windowsgui, and it is the image the service manager is given, because
// service install registers jobdw.exe and refuses when it is not there. Which
// build produces which image is the subsystem column of installer/payload.tsv.
// Named the way the platform has named this pair since pythonw.exe and
// javaw.exe.
//
// Nothing branches on which image is running. The one thing that differs is
// that the windows-subsystem image has no standard output, and speakSomewhere
// is where that is answered, for both.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/openabstractions/abstraction-download/go"
	"github.com/openabstractions/abstraction-download/go/serve"
	job "github.com/openabstractions/abstraction-job/go"
)

func main() {
	speakSomewhere()
	// Bare `jobd` answers the question somebody typing it is actually asking —
	// is anything running, and what is it doing — rather than printing usage at
	// them. Usage is still one keystroke away as `jobd help`.
	if len(os.Args) < 2 {
		cmdStatus(nil)
		return
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "start":
		cmdStart(os.Args[2:])
	case "stop":
		cmdStop(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	case "once":
		cmdOnce(os.Args[2:])
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	case "discover":
		cmdDiscover(os.Args[2:])
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Println(`jobd — finishes transfers nobody is watching

  jobd                         is one running, and what is it doing
  jobd start [--without nas]   make sure a detached supervisor is running; if one
                               already answers, say so and leave it alone
  jobd stop                    stop the one watching this store
  jobd run [--interval 30s]    supervise in the foreground until stopped
  jobd once                    one pass, then exit (what a scheduled task runs)
  jobd install [--at-logon]    register a scheduled task, no elevation needed
  jobd uninstall               remove it
  jobd service install         register the per-user service the installer
                               registers: it starts in your session at sign-in
                               and windows restarts it if it dies. Needs an
                               administrator token
  jobd service uninstall       remove it, and the per-session copies windows
                               made of it
  jobd status [--exit-code]    what is in the store right now; with the flag,
                               exit 1 unless a supervisor is alive (a HEALTHCHECK)
  jobd discover                ask the supervisor over its bus who it is and who
                               it takes this caller for; exit 1 unless it answers
  jobd setup --nas-store <p>   record what this machine has, once, so that every
                               application finds it without being configured
  jobd setup --show            what is configured, and which file said so

on windows there is a second image:
  jobdw.exe is this program with no console, which is what the installer starts
  at logon so that nothing appears on the desktop. It takes the same commands
  and prints nowhere a terminal can see: everything it says goes to jobd.log in
  the store. Type jobd, not jobdw

a window instead:
  the control panel (monitor) finds a NAS on the network, switches a tier off
  with a reason, and tests the result. A running supervisor picks up every one
  of those on its next sweep; nothing here has to be restarted

env:
  ABSTRACTION_STORE      the job store (default ~/.abstraction). Any tool that
                         speaks the job record shares it — jobd knows nothing
                         about what is being downloaded
  MODELGET_STORE         honoured as a legacy alias
  ABSTRACTION_NAS_STORE  a store on a share watched by a jobd elsewhere; when
                         set and reachable, work is handed there rather than run
  ABSTRACTION_SHARED_STORE  set to anything when other machines write this store
                         through a mount whose path does not say so — a mapped
                         drive, an NFS mount, a NAS supervising its own export.
                         Absolute sinks are then left to the machine that named
                         them. A UNC store root is recognised without this

the drop folder:
  a text file put in <store>/wanted/ is a request: a URL per line, optionally a
  sha256:<hex> and a destination inside the store. The folder answers by
  renaming it: .accepted, then .done or .failed; .refused says which line and why`)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "jobd:", err)
	os.Exit(status(err))
}

// storeRoot and openRunner are the library's, wrapped so this tool keeps
// ending the process on a store it cannot open. Everything about how a store is
// found lives in abstraction-download/go/serve, because the supervisor and this tool must
// not be able to disagree about it.
func storeRoot() string {
	root, err := serve.StoreRoot()
	if err != nil {
		fatal(err)
	}
	return root
}

func openRunner(without ...string) (*download.Runner, job.Store, string) {
	r, store, tier, err := serve.OpenRunner(without...)
	if err != nil {
		fatal(err)
	}
	return r, store, tier
}

func cmdOnce(args []string) {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "say nothing unless something happened")
	need(fs, args)

	r, _, tier := openRunner()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	rec, del, ad, dlv, problems := serve.Pass(ctx, r)
	wanted, svc := serve.DropFolder(r)
	taken, more := serve.Sweep(wanted)
	problems = append(problems, more...)
	// With no supervisor announced, the client runs each request in this
	// process; one pass is one sweep, so this pass waits for what it took in.
	for _, id := range taken {
		if _, err := svc.Deliver(ctx, id); err != nil {
			problems = append(problems, fmt.Errorf("wanted: %w", err))
		}
	}
	if len(taken) > 0 {
		problems = append(problems, sweepErrors(wanted.Answer())...)
	}
	// Problems are never quiet. --quiet means "say nothing when there was
	// nothing to do", not "hide work that failed".
	for _, p := range problems {
		fmt.Fprintf(os.Stderr, "jobd: %v\n", p)
	}
	if *quiet && rec == 0 && del == 0 && ad == 0 && dlv == 0 && len(taken) == 0 && len(problems) == 0 {
		return
	}
	fmt.Printf("%s  reconciled=%d delegated=%d adopted=%d delivered=%d wanted=%d delegates-to=%s\n",
		time.Now().Format(time.RFC3339), rec, del, ad, dlv, len(taken), tier)
}

func sweepErrors(err error) []error {
	if err != nil {
		return []error{fmt.Errorf("wanted: %w", err)}
	}
	return nil
}

// cmdRun is the supervisor loop, and it is the library's: the same function
// answers `openabstractions serve jobd`, so the two can never drift into two
// supervisors that behave differently.
func cmdRun(args []string) {
	if err := serve.Jobs(args); err != nil {
		fatal(err)
	}
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	exitCode := fs.Bool("exit-code", false, "exit 1 unless a supervisor is alive on this store")
	need(fs, args)

	r, store, tier := openRunner()
	defer r.Close()
	all, err := store.List()
	if err != nil {
		fatal(err)
	}
	fmt.Printf("store: %s\n", storeRoot())
	// Whether a supervisor is ALIVE is the first thing anyone typing this wants
	// to know, and it was the one thing missing: the old output described what
	// tier this process would use if it ran, which reads as a running service
	// when nothing is running at all.
	sup, live := download.SupervisorOf(store)
	if *exitCode {
		defer func() {
			if !live {
				os.Exit(1)
			}
		}()
	}
	if live {
		where := sup.Tier
		if where == "" {
			where = "here"
		}
		fmt.Printf("supervisor: %s, running, delegates to: %s\n\n", sup.Owner, where)
	} else if sup.Owner != "" {
		fmt.Printf("supervisor: none (%s last seen %s, treated as gone)\n", sup.Owner, sup.Seen.Format(time.RFC3339))
		fmt.Printf("            nothing is finishing these; `jobd start` fixes that\n")
		fmt.Printf("if started here it would delegate to: %s\n\n", tier)
	} else {
		fmt.Printf("supervisor: none — downloads only run while an application is open\n")
		fmt.Printf("            `jobd start` changes that\n")
		fmt.Printf("if started here it would delegate to: %s\n\n", tier)
	}
	n := 0
	for _, rec := range all {
		if rec.Kind != download.Kind {
			continue
		}
		n++
		spec, _ := download.SpecOf(rec)
		who := "here"
		if rec.Delegated() {
			who = rec.Delegation.System
		}
		pct := ""
		if rec.Progress.Total > 0 {
			pct = fmt.Sprintf("%3.0f%%", 100*float64(rec.Progress.Done)/float64(rec.Progress.Total))
		}
		// Only flag work that is genuinely stalled. StateTransferred is finished
		// and proven, waiting to be acknowledged, so nobody should be working on
		// it — saying "nobody is working on it" there reads as a problem when it
		// is the expected end state.
		note := ""
		switch {
		case rec.State == job.StateTransferred:
			note = "  (done, waiting to be taken delivery of)"
		case rec.Paused():
			// Before every other reading, because a paused job is delegated,
			// claimable and not terminal all at once — so every other branch
			// here would describe it as something it is not. Saying "nas is
			// working on it" about a transfer somebody just stopped is the same
			// class of lie as calling a finished one paused.
			by := ""
			if rec.Intent != nil && rec.Intent.By != "" {
				by = " by " + rec.Intent.By
			}
			note = "  (paused" + by + ")"
		case rec.Delegated() && !rec.Delegation.Delivered && !rec.State.Terminal():
			// A delegated job holds no lease HERE and never will, because the
			// work is happening somewhere else — so "claimable" says yes and
			// means nothing. Calling that stalled told a person their download
			// had died while a NAS was actively fetching it, which is the most
			// alarming possible way to be wrong.
			//
			// Unless nothing in this process can speak to that delegate. Then
			// "it is working on it" is a claim this supervisor has no way to
			// make, and the job is going nowhere from here whatever the
			// delegate is in fact doing. The record's own line, printed below,
			// says which kind of missing it is.
			if _, ok := r.Delegators.BySystem(rec.Delegation.System); !ok {
				note = "  (stuck — nothing here can speak to " + rec.Delegation.System + ")"
			} else {
				note = "  (" + rec.Delegation.System + " is working on it)"
			}
		case store.Claimable(rec) && !rec.State.Terminal():
			// Waiting out a failure is not being stalled, and this file already
			// knows what calling one the other costs: a person told their
			// download had died while something was in fact going to finish it.
			if at := download.RetryAfter(rec); time.Now().Before(at) {
				note = "  (last attempt failed — trying again " + at.Format(time.Kitchen) + ")"
			} else {
				note = "  (stalled — nobody is working on it)"
			}
		}
		claimable := note
		fmt.Printf("%-12s %-5s %-10s %s%s\n", rec.State, pct, who, filepath.Base(spec.Sink.Final), claimable)
		// Which phase, when the work has more than one. This is what a delegated
		// download was missing: the far side finishes, and then the bytes still
		// have to cross a share and be re-hashed, which used to show as a job
		// sitting at 100% doing nothing for minutes.
		if st := rec.Progress.Step; st != nil {
			where := ""
			if st.Total > 0 {
				where = fmt.Sprintf(" %3.0f%%", 100*float64(st.Done)/float64(st.Total))
			}
			if st.Of > 0 {
				fmt.Printf("             step %d/%d %s%s\n", st.Ordinal, st.Of, st.Name, where)
			} else {
				fmt.Printf("             %s%s\n", st.Name, where)
			}
		}
		if rec.Error != "" {
			fmt.Printf("             %s\n", rec.Error)
		}
	}
	if n == 0 {
		fmt.Println("nothing here.")
	}
}

const taskName = "jobd"

// cmdInstall registers a scheduled task. No elevation: it runs as the current
// user, which is enough for BITS to keep transferring across an application
// exit and a reboot. It is the fallback for an account that cannot reach
// `jobd service install`, which needs an administrator token.
func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	atLogon := fs.Bool("at-logon", true, "also run at logon, not only on a timer")
	every := fs.Int("every-minutes", 5, "how often the task sweeps")
	need(fs, args)

	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	cmd := fmt.Sprintf(`"%s" once --quiet`, exe)

	fmt.Println("Run these once, in a normal (non-elevated) terminal:")
	fmt.Println()
	if *atLogon {
		fmt.Printf("  schtasks /create /tn %s-logon /sc onlogon /tr %s /f\n", taskName, quote(cmd))
	}
	fmt.Printf("  schtasks /create /tn %s /sc minute /mo %d /tr %s /f\n", taskName, *every, quote(cmd))
	fmt.Println()
	fmt.Println("Then check it with:  jobd status")
	fmt.Println()
	fmt.Println("Deliberately printed rather than executed: registering a scheduled")
	fmt.Println("task is a change to your machine, and you should see exactly what it")
	fmt.Println("is before it happens.")
}

func cmdUninstall(args []string) {
	need(flag.NewFlagSet("uninstall", flag.ContinueOnError), args)
	fmt.Println("Run these once:")
	fmt.Println()
	fmt.Printf("  schtasks /delete /tn %s /f\n", taskName)
	fmt.Printf("  schtasks /delete /tn %s-logon /f\n", taskName)
}

func quote(s string) string { return `"` + s + `"` }
