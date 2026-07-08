package shim

import (
	"encoding/json"
	"fmt"
	"os"
)

// Templates resolves a template id (the value of the pod annotation
// vessel.dev/template) to a vessel driver + snapshot path.
type Templates interface {
	Lookup(id string) (driver, snapshotPath string, ok bool)
}

// Template is one restore source.
type Template struct {
	Driver string `json:"driver,omitempty"` // empty = service default driver
	Path   string `json:"path"`             // snapshot directory
}

// MapTemplates is an in-memory Templates.
type MapTemplates map[string]Template

func (m MapTemplates) Lookup(id string) (string, string, bool) {
	t, ok := m[id]
	return t.Driver, t.Path, ok
}

// LoadTemplates reads a JSON template registry:
//
//	{
//	  "python-3.12": {"driver": "cloudhypervisor", "path": "/var/lib/vessel/tpl/py312"},
//	  "node-22":     {"path": "/var/lib/vessel/tpl/node22"}
//	}
//
// Entries without a driver use the service's default. Every entry must have
// a path; malformed registries fail loudly rather than silently serving
// cold boots where warm restores were promised.
func LoadTemplates(path string) (MapTemplates, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read template registry: %w", err)
	}
	var m MapTemplates
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse template registry %s: %w", path, err)
	}
	for id, t := range m {
		if t.Path == "" {
			return nil, fmt.Errorf("template %q: path is required", id)
		}
	}
	return m, nil
}
