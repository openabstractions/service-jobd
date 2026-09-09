//go:build !windows

package discovery

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// macOS and Linux use a filesystem socket, and NOT a Linux abstract socket.
//
// Abstract sockets carry no permissions at all — any uid in the namespace may
// connect, which contradicts the permission rule outright — and they are
// per-network-namespace, so a container or a Flatpak adopter cannot reach a
// supervisor on the host. A path costs an unlink on an unclean exit and buys
// both of those back.

// sunPathMax is the size of sockaddr_un.sun_path.
//
// 104 on macOS and the BSDs, 108 on Linux. The smaller number is used
// everywhere, because a store that works on one and fails on the other is worse
// than one that fails on both, and 104 is the figure the contract cites.
const sunPathMax = 104

// Address is the endpoint a client dials and a server binds.
//
// $XDG_RUNTIME_DIR when the session has one — it is already 0700, already
// per-user, and already emptied at logout. Otherwise /tmp/abstraction-<uid>/,
// which this process creates 0700 and refuses to use if it exists and is not
// ours or not 0700.
func Address(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("discovery: empty endpoint name")
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = filepath.Join("/tmp", fmt.Sprintf("abstraction-%d", os.Getuid()))
	}
	p := filepath.Join(dir, name+".sock")
	// Loudly, at startup, never a silent decline to listen: a supervisor that
	// quietly did not bind is a machine where every application downloads in
	// process and nobody knows why.
	if len(p) >= sunPathMax {
		return "", fmt.Errorf("discovery: endpoint path %q is %d bytes, over the %d-byte limit on sockaddr_un; set XDG_RUNTIME_DIR to something shorter", p, len(p), sunPathMax)
	}
	return p, nil
}

// prepareDir makes the fallback directory, or refuses.
//
// Only the server calls this. A client builds the path and connects; if the
// directory is wrong, the server never bound and the connect fails, which is
// absent.
func prepareDir(addr string) error {
	dir := filepath.Dir(addr)
	if os.Getenv("XDG_RUNTIME_DIR") != "" {
		// Somebody else's to manage. systemd made it, systemd removes it.
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	st, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && int(sys.Uid) != os.Getuid() {
		return fmt.Errorf("discovery: %s is owned by uid %d, not %d — refusing to listen there", dir, sys.Uid, os.Getuid())
	}
	if perm := st.Mode().Perm(); perm != 0o700 {
		return fmt.Errorf("discovery: %s is mode %04o, not 0700 — refusing to listen there", dir, perm)
	}
	return nil
}

// listen binds the socket at mode 0600.
//
// The umask is set around the bind rather than chmod'ing afterwards. A bind
// creates the socket with 0777 &^ umask, and the window between bind and chmod
// is a window in which anybody can connect — small, real, and avoidable. The
// umask is process-global, so this is done at startup and restored
// immediately; nothing else in a supervisor is creating files at the moment it
// binds its endpoint.
func listen(addr string) (net.Listener, error) {
	old := syscall.Umask(0o177)
	ln, err := net.Listen("unix", addr)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	// Belt as well as braces: a filesystem that ignores the umask (some
	// network mounts do) still gets the mode it was promised.
	if err := os.Chmod(addr, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func dial(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", addr)
}

// unlinkStale removes a socket file a killed supervisor left behind.
//
// The order is the whole rule: connect FIRST, and unlink only when the connect
// fails. A server that removed the path unconditionally could remove a socket
// another supervisor bound a millisecond earlier, leaving that one listening on
// an unlinked inode — running, healthy, and undiscoverable by anybody, which is
// strictly worse than the stale file it was cleaning up. Only a server ever
// calls this, and only immediately before its own bind.
func unlinkStale(addr string) {
	if _, err := os.Lstat(addr); err != nil {
		return
	}
	c, err := net.DialTimeout("unix", addr, 200*time.Millisecond)
	if err == nil {
		// Somebody is there. Leave it alone; bind will fail and the caller
		// will report the name as held, which is the truth.
		c.Close()
		return
	}
	os.Remove(addr)
}

func dialProbe(addr string, budget time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	return dial(ctx, addr)
}
