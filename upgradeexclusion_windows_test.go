package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

type testExclusion struct {
	env       exclusionEnv
	dir       string
	alive     map[uint32]bool
	untrusted map[string]bool
}

func newTestExclusion(t *testing.T) *testExclusion {
	t.Helper()
	x := &testExclusion{dir: t.TempDir(), alive: map[uint32]bool{}, untrusted: map[string]bool{}}
	x.env = exclusionEnv{
		path: func(scope string) (string, error) { return filepath.Join(x.dir, scope, "exclusion.json"), nil },
		read: os.ReadFile,
		write: func(path, _ string, data []byte) error {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				return err
			}
			return os.WriteFile(path, data, 0o600)
		},
		remove: os.Remove,
		trusted: func(path, _ string) error {
			if x.untrusted[path] {
				return errors.New("owned by another account")
			}
			return nil
		},
		alive: func(holder processIdentity, _ time.Time) bool { return x.alive[holder.PID] },
		now:   func() time.Time { return time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC) },
	}
	return x
}

func holderFor(pid uint32) processIdentity {
	return processIdentity{PID: pid, Created: 1000 + int64(pid), Image: `C:\Windows\System32\msiexec.exe`}
}

const machineFolder = `C:\Program Files\OpenAbstractions`

func TestUpgradeExclusionRecordIsStrict(t *testing.T) {
	valid := upgradeExclusion{Version: 1, Scope: "user", Folders: []string{testFolder}, Installer: holderFor(7),
		Begun: time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC), Stopped: []stoppedProcess{{Image: testFolder + `\tools\jobdw.exe`, PID: 10, Created: 5}},
		Services: []string{serviceName + "_1a2b"}}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeExclusion(data)
	if err != nil || !reflect.DeepEqual(*decoded, valid) {
		t.Fatalf("round trip: %+v %v", decoded, err)
	}
	broken := map[string]func(*upgradeExclusion){
		"version":            func(r *upgradeExclusion) { r.Version = 2 },
		"scope":              func(r *upgradeExclusion) { r.Scope = "session" },
		"no folder":          func(r *upgradeExclusion) { r.Folders = nil },
		"relative folder":    func(r *upgradeExclusion) { r.Folders = []string{`Programs\OpenAbstractions`} },
		"volume root":        func(r *upgradeExclusion) { r.Folders = []string{`C:\`} },
		"unclean folder":     func(r *upgradeExclusion) { r.Folders = []string{`C:\Users\x\..\y`} },
		"no installer":       func(r *upgradeExclusion) { r.Installer.PID = 0 },
		"unreadable, no pid": func(r *upgradeExclusion) { r.Installer.PID, r.Installer.Created = 0, 0 },
		"negative creation":  func(r *upgradeExclusion) { r.Installer.Created = -1 },
		"no start time":      func(r *upgradeExclusion) { r.Begun = time.Time{} },
		"relative stopped":   func(r *upgradeExclusion) { r.Stopped[0].Image = "jobdw.exe" },
		"other service":      func(r *upgradeExclusion) { r.Services = []string{"Spooler"} },
		"template service":   func(r *upgradeExclusion) { r.Services = []string{serviceName + "_"} },
	}
	for name, mutate := range broken {
		copy := valid
		copy.Folders = append([]string(nil), valid.Folders...)
		copy.Stopped = append([]stoppedProcess(nil), valid.Stopped...)
		mutate(&copy)
		data, _ := json.Marshal(copy)
		if _, err := decodeExclusion(data); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	text := string(data)
	for name, raw := range map[string]string{
		"unknown field": strings.Replace(text, `"version":1`, `"version":1,"extra":true`, 1),
		"trailing data": text + "{}",
		"oversized":     text + strings.Repeat(" ", exclusionMaxBytes),
	} {
		if _, err := decodeExclusion([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
}

func TestActivationHonoursOnlyLiveCoveringTrustedRecords(t *testing.T) {
	x := newTestExclusion(t)
	x.alive[7] = true
	if err := x.env.begin("user", []string{testFolder}, holderFor(7)); err != nil {
		t.Fatal(err)
	}
	inside := testFolder + `\tools\jobdw.exe`
	if err := x.env.refusal(inside); !errors.Is(err, errUpgradeInProgress) || !strings.Contains(err.Error(), "installer process 7") {
		t.Fatalf("live covering record did not refuse: %v", err)
	}
	if err := x.env.refusal(`D:\Other\OpenAbstractions\tools\jobdw.exe`); err != nil {
		t.Fatalf("record refused another installation: %v", err)
	}
	x.alive[7] = false
	if record, notes := x.env.active(inside); record != nil || len(notes) != 1 || !strings.Contains(notes[0], "is gone") {
		t.Fatalf("stale holder excluded: %+v %v", record, notes)
	}
	x.alive[7] = true
	path, _ := x.env.path("user")
	x.untrusted[path] = true
	if record, notes := x.env.active(inside); record != nil || len(notes) != 1 || !strings.Contains(notes[0], "owned by another account") {
		t.Fatalf("untrusted record excluded: %+v %v", record, notes)
	}
	delete(x.untrusted, path)
	if err := os.WriteFile(path, []byte(`{"version":1`), 0o600); err != nil {
		t.Fatal(err)
	}
	if record, notes := x.env.active(inside); record != nil || len(notes) != 1 {
		t.Fatalf("malformed record excluded: %+v %v", record, notes)
	}
	x.alive[9] = true
	if err := x.env.begin("machine", []string{machineFolder}, holderFor(9)); err != nil {
		t.Fatal(err)
	}
	if err := x.env.refusal(machineFolder + `\tools\jobdw.exe`); !errors.Is(err, errUpgradeInProgress) {
		t.Fatalf("machine record did not refuse its installation: %v", err)
	}
}

func TestBeginRefusesAnotherLiveInstaller(t *testing.T) {
	x := newTestExclusion(t)
	x.alive[7], x.alive[8] = true, true
	if err := x.env.begin("user", []string{testFolder}, holderFor(7)); err != nil {
		t.Fatal(err)
	}
	if err := x.env.begin("user", []string{testFolder}, holderFor(8)); err == nil || !strings.Contains(err.Error(), "installer process 7") {
		t.Fatalf("second live installer accepted: %v", err)
	}
	if err := x.env.begin("user", []string{testFolder}, holderFor(7)); err != nil {
		t.Fatalf("same installer could not rewrite its record: %v", err)
	}
	x.alive[7] = false
	if err := x.env.begin("user", []string{testFolder}, holderFor(8)); err != nil {
		t.Fatalf("stale record blocked a new installer: %v", err)
	}
	record, _, err := x.env.load("user")
	if err != nil || record.Installer != holderFor(8) {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func TestUserStopRecordsBeforeTerminating(t *testing.T) {
	supervisor := held(testFolder+`\tools\jobdw.exe`, testSID, 900)
	child := held(testFolder+`\tools\openabstractions.exe`, testSID, 901)
	ops, _ := fakeOps([][]processEntry{{{10, "jobdw.exe"}, {11, "openabstractions.exe"}}}, map[uint32]*fakeHeld{10: supervisor, 11: child})
	var recorded []predecessor
	terminatedAtRecord := -1
	ops.record = func(found []predecessor) error {
		recorded = append([]predecessor(nil), found...)
		terminatedAtRecord = supervisor.terminated + child.terminated
		return nil
	}
	stopped, err := stopUserPredecessors(context.Background(), []string{testFolder}, testSID, ops)
	if err != nil || stopped != 2 || len(recorded) != 2 || terminatedAtRecord != 0 {
		t.Fatalf("stopped=%d err=%v recorded=%d terminatedAtRecord=%d", stopped, err, len(recorded), terminatedAtRecord)
	}
	refused := held(testFolder+`\tools\jobdw.exe`, testSID, 900)
	ops, _ = fakeOps([][]processEntry{{{10, "jobdw.exe"}}}, map[uint32]*fakeHeld{10: refused})
	ops.record = func([]predecessor) error { return errors.New("record unwritable") }
	if _, err := stopUserPredecessors(context.Background(), []string{testFolder}, testSID, ops); err == nil || refused.terminated != 0 {
		t.Fatalf("unrecorded predecessor was terminated: err=%v terminated=%d", err, refused.terminated)
	}

	x := newTestExclusion(t)
	list := []stoppedProcess{{Image: testFolder + `\tools\jobdw.exe`, PID: 10, Created: 900}}
	if err := x.env.recordStopped(list); err != nil {
		t.Fatal(err)
	}
	if path, _ := x.env.path("user"); fileExists(path) {
		t.Fatal("a stop outside an installer transaction created a record")
	}
	x.alive[7] = true
	if err := x.env.begin("user", []string{testFolder}, holderFor(7)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := x.env.recordStopped(list); err != nil {
			t.Fatal(err)
		}
	}
	record, _, err := x.env.load("user")
	if err != nil || len(record.Stopped) != 1 || record.Stopped[0] != list[0] {
		t.Fatalf("record=%+v err=%v", record, err)
	}
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestUserRollbackRestartsOnlyRecordedProducts(t *testing.T) {
	x := newTestExclusion(t)
	moved := `D:\Tools\OpenAbstractions`
	x.alive[7] = true
	if err := x.env.begin("user", []string{testFolder, moved}, holderFor(7)); err != nil {
		t.Fatal(err)
	}
	if err := x.env.recordStopped([]stoppedProcess{{Image: moved + `\tools\jobdw.exe`, PID: 10, Created: 900}}); err != nil {
		t.Fatal(err)
	}
	path, _ := x.env.path("user")
	record, notes := x.env.release("user")
	if record == nil || len(notes) != 0 || fileExists(path) {
		t.Fatalf("release: %+v %v exists=%v", record, notes, fileExists(path))
	}
	values := map[string]map[string]string{
		codeA: {"VersionString": "0.1.5", "InstallLocation": moved + `\`},
		codeB: {"VersionString": "0.1.6", "InstallLocation": testFolder + `\`},
	}
	var calls []startedCall
	run := func(image string, args []string) error {
		if fileExists(path) {
			t.Fatal("restart ran while the exclusion was still held")
		}
		calls = append(calls, startedCall{image, args})
		return nil
	}
	started, more, err := restartUserPredecessors(codeA+";"+codeB, fakeProductInfo(values, nil), func(string) bool { return true }, run, record.recordedIn)
	if err != nil || started != 1 || !reflect.DeepEqual(calls, []startedCall{{moved + `\tools\jobdw.exe`, []string{"start"}}}) {
		t.Fatalf("started=%d calls=%v err=%v", started, calls, err)
	}
	if len(more) != 1 || !strings.Contains(more[0], codeB) || !strings.Contains(more[0], "had no process stopped") {
		t.Fatalf("notes=%v", more)
	}

	// No record: rollback restarts nothing and says so.
	empty := newTestExclusion(t)
	none, notes := empty.env.release("user")
	calls = nil
	started, _, err = restartUserPredecessors(codeA, fakeProductInfo(values, nil), func(string) bool { return true }, run, none.recordedIn)
	if none != nil || len(notes) != 1 || started != 0 || len(calls) != 0 || err != nil {
		t.Fatalf("no record: %+v %v started=%d calls=%v err=%v", none, notes, started, calls, err)
	}
}

func TestMachineRollbackRestartsOnlyRecordedServices(t *testing.T) {
	x := newTestExclusion(t)
	x.alive[9] = true
	if err := x.env.begin("machine", []string{machineFolder}, holderFor(9)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{serviceName + "_1a", serviceName + "_2b", serviceName + "_1A", serviceName + "_3c", serviceName + "_4d"} {
		if err := x.env.recordService(name); err != nil {
			t.Fatal(err)
		}
	}
	if err := x.env.recordService("Spooler"); err == nil {
		t.Fatal("recorded a service that is not a supervisor instance")
	}
	record, _ := x.env.release("machine")
	var calls []string
	start := func(name string) error {
		calls = append(calls, name)
		switch name {
		case serviceName + "_2b":
			return windows.ERROR_SERVICE_ALREADY_RUNNING
		case serviceName + "_3c":
			return windows.ERROR_SERVICE_DOES_NOT_EXIST
		case serviceName + "_4d":
			return windows.ERROR_ACCESS_DENIED
		}
		return nil
	}
	started, notes, err := restartRecordedServices(record, start)
	want := []string{serviceName + "_1a", serviceName + "_2b", serviceName + "_3c", serviceName + "_4d"}
	if !reflect.DeepEqual(calls, want) || started != 2 || len(notes) != 1 || !strings.Contains(notes[0], "_3c") || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("calls=%v started=%d notes=%v err=%v", calls, started, notes, err)
	}
	if started, notes, err := restartRecordedServices(nil, start); started != 0 || notes != nil || err != nil {
		t.Fatal("no record restarted something")
	}
}

func TestMachineStopRecordsBeforeStopControl(t *testing.T) {
	for _, failRecord := range []bool{false, true} {
		t.Run(fmt.Sprintf("failRecord=%v", failRecord), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			process := &shutdownProcess{}
			stops, records, stopsAtRecord := 0, 0, -1
			ops := removalOps{
				query: func() (windows.SERVICE_STATUS_PROCESS, error) {
					state, pid := uint32(windows.SERVICE_RUNNING), uint32(17)
					if stops > 0 {
						state, pid = windows.SERVICE_STOPPED, 0
					}
					return windows.SERVICE_STATUS_PROCESS{CurrentState: state, ProcessId: pid, ServiceType: windows.SERVICE_WIN32_OWN_PROCESS}, nil
				},
				stop: func() error { stops++; return nil },
				pin:  func(uint32) (removalProcess, error) { return process, nil },
				record: func() error {
					records++
					stopsAtRecord = stops
					if failRecord {
						return errors.New("record unwritable")
					}
					return nil
				},
			}
			stopped, err := stopAndDeleteService(ctx, ops)
			if failRecord {
				if err == nil || stops != 0 || records != 1 {
					t.Fatalf("unrecorded instance was stopped: err=%v stops=%d", err, stops)
				}
				return
			}
			if err != nil || !stopped || records != 1 || stopsAtRecord != 0 || stops != 1 {
				t.Fatalf("stopped=%v err=%v records=%d stopsAtRecord=%d stops=%d", stopped, err, records, stopsAtRecord, stops)
			}
		})
	}
}

// Real files, owners and process identities on this machine. No installer,
// service or other account is involved.
func TestExclusionFilesAndIdentitiesOnThisMachine(t *testing.T) {
	if _, err := windows.SecurityDescriptorFromString(machineExclusionSDDL); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "user", "exclusion.json")
	record := upgradeExclusion{Version: 1, Scope: "user", Folders: []string{testFolder}, Installer: holderFor(7), Begun: time.Now().UTC()}
	data, _ := json.Marshal(record)
	for i := 0; i < 2; i++ { // the second write replaces the first
		if err := writeExclusionFile(path, "user", data); err != nil {
			t.Fatal(err)
		}
	}
	read, err := readExclusionFile(path)
	if err != nil || string(read) != string(data) {
		t.Fatalf("read %q err=%v", read, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "exclusion-*.tmp")); len(leftovers) != 0 {
		t.Fatalf("temporary files left: %v", leftovers)
	}
	if _, err := readExclusionFile(filepath.Dir(path)); err == nil {
		t.Fatal("a directory was read as a record")
	}
	if err := trustedExclusionFile(path, "user"); err != nil {
		t.Fatalf("own user record untrusted: %v", err)
	}
	owner, err := pathOwner(path)
	if err != nil {
		t.Fatal(err)
	}
	if !administrativeSID(owner) {
		if err := trustedExclusionFile(path, "machine"); err == nil {
			t.Fatal("a standard user's file was trusted as a machine record")
		}
	}

	own, err := handleCreated(windows.CurrentProcess())
	if err != nil {
		t.Fatal(err)
	}
	self := processIdentity{PID: uint32(os.Getpid()), Created: own}
	now := time.Now()
	if !processAlive(self, now, now) {
		t.Fatal("this process is not alive")
	}
	if processAlive(processIdentity{PID: self.PID, Created: own + 1}, now, now) {
		t.Fatal("a different creation time named this process")
	}
	if processAlive(processIdentity{PID: 0xFFFFFFF1, Created: own}, now, now) {
		t.Fatal("an invalid PID is alive")
	}
	parent, err := installerIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if parent.Created > own || !filepath.IsAbs(parent.Image) || !processAlive(parent, now, now) {
		t.Fatalf("parent identity %+v", parent)
	}
}

func TestSupervisorRefusesDuringUpgradeBeforeWork(t *testing.T) {
	requests := make(chan svc.ChangeRequest)
	statuses := make(chan svc.Status, 4)
	var worked atomic.Bool
	h := supervisor{
		guard: func() error { return fmt.Errorf("%w: test", errUpgradeInProgress) },
		jobs:  func(context.Context, []string) error { worked.Store(true); return nil },
	}
	specific, code := h.Execute([]string{serviceName}, requests, statuses)
	close(statuses)
	var states []svc.State
	for s := range statuses {
		states = append(states, s.State)
	}
	if !specific || code != 1 || worked.Load() || !reflect.DeepEqual(states, []svc.State{svc.StartPending, svc.Running}) {
		t.Fatalf("specific=%v code=%d worked=%v states=%v", specific, code, worked.Load(), states)
	}
}

func TestUpgradeExclusionArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want serviceRequest
	}{
		{[]string{"begin-upgrade", "--user", testFolder + `\.`, "--related", codeA}, serviceRequest{command: "begin-upgrade", scope: "user", userFolder: testFolder + `\.`, related: codeA}},
		{[]string{"begin-upgrade", "--machine", machineFolder, "--related", codeA}, serviceRequest{command: "begin-upgrade", scope: "machine", userFolder: machineFolder, related: codeA}},
		{[]string{"end-upgrade", "--user"}, serviceRequest{command: "end-upgrade", scope: "user"}},
		{[]string{"end-upgrade", "--machine"}, serviceRequest{command: "end-upgrade", scope: "machine"}},
		{[]string{"upgrade-check"}, serviceRequest{command: "upgrade-check"}},
		{[]string{"start", "--machine"}, serviceRequest{command: "start", scope: "machine"}},
		{[]string{"start", "--related", codeA}, serviceRequest{command: "start", scope: "user", related: codeA}},
	} {
		got, err := serviceArguments(tc.args)
		if err != nil || got != tc.want {
			t.Fatalf("%v: got %+v err=%v", tc.args, got, err)
		}
	}
	for _, args := range [][]string{
		{"begin-upgrade", "--user", testFolder}, {"begin-upgrade", "--session", testFolder, "--related", codeA},
		{"begin-upgrade", "--user", "", "--related", codeA}, {"begin-upgrade", "--user", testFolder, "--related", ""},
		{"end-upgrade"}, {"end-upgrade", "--user", "extra"}, {"upgrade-check", "--user"},
		{"start", "--machine", "extra"}, {"start", "--user"},
	} {
		if _, err := serviceArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
