package cloudhypervisor

import (
	"bufio"
	"fmt"
	"net"
	"strings"
	"time"
)

// DialHybridVsock connects to a guest vsock port through the host-side
// unix socket exposed by Cloud Hypervisor / Firecracker ("hybrid vsock"):
// the host connects to the unix socket, sends "CONNECT <port>\n", and the
// VMM replies "OK <assigned_port>\n" once the guest accepts.
func DialHybridVsock(socketPath string, port uint32, timeout time.Duration) (net.Conn, error) {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("hybrid vsock dial: %w", err)
	}
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		conn.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock handshake send: %w", err)
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock handshake recv: %w", err)
	}
	if !strings.HasPrefix(line, "OK ") {
		conn.Close()
		return nil, fmt.Errorf("hybrid vsock handshake rejected: %q", strings.TrimSpace(line))
	}
	// Clear deadline for the actual session.
	if err := conn.SetDeadline(time.Time{}); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}
