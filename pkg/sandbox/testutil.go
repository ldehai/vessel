package sandbox

import (
	"bytes"
	"context"
	"io"
)

// FakeDriver is an in-memory driver used by unit tests across packages.
type FakeDriver struct {
	DriverName string
	ExecOut    string
	ExecCode   int
	Snapshots  map[string][]byte // path -> data written
	Closed     bool              // set by Close (Manager.Shutdown reaps drivers)
}

// Close records that the driver was closed, exercising Manager.Shutdown's
// driver-reaping path.
func (f *FakeDriver) Close() { f.Closed = true }

func NewFakeDriver() *FakeDriver {
	return &FakeDriver{DriverName: "fake", ExecOut: "fake-out", Snapshots: map[string][]byte{}}
}

func (f *FakeDriver) Name() string { return f.DriverName }

func (f *FakeDriver) Create(_ context.Context, spec *Spec) (Instance, error) {
	return &FakeInstance{id: NewID(), spec: spec, drv: f, state: StateRunning}, nil
}

// Restore implements Restorer: returns a fresh instance "from" the snapshot.
func (f *FakeDriver) Restore(_ context.Context, path string, opts RestoreOpts) (Instance, error) {
	if _, ok := f.Snapshots[path]; !ok {
		return nil, ErrNotSupported
	}
	return &FakeInstance{id: NewID(), drv: f, state: StateRunning, restoredFrom: path, netns: opts.Netns}, nil
}

type FakeInstance struct {
	id           string
	spec         *Spec
	drv          *FakeDriver
	state        State
	restoredFrom string
	netns        string
}

// RestoredFrom reports the snapshot path this instance was forked from.
func (f *FakeInstance) RestoredFrom() string { return f.restoredFrom }

// Netns reports the netns a restore was asked to bridge (test hook).
func (f *FakeInstance) Netns() string { return f.netns }

func (f *FakeInstance) ID() string   { return f.id }
func (f *FakeInstance) State() State { return f.state }

func (f *FakeInstance) Exec(_ context.Context, cmd []string, stdout, _ io.Writer) (int, error) {
	var b bytes.Buffer
	b.WriteString(f.drv.ExecOut)
	_, err := io.Copy(stdout, &b)
	return f.drv.ExecCode, err
}

func (f *FakeInstance) Snapshot(_ context.Context, path string) error {
	f.drv.Snapshots[path] = []byte(f.id)
	f.state = StateSnapshot
	return nil
}

func (f *FakeInstance) Stop(context.Context) error {
	f.state = StateStopped
	return nil
}
