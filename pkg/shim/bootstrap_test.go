package shim

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/ttrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

// TestMain doubles as the shim daemon: RunStart re-execs the current
// binary (the test binary) with VESSEL_SHIM_DAEMON=1 set, which lands here
// and serves the Task API exactly as a containerd-spawned daemon would.
func TestMain(m *testing.M) {
	if os.Getenv("VESSEL_SHIM_DAEMON") == "1" {
		if err := RunDaemon(context.Background(), "test-ns"); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// TestHandshake walks the full containerd contract: start spawns a daemon
// and reports its address; the address serves the Task API over ttrpc;
// Shutdown terminates the idle daemon; delete cleans up the leftovers.
func TestHandshake(t *testing.T) {
	bundle := t.TempDir()
	sockRoot := t.TempDir()
	t.Setenv("VESSEL_SHIM_SOCKET_ROOT", sockRoot)
	t.Setenv("VESSEL_SHIM_DAEMON", "1")
	t.Setenv("VESSEL_SHIM_CONFIG", filepath.Join(bundle, "no-such-config.json")) // missing = defaults

	oldWD, _ := os.Getwd()
	if err := os.Chdir(bundle); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	// --- start ---
	addr, err := RunStart("test-ns", "task1", "/fake/containerd.sock")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(addr, "unix://"+sockRoot) {
		t.Fatalf("address %q not under socket root", addr)
	}
	if got, _ := os.ReadFile(filepath.Join(bundle, "address")); string(got) != addr {
		t.Fatalf("bundle/address = %q, want %q", got, addr)
	}
	pidData, err := os.ReadFile(filepath.Join(bundle, "shim.pid"))
	if err != nil {
		t.Fatal(err)
	}
	daemonPid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	if daemonPid <= 1 {
		t.Fatalf("bad daemon pid %d", daemonPid)
	}
	defer func() { _ = syscall.Kill(daemonPid, syscall.SIGKILL) }()

	// --- containerd connects to the reported address ---
	conn := dialRetry(t, socketPath(addr), 3*time.Second)
	client := ttrpc.NewClient(conn)
	defer client.Close()
	tc := taskapi.NewTaskClient(client)
	ctx := context.Background()

	_, err = tc.State(ctx, &taskapi.StateRequest{ID: "nope"})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("State over handshake socket: %v, want NotFound", err)
	}

	// Task lifecycle through the containerd-spawned daemon.
	writeBundleConfig(t, bundle, nil)
	if _, err := tc.Create(ctx, &taskapi.CreateTaskRequest{ID: "task1", Bundle: bundle}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tc.Start(ctx, &taskapi.StartRequest{ID: "task1"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := tc.Delete(ctx, &taskapi.DeleteRequest{ID: "task1"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// --- Shutdown terminates the idle daemon ---
	if _, err := tc.Shutdown(ctx, &taskapi.ShutdownRequest{ID: "task1"}); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	waitProcessGone(t, daemonPid, 5*time.Second)

	// --- delete cleans up ---
	resp, err := RunDelete("test-ns", "task1", "/fake/containerd.sock")
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalDeleteResponse(resp)
	if err != nil {
		t.Fatal(err)
	}
	var decoded taskapi.DeleteResponse
	if err := proto.Unmarshal(data, &decoded); err != nil || decoded.ExitedAt == nil {
		t.Fatalf("delete response round-trip: %+v err=%v", &decoded, err)
	}
	if _, err := os.Stat(socketPath(addr)); !os.IsNotExist(err) {
		t.Fatal("delete must remove the shim socket")
	}
	if _, err := os.Stat(filepath.Join(bundle, "address")); !os.IsNotExist(err) {
		t.Fatal("delete must remove bundle/address")
	}
}

// A second start for the same task must succeed even if a stale socket
// file was left behind (crashed shim).
func TestStartReplacesStaleSocket(t *testing.T) {
	bundle := t.TempDir()
	sockRoot := t.TempDir()
	t.Setenv("VESSEL_SHIM_SOCKET_ROOT", sockRoot)
	t.Setenv("VESSEL_SHIM_DAEMON", "1")
	t.Setenv("VESSEL_SHIM_CONFIG", filepath.Join(bundle, "none.json"))

	oldWD, _ := os.Getwd()
	if err := os.Chdir(bundle); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(oldWD) }()

	// Plant a stale socket at the derived path.
	stale := socketPath(SocketAddress("/fake/containerd.sock", "test-ns", "task2"))
	_ = os.MkdirAll(filepath.Dir(stale), 0o711)
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.Close() // dead listener, socket file remains

	addr, err := RunStart("test-ns", "task2", "/fake/containerd.sock")
	if err != nil {
		t.Fatalf("start with stale socket: %v", err)
	}
	pidData, _ := os.ReadFile(filepath.Join(bundle, "shim.pid"))
	pid, _ := strconv.Atoi(strings.TrimSpace(string(pidData)))
	defer func() { _ = syscall.Kill(pid, syscall.SIGKILL) }()

	conn := dialRetry(t, socketPath(addr), 3*time.Second)
	conn.Close()
}

func dialRetry(t *testing.T, path string, timeout time.Duration) net.Conn {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		conn, err := net.DialTimeout("unix", path, time.Second)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never came up on %s: %v", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitProcessGone waits for pid to exit. In production the daemon
// reparents to init (its spawner, the `start` command, exits immediately)
// and is reaped there. In this test the spawner is the test process itself,
// which never reaps — an exited daemon lingers as a zombie and
// kill(pid, 0) keeps succeeding. A zombie (state Z in /proc/pid/stat) has
// exited: treat it as gone.
func waitProcessGone(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err == syscall.ESRCH {
			return
		}
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
			// The state field follows "pid (comm)"; comm may contain spaces
			// or parens, so locate it from the last ')'.
			s := string(data)
			if i := strings.LastIndexByte(s, ')'); i >= 0 && i+2 < len(s) && s[i+2] == 'Z' {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("daemon pid %d still alive after Shutdown", pid)
}
