package sandbox

import (
	"context"
	"testing"
)

func TestManagerDelete(t *testing.T) {
	m := NewManager()
	m.RegisterDriver(NewFakeDriver())
	ctx := context.Background()

	inst, err := m.Create(ctx, "fake", &Spec{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get(inst.ID()); !ok {
		t.Fatal("instance not tracked after create")
	}

	if err := m.Delete(ctx, inst.ID()); err != nil {
		t.Fatal(err)
	}
	if inst.(*FakeInstance).State() != StateStopped {
		t.Fatal("Delete must Stop the instance")
	}
	if _, ok := m.Get(inst.ID()); ok {
		t.Fatal("instance still tracked after delete")
	}
	// Deleting again is an error (already gone).
	if err := m.Delete(ctx, inst.ID()); err == nil {
		t.Fatal("second delete should error")
	}
}

func TestManagerShutdownStopsAllAndClosesDrivers(t *testing.T) {
	m := NewManager()
	d := NewFakeDriver()
	m.RegisterDriver(d)
	ctx := context.Background()

	var insts []Instance
	for i := 0; i < 5; i++ {
		inst, err := m.Create(ctx, "fake", &Spec{})
		if err != nil {
			t.Fatal(err)
		}
		insts = append(insts, inst)
	}

	m.Shutdown(ctx)

	for _, inst := range insts {
		if inst.(*FakeInstance).State() != StateStopped {
			t.Fatalf("instance %s not stopped after Shutdown", inst.ID())
		}
	}
	if len(m.List()) != 0 {
		t.Fatalf("Shutdown left %d instances tracked", len(m.List()))
	}
	if !d.Closed {
		t.Fatal("Shutdown must Close drivers that support it")
	}
}
