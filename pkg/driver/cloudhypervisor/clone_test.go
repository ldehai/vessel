package cloudhypervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareCloneSnapshot(t *testing.T) {
	tpl := t.TempDir()
	cloneDir := t.TempDir()

	// Fake template snapshot: config.json + big memory file + state.json.
	cfg := map[string]any{
		"cpus":   map[string]any{"boot_vcpus": 1, "max_vcpus": 1},
		"vsock":  map[string]any{"cid": 3, "socket": "/tmp/source/vsock.sock"},
		"custom": "must-survive-rewrite",
	}
	raw, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(tpl, "config.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	mem := make([]byte, 1<<20)
	_ = os.WriteFile(filepath.Join(tpl, "memory-ranges"), mem, 0o600)
	_ = os.WriteFile(filepath.Join(tpl, "state.json"), []byte(`{"s":1}`), 0o600)

	vsockPath := filepath.Join(cloneDir, "vsock.sock")
	overlay, err := prepareCloneSnapshot(tpl, cloneDir, vsockPath)
	if err != nil {
		t.Fatal(err)
	}

	// Rewritten config points at the clone's socket; other fields survive.
	data, _ := os.ReadFile(filepath.Join(overlay, "config.json"))
	var got map[string]any
	_ = json.Unmarshal(data, &got)
	if got["vsock"].(map[string]any)["socket"] != vsockPath {
		t.Fatalf("vsock.socket = %v", got["vsock"])
	}
	if got["custom"] != "must-survive-rewrite" {
		t.Fatal("unmodeled config field lost in rewrite")
	}

	// memory-ranges is hardlinked, not copied.
	tplInfo, _ := os.Stat(filepath.Join(tpl, "memory-ranges"))
	ovInfo, _ := os.Stat(filepath.Join(overlay, "memory-ranges"))
	if !os.SameFile(tplInfo, ovInfo) {
		t.Fatal("memory-ranges should be a hardlink to the template's file")
	}
	if _, err := os.Stat(filepath.Join(overlay, "state.json")); err != nil {
		t.Fatal("state.json missing from overlay")
	}

	// A second clone from the same template must not conflict.
	clone2 := t.TempDir()
	if _, err := prepareCloneSnapshot(tpl, clone2, filepath.Join(clone2, "vsock.sock")); err != nil {
		t.Fatalf("second clone: %v", err)
	}
}

func TestPrepareCloneSnapshotMissingTemplate(t *testing.T) {
	if _, err := prepareCloneSnapshot("/nonexistent", t.TempDir(), "/x/vsock.sock"); err == nil {
		t.Fatal("want error for missing template")
	}
}
