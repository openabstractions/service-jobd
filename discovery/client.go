package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// State is the answer. Three of them, not two.
type State int

const (
	// Absent — nothing is listening. This is the ORDINARY case on a desktop,
	// not a warning, not a log line above debug, and never an error the caller
	// has to catch. It also covers every way of failing to get a usable
	// answer: a partial line, EOF before the newline, malformed JSON, a line
	// over the cap, a store that is not the one this caller opened, and any
	// timeout. A false absent costs a handoff that could have happened; it is
	// safe only because the lease prevents two owners.
	Absent State = iota

	// Present — a supervisor is listening, watches this store, and needs
	// nothing this client does not have.
	Present

	// Incompatible — something is listening and it is NEWER than this client.
	// The caller must not hand work over. This is not an error to report to a
	// human as a fault; it is a supervisor that has moved on.
	Incompatible
)

func (s State) String() string {
	switch s {
	case Present:
		return "present"
	case Incompatible:
		return "incompatible"
	default:
		return "absent"
	}
}

// Answer is what a caller gets back. There is no error return anywhere in this
// file, and that is the rule about connect failure expressed in a signature
// rather than in a comment: a caller cannot accidentally treat "no supervisor"
// as a fault, because there is nothing to treat.
type Answer struct {
	State State
	// Supervisor is the self-description, when there was one. Nil for absent.
	// NOTHING in it authorises anything — it is what the far end says about
	// itself, and a caller that makes a decision on a name in here has
	// reinvented the ambient authority the endpoint scheme removed.
	Supervisor *Response
	// Why is for a human reading debug output, and for tests. It is never an
	// error, never returned as one, and never logged above debug.
	Why string
}

// Ask resolves a service in the store at root and asks it one question.
//
// One deadline, computed here at entry, spent across resolving the endpoint,
// connecting, writing and reading. Resolving reads a file, and on a store that
// lives on a network share that read is by far the slowest part of the whole
// exchange — see the notes: the contract's budget says "connect, write and
// read" and does not say what happens to the read of the registry that has to
// come first. Including it is the literal reading of "computed at entry", and
// it is the safe one, because the alternative lets one slow stat call spend
// unbounded time before the clock starts.
func Ask(root, service, ask string) Answer {
	deadline := time.Now().Add(Budget)

	endpoint := Resolve(root, service)
	if endpoint == "" {
		return Answer{Why: "no registry entry for " + service}
	}
	addr, err := Address(endpoint)
	if err != nil {
		return Answer{Why: err.Error()}
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if time.Now().After(deadline) {
		return Answer{Why: "budget spent resolving the endpoint"}
	}

	// A client NEVER unlinks. It has no idea whether the path belongs to a
	// corpse or to a supervisor that bound it a millisecond ago, and removing
	// the latter leaves a healthy process listening on an unlinked inode where
	// nobody can ever find it. Refused connection, missing path, missing pipe:
	// all of them are absent, and all of them touch nothing.
	c, err := dial(ctx, addr)
	if err != nil {
		return Answer{Why: "no answer at " + addr + ": " + err.Error()}
	}
	defer c.Close()
	_ = c.SetDeadline(deadline)

	if _, err := c.Write(mustLine(Request{Ask: ask})); err != nil {
		return Answer{Why: "write: " + err.Error()}
	}

	// Bounded, on purpose. A hostile local process that accepts and then
	// streams bytes with no newline in them is otherwise limited only by this
	// caller's memory.
	br := bufio.NewReaderSize(io.LimitReader(c, MaxLine+1), 4096)
	line, err := readLine(br)
	if err != nil {
		// This is the branch that hangs forever in a naive Windows client. A
		// synchronous ReadFile on a named pipe cannot be interrupted, so a
		// server that accepts and never writes is not a 200 ms timeout, it is
		// a wedged process. Every handle this package opens is overlapped,
		// which is what makes the deadline above real.
		return Answer{Why: "read: " + err.Error()}
	}

	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Answer{Why: "unparseable response"}
	}
	if resp.Error != "" {
		// The far end declined to describe itself. It is listening, but it has
		// told us nothing we can act on, so it cannot be present — and it is
		// not incompatible either, because incompatible is a claim about
		// capability names and we were given none.
		return Answer{Why: "error response: " + resp.Error}
	}

	// The store check, before the capability check. A supervisor for a
	// different store on the same machine is not "an incompatible
	// supervisor"; it is not this store's supervisor at all.
	if !SameStore(resp.Store, root) {
		return Answer{Why: "answers for store " + resp.Store + ", not " + root}
	}

	for _, name := range resp.Critical {
		if !known(name) {
			return Answer{State: Incompatible, Supervisor: &resp,
				Why: "critical capability not understood: " + name}
		}
	}
	return Answer{State: Present, Supervisor: &resp}
}

// Query is the common case: is a supervisor watching this store, now.
func Query(root string) Answer { return Ask(root, ServiceJobs, AskWho) }

// Look asks the supervisor to sweep the store now, and reports what answered.
//
// Best effort by construction, like the nudge it replaces. It carries no job
// id and no payload; losing it costs latency and nothing else, because the
// sweep was coming anyway. That property is what makes it safe for
// notification to exist at all — a channel that carried state would be a
// second source of truth alongside the store.
func Look(root string) Answer { return Ask(root, ServiceJobs, AskLook) }

// known reports whether this client understands a capability name.
func known(name string) bool { return name == Base }

// SameStore decides whether two store paths name one store.
//
// This is the ugliest function in the package and it exists because the
// contract asks for a comparison it also proves is unsafe. The endpoint may
// not be derived from the store path — case folding, \\?\ prefixes, short
// names, mapped drives against UNC, /private on macOS, Unicode normalisation —
// and then the response check compares store paths as strings, which walks
// into every one of those hazards a second time. Two processes that opened
// "C:\Users\me\.abstraction" and "c:/users/me/.abstraction" are watching one
// store and would report absent forever.
//
// So: resolve symlinks where possible, clean, and fold case on the platforms
// whose filesystems fold it. It is a heuristic and it is documented as one.
// A comparison that cannot be made exact is one more reason the store field
// should have been an opaque identifier the supervisor minted, not a path.
func SameStore(a, b string) bool {
	return normaliseStore(a) == normaliseStore(b)
}

func normaliseStore(p string) string {
	if p == "" {
		return ""
	}
	if r, err := filepath.EvalSymlinks(p); err == nil {
		p = r
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	p = strings.TrimRight(p, `/\`)
	if foldsCase() {
		p = strings.ToLower(p)
	}
	return p
}

// foldsCase is true where the platform's usual filesystem is case-insensitive.
// Windows always. macOS by default, though APFS can be case-sensitive, which is
// a case this gets wrong in the safe direction: two paths differing only in
// case would be treated as one store on a volume where they are two.
func foldsCase() bool {
	switch runtime.GOOS {
	case "windows", "darwin":
		return true
	}
	return false
}
