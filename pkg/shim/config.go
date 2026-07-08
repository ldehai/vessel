package shim

import (
	"encoding/json"
	"fmt"
	"os"
)

// DefaultConfigPath is where a containerd-launched shim looks for node
// configuration. containerd invokes the shim with a fixed argument set, so
// per-node settings (VM assets, template registry) come from a file rather
// than flags. Override with $VESSEL_SHIM_CONFIG.
const DefaultConfigPath = "/etc/vessel/shim.json"

// Config is the node-level shim configuration.
//
//	{
//	  "kernel":     "/var/lib/vessel/vmlinux",
//	  "rootfs":     "/var/lib/vessel/rootfs.img",
//	  "ch_binary":  "/usr/local/bin/cloud-hypervisor",
//	  "pool":       2,
//	  "templates":  {"python-3.12": {"driver": "cloudhypervisor", "path": "/var/lib/vessel/tpl/py312"}}
//	}
//
// With no kernel configured the shim falls back to the process driver, so
// the handshake and lifecycle remain testable on machines without VM assets.
type Config struct {
	Kernel    string       `json:"kernel,omitempty"`
	Rootfs    string       `json:"rootfs,omitempty"`
	CHBinary  string       `json:"ch_binary,omitempty"`
	Pool      int          `json:"pool,omitempty"`
	Templates MapTemplates `json:"templates,omitempty"`
}

// LoadConfig reads the node config. A missing file is not an error (all
// defaults); a present-but-malformed file is, because silently ignoring a
// node admin's config leads to pods cold-booting where they configured
// warm templates.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("VESSEL_SHIM_CONFIG")
	}
	if path == "" {
		path = DefaultConfigPath
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Config{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read shim config: %w", err)
	}
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse shim config %s: %w", path, err)
	}
	for id, t := range c.Templates {
		if t.Path == "" {
			return nil, fmt.Errorf("shim config %s: template %q: path is required", path, id)
		}
	}
	return &c, nil
}

// DefaultDriver picks the driver the config can actually support.
func (c *Config) DefaultDriver() string {
	if c.Kernel != "" {
		return "cloudhypervisor"
	}
	return "process"
}
