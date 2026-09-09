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
// # Why a scheduled task rather than a Windows service
//
// A real service means SCM plumbing and a dependency, and buys one thing over a
// task: jobs owned by LocalSystem keep running while the user is logged off,
// because that account "is always logged on". Under a normal user account BITS
// still survives the application closing and a reboot — it suspends at logoff
// and resumes at logon. For a desktop that is nearly the whole win, at no cost
// and with no elevation. Install it as a SYSTEM task later if logged-off
// transfers turn out to matter.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	config "github.com/openabstractions/abstraction-config/go"
	"github.com/openabstractions/abstraction-download/go"
	_ "github.com/openabstractions/abstraction-download/go/all"
	identity "github.com/openabstractions/abstraction-identity"
	job "github.com/openabstractions/abstraction-job/go"
)

func main() {
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
  jobd start [--without nas]   replace any running supervisor with a detached one
  jobd stop                    stop the one watching this store
  jobd run [--interval 30s]    supervise in the foreground until stopped
  jobd once                    one pass, then exit (what a scheduled task runs)
  jobd install [--at-logon]    register a scheduled task, no elevation needed
  jobd uninstall               remove it
  jobd status [--exit-code]    what is in the store right now; with the flag,
                               exit 1 unless a supervisor is alive (a HEALTHCHECK)
  jobd discover                ask the supervisor over its bus who it is and who
                               it takes this caller for; exit 1 unless it answers
  jobd setup --nas-store <p>   record what this machine has, once, so that every
                               application finds it without being configured
  jobd setup --show            what is configured, and which file said so

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

func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i > 0 {
		return s[:i]
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, "jobd:", err)
	os.Exit(status(err))
}

// storeRoot finds the job store.
//
// ABSTRACTION_STORE is the name that belongs here. jobd supervises downloads of
// anything — it has never known what a model is — and reading MODELGET_STORE
// meant the generic tier was configured by a variable named after one consumer
// sitting above it. MODELGET_STORE is still honoured, because stores exist on
// disk under it and silently ignoring it would orphan jobs.
func storeRoot() string {
	// MODELGET_STORE is honoured for stores that already exist under it;
	// everything else comes from the shared discovery.
	if v := os.Getenv("MODELGET_STORE"); v != "" {
		return v
	}
	root, err := config.JobStore()
	if err != nil {
		fatal(err)
	}
	return root
}

// openRunner uses the same discovery every other application uses.
//
// It used to hand-wire the tiers itself, which meant the supervisor and the
// applications it supervises could disagree about what this machine has. Now
// jobd is just another caller of Discover: on a NAS that finds nothing — no
// BITS, no further NAS to pass work to — and the supervisor does the transfers
// itself, which is exactly what it is there for.
func openRunner(without ...string) (*download.Runner, job.Store, string) {
	store, err := job.NewFileStore(storeRoot())
	if err != nil {
		fatal(err)
	}
	r := download.DiscoverIn(store)
	// Whether other machines write this store is a property of the store, and
	// this used to be set unconditionally. That made installing the supervisor
	// take a capability away: `dl -o C:\models\x.gguf` delivered before jobd
	// existed and afterwards was refused on every sweep forever, because Get
	// absolutises a destination and a shared store refuses every absolute sink.
	// The refusal is right for a store on a share and wrong for the local
	// directory almost every desktop runs, so the store is asked and, where a
	// share does not show in the path, the operator says.
	r.SharedStore = download.SharedStoreRoot(storeRoot()) || os.Getenv("ABSTRACTION_SHARED_STORE") != ""
	// An operator may run this supervisor one tier lower than the machine would
	// choose, to see what the next one down actually does. Applications get no
	// such control and should not: they do not know what a tier is.
	for _, w := range without {
		if w != "" {
			r.NotServing = append(r.NotServing, w)
		}
	}
	return r, store, r.Rebind()
}

// pass is one sweep, and the order matters.
//
// Reconcile first: a delegated job that has finished needs finalising and
// verifying, and doing that before adopting means the orphan pass does not pick
// up work the delegate has in fact already completed.
// The counts, and what went wrong. Both matter: a sweep that reports only what
// it managed describes a store where nothing needs attention exactly as it
// describes one where a job fails on every single pass.
//
// That is not hypothetical. `reconciled=2` printed every five seconds for two
// hours while one transfer could not progress at all — its error thrown away
// here, by taking the count only when err was nil.
func pass(ctx context.Context, r *download.Runner) (reconciled, delegated, adopted, delivered int, problems []error) {
	note := func(stage string, err error) {
		if err != nil {
			problems = append(problems, fmt.Errorf("%s: %w", stage, err))
		}
	}

	if r.Delegators != nil {
		n, err := r.ReconcileAll(ctx)
		reconciled, _ = n, err
		note("reconcile", err)

		// Then offer anything unclaimed to a better tier. This is the second hop
		// of the chain, and it was missing: applications handed work to this
		// supervisor, and the supervisor downloaded everything itself because
		// nothing ever asked "should this go somewhere better?". A configured,
		// reachable, registered NAS was never used.
		n, err = r.DelegateAll(ctx)
		delegated = n
		note("delegate", err)
	}
	// Whatever nobody else wanted is still ours to finish.
	n, err := r.Adopt(ctx)
	adopted = n
	note("adopt", err)

	// And close out work that is demonstrably done. Without this a finished
	// download waits forever for an acknowledgement from a process that may
	// never come back, and shows up in a download manager as a row stuck at
	// 100% labelled "paused" — for a file that is complete on disk.
	n, err = r.TakeDeliveryAll(ctx)
	delivered = n
	note("deliver", err)

	return reconciled, delegated, adopted, delivered, problems
}

// dropFolder submits through the client so that a request for bytes the store
// already holds, or is already fetching, becomes that job rather than a second
// one.
func dropFolder(r *download.Runner) (download.Wanted, download.Client) {
	svc := download.NewClient(r)
	submit := func(s download.Spec) (string, error) {
		h, err := svc.Submit(s)
		if err != nil {
			return "", err
		}
		return h.ID(), nil
	}
	return download.Wanted{Store: r.Store, Submit: submit}, svc
}

// sweep answers what has finished, then takes in what is new.
func sweep(w download.Wanted) (taken []string, problems []error) {
	if err := w.Answer(); err != nil {
		return nil, []error{fmt.Errorf("wanted: %w", err)}
	}
	taken, err := w.TakeIn()
	if err != nil {
		problems = append(problems, fmt.Errorf("wanted: %w", err))
	}
	return taken, problems
}

// watchWanted is the drop folder's own loop, beside the sweep rather than
// inside it: a pass that fetches a file is one pass, and a person watching the
// folder should see the request move while that happens. It answers on every
// change to a download record and looks for new files on the sweep interval.
// Taking one in submits through the client, whose nudge wakes the pass that
// adopts it.
func watchWanted(ctx context.Context, w download.Wanted, store job.Store, every time.Duration) {
	sub := job.Watch(store, download.Kind)
	defer sub.Close()
	t := time.NewTicker(every)
	defer t.Stop()
	said := map[string]bool{}
	for {
		taken, problems := sweep(w)
		if len(taken) > 0 {
			fmt.Printf("%s  wanted=%d\n", time.Now().Format(time.RFC3339), len(taken))
		}
		complain(problems, said)
		select {
		case <-ctx.Done():
			return
		case <-sub.Changes():
		case <-t.C:
		}
	}
}

// complain says each distinct problem once, not once per sweep. A stuck job
// fails identically every time, and a supervisor polling every five seconds
// would write the same line 17,000 times a day and bury everything else.
func complain(problems []error, said map[string]bool) {
	for _, p := range problems {
		if msg := p.Error(); !said[msg] {
			said[msg] = true
			fmt.Fprintf(os.Stderr, "jobd: %s\n", msg)
		}
	}
}

func cmdOnce(args []string) {
	fs := flag.NewFlagSet("once", flag.ContinueOnError)
	quiet := fs.Bool("quiet", false, "say nothing unless something happened")
	need(fs, args)

	r, _, tier := openRunner()
	defer r.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
	defer cancel()

	rec, del, ad, dlv, problems := pass(ctx, r)
	wanted, svc := dropFolder(r)
	taken, more := sweep(wanted)
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

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	interval := fs.Duration("interval", 30*time.Second, "how often to sweep")
	var without systems
	fs.Var(&without, "without", `ignore a delegation system; repeatable, e.g. --without nas --without bits`)
	need(fs, args)

	r, store, tier := openRunner(without...)
	defer r.Close()
	fmt.Printf("jobd: watching %s (delegates to: %s)\n", storeRoot(), tier)
	wanted, _ := dropFolder(r)
	dropDir, dropErr := wanted.Dir()
	if dropErr == nil {
		fmt.Printf("jobd: a text file put in %s becomes a download\n", dropDir)
	} else {
		fmt.Fprintf(os.Stderr, "jobd: no drop folder (%v)\n", dropErr)
	}

	// Announce, so applications on this machine stop downloading things
	// themselves. This is the entire discovery protocol: a file in the store both
	// sides can already see. An application asks "is a system downloader alive
	// here", never "is there a NAS" — which tier this supervisor uses is its
	// business, one hop further down.
	owner := download.Owner()
	// What this supervisor delegates to is now a live answer rather than one
	// taken at startup, so the sweep publishes it and the beat reads it. Two
	// goroutines, one field the sweep owns: r.Delegators is written by Rebind
	// and read all over the download package, and reading it from here would be
	// a race for a string.
	serving := &atomic.Pointer[string]{}
	serving.Store(&tier)
	// The bus before the heartbeat, because the heartbeat is how a caller
	// learns the bus's name. Without one the supervisor is still a supervisor:
	// it sweeps, and applications reach it through the store alone.
	var looks <-chan struct{}
	endpoint := ""
	if bus, err := download.ListenBus(owner, func() string { return *serving.Load() }); err == nil {
		defer bus.Close()
		looks, endpoint = bus.C(), bus.Endpoint
		l := identity.Ceiling()
		fmt.Printf("jobd: listening at %s (%s/%s; callers bound by %s)\n", endpoint, l.Platform, l.Transport, firstSentence(l.Binding))
	} else {
		fmt.Fprintf(os.Stderr, "jobd: no bus (%v); reachable through the store only, sweeping on the timer\n", err)
	}
	if err := download.Heartbeat(store, owner, tier, endpoint, *interval); err != nil {
		fmt.Fprintf(os.Stderr, "jobd: could not announce (%v); applications will download in-process\n", err)
	}
	// Stop announcing on a clean exit, so nothing hands work to a supervisor that
	// has gone. A kill leaves the heartbeat behind, which is why readers treat it
	// as stale rather than trusting it forever.
	defer download.StopHeartbeat(store)

	// Stop cleanly on Ctrl+C or a service stop. An interrupted sweep is safe —
	// the lease lapses and the next owner continues — but exiting tidily
	// releases it immediately instead of after the expiry.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Announce on a clock of its own, because the thing that starves a heartbeat
	// is the work it is reporting on.
	//
	// This used to beat once per sweep, refreshed before the work rather than
	// after it, which is as far as one thread can get. It is not far enough: a
	// pass that fetches a 40 GB file IS a single sweep, so nothing was written
	// for as long as that took, the heartbeat aged past stale, and every
	// application on the machine was told nothing was watching — while this
	// process was in the middle of doing the work for them. Observed on a 1.5 GB
	// download: `jobd status` said "treated as gone" about a supervisor that was
	// busy on its behalf.
	//
	// Nothing is synchronised. The heartbeat is written by atomic rename, and it
	// reports liveness rather than progress, so a beat that lands mid-sweep says
	// exactly what it should: this process is still here.
	go func() {
		beat := time.NewTicker(*interval)
		defer beat.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-beat.C:
				download.Heartbeat(store, owner, *serving.Load(), endpoint, *interval)
			}
		}
	}()

	if dropErr == nil {
		go watchWanted(ctx, wanted, store, *interval)
	}

	t := time.NewTicker(*interval)
	defer t.Stop()
	// What has already been complained about, so a persistent failure is
	// reported when it starts rather than on every sweep forever.
	said := map[string]bool{}
	for {
		// Before the work, so a switch thrown in the control panel is obeyed by
		// this sweep rather than the one after it. Costs two small file reads
		// unless the answer changed.
		if now := r.Rebind(); now != *serving.Load() {
			serving.Store(&now)
			download.Heartbeat(store, owner, now, endpoint, *interval)
			fmt.Printf("%s  delegates-to=%s\n", time.Now().Format(time.RFC3339), now)
		}
		rec, del, ad, dlv, problems := pass(ctx, r)
		if rec > 0 || del > 0 || ad > 0 || dlv > 0 {
			fmt.Printf("%s  reconciled=%d delegated=%d adopted=%d delivered=%d\n",
				time.Now().Format(time.RFC3339), rec, del, ad, dlv)
		}
		complain(problems, said)
		select {
		case <-ctx.Done():
			fmt.Println("jobd: stopping. Anything in flight keeps its checkpoint.")
			return
		case <-looks:
		case <-t.C:
		}
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
// exit and a reboot. Running as SYSTEM would additionally survive a logoff and
// does need elevation — see the package comment.
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

// systems collects a flag that may be given more than once.
//
// It was a plain string, and openRunner has always taken a variadic list, so
// `--without nas --without bits` parsed without complaint and silently kept
// only the last one. A documented escape hatch that quietly ignores half of
// what it is told is worse than one that refuses: the operator watches the
// supervisor keep using the tier they just excluded.
type systems []string

func (s *systems) String() string { return strings.Join(*s, ",") }

func (s *systems) Set(v string) error {
	// Comma-separated too, because an operator typing this once should not have
	// to know which of the two spellings this program happens to accept.
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			*s = append(*s, part)
		}
	}
	return nil
}
