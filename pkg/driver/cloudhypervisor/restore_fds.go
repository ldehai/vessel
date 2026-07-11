//go:build linux

package cloudhypervisor

// RestoreVMWithFDs performs vm.restore while passing fresh net-device fds
// to cloud-hypervisor over SCM_RIGHTS — the mechanism CH's own ch-remote
// uses (send_with_fds). This is what lets a generic, host-netns pool VMM
// take over a pod's TAP (opened in the pod netns) at restore time, so
// networked pods get the same sub-100ms restore as any other.
//
// CH's micro_http reads the request line (with the ancillary fds) from the
// first datagram, then the headers/body as an ordinary stream. We
// reproduce that framing byte-for-byte rather than going through net/http,
// which cannot attach ancillary data.

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

// RestoreVMWithFDs restores from cfg, handing fds to CH via SCM_RIGHTS.
// With no fds it is equivalent to RestoreVM (but still uses the manual
// transport, so one code path is exercised everywhere).
func (c *APIClient) RestoreVMWithFDs(cfg *RestoreConfig, fds []int) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.apiCallWithFDs("vm.restore", body, fds)
}

// AddNetWithFDs hotplugs a virtio-net device into a running VM, backing it
// with tap fds passed via SCM_RIGHTS. This is how a networked pod restored
// from a net-less template gets its NIC: restore a generic template, then
// add-net with a TAP fd opened in the pod netns. cfg.NumFDs must match
// len(fds); the body carries the device shape, the fds ride the socket.
func (c *APIClient) AddNetWithFDs(cfg *NetDevice, fds []int) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	return c.apiCallWithFDs("vm.add-net", body, fds)
}

// apiCallWithFDs issues one PUT to /api/v1/<endpoint> with body, attaching
// fds as an SCM_RIGHTS control message on the request line — byte-for-byte
// what cloud-hypervisor's ch-remote send_with_fds does. net/http cannot
// attach ancillary data, so the request is hand-framed.
func (c *APIClient) apiCallWithFDs(endpoint string, body []byte, fds []int) error {
	conn, err := net.DialTimeout("unix", c.socketPath, 5*time.Second)
	if err != nil {
		return fmt.Errorf("dial CH api socket: %w", err)
	}
	defer conn.Close()
	uc := conn.(*net.UnixConn)

	// 1. Request line + minimal headers carry the fds (fds ride the first write).
	reqLine := fmt.Sprintf("PUT /api/v1/%s HTTP/1.1\r\nHost: localhost\r\nAccept: */*\r\n", endpoint)
	if err := writeWithFDs(uc, []byte(reqLine), fds); err != nil {
		return fmt.Errorf("send %s request line + fds: %w", endpoint, err)
	}

	// 2. Content-Length, blank line, then the JSON body as a plain stream.
	rest := fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	if _, err := uc.Write([]byte(rest)); err != nil {
		return fmt.Errorf("send %s body: %w", endpoint, err)
	}

	return readHTTPStatus(uc)
}

// writeWithFDs sends data on uc with fds attached as an SCM_RIGHTS control
// message (a single sendmsg, like ch-remote).
func writeWithFDs(uc *net.UnixConn, data []byte, fds []int) error {
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	oob := unix.UnixRights(fds...)
	var writeErr error
	if err := raw.Write(func(fd uintptr) bool {
		if len(fds) == 0 {
			_, writeErr = unix.Write(int(fd), data)
		} else {
			writeErr = unix.Sendmsg(int(fd), data, oob, nil, 0)
		}
		return true // done; don't wait for writability
	}); err != nil {
		return err
	}
	return writeErr
}

// readHTTPStatus reads just enough of the HTTP response to learn whether
// the restore succeeded. CH answers 204 No Content on success.
func readHTTPStatus(uc *net.UnixConn) error {
	_ = uc.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 4096)
	n, err := uc.Read(buf)
	if err != nil && n == 0 {
		return fmt.Errorf("read restore response: %w", err)
	}
	statusLine, _, _ := strings.Cut(string(buf[:n]), "\r\n")
	// "HTTP/1.1 204 No Content"
	fields := strings.SplitN(statusLine, " ", 3)
	if len(fields) < 2 {
		return fmt.Errorf("malformed restore response: %q", statusLine)
	}
	switch fields[1] {
	case "200", "201", "204":
		return nil
	default:
		return fmt.Errorf("restore failed: %s", statusLine)
	}
}
