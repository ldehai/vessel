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
}

func NewFakeDriver() *FakeDriver {
	return &FakeDriver{DriverName: "fake", ExecOut: "fake-out", Snapshots: map[string][]byte{}}
}

func (f *FakeDriver) Name() string { return f.DriverName }

func (f *FakeDriver) Create(_ context.Context, spec *Spec) (Instance, error) {
	return &FakeInstance{id: NewID(), spec: spec, drv: f, state: StateRunning}, nil
}

type FakeInstance struct {
	id    string
	spec  *Spec
	drv   *FakeDriver
	state State
}

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
