package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/ldehai/vessel/pkg/agent"
	"github.com/ldehai/vessel/pkg/sandbox"
)

const (
	agentVsockPort = 5000
	guestCID       = 3
)

// Config locates host resources the driver needs.
type Config struct {
	BinaryPath string        // cloud-hypervisor binary (default: $PATH lookup)
	KernelPath string        // guest kernel image (vmlinux)
	RootfsPath string        // default rootfs disk image (raw/erofs)
	StateDir   string        // per-sandbox sockets and snapshots
	Cmdline    string        // kernel cmdline (default provided)
	BootWait   time.Duration // max wait for API socket + agent (default 10s)
	PoolSize   int           // prewarmed VMM processes (0 = spawn per request)
}

func (c *Config) withDefaults() Config {
	out := *c
	if out.BinaryPath == "" {
		out.BinaryPath = "cloud-hypervisor"
	}
	if out.StateDir == "" {
		out.StateDir = filepath.Join(os.TempDir(), "vessel-ch")
	}
	if out.Cmdline == "" {
		// ttyS0 = x86_64 serial, ttyAMA0 = aarch64 pl011; the kernel keeps
		// whichever exists. Serial output lands in <state>/serial.log.
		out.Cmdline = "console=ttyS0 console=ttyAMA0 root=/dev/vda ro init=/sbin/init"
	}
	if out.BootWait == 0 {
		out.BootWait = 10 * time.Second
	}
	return out
}

type Driver struct {
	cfg  Config
	pool *vmmPool
}

func New(cfg Config) *Driver {
	c := cfg.withDefaults()
	return &Driver{
		cfg:  c,
		pool: newVMMPool(c.BinaryPath, c.StateDir, c.PoolSize, c.BootWait),
	}
}

func (d *Driver) Name() string { return "cloudhypervisor" }

// Warm pre-spawns PoolSize VMM processes in the background. Call once at
// daemon startup; one-shot CLI use can skip it.
func (d *Driver) Warm() { d.pool.kickRefill() }

// Create takes a ready VMM (prewarmed or freshly spawned), boots a microVM
// and waits for the in-guest vessel-agent to answer over hybrid vsock.
func (d *Driver) Create(ctx context.Context, spec *sandbox.Spec) (sandbox.Instance, error) {
	h, err := d.pool.get(ctx)
	if err != nil {
		return nil, err
	}

	inst := &Instance{
		id:     h.id,
		dir:    h.dir,
		vsock:  filepath.Join(h.dir, "vsock.sock"),
		api:    h.api,
		vmm:    h.cmd,
		state:  sandbox.StateCreated,
		cfgDrv: d.cfg,
	}

	if err := inst.boot(ctx, spec); err != nil {
		_ = inst.Stop(context.Background())
		return nil, err
	}
	return inst, nil
}

// Restore spawns a fresh cloud-hypervisor process and restores VM state
// from a snapshot directory produced by Snapshot. This is the fast path
// behind Manager.Fork: restoring a prewarmed template skips kernel boot
// entirely, which is how cold starts get under 100ms.
//
// Note: CH restores the vsock device with the host socket path recorded
// in the snapshot (the SOURCE instance's vsock.sock). Binding would fail
// with EADDRINUSE while the source's socket file exists, so we unlink it
// first: established connections (source VM <-> its agent client) keep
// working on their open fds; only NEW dials on that path reach the clone.
// Proper per-clone socket remapping is tracked for M4.
func (d *Driver) Restore(ctx context.Context, snapshotPath string) (sandbox.Instance, error) {
	vsockPath := snapshotVsockPath(snapshotPath)
	_ = os.Remove(vsockPath) // free the path for the restored VMM's listener

	h, err := d.pool.get(ctx)
	if err != nil {
		return nil, err
	}

	inst := &Instance{
		id:     h.id,
		dir:    h.dir,
		vsock:  vsockPath,
		api:    h.api,
		vmm:    h.cmd,
		state:  sandbox.StateCreated,
		cfgDrv: d.cfg,
	}
	fail := func(err error) (sandbox.Instance, error) {
		_ = inst.Stop(context.Background())
		return nil, err
	}

	if err := inst.api.RestoreVM(ctx, "file://"+snapshotPath); err != nil {
		return fail(fmt.Errorf("restore: %w", err))
	}
	if err := inst.api.ResumeVM(ctx); err != nil {
		return fail(fmt.Errorf("resume: %w", err))
	}
	var conn io.ReadWriteCloser
	if err := inst.waitFor(ctx, func() error {
		c, err := DialHybridVsock(inst.vsock, agentVsockPort, time.Second)
		if err != nil {
			return err
		}
		conn = c
		return nil
	}); err != nil {
		return fail(fmt.Errorf("guest agent not reachable after restore: %w", err))
	}
	inst.agent = agent.NewClient(conn)
	inst.state = sandbox.StateRunning
	return inst, nil
}

type Instance struct {
	id     string
	dir    string
	vsock  string
	api    *APIClient
	vmm    *exec.Cmd
	agent  *agent.Client
	state  sandbox.State
	cfgDrv Config
}

func (i *Instance) ID() string           { return i.id }
func (i *Instance) State() sandbox.State { return i.state }

func (i *Instance) boot(ctx context.Context, spec *sandbox.Spec) error {
	if err := i.waitFor(ctx, func() error { return i.api.Ping(ctx) }); err != nil {
		return fmt.Errorf("VMM API not ready: %w", err)
	}

	rootfs := spec.Rootfs
	if rootfs == "" {
		rootfs = i.cfgDrv.RootfsPath
	}
	vcpus, mem := spec.VCPUs, spec.MemMiB
	if vcpus == 0 {
		vcpus = 1
	}
	if mem == 0 {
		mem = 256
	}
	vmCfg := &VMConfig{
		CPUs:    CPUsConfig{BootVCPUs: vcpus, MaxVCPUs: vcpus},
		Memory:  MemoryConfig{Size: int64(mem) << 20, Shared: true}, // shared => snapshot/fork friendly
		Payload: PayloadConfig{Kernel: i.cfgDrv.KernelPath, Cmdline: i.cfgDrv.Cmdline},
		Disks:   []DiskConfig{{Path: rootfs, Readonly: true}},
		Vsock:   &VsockConfig{CID: guestCID, Socket: i.vsock},
		Serial:  &ConsoleCfg{Mode: "File", File: filepath.Join(i.dir, "serial.log")},
		Console: &ConsoleCfg{Mode: "Off"},
	}
	if err := i.api.CreateVM(ctx, vmCfg); err != nil {
		return err
	}
	if err := i.api.BootVM(ctx); err != nil {
		return err
	}

	// Wait for the guest agent to come up on vsock.
	var conn io.ReadWriteCloser
	err := i.waitFor(ctx, func() error {
		c, err := DialHybridVsock(i.vsock, agentVsockPort, time.Second)
		if err != nil {
			return err
		}
		conn = c
		return nil
	})
	if err != nil {
		return fmt.Errorf("guest agent not reachable: %w", err)
	}
	i.agent = agent.NewClient(conn)
	if err := i.agent.Ping(); err != nil {
		return fmt.Errorf("guest agent ping: %w", err)
	}
	i.state = sandbox.StateRunning
	return nil
}

func (i *Instance) Exec(ctx context.Context, cmd []string, stdout, stderr io.Writer) (int, error) {
	if i.agent == nil {
		return -1, fmt.Errorf("sandbox %s: agent not connected", i.id)
	}
	code, out, errOut, err := i.agent.Exec(cmd, nil)
	if err != nil {
		return -1, err
	}
	if stdout != nil {
		_, _ = stdout.Write(out)
	}
	if stderr != nil {
		_, _ = stderr.Write(errOut)
	}
	return code, nil
}

// Snapshot pauses the VM, snapshots full state to path (a directory), and
// resumes. The result can seed Restore/fork on any host with the same CH
// version. The destination is replaced: CH refuses to overwrite existing
// snapshot files ("File exists"), so stale contents are cleared first.
func (i *Instance) Snapshot(ctx context.Context, path string) error {
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("clear snapshot dir: %w", err)
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return err
	}
	if err := i.api.PauseVM(ctx); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	snapErr := i.api.SnapshotVM(ctx, "file://"+path)
	if err := i.api.ResumeVM(ctx); err != nil && snapErr == nil {
		return fmt.Errorf("resume: %w", err)
	}
	if snapErr != nil {
		return fmt.Errorf("snapshot: %w", snapErr)
	}
	i.state = sandbox.StateRunning
	return nil
}

func (i *Instance) Stop(ctx context.Context) error {
	if i.agent != nil {
		_ = i.agent.Close()
	}
	_ = i.api.ShutdownVMM(ctx)
	if i.vmm != nil && i.vmm.Process != nil {
		done := make(chan struct{})
		go func() { _, _ = i.vmm.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = i.vmm.Process.Kill()
		}
	}
	i.state = sandbox.StateStopped
	return nil
}

// snapshotVsockPath reads the host-side vsock socket path recorded in a
// snapshot's config.json. Falls back to <snapshot>/vsock.sock if the file
// cannot be parsed (older/unknown layouts).
func snapshotVsockPath(snapshotPath string) string {
	fallback := filepath.Join(snapshotPath, "vsock.sock")
	data, err := os.ReadFile(filepath.Join(snapshotPath, "config.json"))
	if err != nil {
		return fallback
	}
	// The config may be the VmConfig itself or wrapped under "config".
	var direct struct {
		Vsock *VsockConfig `json:"vsock"`
	}
	if json.Unmarshal(data, &direct) == nil && direct.Vsock != nil && direct.Vsock.Socket != "" {
		return direct.Vsock.Socket
	}
	var wrapped struct {
		Config struct {
			Vsock *VsockConfig `json:"vsock"`
		} `json:"config"`
	}
	if json.Unmarshal(data, &wrapped) == nil && wrapped.Config.Vsock != nil && wrapped.Config.Vsock.Socket != "" {
		return wrapped.Config.Vsock.Socket
	}
	return fallback
}

// startVMM launches cloud-hypervisor with its stdout/stderr captured to
// <dir>/vmm.log so boot failures are diagnosable per instance.
func startVMM(binary, apiSock, dir string) (*exec.Cmd, error) {
	logf, err := os.Create(filepath.Join(dir, "vmm.log"))
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(binary, "--api-socket", apiSock)
	cmd.Stdout, cmd.Stderr = logf, logf
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start cloud-hypervisor (%s): %w", binary, err)
	}
	return cmd, nil
}

// waitFor polls fn until success, ctx cancellation, or BootWait elapses.
func (i *Instance) waitFor(ctx context.Context, fn func() error) error {
	return waitReady(ctx, i.cfgDrv.BootWait, fn)
}
