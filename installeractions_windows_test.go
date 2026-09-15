package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

func fakeIdentityOps(inspectErr error) identityOps {
	return identityOps{
		self:    func() uint32 { return 40 },
		parent:  func(pid uint32) (uint32, string, error) { return 30, "msiexec.exe", nil },
		created: func() (int64, error) { return 5000, nil },
		inspect: func(pid uint32) (int64, string, error) {
			if inspectErr != nil {
				return 0, "", inspectErr
			}
			return 4000, `C:\Windows\System32\msiexec.exe`, nil
		},
	}
}

func TestInstallerIdentityRecordsAnUnreadableParent(t *testing.T) {
	got, err := installerIdentityWith(fakeIdentityOps(&openProcessError{pid: 30, err: windows.ERROR_ACCESS_DENIED}))
	if err != nil || got != (processIdentity{PID: 30, Created: 0, Image: "msiexec.exe"}) {
		t.Fatalf("access-denied parent: %+v %v", got, err)
	}
	record := upgradeExclusion{Version: 1, Scope: "user", Folders: []string{testFolder}, Installer: got, Begun: time.Now().UTC()}
	if err := record.validate(); err != nil {
		t.Fatalf("record with an unreadable holder refused: %v", err)
	}
	data, _ := json.Marshal(record)
	if _, err := decodeExclusion(data); err != nil {
		t.Fatalf("decode: %v", err)
	}

	got, err = installerIdentityWith(fakeIdentityOps(nil))
	if err != nil || got != (processIdentity{PID: 30, Created: 4000, Image: `C:\Windows\System32\msiexec.exe`}) {
		t.Fatalf("readable parent: %+v %v", got, err)
	}
	for name, inspectErr := range map[string]error{
		"another open failure": &openProcessError{pid: 30, err: windows.ERROR_INVALID_PARAMETER},
		"denied after opening": windows.ERROR_ACCESS_DENIED,
	} {
		if _, err := installerIdentityWith(fakeIdentityOps(inspectErr)); err == nil {
			t.Fatalf("%s accepted", name)
		}
	}
	reused := fakeIdentityOps(nil)
	reused.created = func() (int64, error) { return 3000, nil }
	if _, err := installerIdentityWith(reused); err == nil || !strings.Contains(err.Error(), "reused") {
		t.Fatalf("a parent younger than this process was accepted: %v", err)
	}
}

// A standard token cannot open a SYSTEM service host, as the impersonated
// begin-upgrade cannot open the SYSTEM msiexec server above it.
func TestInstallerIdentityOfARealDeniedSystemParent(t *testing.T) {
	if windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("elevated: SYSTEM processes are readable, so no open is denied")
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(snap)
	var pid uint32
	var name string
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil && pid == 0; err = windows.Process32Next(snap, &entry) {
		image := windows.UTF16ToString(entry.ExeFile[:])
		if !strings.EqualFold(image, "svchost.exe") && !strings.EqualFold(image, "spoolsv.exe") {
			continue
		}
		var open *openProcessError
		if _, _, inspectErr := inspectProcess(entry.ProcessID); errors.As(inspectErr, &open) && errors.Is(open, windows.ERROR_ACCESS_DENIED) {
			pid, name = entry.ProcessID, image
		}
	}
	if pid == 0 {
		t.Skip("no service host denied this token")
	}
	ops := systemIdentityOps()
	ops.parent = func(uint32) (uint32, string, error) { return pid, name, nil }
	got, err := installerIdentityWith(ops)
	if err != nil || got != (processIdentity{PID: pid, Created: 0, Image: name}) {
		t.Fatalf("denied SYSTEM parent %d: %+v %v", pid, got, err)
	}
	begun := time.Now()
	if !processAlive(got, begun, begun) || processAlive(got, begun, begun.Add(exclusionUnverifiedLimit)) {
		t.Fatalf("denied holder %d liveness is not bounded by the unverified limit", pid)
	}
}

func TestUnreadableHolderIsHonouredOnlyWithinTheLimit(t *testing.T) {
	self := processIdentity{PID: uint32(os.Getpid()), Created: 0}
	begun := time.Now()
	if !processAlive(self, begun, begun.Add(exclusionUnverifiedLimit-time.Minute)) {
		t.Fatal("a present unreadable holder was not honoured within the limit")
	}
	if processAlive(self, begun, begun.Add(exclusionUnverifiedLimit)) {
		t.Fatal("an unreadable holder was honoured after the limit")
	}
	if processAlive(processIdentity{PID: 0xFFFFFFF1, Created: 0}, begun, begun) {
		t.Fatal("a gone unreadable holder was honoured")
	}
}

func TestBeginRefusesAnotherLiveUnreadableHolder(t *testing.T) {
	x := newTestExclusion(t)
	first := processIdentity{PID: 7, Image: "msiexec.exe"}
	second := processIdentity{PID: 8, Image: "msiexec.exe"}
	x.alive[7], x.alive[8] = true, true
	if err := x.env.begin("user", []string{testFolder}, first); err != nil {
		t.Fatal(err)
	}
	if err := x.env.begin("user", []string{testFolder}, second); err == nil || !strings.Contains(err.Error(), "installer process 7") {
		t.Fatalf("second live installer accepted: %v", err)
	}
	if err := x.env.begin("user", []string{testFolder}, first); err != nil {
		t.Fatalf("same unreadable installer could not rewrite its record: %v", err)
	}
}

func TestInstallerActionFailureIsRecordedBestEffort(t *testing.T) {
	dir := t.TempDir()
	at := time.Date(2026, 9, 15, 12, 0, 0, 0, time.FixedZone("x", 3600))
	begin := serviceRequest{command: "begin-upgrade", scope: "user", userFolder: testFolder, related: codeA}
	path := func(scope string) (string, error) {
		return filepath.Join(dir, scope, "upgrade-v1", "installer-actions.txt"), nil
	}
	var stderr bytes.Buffer
	failure := errors.New("open parent process 30:\nAccess is denied.")
	if code := serviceFailure(begin, failure, &stderr, path, at); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if code := serviceFailure(begin, fmt.Errorf("%w: busy", errUpgradeInProgress), &stderr, path, at); code != exitUpgradeInProgress {
		t.Fatalf("refusal exit %d", code)
	}
	data, err := os.ReadFile(filepath.Join(dir, "user", "upgrade-v1", "installer-actions.txt"))
	want := "2026-09-15T11:00:00Z service begin-upgrade --user: open parent process 30: Access is denied.\n"
	if err != nil || !strings.HasPrefix(string(data), want) || strings.Count(string(data), "\n") != 2 {
		t.Fatalf("recorded %q err=%v", data, err)
	}
	if !strings.Contains(stderr.String(), "jobd: open parent process 30") {
		t.Fatalf("stderr %q", stderr.String())
	}

	blocker := filepath.Join(dir, "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unwritable := func(string) (string, error) {
		return filepath.Join(blocker, "upgrade-v1", "installer-actions.txt"), nil
	}
	unknown := func(string) (string, error) { return "", errors.New("no known folder") }
	for _, p := range []func(string) (string, error){unwritable, unknown} {
		if code := serviceFailure(begin, failure, &stderr, p, at); code != 1 {
			t.Fatalf("an unwritable diagnostic changed the exit to %d", code)
		}
	}

	for _, tc := range []struct {
		request        serviceRequest
		command, scope string
	}{
		{serviceRequest{command: "end-upgrade", scope: "machine"}, "end-upgrade --machine", "machine"},
		{serviceRequest{command: "stop", userFolder: testFolder}, "stop --user", "user"},
		{serviceRequest{command: "start", scope: "user", related: codeA}, "start --related", "user"},
		{serviceRequest{command: "start", scope: "machine"}, "start --machine", "machine"},
		{serviceRequest{command: "stop"}, "", ""},
		{serviceRequest{command: "upgrade-check"}, "", ""},
		{serviceRequest{command: "run"}, "", ""},
	} {
		if command, scope := installerAction(tc.request); command != tc.command || scope != tc.scope {
			t.Fatalf("%+v: %q %q", tc.request, command, scope)
		}
	}
	quiet := filepath.Join(dir, "quiet")
	serviceFailure(serviceRequest{command: "upgrade-check"}, failure, &stderr, func(string) (string, error) { return quiet, nil }, at)
	if _, err := os.Stat(quiet); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a command the installer does not run was recorded: %v", err)
	}
}
