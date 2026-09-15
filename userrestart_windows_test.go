package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

func TestActivationArgumentsFollowReleasedShortcuts(t *testing.T) {
	for version, want := range map[string][]string{
		"0.1.5":   {"start"},
		"0.1.4":   {"start"},
		"0.0.9":   {"start"},
		"0.1.5.0": {"start"},
		"0.1.6":   {"start", "--runtime"},
		"0.2.0":   {"start", "--runtime"},
		"1.0.0":   {"start", "--runtime"},
	} {
		got, err := activationArguments(version)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v err=%v, want %v", version, got, err, want)
		}
	}
	for _, bad := range []string{"", "0.1", "a.b.c", "0.-1.5", "0.1.x"} {
		if _, err := activationArguments(bad); err == nil {
			t.Fatalf("accepted version %q", bad)
		}
	}
}

type startedCall struct {
	image string
	args  []string
}

func TestRestartUserPredecessorsRunsEachProductsActivation(t *testing.T) {
	codeC := "{00000000-0000-0000-0000-00000000000C}"
	codeD := "{00000000-0000-0000-0000-00000000000D}"
	codeE := "{00000000-0000-0000-0000-00000000000E}"
	values := map[string]map[string]string{
		codeA: {"VersionString": "0.1.5", "InstallLocation": `D:\Tools\OpenAbstractions\`},
		codeB: {"VersionString": "0.1.6", "InstallLocation": testFolder + `\`},
		codeC: {"VersionString": "0.1.6"},
		codeD: {"VersionString": "0.1.7", "InstallLocation": `D:\Removed\OpenAbstractions\`},
		codeE: {"VersionString": "0.1.8", "InstallLocation": `D:\Broken\OpenAbstractions\`},
	}
	present := map[string]bool{
		`D:\Tools\OpenAbstractions\tools\jobdw.exe`:  true,
		testFolder + `\tools\jobdw.exe`:              true,
		`D:\Broken\OpenAbstractions\tools\jobdw.exe`: true,
	}
	var calls []startedCall
	run := func(image string, args []string) error {
		calls = append(calls, startedCall{image, args})
		if strings.HasPrefix(image, `D:\Broken`) {
			return errors.New("exit status 1")
		}
		return nil
	}
	list := strings.Join([]string{codeA, codeB, codeC, codeD, codeE}, ";")
	started, notes, err := restartUserPredecessors(list, fakeProductInfo(values, nil), func(p string) bool { return present[p] }, run, func(string) bool { return true })
	if started != 2 {
		t.Fatalf("started=%d calls=%v", started, calls)
	}
	want := []startedCall{
		{`D:\Tools\OpenAbstractions\tools\jobdw.exe`, []string{"start"}},
		{testFolder + `\tools\jobdw.exe`, []string{"start", "--runtime"}},
		{`D:\Broken\OpenAbstractions\tools\jobdw.exe`, []string{"start", "--runtime"}},
	}
	if !reflect.DeepEqual(calls, want) {
		t.Fatalf("calls=%v, want %v", calls, want)
	}
	if len(notes) != 2 || !strings.Contains(notes[0], "records no install location") || !strings.Contains(notes[1], `has no D:\Removed`) {
		t.Fatalf("notes=%v", notes)
	}
	if err == nil || !strings.Contains(err.Error(), codeE) || !strings.Contains(err.Error(), "exit status 1") {
		t.Fatalf("failed start not reported: %v", err)
	}
}

func TestRestartUserPredecessorsRefusesUnregisteredProduct(t *testing.T) {
	ran := false
	all := func(string) bool { return true }
	_, _, err := restartUserPredecessors(codeA, fakeProductInfo(nil, nil), func(string) bool { return true },
		func(string, []string) error { ran = true; return nil }, all)
	if err == nil || ran {
		t.Fatalf("err=%v ran=%v", err, ran)
	}
	denied := map[string]error{codeA + "/VersionString": syscall.Errno(5)}
	if _, _, err := restartUserPredecessors(codeA, fakeProductInfo(map[string]map[string]string{codeA: {}}, denied),
		func(string) bool { return true }, func(string, []string) error { ran = true; return nil }, all); err == nil || ran {
		t.Fatalf("registration read failure hidden: err=%v ran=%v", err, ran)
	}
}

func TestServiceStartArguments(t *testing.T) {
	request, err := serviceArguments([]string{"start", "--related", codeA})
	if err != nil || request.command != "start" || request.related != codeA || request.userFolder != "" || request.runtime {
		t.Fatal(request, err)
	}
	for _, args := range [][]string{{"start"}, {"start", "--related"}, {"start", "--related", ""}, {"start", "--runtime"},
		{"start", "--user", testFolder}, {"start", "--related", codeA, "--user", testFolder}} {
		if _, err := serviceArguments(args); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

// The real runner waits for an inert activation that exits and reports a
// nonzero exit. It inherits no output pipes.
func TestRunActivationWaitsForCommandExit(t *testing.T) {
	comspec := os.Getenv("ComSpec")
	if comspec == "" {
		comspec = filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe")
	}
	if err := runActivation(comspec, []string{"/d", "/c", "exit", "0"}); err != nil {
		t.Fatal(err)
	}
	if err := runActivation(comspec, []string{"/d", "/c", "exit", "7"}); err == nil {
		t.Fatal("nonzero activation exit accepted")
	}
}
