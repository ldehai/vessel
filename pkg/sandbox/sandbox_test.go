package sandbox

import (
	"bytes"
	"context"
	"testing"
)

func TestManagerLifecycle(t *testing.T) {
	m := NewManager()
	m.RegisterDriver(NewFakeDriver())

	if got := m.Drivers(); len(got) != 1 || got[0] != "fake" {
		t.Fatalf("Drivers() = %v, want [fake]", got)
	}

	inst, err := m.Create(context.Background(), "fake", &Spec{Name: "t1"})
	if err != nil {
		t.Fatal(err)
	}
	if inst.State() != StateRunning {
		t.Fatalf("state = %s, want running", inst.State())
	}

	got, ok := m.Get(inst.ID())
	if !ok || got.ID() != inst.ID() {
		t.Fatalf("Get(%s) failed", inst.ID())
	}
	if n := len(m.List()); n != 1 {
		t.Fatalf("List() len = %d, want 1", n)
	}

	var out bytes.Buffer
	code, err := inst.Exec(context.Background(), []string{"echo"}, &out, nil)
	if err != nil || code != 0 {
		t.Fatalf("Exec: code=%d err=%v", code, err)
	}
	if out.String() != "fake-out" {
		t.Fatalf("stdout = %q", out.String())
	}

	if err := inst.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if inst.State() != StateStopped {
		t.Fatalf("state after stop = %s", inst.State())
	}
}

func TestManagerUnknownDriver(t *testing.T) {
	m := NewManager()
	if _, err := m.Create(context.Background(), "nope", &Spec{}); err == nil {
		t.Fatal("want error for unknown driver")
	}
}

func TestNewIDUnique(t *testing.T) {
	a, b := NewID(), NewID()
	if a == b || len(a) != 12 {
		t.Fatalf("bad ids: %s %s", a, b)
	}
}
