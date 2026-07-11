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
	// Netns, when set, is the path to a CNI-configured network namespace
	// (the pod netns from a containerd bundle). VM drivers bridge its eth0
	// into the guest; empty means no pod networking.
	Netns string
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

// Restorer is implemented by drivers that can restore an instance from a
// snapshot. Fork = Snapshot + Restore; this is vessel's core primitive
// for agent workloads (template sandbox -> N cheap session clones).
type Restorer interface {
	Restore(ctx context.Context, snapshotPath string) (Instance, error)
}

// ErrNotSupported is returned for capabilities a driver does not implement.
var ErrNotSupported = errors.New("operation not supported by this driver")

type entry struct {
	inst   Instance
	driver Driver
}

// Manager tracks live sandboxes across drivers.
type Manager struct {
	mu        sync.RWMutex
	drivers   map[string]Driver
	instances map[string]entry
}

func NewManager() *Manager {
	return &Manager{
		drivers:   map[string]Driver{},
		instances: map[string]entry{},
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
	m.track(inst, d)
	return inst, nil
}

func (m *Manager) track(inst Instance, d Driver) {
	m.mu.Lock()
	m.instances[inst.ID()] = entry{inst: inst, driver: d}
	m.mu.Unlock()
}

func (m *Manager) Get(id string) (Instance, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.instances[id]
	return e.inst, ok
}

func (m *Manager) List() []Instance {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]Instance, 0, len(m.instances))
	for _, e := range m.instances {
		out = append(out, e.inst)
	}
	return out
}

// Delete stops sandbox id and drops it from tracking. Idempotent-ish: an
// unknown id is an error so callers can distinguish "already gone".
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	e, ok := m.instances[id]
	if ok {
		delete(m.instances, id)
	}
	m.mu.Unlock()
	if !ok {
		return errors.New("unknown sandbox: " + id)
	}
	return e.inst.Stop(ctx)
}

// Shutdown stops every tracked sandbox. Called on daemon exit so VMM
// child processes never outlive the manager. Also closes any driver that
// holds background resources (e.g. a prewarmed VMM pool).
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	insts := make([]Instance, 0, len(m.instances))
	for _, e := range m.instances {
		insts = append(insts, e.inst)
	}
	m.instances = map[string]entry{}
	drivers := make([]Driver, 0, len(m.drivers))
	for _, d := range m.drivers {
		drivers = append(drivers, d)
	}
	m.mu.Unlock()

	for _, inst := range insts {
		_ = inst.Stop(ctx)
	}
	for _, d := range drivers {
		if c, ok := d.(interface{ Close() }); ok {
			c.Close()
		}
	}
}

// Snapshot persists sandbox id to path.
func (m *Manager) Snapshot(ctx context.Context, id, path string) error {
	m.mu.RLock()
	e, ok := m.instances[id]
	m.mu.RUnlock()
	if !ok {
		return errors.New("unknown sandbox: " + id)
	}
	return e.inst.Snapshot(ctx, path)
}

// RestoreFrom creates a new instance from an existing snapshot directory
// without touching any running sandbox. This is the hot path for agent
// workloads: snapshot a prewarmed template once, restore per session.
func (m *Manager) RestoreFrom(ctx context.Context, driver, path string) (Instance, error) {
	m.mu.RLock()
	d, ok := m.drivers[driver]
	m.mu.RUnlock()
	if !ok {
		return nil, errors.New("unknown driver: " + driver)
	}
	r, ok := d.(Restorer)
	if !ok {
		return nil, ErrNotSupported
	}
	inst, err := r.Restore(ctx, path)
	if err != nil {
		return nil, err
	}
	m.track(inst, d)
	return inst, nil
}

// Fork snapshots sandbox id into dir and restores a new instance from it.
// The source sandbox keeps running; the clone starts from the exact same
// memory/filesystem state.
func (m *Manager) Fork(ctx context.Context, id, dir string) (Instance, error) {
	m.mu.RLock()
	e, ok := m.instances[id]
	m.mu.RUnlock()
	if !ok {
		return nil, errors.New("unknown sandbox: " + id)
	}
	r, ok := e.driver.(Restorer)
	if !ok {
		return nil, ErrNotSupported
	}
	if err := e.inst.Snapshot(ctx, dir); err != nil {
		return nil, err
	}
	clone, err := r.Restore(ctx, dir)
	if err != nil {
		return nil, err
	}
	m.track(clone, e.driver)
	return clone, nil
}

// NewID returns a random 12-hex-char sandbox ID.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
