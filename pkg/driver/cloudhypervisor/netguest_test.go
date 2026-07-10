package cloudhypervisor

import "testing"

// TAP names must fit IFNAMSIZ (15 usable chars) for any instance id.
func TestTapNameFitsIfnamsiz(t *testing.T) {
	for _, id := range []string{"a", "843f025169c2", "0123456789abcdef0123"} {
		name := tapName(id)
		if len(name) > 15 {
			t.Fatalf("tapName(%q) = %q (%d chars), exceeds IFNAMSIZ-1", id, name, len(name))
		}
		if name[:4] != "vtap" {
			t.Fatalf("tapName(%q) = %q, want vtap prefix", id, name)
		}
	}
	// Distinct ids must not collide within the first 8 chars they keep.
	if tapName("843f025169c2") == tapName("943f025169c2") {
		t.Fatal("distinct ids collided")
	}
}
