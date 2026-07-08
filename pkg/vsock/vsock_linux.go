//go:build linux

// Package vsock provides AF_VSOCK dial/listen on Linux. vsock is the
// standard host<->guest channel for microVMs — no network stack required
// inside the guest.
//
// Go's net package does not understand AF_VSOCK sockaddrs, so
// net.FileListener/net.FileConn fail with "address family not supported".
// We therefore implement net.Listener/net.Conn directly on raw fds.
package vsock

import (
	"fmt"
	"net"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// Well-known CID values.
const (
	CIDHypervisor = 0
	CIDLocal      = 1
	CIDHost       = 2
)

// Addr is a vsock endpoint address.
type Addr struct {
	CID  uint32
	Port uint32
}

func (a Addr) Network() string { return "vsock" }
func (a Addr) String() string  { return fmt.Sprintf("vsock(%d:%d)", a.CID, a.Port) }

// Conn is a stream vsock connection implementing net.Conn.
type Conn struct {
	f             *os.File
	local, remote Addr
}

func newConn(fd int, local, remote Addr) (*Conn, error) {
	// Non-blocking + os.NewFile registers the fd with Go's runtime poller,
	// which makes deadlines work.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return &Conn{f: os.NewFile(uintptr(fd), remote.String()), local: local, remote: remote}, nil
}

func (c *Conn) Read(b []byte) (int, error)         { return c.f.Read(b) }
func (c *Conn) Write(b []byte) (int, error)        { return c.f.Write(b) }
func (c *Conn) Close() error                       { return c.f.Close() }
func (c *Conn) LocalAddr() net.Addr                { return c.local }
func (c *Conn) RemoteAddr() net.Addr               { return c.remote }
func (c *Conn) SetDeadline(t time.Time) error      { return c.f.SetDeadline(t) }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.f.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.f.SetWriteDeadline(t) }

// Dial connects to the given context ID and port.
func Dial(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	if err := unix.Connect(fd, &unix.SockaddrVM{CID: cid, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w", cid, port, err)
	}
	local := localAddr(fd)
	return newConn(fd, local, Addr{CID: cid, Port: port})
}

// Listener is a vsock listener implementing net.Listener.
type Listener struct {
	fd   int
	addr Addr
}

// Listen binds to the given port on all CIDs (VMADDR_CID_ANY).
// The listening socket stays blocking; Accept blocks an OS thread, which
// is fine for the guest agent's single listener.
func Listen(port uint32) (net.Listener, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}
	if err := unix.Bind(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock bind port=%d: %w", port, err)
	}
	if err := unix.Listen(fd, 16); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &Listener{fd: fd, addr: localAddr(fd)}, nil
}

func (l *Listener) Accept() (net.Conn, error) {
	for {
		fd, sa, err := unix.Accept4(l.fd, unix.SOCK_CLOEXEC)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, &net.OpError{Op: "accept", Net: "vsock", Err: err}
		}
		remote := Addr{}
		if vm, ok := sa.(*unix.SockaddrVM); ok {
			remote = Addr{CID: vm.CID, Port: vm.Port}
		}
		return newConn(fd, l.addr, remote)
	}
}

func (l *Listener) Close() error {
	// Shutdown unblocks a pending Accept before the fd is closed.
	_ = unix.Shutdown(l.fd, unix.SHUT_RDWR)
	return unix.Close(l.fd)
}

func (l *Listener) Addr() net.Addr { return l.addr }

func localAddr(fd int) Addr {
	if sa, err := unix.Getsockname(fd); err == nil {
		if vm, ok := sa.(*unix.SockaddrVM); ok {
			return Addr{CID: vm.CID, Port: vm.Port}
		}
	}
	return Addr{}
}
