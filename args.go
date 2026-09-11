package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	download "github.com/openabstractions/abstraction-download/go"
)

// parse reads the whole command line or refuses it, and returns the positional
// arguments named by want. A name ending in "..." takes every one that is left.
//
// A flag or an argument a command does not honour is a refusal, never a skip:
// dl accepted `--verify sha256:…`, downloaded, verified nothing and exited 0,
// and `jobd run --once /some/path` ran a supervisor that never looked at the
// path. flag hands leftover positionals back; nobody was looking.
func parse(fs *flag.FlagSet, args []string, want ...string) ([]string, error) {
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			break
		}
		if fs.Arg(0) == "" {
			return nil, fmt.Errorf("%s was given an empty argument", fs.Name())
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
	takesRest := len(want) > 0 && strings.HasSuffix(want[len(want)-1], "...")
	switch {
	case len(pos) < len(want):
		return nil, fmt.Errorf("%s needs %s", fs.Name(), strings.TrimSuffix(want[len(pos)], "..."))
	case len(pos) > len(want) && !takesRest:
		return nil, fmt.Errorf("%s does not know what to do with %q", fs.Name(), pos[len(want)])
	}
	// An empty value is the same lie as an unknown flag: `--nas-store "$P"`
	// with P unset would report a store configured at nowhere.
	var blank string
	fs.Visit(func(f *flag.Flag) {
		if f.Value.String() == "" {
			blank = f.Name
		}
	})
	if blank != "" {
		return nil, fmt.Errorf("-%s wants a value", blank)
	}
	return pos, nil
}

// need parses or ends the process. A command line this tool will not act on
// exits 2 — nothing was attempted, which is a different thing for a script to
// know than a sweep that could not finish. See abstraction-download/CONTRACT.md
// § What a status byte can carry.
func need(fs *flag.FlagSet, args []string, want ...string) []string {
	pos, err := parse(fs, args, want...)
	if errors.Is(err, flag.ErrHelp) {
		usage()
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "jobd:", err)
		os.Exit(2)
	}
	return pos
}

// status is the class of a failure as a number a shell can branch on.
//
// The class comes from the error itself — a list kept here would be the second
// place to update, which is the defect [DL-E3] names. abstraction-download/CONTRACT.md
// § What a status byte can carry says what each number means.
func status(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, download.ErrOutcomeUnknown):
		return 4
	case download.Permanent(err):
		return 3
	default:
		return 1
	}
}
