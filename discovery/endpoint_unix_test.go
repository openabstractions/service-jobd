//go:build !windows

package discovery

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The Unix half of the contract: a socket is a FILE, and every rule about
// stale endpoints, permissions and path length exists because of that. None of
// it has a Windows analogue — a pipe is a kernel object that vanishes with its
// last handle — so these are the cases the primary platform cannot exercise at
// all.

// Socket mode 0600, owned by the supervisor's uid.
//
// Checked on the bound socket rather than on the chmod call, because a bind
// creates the file with 0777 &^ umask and the window before a chmod is a window
// in which anyone can connect.
func TestSocketIsOwnerOnly(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	st, err := os.Stat(s.Addr())
	if err != nil {
		t.Fatalf("stat %s: %v", s.Addr(), err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("socket mode is %04o, want 0600", perm)
	}
}

// A socket file a killed process left behind: the next supervisor unlinks it,
// and only after its own connect to it has failed.
func TestStaleSocketFileIsInheritedNotRefused(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()

	ep, err := Claim(root, "", ServiceJobs)
	if err != nil {
		t.Fatal(err)
	}
	addr, _ := Address(ep)
	ln, err := listen(addr)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a kill: the socket file survives the listener, because nothing
	// ran to tidy it up.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	ln.Close()
	if _, err := os.Lstat(addr); err != nil {
		t.Fatalf("the test needs a stale socket file at %s: %v", addr, err)
	}

	// A client sees absent, and touches nothing.
	if a := Query(root); a.State != Absent {
		t.Fatalf("stale socket answered %v", a.State)
	}
	if _, err := os.Lstat(addr); err != nil {
		t.Fatal("a client removed the socket file; only a server may do that")
	}

	// A server inherits the name, removes the corpse and binds.
	again, err := Claim(root, "", ServiceJobs)
	if err != nil {
		t.Fatalf("takeover refused: %v", err)
	}
	if again != ep {
		t.Fatalf("takeover minted %q rather than inheriting %q, abandoning the socket file", again, ep)
	}
	s, err := Serve(again, testIdentity(root))
	if err != nil {
		t.Fatalf("serve over a stale socket: %v", err)
	}
	defer s.Close()
	if a := Query(root); a.State != Present {
		t.Fatalf("after takeover: %v (%s)", a.State, a.Why)
	}
}

// unlinkStale must connect first and remove only when that fails. Removing
// unconditionally can delete a socket another supervisor bound a millisecond
// earlier, which leaves it listening on an unlinked inode: alive, healthy and
// findable by nobody, for as long as it runs.
func TestUnlinkStaleLeavesALiveSocketAlone(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	root := t.TempDir()
	s := serveFor(t, root, testIdentity(root))

	unlinkStale(s.Addr())

	if _, err := os.Lstat(s.Addr()); err != nil {
		t.Fatal("unlinkStale removed a socket that was answering")
	}
	if a := Query(root); a.State != Present {
		t.Fatalf("after unlinkStale: %v", a.State)
	}
}

// XDG_RUNTIME_DIR when there is one; /tmp/abstraction-<uid> otherwise.
func TestAddressLocation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	got, err := Address("abstraction-0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(got) != dir {
		t.Errorf("with XDG_RUNTIME_DIR set, address is %q", got)
	}

	t.Setenv("XDG_RUNTIME_DIR", "")
	got, err = Address("abstraction-0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf("/tmp/abstraction-%d", os.Getuid())
	if filepath.Dir(got) != want {
		t.Errorf("fallback address is %q, want it under %s", got, want)
	}
}

// Over the sockaddr_un limit is a loud failure at startup, never a silent
// decline to listen.
func TestOverlongPathFailsLoudly(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "/"+strings.Repeat("d", 90))
	_, err := Address("abstraction-0123456789abcdef0123456789abcdef")
	if err == nil {
		t.Fatal("an unbindable path was accepted")
	}
	if !strings.Contains(err.Error(), "sockaddr_un") {
		t.Errorf("error does not say why: %v", err)
	}
}

// The fallback directory is refused if it is not 0700 and ours.
func TestFallbackDirectoryMustBeOurs(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	dir := fmt.Sprintf("/tmp/abstraction-%d", os.Getuid())
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Skipf("cannot use %s: %v", dir, err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Skip(err)
	}
	defer os.Chmod(dir, 0o700)

	err := prepareDir(filepath.Join(dir, "x.sock"))
	if err == nil {
		t.Fatal("listened in a world-readable directory")
	}
	if !strings.Contains(err.Error(), "0700") {
		t.Errorf("error does not say why: %v", err)
	}
}
