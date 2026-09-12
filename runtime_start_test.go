package main

import (
	"context"
	"errors"
	download "github.com/openabstractions/abstraction-download/go"
	"testing"
)

func TestUnelevatedStartGuardRefusesElevatedAndUnobservedToken(t *testing.T) {
	for _, test := range []struct {
		elevated bool
		err      error
		allowed  bool
	}{
		{false, nil, true}, {true, nil, false}, {false, errors.New("token query refused"), false},
	} {
		err := checkUnelevated(func() (bool, error) { return test.elevated, test.err })
		if (err == nil) != test.allowed {
			t.Fatalf("elevated=%v query=%v result=%v", test.elevated, test.err, err)
		}
	}
}

func TestRuntimeStartRequiresCapabilitiesForEveryBusReply(t *testing.T) {
	for _, already := range []bool{false, true} {
		for _, ready := range []bool{false, true} {
			checked := false
			got, err := startWithRuntime(context.Background(), download.Starting{Args: []string{"run", "--runtime"}}, true,
				func(options download.Starting) (download.Started, error) {
					if len(options.Args) != 2 || options.Args[1] != "--runtime" {
						t.Fatal("child flags changed")
					}
					// A foreign/concurrent responder can supply these claims. They do not
					// bypass the separate readiness check, even after a local child spawn.
					return download.Started{Already: already, Owner: "foreign claim", PID: 123}, nil
				}, func(context.Context) error {
					checked = true
					if !ready {
						return errors.New("no logging/config")
					}
					return nil
				})
			if !checked || (err == nil) != ready || got.Already != already {
				t.Fatalf("already=%v ready=%v checked=%v err=%v", already, ready, checked, err)
			}
		}
	}
}

func TestPlainStartPreservesBusIdempotence(t *testing.T) {
	got, err := startWithRuntime(context.Background(), download.Starting{}, false,
		func(download.Starting) (download.Started, error) { return download.Started{Already: true}, nil },
		func(context.Context) error { t.Fatal("plain start checked runtime"); return nil })
	if err != nil || !got.Already {
		t.Fatalf("start = %+v %v", got, err)
	}
}

func TestFailedStartDoesNotRunStatus(t *testing.T) {
	failure := errors.New("start failed")
	_, err := startWithRuntime(context.Background(), download.Starting{}, true,
		func(download.Starting) (download.Started, error) { return download.Started{}, failure },
		func(context.Context) error { t.Fatal("status after failed start"); return nil })
	if !errors.Is(err, failure) {
		t.Fatal(err)
	}
}
