package main

import (
	"log"
	"os"
	"path/filepath"

	"github.com/openabstractions/abstraction-download/go/serve"
)

// LogName is where this tool says things when nobody can hear it, and it is the
// file `jobd start` already names as the supervisor's log. One file: a person
// told to look somewhere should not have to be told which of two somewheres.
const LogName = "jobd.log"

// speakSomewhere points this process's output at the log when it has nowhere
// else to put it, and reports whether it had to.
//
// Two images are built from this package. jobd.exe is a console program. The
// second is the same program linked for the windows subsystem, because a
// console image started at logon is given a console window whatever the shell
// asks for, and an image in the windows subsystem is given none. What it is also given none of is standard
// output: every line this tool prints would be written to a handle that is not
// there, and a supervisor that failed to start would look exactly like one that
// started. That is the trade this function pays for.
//
// It asks the handle rather than the image. A console image run by a scheduler
// that gave it no handles is just as mute, and the answer is the same one; the
// image is not a thing a running process can ask about anyway. Where stdout
// works this returns immediately and changes nothing, which is every ordinary
// run of jobd.exe on all three platforms.
// The file is deliberately not closed and not returned: every exit from this
// tool is os.Exit, which runs no deferred close, and the handle is wanted for
// as long as the process can still print.
func speakSomewhere() bool {
	if _, err := os.Stdout.Stat(); err == nil {
		return false
	}
	root, err := serve.StoreRoot()
	if err != nil {
		return false
	}
	f, err := os.OpenFile(filepath.Join(root, LogName), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return false
	}
	os.Stdout, os.Stderr = f, f
	log.SetOutput(f)
	return true
}
