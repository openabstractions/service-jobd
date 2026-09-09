//go:build windows

package discovery

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

// Windows is the primary target, and it is also where every naive
// implementation of this protocol breaks, for two reasons that have nothing to
// do with each other:
//
//  1. A synchronous read on a named pipe cannot be interrupted. A client using
//     ordinary blocking I/O against a server that accepts and never writes
//     waits forever — not 200 ms, forever — and the whole point of the deadline
//     rule is to prevent exactly that. Every handle below is opened
//     FILE_FLAG_OVERLAPPED, which is what makes SetDeadline mean anything.
//  2. There is no CreateNamedPipe flag called "fail if it exists" that anyone
//     reaches for by default. A process that creates the pipe first, with a
//     permissive DACL, will happily accept a second instance from the real
//     supervisor, and connections then land on whichever instance the kernel
//     picks. go-winio creates its first instance with NT disposition FILE_CREATE
//     — the equivalent of FILE_FLAG_FIRST_PIPE_INSTANCE — so binding a name
//     somebody else already owns fails rather than joining them.

// Address is the endpoint a client dials and a server binds.
func Address(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("discovery: empty endpoint name")
	}
	return `\\.\pipe\` + name, nil
}

// listen binds the pipe.
//
// The DACL grants exactly one SID — this process's user — and is marked
// protected, so nothing is inherited from the object's parent. That is what
// the contract asks for, and it does more than the contract says: on Windows
// the DACL is also the only thing preventing another process that has learned
// the name from adding an instance to a pipe that already exists and answering
// half the callers. Administrators and SYSTEM are excluded along with everyone
// else, which is correct for version one, where the supervisor runs as the same
// user as its callers.
func listen(addr string) (net.Listener, error) {
	sd, err := ownerOnlySDDL()
	if err != nil {
		return nil, err
	}
	return winio.ListenPipe(addr, &winio.PipeConfig{
		SecurityDescriptor: sd,
		// Byte mode. Message mode would frame for us and then diverge from
		// every other platform, where the newline is the frame.
		MessageMode:      false,
		InputBufferSize:  4096,
		OutputBufferSize: 4096,
	})
}

// ownerOnlySDDL is "protected DACL, generic-all to me, nothing to anybody".
func ownerOnlySDDL() (string, error) {
	tok := windows.GetCurrentProcessToken()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", fmt.Errorf("discovery: cannot read this process's user: %w", err)
	}
	return "D:P(A;;GA;;;" + u.User.Sid.String() + ")", nil
}

// dial opens the pipe, spending no more than the caller's remaining budget.
//
// SECURITY_SQOS_PRESENT with SECURITY_IDENTIFICATION: the server may find out
// who connected, and may not act as them. go-winio's own DialPipe uses
// SECURITY_ANONYMOUS, which is stricter still but leaves a server unable to
// identify a caller at all, so the impersonation level is passed explicitly
// here rather than taken from the default.
//
// ERROR_PIPE_BUSY is not absent. The name exists and every instance is in use,
// which is the ordinary state of a busy supervisor between accepts; go-winio
// retries every 10 ms until the context expires. The contract says "retries
// once", which is not enough — see the notes returned with this work.
func dial(ctx context.Context, addr string) (net.Conn, error) {
	return winio.DialPipeAccessImpLevel(ctx, addr,
		uint32(windows.GENERIC_READ|windows.GENERIC_WRITE),
		winio.PipeImpLevelIdentification)
}

// unlinkStale does nothing on Windows, and cannot do anything.
//
// A named pipe is a kernel object, not a file. It disappears when its last
// handle closes, so a killed supervisor leaves nothing behind to remove — the
// registry entry survives, the pipe does not, and a client dialling it gets
// ERROR_FILE_NOT_FOUND, which is absent. The contract's rule about unlinking a
// stale socket is a Unix rule that reads as if it were universal.
func unlinkStale(string) {}

// prepareDir does nothing on Windows: the pipe namespace needs no directory
// and has no mode.
func prepareDir(string) error { return nil }

// dialProbe is the connect a would-be server makes before binding, to find out
// whether the name is really held.
func dialProbe(addr string, budget time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return dial(ctx, addr)
}
