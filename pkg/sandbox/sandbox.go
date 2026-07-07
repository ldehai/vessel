// Package sandbox defines the core domain model: sandbox spec, lifecycle
// state machine, and the pluggable Driver interface every isolation backend
// (process namespaces, Cloud Hypervisor, Firecracker, ...) must implement.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"sync"
	"time"
)

// State is the lifecycle state of a sandbox.
type State string

const (
	StateCreated  State = "created"
	StateRunning  State = "running"
	StateStopped  State = "stopped"
	StateSnapshot State = "snapshotted"
)

// Spec describes the desired sandbox.
type Spec struct {
	Name    string            // human-friendly name
	Rootfs  string            // path to root filesystem (empty = host fs, dev only)
	Cmd     []string          // init command
	Env     map[string]string // environment variables
	VCPUs   int               // vCPU count (VM drivers)
	MemMiB  int               // memory limit
	Timeout time.Duration     // max lifetime, 0 = unlimited
}

// Instance is a live sandbox created by a Driver.
type Instance interface {
	ID() string
	State() State
	// Exec runs a command inside the sandbox and returns its exit code.
	Exec(ctx context.Context, cmd []string, stdout, stderr io.Writer) (int, error)
	// Snapshot persists the sandbox state to path. VM drivers implement
	// this natively; the process driver returns ErrNotSupported.
	Snapshot(ctx context.Context, path string) error
	Stop(ctx context.Context) error
}

// Driver creates sandbox instances using a specific isolation technology.
type Driver interface {
	Name() string
	Create(ctx context.Context, spec *Spec) (Instance, error)
}

// ErrNotSupported is returned for capabilities a driver does not implement.
var ErrNotSupported = errors.New("operation not supported by this driver")

// Manager tracks live sandboxes across drivers.
type Manager struct {
	mu        sync.RWMutex
	drivers   map[string]Driver
	instances map[string]Instance
}

func NewManager() *Manager {
	return &Manager{
		drivers:   map[string]Driver{},
		instances: map[string]Instance{},
	}
}

func (m *Manager) RegisterDriver(d Driver) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.drivers[d.Name()] = d
}

func (m *Manager) Drivers() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.drivers))
	for n := range m.drivers {
		names = append(names, n)
	}
	return names
}

// Create builds a sandbox with the named driver and tracks it.
func (m *Manager) Create(ctx context.Context, driver string, spec *Spec) (Instance, error) {
	m.mu.RLock()
	d, ok := m.drivers[driver]
	m.mu.RUnlock()
	if !ok {
		return nil, errors.New("unknown driver: " + driver)
	}
	inst, err := d.Create(ctx, spec)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.instances[inst.ID()] = inst
	m.mu.Unlock()
	return inst, nil
}

func (m *Manager) Get(id string) (Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	inst, ok := m.instances[id]
	return inst, ok
}

func (m *Manager) List() []Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Instance, 0, len(m.instances))
	for _, i := range m.instances {
		out = append(out, i)
	}
	return out
}

// NewID returns a random 12-hex-char sandbox ID.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
