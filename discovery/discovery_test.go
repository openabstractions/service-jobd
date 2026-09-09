package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The conformance list from the contract, one test each, plus the cases
// implementing it turned up.
//
// The one that matters is TestServerAcceptsAndNeverWrites. On Windows a
// synchronous read on a named pipe cannot be interrupted: a client that used
// ordinary blocking I/O would not time out there, it would wedge, and every
// application that asked would wedge with it. It runs against a real pipe on
// this platform because that is the only place it proves anything.

func testIdentity(root string) Identity {
	return Identity{
		Content:     []string{Base},
		Critical:    []string{Base},
		Owner:       "jobdtest@testhost:4242",
		Host:        "testhost",
		Store:       root,
		DelegatesTo: "here",
		PID:         4242,
		StartedAt:   time.Now(),
	}
}

// serveFor binds a real endpoint for a store and registers it, the way the
// supervisor does.
func serveFor(t *testing.T, root string, id Identity) *Server {
	t.Helper()
	ep, err := Claim(root, "", ServiceJobs, ServiceDownloads)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	s, err := Serve(ep, id)
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// bareListener binds an endpoint and registers it WITHOUT this package's
// server behind it, so a test can misbehave on the wire deliberately.
func bareListener(t *testing.T, root string) (net.Listener, string) {
	t.Helper()
	ep := NewEndpointName()
	if _, err := Claim(root, ep, ServiceJobs); err != nil {
		t.Fatalf("claim: %v", err)
	}
	addr, err := Address(ep)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	if err := prepareDir(addr); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	ln, err := listen(addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, ep
}

// rawAsk speaks the protocol by hand and returns the raw line.
func rawAsk(t *testing.T, endpoint, request string) []byte {
	t.Helper()
	addr, err := Address(endpoint)
	if err != nil {
		t.Fatalf("address: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	c, err := dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte(request)); err != nil {
		t.Fatalf("write: %v", err)
	}
	line, err := bufio.NewReader(io.LimitReader(c, MaxLine+1)).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v (got %q)", err, line)
	}
	return line
}

func TestPresent(t *testing.T) {
	root := t.TempDir()
	serveFor(t, root, testIdentity(root))

	a := Query(root)
	if a.State != Present {
		t.Fatalf("state = %v (%s), want present", a.State, a.Why)
	}
	if a.Supervisor.Owner != "jobdtest@testhost:4242" {
		t.Errorf("owner = %q", a.Supervisor.Owner)
	}
	if a.Supervisor.PID != 4242 {
		t.Errorf("pid = %d", a.Supervisor.PID)
	}
	if a.Supervisor.Host == "" {
		t.Error("host is required by the contract and is empty")
	}
}

// Connecting where nothing listens returns absent within the timeout.
func TestAbsentNothingListening(t *testing.T) {
	root := t.TempDir()
	// A registered endpoint nobody ever bound: exactly what a killed
	// supervisor leaves behind.
	if _, err := Claim(root, NewEndpointName(), ServiceJobs); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	a := Query(root)
	took := time.Since(start)
	if a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
	if took > time.Second {
		t.Fatalf("took %v, well over the %v budget", took, Budget)
	}
}

// A store nothing has ever registered in. Not an error, not a warning.
func TestAbsentNoRegistry(t *testing.T) {
	if a := Query(t.TempDir()); a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
}

// THE ONE THAT MATTERS.
//
// A server that accepts the connection and then never writes a byte. A
// synchronous implementation hangs here forever on Windows; the client must
// give up inside its own budget.
func TestServerAcceptsAndNeverWrites(t *testing.T) {
	root := t.TempDir()
	ln, _ := bareListener(t, root)

	held := make(chan net.Conn, 4)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			// Accept, hold the connection open, say nothing. Ever.
			held <- c
		}
	}()
	t.Cleanup(func() {
		close(held)
		for c := range held {
			c.Close()
		}
	})

	start := time.Now()
	a := Query(root)
	took := time.Since(start)

	if a.State != Absent {
		t.Fatalf("state = %v, want absent (%s)", a.State, a.Why)
	}
	if took > time.Second {
		t.Fatalf("gave up after %v; the budget is %v and a hang here is the whole reason the rule exists", took, Budget)
	}
	if took < Budget/2 {
		t.Fatalf("gave up after %v, before the budget was spent — that is not a timeout, that is a connect failure in disguise", took)
	}
}

// A name that is bound but whose owner is not accepting. On Windows this is
// the ERROR_PIPE_BUSY path, which is NOT absent-on-sight: the client must wait
// inside its remaining budget and only then give up.
func TestBoundButNotAccepting(t *testing.T) {
	root := t.TempDir()
	bareListener(t, root) // never calls Accept

	start := time.Now()
	a := Query(root)
	took := time.Since(start)
	if a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
	if took > time.Second {
		t.Fatalf("took %v", took)
	}
}

// A stale endpoint left by a killed process reports absent, not an error.
func TestStaleEndpointIsAbsentNotAnError(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))
	if a := Query(root); a.State != Present {
		t.Fatalf("precondition: %v", a.State)
	}
	// Gone, without withdrawing its registry entry — which is what a kill
	// looks like.
	s.Close()

	a := Query(root)
	if a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
	if Resolve(root, ServiceJobs) == "" {
		t.Fatal("the registry entry should survive a kill; that is what makes takeover possible")
	}
}

// An unknown ask returns the error object rather than a closed connection.
func TestUnknownAsk(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	line := rawAsk(t, s.Endpoint(), `{"ask":"weather"}`+"\n")
	var got map[string]string
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unparseable: %q", line)
	}
	if got["error"] != "unknown ask" {
		t.Fatalf(`got %q, want {"error":"unknown ask"}`, line)
	}
}

func TestInvalidRequest(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	line := rawAsk(t, s.Endpoint(), "this is not json\n")
	var got map[string]string
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("unparseable: %q", line)
	}
	if got["error"] != "invalid request" {
		t.Fatalf(`got %q, want {"error":"invalid request"}`, line)
	}
}

// A client meeting an error object gets absent: something is listening, but it
// has described nothing this caller can act on.
func TestErrorResponseIsAbsent(t *testing.T) {
	root := t.TempDir()
	ln, _ := bareListener(t, root)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(2 * time.Second))
				bufio.NewReader(c).ReadBytes('\n')
				c.Write([]byte(`{"error":"unknown ask"}` + "\n"))
				io.Copy(io.Discard, c)
			}(c)
		}
	}()
	if a := Query(root); a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
}

// An unknown critical name returns incompatible, and the caller is told not to
// hand work over.
func TestIncompatible(t *testing.T) {
	root := t.TempDir()
	id := testIdentity(root)
	id.Content = []string{Base, "abstraction.discovery/leases@2"}
	id.Critical = []string{"abstraction.discovery/leases@2"}
	serveFor(t, root, id)

	a := Query(root)
	if a.State != Incompatible {
		t.Fatalf("state = %v, want incompatible (%s)", a.State, a.Why)
	}
	if a.Supervisor == nil {
		t.Fatal("an incompatible answer should still carry what the far end said")
	}
}

// A capability listed under content but not critical is not a reason to
// refuse: that is the whole difference between the two lists.
func TestNewContentIsStillPresent(t *testing.T) {
	root := t.TempDir()
	id := testIdentity(root)
	id.Content = []string{Base, "abstraction.discovery/something@9"}
	id.Critical = []string{Base}
	serveFor(t, root, id)

	if a := Query(root); a.State != Present {
		t.Fatalf("state = %v, want present (%s)", a.State, a.Why)
	}
}

// A response whose store differs from the client's returns absent.
func TestStoreMismatchIsAbsent(t *testing.T) {
	root := t.TempDir()
	id := testIdentity(root)
	id.Store = t.TempDir() // a supervisor for a different store entirely
	serveFor(t, root, id)

	a := Query(root)
	if a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
}

// The store comparison must survive the things a path does on the way through
// two processes. The contract asks for it as a plain string match; it is not
// one, and pretending otherwise is a permanent silent absent.
func TestStoreComparisonNormalises(t *testing.T) {
	root := t.TempDir()
	if !SameStore(root, root+string(os.PathSeparator)) {
		t.Error("a trailing separator should not make two stores")
	}
	if !SameStore(root, root) {
		t.Error("a store is itself")
	}
	if SameStore(root, t.TempDir()) {
		t.Error("two different directories are two stores")
	}
	if foldsCase() && !SameStore(strings.ToUpper(root), strings.ToLower(root)) {
		t.Error("case folding platform: one store spelled two ways is one store")
	}
}

// Two stores on one machine have different endpoints and do not cross-talk.
func TestTwoStoresDoNotCrossTalk(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	idA, idB := testIdentity(a), testIdentity(b)
	idA.Owner, idB.Owner = "a@h:1", "b@h:2"
	sa := serveFor(t, a, idA)
	sb := serveFor(t, b, idB)

	if sa.Endpoint() == sb.Endpoint() {
		t.Fatal("two stores got one endpoint")
	}
	if got := Query(a); got.State != Present || got.Supervisor.Owner != "a@h:1" {
		t.Errorf("store a answered %v/%v", got.State, got.Supervisor)
	}
	if got := Query(b); got.State != Present || got.Supervisor.Owner != "b@h:2" {
		t.Errorf("store b answered %v/%v", got.State, got.Supervisor)
	}
}

// A line longer than the cap returns absent rather than consuming memory.
func TestOverLongLineIsAbsent(t *testing.T) {
	root := t.TempDir()
	ln, _ := bareListener(t, root)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				bufio.NewReader(c).ReadBytes('\n')
				// No newline, ever. Just bytes.
				junk := make([]byte, 8192)
				for i := range junk {
					junk[i] = 'x'
				}
				for i := 0; i < 64; i++ {
					if _, err := c.Write(junk); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	start := time.Now()
	a := Query(root)
	if a.State != Absent {
		t.Fatalf("state = %v, want absent", a.State)
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("took %v — a capped read should not wait for the writer to finish", took)
	}
}

// One connection must not block accept. A single local process that connects
// and says nothing would otherwise put every application on the machine into
// its own timeout, and all of them report absent at once.
func TestOneConnectionDoesNotBlockAccept(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	addr, _ := Address(s.Endpoint())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	silent, err := dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer silent.Close() // connected, never writes, holds the connection

	// Give the server a moment to have accepted it.
	time.Sleep(50 * time.Millisecond)

	for i := 0; i < 3; i++ {
		if a := Query(root); a.State != Present {
			t.Fatalf("query %d = %v (%s); one silent caller starved the endpoint", i, a.State, a.Why)
		}
	}
}

// look asks for a sweep and still gets an answer, so a caller that wants both
// spends one round trip.
func TestLookAsksForASweepAndAnswers(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	a := Look(root)
	if a.State != Present {
		t.Fatalf("state = %v, want present (%s)", a.State, a.Why)
	}
	select {
	case <-s.Look():
	case <-time.After(2 * time.Second):
		t.Fatal("look did not reach the supervisor")
	}
}

// Ten looks in a row are one reason to sweep, not ten.
func TestLookCoalesces(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))
	for i := 0; i < 10; i++ {
		Look(root)
	}
	// The server signals after it has answered, so the last few signals land
	// slightly behind the last client returning. Wait for it to go quiet
	// before asserting on the depth of the queue.
	time.Sleep(300 * time.Millisecond)
	select {
	case <-s.Look():
	case <-time.After(2 * time.Second):
		t.Fatal("no sweep asked for")
	}
	select {
	case <-s.Look():
		t.Fatal("a second sweep was queued; the channel must coalesce")
	case <-time.After(100 * time.Millisecond):
	}
}

// The framing rules, checked on the bytes rather than on the struct: one
// object, one 0x0A at the end, no carriage return, no BOM, nothing embedded.
func TestResponseFraming(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	line := rawAsk(t, s.Endpoint(), `{"ask":"who"}`+"\n")
	if len(line) == 0 || line[len(line)-1] != '\n' {
		t.Fatalf("response does not end in a single 0x0A: %q", line)
	}
	body := line[:len(line)-1]
	if bytesContains(body, '\n') {
		t.Error("embedded newline: the response is unframeable")
	}
	if bytesContains(body, '\r') {
		t.Error("carriage return in the response")
	}
	if bytesContains(body, ' ') && strings.Contains(string(body), ":  ") {
		t.Error("looks pretty-printed")
	}
	if len(line) >= 3 && line[0] == 0xEF && line[1] == 0xBB && line[2] == 0xBF {
		t.Error("BOM")
	}
	var r Response
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("not one JSON object: %v", err)
	}
	// started_at is the job record's format exactly. Not RFC3339Nano, which
	// trims trailing zeros and makes two correct implementations disagree
	// about the bytes for one instant.
	if !regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{6}Z$`).MatchString(r.StartedAt) {
		t.Errorf("started_at = %q, want six fractional digits and a trailing Z", r.StartedAt)
	}
}

func bytesContains(b []byte, c byte) bool {
	for _, x := range b {
		if x == c {
			return true
		}
	}
	return false
}

// A request the server cannot frame is still answered. Silence and a hang are
// indistinguishable to the far end, so the server never closes without saying
// something.
func TestOverLongRequestIsAnsweredNotDropped(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))
	addr, _ := Address(s.Endpoint())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dial(ctx, addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	junk := make([]byte, 4096)
	for i := range junk {
		junk[i] = 'x'
	}
	// More than the cap, no newline. The write may fail partway once the
	// server has stopped reading, which is fine and is the point.
	for i := 0; i < 32; i++ {
		if _, err := c.Write(junk); err != nil {
			break
		}
	}
	line, err := bufio.NewReader(io.LimitReader(c, MaxLine+1)).ReadBytes('\n')
	if err != nil {
		t.Fatalf("server went silent on an over-long request: %v", err)
	}
	if !strings.Contains(string(line), "invalid request") {
		t.Fatalf("got %q", line)
	}
}

// The registry is the whole topology mechanism: two names, one endpoint, and
// no caller can tell how many processes there are.
func TestTwoNamesOneEndpoint(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	jobs := Resolve(root, ServiceJobs)
	downloads := Resolve(root, ServiceDownloads)
	if jobs != s.Endpoint() || downloads != s.Endpoint() {
		t.Fatalf("registry has jobs=%q downloads=%q, endpoint=%q", jobs, downloads, s.Endpoint())
	}
	for _, svc := range []string{ServiceJobs, ServiceDownloads} {
		if a := Ask(root, svc, AskWho); a.State != Present {
			t.Errorf("%s = %v (%s)", svc, a.State, a.Why)
		}
	}
}

// A name held by something that answers is refused. A name held by something
// that does not is inherited, endpoint and all, so the socket it left behind
// can be unlinked instead of abandoned.
func TestClaimRefusesALiveNameAndInheritsADeadOne(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))
	first := s.Endpoint()

	if _, err := Claim(root, "", ServiceJobs); err == nil {
		t.Fatal("claimed a name that answers")
	}

	s.Close()
	again, err := Claim(root, "", ServiceJobs)
	if err != nil {
		t.Fatalf("could not take over a dead name: %v", err)
	}
	if again != first {
		t.Fatalf("takeover minted %q instead of inheriting %q; the stale socket at the old name is now unreachable litter", again, first)
	}
}

func TestWithdrawOnlyRemovesItsOwn(t *testing.T) {
	root := t.TempDir()
	mine, err := Claim(root, "", ServiceJobs)
	if err != nil {
		t.Fatal(err)
	}
	Withdraw(root, "somebody-elses-endpoint", ServiceJobs)
	if got := Resolve(root, ServiceJobs); got != mine {
		t.Fatalf("withdraw removed an entry it did not write: %q", got)
	}
	Withdraw(root, mine, ServiceJobs)
	if got := Resolve(root, ServiceJobs); got != "" {
		t.Fatalf("withdraw left %q", got)
	}
}

func TestEndpointNamesAreRandomAndNotDerived(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 500; i++ {
		n := NewEndpointName()
		if len(n) != len("abstraction-")+32 {
			t.Fatalf("name %q is not 128 bits of hex", n)
		}
		if seen[n] {
			t.Fatalf("collision on %q", n)
		}
		seen[n] = true
	}
}

// Everything the client does with a dead endpoint must leave the filesystem
// exactly as it found it. A client that unlinks can remove a socket a
// supervisor bound a millisecond earlier, leaving it listening on an unlinked
// inode: running, healthy, and undiscoverable.
func TestClientTouchesNothing(t *testing.T) {
	root := t.TempDir()
	ep := NewEndpointName()
	if _, err := Claim(root, ep, ServiceJobs); err != nil {
		t.Fatal(err)
	}
	before := dirState(t, root)
	for i := 0; i < 3; i++ {
		Query(root)
	}
	if after := dirState(t, root); after != before {
		t.Fatalf("the store changed under a client:\n before %s\n after  %s", before, after)
	}
}

func dirState(t *testing.T, dir string) string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, e := range ents {
		info, err := e.Info()
		if err != nil {
			continue
		}
		b.WriteString(e.Name())
		b.WriteString(":")
		b.WriteString(info.ModTime().Format(time.RFC3339Nano))
		b.WriteString(" ")
	}
	return b.String()
}

// Answering must not read the store. A supervisor whose store has become
// unreadable is still a supervisor, and it must still be able to say so.
func TestAnsweringSurvivesAnUnreadableStore(t *testing.T) {
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))
	endpoint := s.Endpoint()

	// Take the registry away entirely: the server holds no reference to it.
	if err := os.Remove(RegistryPath(root)); err != nil {
		t.Fatal(err)
	}
	line := rawAsk(t, endpoint, `{"ask":"who"}`+"\n")
	var r Response
	if err := json.Unmarshal(line[:len(line)-1], &r); err != nil {
		t.Fatalf("no answer with the store gone: %v", err)
	}
	if r.Owner != "jobdtest@testhost:4242" {
		t.Fatalf("owner = %q", r.Owner)
	}
	// And the client now reports absent, because it cannot resolve the
	// endpoint — which is correct and is a different thing from the server
	// being down.
	if a := Query(root); a.State != Absent {
		t.Errorf("client state = %v with no registry", a.State)
	}
}
