package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	testFolder = `C:\Users\someone\AppData\Local\Programs\OpenAbstractions`
	testSID    = "S-1-5-21-1-2-3-1001"
)

type fakeHeld struct {
	mu         sync.Mutex
	img, sid   string
	times      []int64
	calls      int
	alive      bool
	refuses    bool
	terminated int
	closed     bool
}

func (p *fakeHeld) image() (string, error) { return p.img, nil }
func (p *fakeHeld) owner() (string, error) { return p.sid, nil }
func (p *fakeHeld) created() (int64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	i := p.calls
	if i >= len(p.times) {
		i = len(p.times) - 1
	}
	p.calls++
	return p.times[i], nil
}
func (p *fakeHeld) terminate(int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.terminated++
	if !p.refuses {
		p.alive = false
	}
	return nil
}
func (p *fakeHeld) exited() (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.alive, nil
}
func (p *fakeHeld) close() error { p.closed = true; return nil }

func fakeOps(snapshots [][]processEntry, processes map[uint32]*fakeHeld) (userStopOps, *int) {
	taken := 0
	return userStopOps{
		snapshot: func() (int64, []processEntry, error) {
			i := taken
			if i >= len(snapshots) {
				i = len(snapshots) - 1
			}
			taken++
			return 1000, snapshots[i], nil
		},
		open: func(pid uint32) (heldProcess, error) {
			p, ok := processes[pid]
			if !ok {
				return nil, errNotCandidate
			}
			return p, nil
		},
	}, &taken
}

func held(image, sid string, created ...int64) *fakeHeld {
	return &fakeHeld{img: image, sid: sid, times: created, alive: true}
}

func TestUserPredecessorMatcher(t *testing.T) {
	inside := testFolder + `\tools\jobdw.exe`
	for name, tc := range map[string]struct {
		process *fakeHeld
		match   bool
		fails   bool
	}{
		"verified":              {held(inside, testSID, 900), true, false},
		"case-insensitive path": {held(strings.ToUpper(testFolder)+`\TOOLS\OPENABSTRACTIONS.EXE`, strings.ToLower(testSID), 900), true, false},
		"wrong owner":           {held(inside, "S-1-5-21-1-2-3-1002", 900), false, false},
		"image outside folder":  {held(`C:\Users\someone\Downloads\jobdw.exe`, testSID, 900), false, false},
		"sibling folder prefix": {held(testFolder+`Old\tools\jobd.exe`, testSID, 900), false, false},
		"unrelated image":       {held(testFolder+`\tools\dl.exe`, testSID, 900), false, false},
		"created after listing": {held(inside, testSID, 1001), false, true},
	} {
		t.Run(name, func(t *testing.T) {
			match, _, _, err := matchPredecessor(tc.process, testFolder, testSID, 1000)
			if match != tc.match || (err != nil) != tc.fails {
				t.Fatalf("match=%v err=%v", match, err)
			}
		})
	}
}

func TestUserStopTerminatesVerifiedPredecessorsOnly(t *testing.T) {
	supervisor := held(testFolder+`\tools\jobdw.exe`, testSID, 900)
	runtime := held(testFolder+`\tools\openabstractions.exe`, testSID, 901)
	stranger := held(testFolder+`\tools\jobd.exe`, "S-1-5-21-9", 902)
	elsewhere := held(`C:\Other\jobd.exe`, testSID, 903)
	entries := []processEntry{{10, "jobdw.exe"}, {11, "openabstractions.exe"}, {12, "jobd.exe"}, {13, "jobd.exe"}, {14, "notepad.exe"}}
	ops, taken := fakeOps([][]processEntry{entries}, map[uint32]*fakeHeld{10: supervisor, 11: runtime, 12: stranger, 13: elsewhere})
	stopped, err := stopUserPredecessors(context.Background(), testFolder, testSID, ops)
	if err != nil || stopped != 2 {
		t.Fatalf("stopped=%d err=%v", stopped, err)
	}
	if supervisor.terminated != 1 || runtime.terminated != 1 || stranger.terminated != 0 || elsewhere.terminated != 0 {
		t.Fatal("terminated an unverified process or missed a predecessor")
	}
	if !supervisor.closed || !runtime.closed || !stranger.closed || !elsewhere.closed {
		t.Fatal("process handle leaked")
	}
	if *taken != 2 {
		t.Fatalf("post-shutdown verification snapshots = %d, want 2 total", *taken)
	}
}

func TestUserStopNothingRunning(t *testing.T) {
	ops, _ := fakeOps([][]processEntry{{{4, "System"}}}, nil)
	if stopped, err := stopUserPredecessors(context.Background(), testFolder, testSID, ops); err != nil || stopped != 0 {
		t.Fatalf("stopped=%d err=%v", stopped, err)
	}
}

func TestUserStopRefusesChangedCreationTime(t *testing.T) {
	p := held(testFolder+`\tools\jobdw.exe`, testSID, 900, 950)
	ops, _ := fakeOps([][]processEntry{{{10, "jobdw.exe"}}}, map[uint32]*fakeHeld{10: p})
	_, err := stopUserPredecessors(context.Background(), testFolder, testSID, ops)
	if err == nil || !strings.Contains(err.Error(), "changed identity") || p.terminated != 0 {
		t.Fatalf("err=%v terminated=%d", err, p.terminated)
	}
}

func TestUserStopRefusesProcessThatDoesNotExit(t *testing.T) {
	p := held(testFolder+`\tools\jobdw.exe`, testSID, 900)
	p.refuses = true
	ops, _ := fakeOps([][]processEntry{{{10, "jobdw.exe"}}}, map[uint32]*fakeHeld{10: p})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := stopUserPredecessors(ctx, testFolder, testSID, ops)
	if err == nil || !strings.Contains(err.Error(), "did not exit") || !strings.Contains(err.Error(), "pid 10") || !p.closed {
		t.Fatalf("err=%v closed=%v", err, p.closed)
	}
}

func TestUserStopRefusesProcessStartedDuringShutdown(t *testing.T) {
	first := held(testFolder+`\tools\jobdw.exe`, testSID, 900)
	late := held(testFolder+`\tools\jobdw.exe`, testSID, 990)
	ops, _ := fakeOps([][]processEntry{{{10, "jobdw.exe"}}, {{10, "jobdw.exe"}, {11, "jobdw.exe"}}},
		map[uint32]*fakeHeld{10: first, 11: late})
	_, err := stopUserPredecessors(context.Background(), testFolder, testSID, ops)
	if err == nil || !strings.Contains(err.Error(), "appeared during shutdown") || !late.closed {
		t.Fatalf("err=%v", err)
	}
}

func TestUserStopArguments(t *testing.T) {
	request, err := serviceArguments([]string{"stop", "--user", testFolder + `\.`})
	if err != nil || request.command != "stop" || request.runtime || request.userFolder != testFolder+`\.` {
		t.Fatal(request, err)
	}
	for _, args := range [][]string{{"stop", "--user"}, {"stop", "--user", ""}, {"stop", "--user", testFolder, "--runtime"},
		{"uninstall", "--user", testFolder}, {"install", "--user", testFolder}, {"stop", "--runtime", testFolder}} {
		if _, err := serviceArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestUserInstallFolder(t *testing.T) {
	// The MSI passes "[APPLICATIONFOLDER]." so the trailing backslash cannot
	// escape the closing quote.
	dir := t.TempDir()
	got, err := userInstallFolder(dir + `\.`)
	if err != nil || !strings.EqualFold(got, longPath(dir)) {
		t.Fatalf("got %q err=%v", got, err)
	}
	for _, bad := range []string{`relative\OpenAbstractions`, `C:\`, `C:\.`, `C:\Users\x\OpenAbstractions" --other`} {
		if _, err := userInstallFolder(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

// The helper is this test binary copied under an inert jobd.exe name. It does
// nothing but wait, and only the test that started it ends it.
func TestUserStopInertHelper(t *testing.T) {
	if os.Getenv("OA_JOBD_USERSTOP_HELPER") == "" {
		return
	}
	time.Sleep(45 * time.Second)
}

type inertProcess struct {
	command *exec.Cmd
	done    chan error
}

func (p inertProcess) wait(within time.Duration) (error, bool) {
	select {
	case err := <-p.done:
		p.done <- err
		return err, true
	case <-time.After(within):
		return nil, false
	}
}

func (p inertProcess) end() {
	p.command.Process.Kill()
	p.wait(5 * time.Second)
}

func startInertPredecessor(t *testing.T, folder string) inertProcess {
	t.Helper()
	tools := filepath.Join(folder, "tools")
	if err := os.MkdirAll(tools, 0o700); err != nil {
		t.Fatal(err)
	}
	image := filepath.Join(tools, "jobd.exe")
	src, err := os.Open(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	dst, err := os.Create(image)
	if err == nil {
		_, err = io.Copy(dst, src)
		if closeErr := dst.Close(); err == nil {
			err = closeErr
		}
	}
	src.Close()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(image, "-test.run=^TestUserStopInertHelper$", "-test.timeout=60s")
	command.Env = append(os.Environ(), "OA_JOBD_USERSTOP_HELPER=1")
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	p := inertProcess{command: command, done: make(chan error, 1)}
	go func() { p.done <- command.Wait() }()
	return p
}

func TestUserStopEndsInertProcessUnderFolder(t *testing.T) {
	folder := filepath.Join(t.TempDir(), "OpenAbstractions")
	process := startInertPredecessor(t, folder)
	defer process.end()
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	root, err := userInstallFolder(folder + `\.`)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stopped, err := stopUserPredecessors(ctx, root, sid, systemUserStopOps())
	if err != nil || stopped != 1 {
		t.Fatalf("stopped=%d err=%v", stopped, err)
	}
	// The stop already confirmed exit on its retained handle.
	waitErr, exited := process.wait(time.Second)
	var exitErr *exec.ExitError
	if !exited || !errors.As(waitErr, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("inert predecessor exit not observed as terminated: exited=%v err=%v", exited, waitErr)
	}
}

func TestUserStopLeavesProcessOutsideFolder(t *testing.T) {
	base := t.TempDir()
	process := startInertPredecessor(t, filepath.Join(base, "Elsewhere"))
	defer process.end()
	upgraded := filepath.Join(base, "OpenAbstractions")
	if err := os.MkdirAll(upgraded, 0o700); err != nil {
		t.Fatal(err)
	}
	sid, err := currentUserSID()
	if err != nil {
		t.Fatal(err)
	}
	root, err := userInstallFolder(upgraded)
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := stopUserPredecessors(context.Background(), root, sid, systemUserStopOps())
	if err != nil || stopped != 0 {
		t.Fatalf("stopped=%d err=%v", stopped, err)
	}
	if _, exited := process.wait(300 * time.Millisecond); exited {
		t.Fatal("process outside the upgraded folder was ended")
	}
}
