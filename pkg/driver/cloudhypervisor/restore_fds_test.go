//go:build linux

package cloudhypervisor

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// fakeRestoreServer accepts one connection on a unix socket, reads the
// request with its ancillary fds (mirroring CH's micro_http), and reports
// what it received.
type restoreReceipt struct {
	body    string
	numFDs  int
	fdValid bool
}

func serveFakeRestore(t *testing.T, sock string) <-chan restoreReceipt {
	t.Helper()
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })

	out := make(chan restoreReceipt, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		uc := conn.(*net.UnixConn)

		// First read carries the request line + SCM_RIGHTS fds.
		buf := make([]byte, 4096)
		oob := make([]byte, 256)
		_ = uc.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
		if err != nil {
			out <- restoreReceipt{}
			return
		}
		var got restoreReceipt
		if oobn > 0 {
			scms, _ := unix.ParseSocketControlMessage(oob[:oobn])
			for _, scm := range scms {
				fds, _ := unix.ParseUnixRights(&scm)
				got.numFDs += len(fds)
				for _, fd := range fds {
					// The received fd is a real, independent handle to the
					// same file — fstat proves it's valid.
					var st unix.Stat_t
					if unix.Fstat(fd, &st) == nil {
						got.fdValid = true
					}
					unix.Close(fd)
				}
			}
		}

		// Drain the rest (Content-Length + body) to find the JSON.
		data := string(buf[:n])
		for !strings.Contains(data, "\r\n\r\n") {
			m, err := uc.Read(buf)
			if err != nil {
				break
			}
			data += string(buf[:m])
		}
		if i := strings.Index(data, "\r\n\r\n"); i >= 0 {
			got.body = data[i+4:]
		}
		// Reply 204 so the client's readHTTPStatus succeeds.
		_, _ = uc.Write([]byte("HTTP/1.1 204 No Content\r\n\r\n"))
		out <- got
	}()
	return out
}

func TestRestoreVMWithFDs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	receipts := serveFakeRestore(t, sock)
	c := NewAPIClient(sock)

	// A real fd to pass (any file works for the SCM_RIGHTS mechanics).
	f, err := os.CreateTemp(t.TempDir(), "fd")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	cfg := &RestoreConfig{
		SourceURL: "file:///snap",
		Resume:    true,
		NetFDs:    []RestoredNetConfig{{ID: "_net0", NumFDs: 1}},
	}
	if err := c.RestoreVMWithFDs(cfg, []int{int(f.Fd())}); err != nil {
		t.Fatal(err)
	}

	got := <-receipts
	if got.numFDs != 1 || !got.fdValid {
		t.Fatalf("server received numFDs=%d valid=%v, want 1/true", got.numFDs, got.fdValid)
	}
	var sent RestoreConfig
	if err := json.Unmarshal([]byte(got.body), &sent); err != nil {
		t.Fatalf("server got unparseable body %q: %v", got.body, err)
	}
	if sent.SourceURL != "file:///snap" || !sent.Resume ||
		len(sent.NetFDs) != 1 || sent.NetFDs[0].ID != "_net0" || sent.NetFDs[0].NumFDs != 1 {
		t.Fatalf("body round-trip wrong: %+v", sent)
	}
}

func TestRestoreVMWithFDsNoFDs(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "api.sock")
	receipts := serveFakeRestore(t, sock)
	c := NewAPIClient(sock)

	// No fds: still uses the manual transport, sends zero ancillary fds.
	if err := c.RestoreVMWithFDs(&RestoreConfig{SourceURL: "file:///snap"}, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-receipts; got.numFDs != 0 {
		t.Fatalf("numFDs = %d, want 0", got.numFDs)
	}
}
