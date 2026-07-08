package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/ldehai/vessel/pkg/sandbox"
)

// vmmHandle is a spawned cloud-hypervisor process whose API socket has
// already answered vmm.ping — ready to receive vm.create or vm.restore.
type vmmHandle struct {
	id  string
	dir string
	cmd *exec.Cmd
	api *APIClient
}

// vmmPool keeps prewarmed VMM processes so sandbox create/restore skips
// process spawn + API readiness (~tens of ms per request). The pool fills
// lazily: nothing is spawned until the first get (or an explicit Warm),
// so one-shot CLI use pays no cost.
type vmmPool struct {
	binary   string
	stateDir string
	target   int
	bootWait time.Duration

	mu        sync.Mutex
	idle      []*vmmHandle
	refilling bool
	closed    bool
}

func newVMMPool(binary, stateDir string, target int, bootWait time.Duration) *vmmPool {
	return &vmmPool{binary: binary, stateDir: stateDir, target: target, bootWait: bootWait}
}

// get returns a ready VMM: from the pool when warm, spawned synchronously
// otherwise. Either way a background refill is kicked.
func (p *vmmPool) get(ctx context.Context) (*vmmHandle, error) {
	p.mu.Lock()
	if n := len(p.idle); n > 0 {
		h := p.idle[n-1]
		p.idle = p.idle[:n-1]
		p.mu.Unlock()
		p.kickRefill()
		return h, nil
	}
	p.mu.Unlock()
	p.kickRefill()
	return p.spawn(ctx)
}

// kickRefill tops the pool up to target in the background.
func (p *vmmPool) kickRefill() {
	if p.target <= 0 {
		return
	}
	p.mu.Lock()
	if p.refilling || p.closed {
		p.mu.Unlock()
		return
	}
	p.refilling = true
	p.mu.Unlock()

	go func() {
		defer func() {
			p.mu.Lock()
			p.refilling = false
			p.mu.Unlock()
		}()
		for {
			p.mu.Lock()
			need := p.target - len(p.idle)
			closed := p.closed
			p.mu.Unlock()
			if need <= 0 || closed {
				return
			}
			h, err := p.spawn(context.Background())
			if err != nil {
				return // e.g. binary missing; next get() reports it synchronously
			}
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				h.kill()
				return
			}
			p.idle = append(p.idle, h)
			p.mu.Unlock()
		}
	}()
}

func (p *vmmPool) spawn(ctx context.Context) (*vmmHandle, error) {
	id := sandbox.NewID()
	dir := filepath.Join(p.stateDir, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	apiSock := filepath.Join(dir, "api.sock")
	cmd, err := startVMM(p.binary, apiSock, dir)
	if err != nil {
		return nil, err
	}
	api := NewAPIClient(apiSock)
	if err := waitReady(ctx, p.bootWait, func() error { return api.Ping(ctx) }); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("prewarmed VMM API not ready: %w", err)
	}
	return &vmmHandle{id: id, dir: dir, cmd: cmd, api: api}, nil
}

// close kills all idle VMMs (used by tests and shutdown).
func (p *vmmPool) close() {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	for _, h := range idle {
		h.kill()
	}
}

func (p *vmmPool) idleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.idle)
}

func (h *vmmHandle) kill() {
	if h.cmd != nil && h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_, _ = h.cmd.Process.Wait()
	}
	_ = os.RemoveAll(h.dir)
}

// waitReady polls fn every 5ms until success or timeout.
func waitReady(ctx context.Context, timeout time.Duration, fn func() error) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = fn(); lastErr == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Millisecond):
		}
	}
	return lastErr
}
