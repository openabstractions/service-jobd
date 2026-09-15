package main

import (
	"flag"
	"fmt"
	"os"

	config "github.com/openabstractions/abstraction-config/go"
	download "github.com/openabstractions/abstraction-download/go"
)

// cmdSetup writes the machine's configuration once, so that no application ever
// has to be configured again.
//
// This is the step that makes discovery work for programs that know nothing —
// a fork of Lemonade, ComfyUI, anything. They call download.Discover and get the
// right tier because somebody, once, said what this machine has. Compare with
// the alternative, which is every application growing its own NAS setting.
func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	nasStore := fs.String("nas-store", "", "a job store on a share that a jobd elsewhere watches")
	store := fs.String("store", "", "the local job store (default ~/.abstraction)")
	logSink := fs.String("log-sink", "", "a file every tool appends structured log records to")
	logService := fs.String("log-service", "", "a local socket that attests identity")
	machine := fs.Bool("machine", false, "write the machine-wide file instead of this user's")
	show := fs.Bool("show", false, "print what is configured and where it came from")
	need(fs, args)
	say := func(format string, a ...any) {
		if _, err := fmt.Printf(format, a...); err != nil {
			fatal(err)
		}
	}

	if *show {
		// jobd is the provider that owns the embedded file configuration, so it
		// selects it explicitly and reports no application deprecation warning.
		cfg := config.LegacyLoad()
		say("%s", cfg.Describe())
		say("\ntiers linked into this build: %v\n", download.RegisteredTiers())
		return
	}

	path := config.UserPath()
	if *machine {
		path = config.MachinePath()
		if path == "" {
			fatal(fmt.Errorf("no machine-wide configuration location on this OS"))
		}
	}

	// Edit the file being written, not the merged view of every file.
	//
	// This read Load() and saved the result, which starts from an answer built
	// out of the machine file AND the environment: setting one thing here baked
	// an administrator's value, or a variable set in this shell, permanently
	// into the user's file. With --machine it copied the user's answers the
	// other way. It also flattens anything this build does not know about, and
	// the control panel's tier switches are exactly that.
	set := func(field *string, given string) {
		if given != "" {
			*field = given
		}
	}
	err := config.Edit(path, func(c *config.Config) error {
		set(&c.NASStore, *nasStore)
		set(&c.Store, *store)
		set(&c.LogSink, *logSink)
		set(&c.LogService, *logService)
		return nil
	})
	if err != nil {
		fatal(err)
	}
	say("wrote %s\n\n", path)
	say("%s", config.LegacyLoad().Describe())
	say("\n")
	say("Every tool that speaks these abstractions now finds this by itself.\n")
	say("Nothing else needs configuring, and no application needs to know.\n")
	os.Exit(0)
}
