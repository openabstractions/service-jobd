package discovery

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// The registry maps a SERVICE NAME to an endpoint, and it is the only reason
// a caller can be ignorant of process topology.
//
// One process serving jobs and downloads publishes one endpoint under two
// names. Split it in two tomorrow and it publishes two. The client code is
// identical and no caller can tell which happened, because the number of
// processes never appears anywhere a caller can see it. That is the whole
// mechanism, and everything else in this file is bookkeeping in service of it.
//
// A client must never assume two names sharing an endpoint are related, or
// that two names with different endpoints are unrelated.

// RegistryName sits beside supervisor.json in the store.
//
// The topology contract shows the registry's CONTENT and never names the file.
// This is the invented part: "registry.json", in the store root, one object
// with one "services" key. A second implementer choosing "services.json" or
// nesting the map inside supervisor.json produces a registry the first cannot
// read, and the symptom is a permanent, silent absent — exactly the class of
// divergence the endpoint rule exists to prevent.
const RegistryName = "registry.json"

// ServiceJobs and ServiceDownloads are what jobd publishes. Both point at one
// endpoint today because one process serves both; that is a deployment fact and
// no caller may depend on it.
const (
	ServiceJobs      = "abstraction.jobs"
	ServiceDownloads = "abstraction.downloads"
)

// Registry is the file.
type Registry struct {
	Services map[string]string `json:"services"`
}

// RegistryPath is where the registry lives for a store rooted at root.
func RegistryPath(root string) string { return filepath.Join(root, RegistryName) }

// LoadRegistry reads it. A missing file is an empty registry and not an error:
// no service has ever registered here, which is the state every store starts
// in.
func LoadRegistry(root string) (Registry, error) {
	b, err := os.ReadFile(RegistryPath(root))
	if errors.Is(err, os.ErrNotExist) {
		return Registry{Services: map[string]string{}}, nil
	}
	if err != nil {
		return Registry{}, err
	}
	var r Registry
	if err := json.Unmarshal(b, &r); err != nil {
		return Registry{}, fmt.Errorf("registry %s: %w", RegistryPath(root), err)
	}
	if r.Services == nil {
		r.Services = map[string]string{}
	}
	return r, nil
}

// Resolve turns a service name into an endpoint name, or "" if nobody has
// claimed it. Every failure — no store, no registry, unreadable registry — is
// "" as well, because a client that cannot resolve reports absent and a client
// that reports absent is never wrong in a way that loses data.
func Resolve(root, service string) string {
	r, err := LoadRegistry(root)
	if err != nil {
		return ""
	}
	return r.Services[service]
}

// ErrNameHeld means another live service already answers under this name.
var ErrNameHeld = errors.New("discovery: service name is held by something that answers")

// Claim takes a set of service names for one endpoint and writes the registry.
//
// The discipline is the store's lease discipline, applied to a name rather than
// a job: refuse to start if the name is held AND its endpoint answers; if it is
// held and does not answer, the holder is gone and the name may be taken.
//
// Taking it over REUSES the dead endpoint name rather than minting a fresh one.
// That is a choice the contract does not make. Minting fresh is tidier to
// reason about — a brand new random name cannot collide with anything — but it
// leaks a socket file into $XDG_RUNTIME_DIR on every unclean exit, forever,
// and it makes the contract's own rule about unlinking a stale socket dead
// code, since a name nobody has ever used cannot have a stale socket at it.
// Reuse keeps that rule meaningful and keeps the directory bounded.
func Claim(root string, endpoint string, names ...string) (string, error) {
	unlock, err := lockRegistry(root)
	if err != nil {
		return "", err
	}
	defer unlock()

	reg, err := LoadRegistry(root)
	if err != nil {
		return "", err
	}

	// Every name a live service holds stops us dead. Probing is a connect, and
	// a connect is the only honest test: the registry says who claimed the
	// name, never who is still there.
	reuse := ""
	for _, n := range names {
		held := reg.Services[n]
		if held == "" {
			continue
		}
		if Answers(held) {
			return "", fmt.Errorf("%w: %s at %s", ErrNameHeld, n, held)
		}
		// Dead. Reuse its endpoint if every held name agrees on one; if two
		// names were held by two different corpses there is no single name to
		// inherit, so mint instead. The contract does not cover the split case
		// because it never contemplates one process claiming two names that
		// were previously served by two.
		switch {
		case reuse == "":
			reuse = held
		case reuse != held:
			reuse = "-"
		}
	}
	if endpoint == "" {
		if reuse != "" && reuse != "-" {
			endpoint = reuse
		} else {
			endpoint = NewEndpointName()
		}
	}

	for _, n := range names {
		reg.Services[n] = endpoint
	}
	if err := writeRegistry(root, reg); err != nil {
		return "", err
	}
	return endpoint, nil
}

// Withdraw removes names this process published, on a clean exit.
//
// Best effort, and deliberately so: an unclean exit leaves the entry behind,
// a client connects to it, the connect fails, and the answer is absent. That
// is the designed behaviour, not a leak to be tidied — which is why nothing
// anywhere waits for this to have happened.
func Withdraw(root string, endpoint string, names ...string) {
	unlock, err := lockRegistry(root)
	if err != nil {
		return
	}
	defer unlock()
	reg, err := LoadRegistry(root)
	if err != nil {
		return
	}
	for _, n := range names {
		// Only our own. Removing an entry another supervisor wrote a
		// millisecond ago would make it undiscoverable while it is running.
		if reg.Services[n] == endpoint {
			delete(reg.Services, n)
		}
	}
	_ = writeRegistry(root, reg)
}

// writeRegistry replaces the file atomically. A reader must never see half a
// registry and conclude the service is unregistered.
func writeRegistry(root string, reg Registry) error {
	if reg.Services == nil {
		reg.Services = map[string]string{}
	}
	b, err := json.MarshalIndent(reg, "", "  ")
	if err != nil {
		return err
	}
	// LF, unconditionally. A CRLF here would be invisible on Windows and
	// change the bytes of a file another implementation compares.
	b = append(b, '\n')
	tmp := RegistryPath(root) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, RegistryPath(root))
}

// lockRegistry serialises read-modify-write across processes.
//
// Invented. The topology contract says a service claims its name by "create
// exclusively", which is a lease discipline for one file per claim. A registry
// is one file holding MANY claims, so exclusive creation of the registry itself
// is wrong — the second service to ever start would find the file present and
// have nothing to do about it. What is actually needed is a mutex over the
// read-modify-write, and O_EXCL on a lock file beside it is the smallest thing
// that provides one on all three platforms.
//
// A stale lock left by a killed process is broken after it goes cold, because
// refusing to ever start again because of a corpse is worse than the race.
func lockRegistry(root string) (func(), error) {
	path := RegistryPath(root) + ".lock"
	deadline := time.Now().Add(2 * time.Second)
	for {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			fmt.Fprintf(f, "%d\n", os.Getpid())
			f.Close()
			return func() { os.Remove(path) }, nil
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if st, serr := os.Stat(path); serr == nil && time.Since(st.ModTime()) > 30*time.Second {
			os.Remove(path)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("discovery: %s is held by another process", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// ServiceNames is the sorted list of names in a registry, for humans.
func (r Registry) ServiceNames() []string {
	out := make([]string, 0, len(r.Services))
	for n := range r.Services {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
