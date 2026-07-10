package shim

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeOCIConfig(t *testing.T, dir string, cfg map[string]any) {
	t.Helper()
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBundleNetns(t *testing.T) {
	dir := t.TempDir()
	writeOCIConfig(t, dir, map[string]any{
		"linux": map[string]any{
			"namespaces": []map[string]any{
				{"type": "pid"},
				{"type": "network", "path": "/var/run/netns/cni-abc123"},
				{"type": "mount"},
			},
		},
	})
	if got := bundleNetns(dir); got != "/var/run/netns/cni-abc123" {
		t.Fatalf("netns = %q", got)
	}
}

func TestBundleNetnsAbsent(t *testing.T) {
	// No network namespace entry (or no path) -> "".
	dir := t.TempDir()
	writeOCIConfig(t, dir, map[string]any{
		"linux": map[string]any{
			"namespaces": []map[string]any{{"type": "network"}}, // no path = private ns, not CNI
		},
	})
	if got := bundleNetns(dir); got != "" {
		t.Fatalf("netns = %q, want empty", got)
	}
	if got := bundleNetns(t.TempDir()); got != "" { // no config.json at all
		t.Fatalf("netns without config = %q, want empty", got)
	}
	if got := bundleNetns(""); got != "" {
		t.Fatalf("netns for empty bundle = %q, want empty", got)
	}
}
