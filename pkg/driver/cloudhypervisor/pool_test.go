package cloudhypervisor

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestMain doubles as a fake VMM: when re-executed with VESSEL_FAKE_VMM=1
// (as the pool's "cloud-hypervisor binary"), it serves vmm.ping on the
// --api-socket path instead of running tests.
func TestMain(m *testing.M) {
	if os.Getenv("VESSEL_FAKE_VMM") == "1" {
		fakeVMM()
		return
	}
	os.Exit(m.Run())
}

func fakeVMM() {
	var sock string
	for i, a := range os.Args {
		if a == "--api-socket" && i+1 < len(os.Args) {
			sock = os.Args[i+1]
		}
	}
	l, err := net.Listen("unix", sock)
	if err != nil {
		os.Exit(1)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/vmm.ping", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	_ = http.Serve(l, mux)
}

func newTestPool(t *testing.T, target int) *vmmPool {
	t.Helper()
	t.Setenv("VESSEL_FAKE_VMM", "1")
	p := newVMMPool(os.Args[0], t.TempDir(), target, 5*time.Second)
	t.Cleanup(p.close)
	return p
}

func waitIdle(t *testing.T, p *vmmPool, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if p.idleCount() >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pool idle = %d, want >= %d", p.idleCount(), want)
}

func TestPoolGetSpawnsAndRefills(t *testing.T) {
	p := newTestPool(t, 2)

	// Cold get: spawns synchronously and kicks background refill.
	h, err := p.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.kill()
	if err := h.api.Ping(context.Background()); err != nil {
		t.Fatalf("handle not ready: %v", err)
	}

	// Background refill tops up to target.
	waitIdle(t, p, 2)

	// Warm get: served from the pool instantly.
	start := time.Now()
	h2, err := p.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h2.kill()
	if d := time.Since(start); d > 50*time.Millisecond {
		t.Fatalf("warm get took %v, want <50ms", d)
	}
	if h2.id == h.id {
		t.Fatal("handles must be distinct")
	}
}

func TestPoolDisabled(t *testing.T) {
	p := newTestPool(t, 0)
	h, err := p.get(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer h.kill()
	time.Sleep(50 * time.Millisecond)
	if n := p.idleCount(); n != 0 {
		t.Fatalf("disabled pool should stay empty, got %d", n)
	}
}

func TestPoolCloseKillsIdle(t *testing.T) {
	p := newTestPool(t, 1)
	h, _ := p.get(context.Background())
	defer h.kill()
	waitIdle(t, p, 1)
	p.close()
	if n := p.idleCount(); n != 0 {
		t.Fatalf("idle after close = %d", n)
	}
}
