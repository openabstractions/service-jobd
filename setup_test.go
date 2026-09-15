package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	config "github.com/openabstractions/abstraction-config/go"
)

// TestSetupStderrHelper runs `jobd setup` in a child process. It does nothing in
// the ordinary test run.
func TestSetupStderrHelper(t *testing.T) {
	switch os.Getenv("OA_JOBD_SETUP_HELPER") {
	case "":
		return
	case "show":
		cmdSetup([]string{"--show"})
	case "write":
		cmdSetup([]string{"--store", os.Getenv("OA_JOBD_SETUP_STORE")}) // exits 0
	}
}

// jobd owns the embedded file configuration, so `jobd setup` selects it
// explicitly and prints no application deprecation warning on either path.
func TestSetupWritesNothingToStandardError(t *testing.T) {
	root := t.TempDir()
	store := filepath.Join(root, "store")
	run := func(mode string) (string, string, error) {
		command := exec.Command(os.Args[0], "-test.run=^TestSetupStderrHelper$", "-test.count=1")
		var env []string
		for _, entry := range os.Environ() {
			name := strings.ToUpper(strings.SplitN(entry, "=", 2)[0])
			if strings.HasPrefix(name, "ABSTRACTION_") || strings.HasPrefix(name, "MODELGET_") || name == "JOB_STORE" ||
				name == "APPDATA" || name == "XDG_CONFIG_HOME" || name == "HOME" || name == "PROGRAMDATA" || name == "USERPROFILE" {
				continue
			}
			env = append(env, entry)
		}
		command.Env = append(env, "OA_JOBD_SETUP_HELPER="+mode, "OA_JOBD_SETUP_STORE="+store,
			"APPDATA="+filepath.Join(root, "appdata"), "XDG_CONFIG_HOME="+filepath.Join(root, "config"),
			"HOME="+filepath.Join(root, "home"), "USERPROFILE="+filepath.Join(root, "home"), "ProgramData="+filepath.Join(root, "programdata"))
		var stdout, stderr bytes.Buffer
		command.Stdout, command.Stderr = &stdout, &stderr
		err := command.Run()
		return stdout.String(), stderr.String(), err
	}

	for _, mode := range []string{"show", "write"} {
		stdout, stderr, err := run(mode)
		if err != nil || stderr != "" {
			t.Fatalf("jobd setup (%s): err=%v stderr=%q stdout=%q", mode, err, stderr, stdout)
		}
		if mode == "write" && !strings.Contains(stdout, "wrote ") {
			t.Fatalf("jobd setup --store did not report its write: %q", stdout)
		}
	}
	written, err := os.ReadFile(config.UserPath())
	if err == nil && strings.Contains(string(written), store) {
		t.Fatal("the helper wrote the real user configuration instead of the isolated one")
	}
}
