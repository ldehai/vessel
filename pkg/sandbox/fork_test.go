package sandbox

import (
	"context"
	"testing"
)

func TestSnapshotAndFork(t *testing.T) {
	m := NewManager()
	m.RegisterDriver(NewFakeDriver())
	ctx := context.Background()

	src, err := m.Create(ctx, "fake", &Spec{Name: "template"})
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Snapshot(ctx, src.ID(), "/snap/a"); err != nil {
		t.Fatal(err)
	}

	clone, err := m.Fork(ctx, src.ID(), "/snap/b")
	if err != nil {
		t.Fatal(err)
	}
	if clone.ID() == src.ID() {
		t.Fatal("clone must have a new ID")
	}
	if clone.State() != StateRunning {
		t.Fatalf("clone state = %s", clone.State())
	}
	if got := clone.(*FakeInstance).RestoredFrom(); got != "/snap/b" {
		t.Fatalf("restoredFrom = %q", got)
	}
	// clone is tracked by the manager
	if _, ok := m.Get(clone.ID()); !ok {
		t.Fatal("clone not tracked")
	}
}

func TestRestoreFrom(t *testing.T) {
	m := NewManager()
	d := NewFakeDriver()
	m.RegisterDriver(d)
	ctx := context.Background()

	src, _ := m.Create(ctx, "fake", &Spec{})
	if err := m.Snapshot(ctx, src.ID(), "/snap/tpl"); err != nil {
		t.Fatal(err)
	}
	clone, err := m.RestoreFrom(ctx, "fake", "/snap/tpl", RestoreOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if clone.ID() == src.ID() || clone.State() != StateRunning {
		t.Fatalf("clone: id=%s state=%s", clone.ID(), clone.State())
	}
	if _, ok := m.Get(clone.ID()); !ok {
		t.Fatal("clone not tracked")
	}
	if _, err := m.RestoreFrom(ctx, "nope", "/snap/tpl", RestoreOpts{}); err == nil {
		t.Fatal("want error for unknown driver")
	}
}

func TestForkUnknownSandbox(t *testing.T) {
	m := NewManager()
	if _, err := m.Fork(context.Background(), "nope", "/snap"); err == nil {
		t.Fatal("want error")
	}
}

func TestForkUnsupportedDriver(t *testing.T) {
	m := NewManager()
	d := &noRestoreDriver{}
	m.RegisterDriver(d)
	inst, err := m.Create(context.Background(), "norestore", &Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Fork(context.Background(), inst.ID(), "/snap"); err != ErrNotSupported {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

// noRestoreDriver implements Driver but NOT Restorer.
type noRestoreDriver struct{}

func (d *noRestoreDriver) Name() string { return "norestore" }

func (d *noRestoreDriver) Create(ctx context.Context, spec *Spec) (Instance, error) {
	return NewFakeDriver().Create(ctx, spec)
}
