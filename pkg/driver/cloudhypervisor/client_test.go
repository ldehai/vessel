package cloudhypervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andyliu/vessel/pkg/agent"
)

// mockCH serves a fake Cloud Hypervisor REST API on a unix socket and
// records the endpoints hit.
func mockCH(t *testing.T) (string, *[]string) {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "api.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		ep := strings.TrimPrefix(r.URL.Path, "/api/v1/")
		calls = append(calls, ep)
		switch ep {
		case "vm.create":
			var cfg VMConfig
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			if cfg.CPUs.BootVCPUs == 0 || cfg.Payload.Kernel == "" {
				http.Error(w, "invalid config", 400)
				return
			}
			w.WriteHeader(204)
		case "vm.info":
			_ = json.NewEncoder(w).Encode(map[string]any{"state": "Running"})
		default:
			w.WriteHeader(204)
		}
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &calls
}

func TestAPIClientVMLifecycle(t *testing.T) {
	sock, calls := mockCH(t)
	c := NewAPIClient(sock)
	ctx := context.Background()

	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	cfg := &VMConfig{
		CPUs:    CPUsConfig{BootVCPUs: 2, MaxVCPUs: 2},
		Memory:  MemoryConfig{Size: 256 << 20},
		Payload: PayloadConfig{Kernel: "/k/vmlinux"},
	}
	if err := c.CreateVM(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if err := c.BootVM(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.PauseVM(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.SnapshotVM(ctx, "file:///tmp/snap"); err != nil {
		t.Fatal(err)
	}
	if err := c.ResumeVM(ctx); err != nil {
		t.Fatal(err)
	}
	info, err := c.VMInfo(ctx)
	if err != nil || info["state"] != "Running" {
		t.Fatalf("info=%v err=%v", info, err)
	}

	want := []string{"vmm.ping", "vm.create", "vm.boot", "vm.pause", "vm.snapshot", "vm.resume", "vm.info"}
	if len(*calls) != len(want) {
		t.Fatalf("calls = %v, want %v", *calls, want)
	}
	for i := range want {
		if (*calls)[i] != want[i] {
			t.Fatalf("call[%d] = %s, want %s", i, (*calls)[i], want[i])
		}
	}
}

func TestAPIClientRejectsBadConfig(t *testing.T) {
	sock, _ := mockCH(t)
	c := NewAPIClient(sock)
	err := c.CreateVM(context.Background(), &VMConfig{}) // missing kernel/cpus
	if err == nil {
		t.Fatal("want error for invalid config")
	}
}

// mockHybridVsock emulates the CH host-side vsock unix socket: handshake
// "CONNECT <port>" -> "OK <port>", then bridges to an in-process agent.
func mockHybridVsock(t *testing.T, port uint32) string {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "vsock.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				line, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || strings.TrimSpace(line) != fmt.Sprintf("CONNECT %d", port) {
					fmt.Fprintf(conn, "KO\n")
					conn.Close()
					return
				}
				fmt.Fprintf(conn, "OK %d\n", port)
				agent.NewServer().ServeConn(context.Background(), conn)
			}(conn)
		}
	}()
	return sock
}

func TestHybridVsockHandshakeAndExec(t *testing.T) {
	sock := mockHybridVsock(t, agentVsockPort)
	conn, err := DialHybridVsock(sock, agentVsockPort, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c := agent.NewClient(conn)
	defer c.Close()

	if err := c.Ping(); err != nil {
		t.Fatal(err)
	}
	code, stdout, _, err := c.Exec([]string{"echo", "-n", "via-vsock"}, nil)
	if err != nil || code != 0 || string(stdout) != "via-vsock" {
		t.Fatalf("code=%d stdout=%q err=%v", code, stdout, err)
	}
}

func TestHybridVsockRejectedPort(t *testing.T) {
	sock := mockHybridVsock(t, agentVsockPort)
	if _, err := DialHybridVsock(sock, 9999, time.Second); err == nil {
		t.Fatal("want handshake rejection for wrong port")
	}
}
