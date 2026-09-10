package main

import (
	"errors"
	"flag"
	"fmt"
	"testing"
	"time"

	download "github.com/openabstractions/abstraction-download/go"
	"github.com/openabstractions/abstraction-download/go/serve"
	job "github.com/openabstractions/abstraction-job/go"
)

func runFlags() (*flag.FlagSet, *time.Duration, *serve.Systems) {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	d := fs.Duration("interval", 30*time.Second, "how often to sweep")
	var w serve.Systems
	fs.Var(&w, "without", "ignore a delegation system")
	return fs, d, &w
}

func TestParseRefuses(t *testing.T) {
	for _, c := range []struct {
		name string
		fs   *flag.FlagSet
		args []string
	}{
		{"run given a path it will not read", newRun(), []string{"/some/path"}},
		{"run given a flag it does not honour", newRun(), []string{"--once"}},
		{"run --interval with nothing after it", newRun(), []string{"--interval"}},
		{"run --without given an empty value", newRun(), []string{"--without", ""}},
		{"once given a positional", flag.NewFlagSet("once", flag.ContinueOnError), []string{"now"}},
		{"stop given a positional", flag.NewFlagSet("stop", flag.ContinueOnError), []string{"nas"}},
		{"uninstall given a positional", flag.NewFlagSet("uninstall", flag.ContinueOnError), []string{"jobd"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := parse(c.fs, c.args); err == nil {
				t.Fatal("accepted; an argument the command will not act on must be a refusal")
			}
		})
	}
}

func TestParseAccepts(t *testing.T) {
	fs, interval, without := runFlags()
	if _, err := parse(fs, []string{"--interval", "5s", "--without", "nas", "--without", "bits"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if *interval != 5*time.Second || len(*without) != 2 {
		t.Fatalf("interval=%v without=%v", *interval, *without)
	}
}

// The number a shell sees is the class of the failure, and it comes from the
// error rather than from a list kept here.
func TestStatusCarriesTheClass(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
		want int
	}{
		{"nothing went wrong", nil, 0},
		{"a dropped connection is not now", errors.New("read: connection reset"), 1},
		{"nothing can serve these sources is no", download.ErrNoFetcher, 3},
		{"the source refused is no", download.ErrRefused, 3},
		{"an unreadable record is no", fmt.Errorf("%w: spec", job.ErrInvalid), 3},
		{"a wrapped refusal is still no", fmt.Errorf("adopt: %w", download.ErrRefused), 3},
		{"a lost delegate answer is unknown", fmt.Errorf("nas: %w", download.ErrOutcomeUnknown), 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := status(c.err); got != c.want {
				t.Fatalf("status = %d, want %d", got, c.want)
			}
		})
	}
}

func newRun() *flag.FlagSet { fs, _, _ := runFlags(); return fs }
