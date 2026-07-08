package shim

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTemplates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "templates.json")
	_ = os.WriteFile(p, []byte(`{
		"python-3.12": {"driver": "cloudhypervisor", "path": "/var/lib/vessel/tpl/py312"},
		"node-22":     {"path": "/var/lib/vessel/tpl/node22"}
	}`), 0o644)

	m, err := LoadTemplates(p)
	if err != nil {
		t.Fatal(err)
	}
	drv, path, ok := m.Lookup("python-3.12")
	if !ok || drv != "cloudhypervisor" || path != "/var/lib/vessel/tpl/py312" {
		t.Fatalf("python-3.12 -> %q %q %v", drv, path, ok)
	}
	drv, _, ok = m.Lookup("node-22")
	if !ok || drv != "" { // empty driver = service default
		t.Fatalf("node-22 driver = %q, want empty", drv)
	}
	if _, _, ok := m.Lookup("nope"); ok {
		t.Fatal("unknown id must not resolve")
	}
}

func TestLoadTemplatesRejectsMissingPath(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(p, []byte(`{"broken": {"driver": "fake"}}`), 0o644)
	if _, err := LoadTemplates(p); err == nil {
		t.Fatal("registry entry without path must be rejected")
	}
}

func TestLoadTemplatesMissingFile(t *testing.T) {
	if _, err := LoadTemplates("/nonexistent/templates.json"); err == nil {
		t.Fatal("want error for missing registry file")
	}
}
