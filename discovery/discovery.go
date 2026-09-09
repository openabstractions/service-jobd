// Package discovery answers "is a supervisor alive on this machine, right now?"
// by connecting to a socket instead of reading a timestamp out of a file.
//
// It implements docs/discovery-ipc.md and docs/service-topology.md. The reason
// for it is that a heartbeat file is a liveness *heuristic*: a supervisor that
// was killed leaves a record saying "running" for as long as the staleness
// window lasts, and everything that reads the store in that window is lied to.
// A connection either succeeds — something is listening now — or it does not.
//
// # What is still the file's job
//
// The store. Records must outlive every process and be readable by a machine
// that was switched off, so they stay files. And a local socket does not cross
// to another machine: when the supervisor is a NAS, the only thing both ends
// share is the store, so cross-machine discovery keeps the heartbeat. See
// Locate in the jobd main package for the algorithm that chooses between them.
//
// # This package holds no policy
//
// Nothing here decides what a caller does with an answer. It reports absent,
// present or incompatible, and connect failure is the ordinary answer rather
// than an error — no supervisor is the common case on a desktop.
package discovery

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"
)

// Base is the one capability name this implementation knows. A response
// listing anything else under "critical" describes a supervisor this client
// cannot safely hand work to.
const Base = "abstraction.discovery/base@1"

// Budget is the whole client deadline: connect, write and read together, and
// on this side the store read that resolves the endpoint as well.
//
// The contract fixes it at 200 ms and says it is computed at entry. Anything
// slower than that is indistinguishable, for the caller's purposes, from
// nothing listening — the caller downloads in-process and loses a handoff,
// which is the cheap failure.
const Budget = 200 * time.Millisecond

// ConnDeadline is what the SERVER allows one connection, start to finish.
//
// It exists so that a single local process which connects and never sends
// cannot push every other caller into its own timeout. One second is ample for
// a request that is one short line.
const ConnDeadline = time.Second

// MaxLine caps a response, and a request, at 64 KiB.
//
// A hostile local process can otherwise feed unbounded bytes to a reader that
// is waiting for a newline, and the reader's memory is the only limit. Both
// ends cap: the contract only names the client, but a server with no cap is a
// supervisor anybody on the machine can exhaust.
const MaxLine = 64 * 1024

// AskWho asks a supervisor to describe itself.
const AskWho = "who"

// AskLook asks it to sweep the store now.
//
// It shares this endpoint rather than getting one of its own. Two sockets on
// one daemon means two stale-file policies, which is one more than anybody can
// keep correct.
const AskLook = "look"

// Request is the whole of what a caller may send.
type Request struct {
	Ask string `json:"ask"`
}

// Response is a supervisor describing itself. Nothing in it authorises
// anything: every field is self-description, and self-description is forgeable
// in principle, so no decision anywhere may turn on a name in here. The
// reference — knowing the endpoint, which means having been able to read the
// store — is the authority.
type Response struct {
	// Content is everything this supervisor implements.
	Content []string `json:"content"`
	// Critical is the subset a caller MUST understand before handing work
	// over. A name here that a client does not know means incompatible.
	Critical []string `json:"critical"`
	// Owner is "program@host:pid", the same string the heartbeat carries.
	Owner string `json:"owner"`
	// Host is required: the cross-machine rule cannot be applied without it.
	Host string `json:"host"`
	// Store is the store this supervisor watches. A client compares it with
	// the store it opened itself and treats a mismatch as absent.
	Store string `json:"store"`
	// DelegatesTo is advisory. A client must work without understanding it.
	DelegatesTo string `json:"delegates_to,omitempty"`
	PID         int    `json:"pid"`
	// StartedAt is the job record's timestamp format exactly: UTC, six
	// fractional digits, trailing Z. Not RFC3339Nano, which trims trailing
	// zeros and makes two correct implementations write different bytes.
	StartedAt string `json:"started_at"`
	// Error is set instead of everything else, and only by the server.
	Error string `json:"error,omitempty"`
}

// TimestampLayout is the job record's format, repeated here rather than
// imported so that this package depends on nothing but the standard library
// and the platform. Six digits because Python's datetime cannot represent
// nanoseconds, and a wire contract is set by its least precise participant.
const TimestampLayout = "2006-01-02T15:04:05.000000Z"

// FormatTime renders an instant the way a job record does.
func FormatTime(t time.Time) string {
	return t.UTC().Truncate(time.Microsecond).Format(TimestampLayout)
}

// errUnknownAsk and errInvalidRequest are the two error responses, pre-encoded
// at package level because answering must not allocate its way through the
// store, the heap, or anything else that can fail while a supervisor is
// otherwise healthy.
var (
	errUnknownAsk     = mustLine(map[string]string{"error": "unknown ask"})
	errInvalidRequest = mustLine(map[string]string{"error": "invalid request"})
)

// mustLine encodes one object as one line: UTF-8 JSON, no indentation, no
// carriage return, no BOM, terminated by a single 0x0A.
//
// json.Marshal is used rather than MarshalIndent deliberately. The heartbeat
// writer indents, and anything copied from it produces a pretty-printed object
// which is unframeable — the reader is looking for a newline that arrives in
// the middle of the value.
func mustLine(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		// Nothing reachable can fail here: the inputs are strings, ints and
		// string slices. Panicking beats shipping a server that answers with
		// an empty line, because silence and a hang are indistinguishable.
		panic("discovery: response does not encode: " + err.Error())
	}
	return append(b, '\n')
}

// NewEndpointName invents an endpoint: 128 random bits, hex, with a prefix that
// makes it recognisable in a directory listing or in `pipelist`.
//
// It is NOT derived from the store path, and the reasons are all failures that
// happen silently. Case folding, \\?\ prefixes, 8.3 short names, a mapped drive
// against the UNC path behind it, /private on macOS, Unicode normalisation on
// HFS+, and a legacy store alias each give two names for one store: both sides
// correct, different endpoint, caller reports absent forever.
//
// Random also means unguessable, which turns the endpoint into a capability.
// Being able to read the store is what confers the reference, so a process that
// cannot read the store cannot squat the name either.
func NewEndpointName() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on any platform this runs on; if it did,
		// carrying on with a predictable name would hand the endpoint to
		// anyone who can guess it.
		panic("discovery: no randomness for an endpoint name: " + err.Error())
	}
	return "abstraction-" + hex.EncodeToString(b[:])
}
