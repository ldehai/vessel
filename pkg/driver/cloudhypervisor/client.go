// Package cloudhypervisor implements the production VM driver on top of
// the Cloud Hypervisor REST API (served over a unix socket by the
// cloud-hypervisor process itself).
package cloudhypervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// APIClient talks to one cloud-hypervisor process via its --api-socket.
type APIClient struct {
	http *http.Client
}

func NewAPIClient(socketPath string) *APIClient {
	return &APIClient{
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// VMConfig is the subset of Cloud Hypervisor's VmConfig that vessel uses.
type VMConfig struct {
	CPUs    CPUsConfig    `json:"cpus"`
	Memory  MemoryConfig  `json:"memory"`
	Payload PayloadConfig `json:"payload"`
	Disks   []DiskConfig  `json:"disks,omitempty"`
	Vsock   *VsockConfig  `json:"vsock,omitempty"`
	Serial  *ConsoleCfg   `json:"serial,omitempty"`
	Console *ConsoleCfg   `json:"console,omitempty"`
}

type CPUsConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
	MaxVCPUs  int `json:"max_vcpus"`
}

type MemoryConfig struct {
	Size   int64 `json:"size"` // bytes
	Shared bool  `json:"shared,omitempty"`
}

type PayloadConfig struct {
	Kernel  string `json:"kernel"`
	Cmdline string `json:"cmdline,omitempty"`
}

type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

type VsockConfig struct {
	CID    uint32 `json:"cid"`
	Socket string `json:"socket"`
}

type ConsoleCfg struct {
	Mode string `json:"mode"` // "Off", "Tty", "File", "Null"
	File string `json:"file,omitempty"`
}

// SnapshotConfig / RestoreConfig for vm.snapshot & vm.restore.
type SnapshotConfig struct {
	DestinationURL string `json:"destination_url"` // file:///path
}

type RestoreConfig struct {
	SourceURL string `json:"source_url"`
}

func (c *APIClient) Ping(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "vmm.ping", nil, nil)
}

func (c *APIClient) CreateVM(ctx context.Context, cfg *VMConfig) error {
	return c.call(ctx, http.MethodPut, "vm.create", cfg, nil)
}

func (c *APIClient) BootVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.boot", nil, nil)
}

func (c *APIClient) PauseVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.pause", nil, nil)
}

func (c *APIClient) ResumeVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.resume", nil, nil)
}

// SnapshotVM requires the VM to be paused first.
func (c *APIClient) SnapshotVM(ctx context.Context, destURL string) error {
	return c.call(ctx, http.MethodPut, "vm.snapshot", &SnapshotConfig{DestinationURL: destURL}, nil)
}

func (c *APIClient) RestoreVM(ctx context.Context, srcURL string) error {
	return c.call(ctx, http.MethodPut, "vm.restore", &RestoreConfig{SourceURL: srcURL}, nil)
}

func (c *APIClient) ShutdownVM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vm.shutdown", nil, nil)
}

func (c *APIClient) ShutdownVMM(ctx context.Context) error {
	return c.call(ctx, http.MethodPut, "vmm.shutdown", nil, nil)
}

// VMInfo returns the raw vm.info JSON (state, config).
func (c *APIClient) VMInfo(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.call(ctx, http.MethodGet, "vm.info", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (c *APIClient) call(ctx context.Context, method, endpoint string, body, out any) error {
	var reader io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(buf)
	}
	// Host is ignored for unix sockets but required by http.
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost/api/v1/"+endpoint, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("ch %s: %w", endpoint, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("ch %s: HTTP %d: %s", endpoint, resp.StatusCode, bytes.TrimSpace(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
