package discovery

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"time"
)

// Identity is everything a supervisor says about itself, captured once.
//
// Captured once because of the rule that matters most here: ANSWERING TOUCHES
// NO DISK. Not the store, not the lease, not the heartbeat. The answer is what
// the process already knows about itself, so a supervisor whose store has
// become unreadable — an unmounted share, a full disk, a permission change —
// can still tell a caller it is alive. A server that re-read the store to
// answer would go silent at exactly the moment somebody needed to know it was
// there.
type Identity struct {
	Content     []string
	Critical    []string
	Owner       string
	Host        string
	Store       string
	DelegatesTo string
	PID         int
	StartedAt   time.Time
}

// Server answers on one endpoint.
type Server struct {
	ln       net.Listener
	addr     string
	endpoint string

	// who is the entire response, encoded at construction. Answering is a
	// write of these bytes and nothing else.
	who []byte

	look chan struct{}

	closeOnce sync.Once
	closed    chan struct{}
}

// Look fires when somebody asked the supervisor to sweep now.
//
// Coalescing, one slot: ten submissions in a second are one reason to sweep,
// not ten sweeps. A supervisor that swept per message is a supervisor anybody
// on the machine can wedge with a loop.
func (s *Server) Look() <-chan struct{} { return s.look }

// Endpoint is the random name this server bound.
func (s *Server) Endpoint() string { return s.endpoint }

// Addr is the platform path or pipe name.
func (s *Server) Addr() string { return s.addr }

// Serve binds the endpoint and starts answering.
//
// The stale-socket dance lives here and nowhere else. Only a server unlinks,
// only before its own bind, and only after its own connect to that path has
// failed.
func Serve(endpoint string, id Identity) (*Server, error) {
	addr, err := Address(endpoint)
	if err != nil {
		return nil, err
	}
	if err := prepareDir(addr); err != nil {
		return nil, err
	}
	unlinkStale(addr)

	ln, err := listen(addr)
	if err != nil {
		return nil, fmt.Errorf("discovery: cannot listen on %s: %w", addr, err)
	}

	if id.Content == nil {
		id.Content = []string{Base}
	}
	if id.Critical == nil {
		id.Critical = []string{Base}
	}
	s := &Server{
		ln:       ln,
		addr:     addr,
		endpoint: endpoint,
		look:     make(chan struct{}, 1),
		closed:   make(chan struct{}),
		who: mustLine(Response{
			Content:     id.Content,
			Critical:    id.Critical,
			Owner:       id.Owner,
			Host:        id.Host,
			Store:       id.Store,
			DelegatesTo: id.DelegatesTo,
			PID:         id.PID,
			StartedAt:   FormatTime(id.StartedAt),
		}),
	}
	go s.accept()
	return s, nil
}

// accept never does work on the accepting goroutine.
//
// One connection must not be able to block accept. A single local process that
// connects and never sends would otherwise hold the loop for its whole
// per-connection deadline, and while it did, every other caller on the machine
// would sit in its own 200 ms budget and then report absent — every
// application, at once, because of one connection.
func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.closed:
				return
			default:
			}
			// A transient accept error must not end the listener. Anything
			// permanent will repeat, and Close() is what actually stops this
			// loop.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			if errors.Is(err, net.ErrClosed) || errors.Is(err, os.ErrClosed) {
				return
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		go s.answer(c)
	}
}

// answer handles one connection, start to finish, inside one deadline.
func (s *Server) answer(c net.Conn) {
	defer c.Close()
	// One deadline over the whole exchange, set once. A per-operation deadline
	// can be refreshed by a peer that keeps dribbling bytes, which is the same
	// hang wearing a different hat.
	_ = c.SetDeadline(time.Now().Add(ConnDeadline))

	// The cap applies on this side too. The contract only caps the client, but
	// a server reading toward a newline with no limit is a supervisor any local
	// process can exhaust by writing zeros.
	br := bufio.NewReaderSize(io.LimitReader(c, MaxLine+1), 4096)
	line, err := readLine(br)
	switch {
	case err == io.EOF && len(line) == 0:
		// Connected and said nothing, then hung up. There is nobody left to
		// answer and nothing to answer about.
		return
	case err != nil:
		// Timed out, over the cap, or a partial line. The connection is still
		// open from our side, so it gets the error object rather than a close:
		// silence and a hang are indistinguishable to the other end, and the
		// contract forbids being indistinguishable from a hang.
		s.write(c, errInvalidRequest)
		return
	}

	var req Request
	if err := json.Unmarshal(line, &req); err != nil {
		s.write(c, errInvalidRequest)
		return
	}
	switch req.Ask {
	case AskWho:
		s.write(c, s.who)
	case AskLook:
		// Answer first, sweep second. The response is the same
		// self-description, so "look" doubles as a probe; a caller that wants
		// a sweep and a liveness answer needs one round trip, not two.
		s.write(c, s.who)
		select {
		case s.look <- struct{}{}:
		default:
		}
	default:
		s.write(c, errUnknownAsk)
	}
}

// write sends one line and then waits for the peer to hang up.
//
// The wait is not politeness. On Windows, closing the server end of a named
// pipe DISCARDS anything the client has not read yet, and the documented fix —
// FlushFileBuffers before disconnecting — needs the raw HANDLE, which no Go
// named-pipe library exposes. Reading until the peer closes achieves the same
// thing through the same deadline, on every platform, without reaching for a
// handle: the client has provably taken the bytes before this end goes away.
func (s *Server) write(c net.Conn, line []byte) {
	if _, err := c.Write(line); err != nil {
		return
	}
	var scratch [1]byte
	for {
		n, err := c.Read(scratch[:])
		if err != nil || n == 0 {
			return
		}
	}
}

// readLine reads up to and including one 0x0A, refusing anything over the cap
// and refusing a carriage return.
func readLine(br *bufio.Reader) ([]byte, error) {
	var out []byte
	for {
		b, err := br.ReadByte()
		if err != nil {
			if err == io.EOF && len(out) > 0 {
				// EOF before the newline is a partial line, which is not a
				// request. Distinguished from a clean EOF so the caller can
				// tell "said nothing" from "said half of something".
				return out, io.ErrUnexpectedEOF
			}
			return out, err
		}
		if b == '\n' {
			return out, nil
		}
		if b == '\r' {
			return out, fmt.Errorf("discovery: carriage return in a request")
		}
		out = append(out, b)
		if len(out) > MaxLine {
			return out, fmt.Errorf("discovery: request over %d bytes", MaxLine)
		}
	}
}

// Close stops answering and takes the endpoint down.
func (s *Server) Close() error {
	var err error
	s.closeOnce.Do(func() {
		close(s.closed)
		err = s.ln.Close()
		// Go's unix listener unlinks its own socket on Close. Windows has
		// nothing to unlink: the pipe object goes when the last handle does.
	})
	return err
}

// Answers reports whether anything is listening on an endpoint name, right now.
//
// This is the probe a would-be server makes before claiming a name that the
// registry says is already held, and the only honest test of whether the holder
// is still there. It connects and hangs up: it deliberately does not ask
// anything, because a supervisor mid-answer to somebody else is still a
// supervisor.
func Answers(endpoint string) bool {
	addr, err := Address(endpoint)
	if err != nil {
		return false
	}
	c, err := dialProbe(addr, Budget)
	if err != nil {
		return false
	}
	c.Close()
	return true
}
