package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	download "github.com/openabstractions/abstraction-download/go"
	job "github.com/openabstractions/abstraction-job/go"
)

// cmdDiscover asks over the bus what the heartbeat can only predict: whether
// the announced supervisor answers, and who it takes this process for.
func cmdDiscover(args []string) {
	need(flag.NewFlagSet("discover", flag.ContinueOnError), args)
	store, err := job.NewFileStore(storeRoot())
	if err != nil {
		fatal(err)
	}
	fmt.Printf("store:     %s\n", storeRoot())
	sup, live := download.SupervisorOf(store)
	switch {
	case live:
		fmt.Printf("announced: %s, seen %s ago, every %s, delegates to: %s\n",
			sup.Owner, time.Since(sup.Seen.Time).Round(time.Second), sup.Every, tierOr(sup.Tier))
		if sup.Endpoint == "" {
			fmt.Println("bus:       none; reachable through the store only")
		} else {
			fmt.Printf("bus:       %s\n", sup.Endpoint)
		}
	case sup.Owner != "":
		fmt.Printf("announced: %s, last seen %s, treated as gone\n", sup.Owner, sup.Seen.Format(time.RFC3339))
	default:
		fmt.Println("announced: nothing")
	}
	a, err := download.Who(store)
	switch {
	case err == nil:
		fmt.Printf("answered:  %s, delegates to: %s\n", a.Owner, tierOr(a.Tier))
	case errors.Is(err, download.ErrCallerRefused):
		fmt.Printf("refused:   %s\n", a.Error)
	default:
		fmt.Printf("answered:  nobody (%v)\n", err)
		os.Exit(1)
	}
	fmt.Printf("caller:    %s\n", a.Caller)
	if a.Caller.Notes != "" {
		fmt.Printf("notes:     %s\n", a.Caller.Notes)
	}
	if err != nil {
		os.Exit(1)
	}
}

func tierOr(tier string) string {
	if tier == "" {
		return "here"
	}
	return tier
}
