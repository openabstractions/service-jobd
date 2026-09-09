package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/openabstractions/service-jobd/discovery"
)

// End to end, against the real binary and a real endpoint.
//
// The unit tests exercise the protocol; this exercises the daemon: that it
// claims its names before it announces anything, that a second supervisor is
// refused rather than allowed to shadow the first, that killing it makes the
// answer absent immediately instead of after a staleness window, and that the
// heartbeat file — the thing the NAS path depends on — is still written.

func buildJobd(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "jobd")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return bin
}

func jobdCmd(bin, store string, args ...string) *exec.Cmd {
	c := exec.Command(bin, args...)
	c.Env = append(os.Environ(),
		"ABSTRACTION_STORE="+store,
		// The legacy alias would otherwise win over the variable under test.
		"MODELGET_STORE=",
	)
	return c
}

func TestSupervisorAnswersAndStopsAnswering(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildJobd(t)
	store := t.TempDir()

	var log bytes.Buffer
	run := jobdCmd(bin, store, "run", "--interval", "60s")
	run.Stdout, run.Stderr = &log, &log
	if err := run.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() {
		if run.Process != nil {
			run.Process.Kill()
			run.Wait()
		}
	}()

	// Present, by connection, not by file.
	var a discovery.Answer
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a = discovery.Query(store); a.State == discovery.Present {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.State != discovery.Present {
		t.Fatalf("supervisor never answered: %v (%s)\n%s", a.State, a.Why, log.String())
	}
	if a.Supervisor.PID != run.Process.Pid {
		t.Errorf("answered pid %d, process is %d", a.Supervisor.PID, run.Process.Pid)
	}
	if !discovery.SameStore(a.Supervisor.Store, store) {
		t.Errorf("answered store %q, asked about %q", a.Supervisor.Store, store)
	}

	// Both service names, one endpoint, 128 random bits of it.
	reg, err := discovery.LoadRegistry(store)
	if err != nil {
		t.Fatalf("registry: %v", err)
	}
	jobs, downloads := reg.Services[discovery.ServiceJobs], reg.Services[discovery.ServiceDownloads]
	if jobs == "" || jobs != downloads {
		t.Fatalf("registry = %v; one process serving both should publish one endpoint under both names", reg.Services)
	}
	if !regexp.MustCompile(`^abstraction-[0-9a-f]{32}$`).MatchString(jobs) {
		t.Errorf("endpoint %q is not 128 bits of hex", jobs)
	}
	if strings.Contains(jobs, filepath.Base(store)) {
		t.Errorf("endpoint %q looks derived from the store path", jobs)
	}

	// The heartbeat file is still written. Cross-machine discovery is the only
	// thing that crosses a share, and it is the file that crosses.
	if _, err := os.Stat(filepath.Join(store, "supervisor.json")); err != nil {
		t.Errorf("the heartbeat is gone; the NAS path depends on it: %v", err)
	}

	// A second supervisor on the same store is refused, not allowed to shadow
	// the first. The name is held and its endpoint answers.
	second := jobdCmd(bin, store, "run", "--interval", "60s")
	out, err := second.CombinedOutput()
	if err == nil {
		t.Errorf("a second supervisor started alongside the first:\n%s", out)
	}
	if !strings.Contains(string(out), "held") {
		t.Errorf("the refusal does not say why:\n%s", out)
	}

	// `jobd discover` agrees.
	out, err = jobdCmd(bin, store, "discover").CombinedOutput()
	if err != nil {
		t.Fatalf("discover: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "present") {
		t.Errorf("discover said:\n%s", out)
	}

	// Kill it. Not a clean stop: nothing gets to tidy up, which is exactly the
	// case the heartbeat gets wrong and a connection gets right.
	run.Process.Kill()
	run.Wait()

	gone := time.Now().Add(5 * time.Second)
	for time.Now().Before(gone) {
		if a = discovery.Query(store); a.State == discovery.Absent {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if a.State != discovery.Absent {
		t.Fatalf("still %v after the supervisor was killed", a.State)
	}
	// And the file still says it is running, which is the bug this replaces.
	if _, err := os.Stat(filepath.Join(store, "supervisor.json")); err != nil {
		t.Log("heartbeat already gone; the point stands either way")
	}

	// The registry entry survives the kill, so the next supervisor can inherit
	// the name and clean up after this one.
	if discovery.Resolve(store, discovery.ServiceJobs) != jobs {
		t.Error("the registry entry did not survive a kill")
	}

	// A supervisor started now takes the name over.
	restart := jobdCmd(bin, store, "run", "--interval", "60s")
	restart.Stdout, restart.Stderr = &log, &log
	if err := restart.Start(); err != nil {
		t.Fatalf("restart: %v", err)
	}
	defer func() { restart.Process.Kill(); restart.Wait() }()

	deadline = time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if a = discovery.Query(store); a.State == discovery.Present {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if a.State != discovery.Present {
		t.Fatalf("could not take over a dead endpoint: %v (%s)\n%s", a.State, a.Why, log.String())
	}
}

// An empty store answers absent, quickly, and says nothing alarming while
// doing it. No supervisor is the ordinary case.
func TestNoSupervisorIsOrdinary(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binary")
	}
	bin := buildJobd(t)
	store := t.TempDir()

	start := time.Now()
	out, err := jobdCmd(bin, store, "discover").CombinedOutput()
	took := time.Since(start)
	if err != nil {
		t.Fatalf("asking about a store with no supervisor is not an error: %v\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "absent") {
		t.Errorf("discover said:\n%s", out)
	}
	// Generous: this includes process start. The point is that nothing waited
	// on a socket that was never there.
	if took > 5*time.Second {
		t.Errorf("took %v", took)
	}
}
