package agent

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// pipeClient wires a Client to a Server over an in-memory duplex pipe.
func pipeClient(t *testing.T) *Client {
	t.Helper()
	hostSide, guestSide := net.Pipe()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go NewServer().ServeConn(ctx, guestSide)
	c := NewClient(hostSide)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestPing(t *testing.T) {
	if err := pipeClient(t).Ping(); err != nil {
		t.Fatal(err)
	}
}

func TestExec(t *testing.T) {
	c := pipeClient(t)
	code, stdout, _, err := c.Exec([]string{"sh", "-c", "echo -n hello-$FOO"}, []string{"FOO=bar"})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 || string(stdout) != "hello-bar" {
		t.Fatalf("code=%d stdout=%q", code, stdout)
	}
}

func TestExecNonZeroExit(t *testing.T) {
	c := pipeClient(t)
	code, _, _, err := c.Exec([]string{"sh", "-c", "exit 3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if code != 3 {
		t.Fatalf("code = %d, want 3", code)
	}
}

func TestFileRoundTrip(t *testing.T) {
	c := pipeClient(t)
	path := filepath.Join(t.TempDir(), "sub", "f.txt")
	want := []byte("snapshot me")

	if err := c.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Fatalf("got %q", got)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", info.Mode())
	}
}

func TestUnknownOp(t *testing.T) {
	c := pipeClient(t)
	_, err := c.roundTrip(&Request{Op: "bogus"})
	if err == nil {
		t.Fatal("want error for unknown op")
	}
}

func TestUnixSocketEndToEnd(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "agent.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() { _ = NewServer().Serve(l) }()

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	c := NewClient(conn)
	defer c.Close()
	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	code, stdout, _, err := c.Exec([]string{"echo", "vsock-ready"}, nil)
	if err != nil || code != 0 || string(stdout) != "vsock-ready\n" {
		t.Fatalf("code=%d stdout=%q err=%v", code, stdout, err)
	}
}
