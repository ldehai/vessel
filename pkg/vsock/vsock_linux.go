//go:build linux

// Package vsock provides AF_VSOCK dial/listen on Linux. vsock is the
// standard host<->guest channel for microVMs (Firecracker, Cloud
// Hypervisor, QEMU) — no network stack required inside the guest.
package vsock

import (
	"fmt"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// Well-known guest CID values.
const (
	CIDHypervisor = 0
	CIDLocal      = 1
	CIDHost       = 2
)

// Dial connects to the given context ID and port.
func Dial(cid, port uint32) (net.Conn, error) {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := &unix.SockaddrVM{CID: cid, Port: port}
	if err := unix.Connect(fd, sa); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("vsock connect cid=%d port=%d: %w", cid, port, err)
	}
	return fdConn(fd, fmt.Sprintf("vsock:%d:%d", cid, port))
}

// Listen binds to the given port on all CIDs (VMADDR_CID_ANY).
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
	f := os.NewFile(uintptr(fd), fmt.Sprintf("vsock-listen:%d", port))
	defer f.Close()
	return net.FileListener(f)
}

func fdConn(fd int, name string) (net.Conn, error) {
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	return net.FileConn(f)
}
