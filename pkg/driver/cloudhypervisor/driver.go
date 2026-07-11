package cloudhypervisor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/ldehai/vessel/pkg/agent"
	"github.com/ldehai/vessel/pkg/image"
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
	// RestoreMode: MemoryRestoreOnDemand (default) faults snapshot pages in
	// on first access, decoupling restore latency from template memory size.
	// Automatically falls back to Copy on CH versions without support.
	RestoreMode string
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
	if out.RestoreMode == "" {
		out.RestoreMode = MemoryRestoreOnDemand
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

// Close kills the idle VMMs the pool is holding. Live sandboxes are owned
// by the Manager (Shutdown stops them); this reaps only the prewarmed
// spares so none outlive the daemon.
func (d *Driver) Close() { d.pool.close() }

// Create takes a ready VMM (prewarmed or freshly spawned), boots a microVM
// and waits for the in-guest vessel-agent to answer over hybrid vsock.
func (d *Driver) Create(ctx context.Context, spec *sandbox.Spec) (sandbox.Instance, error) {
	// A pod netns'd VM must run inside that netns to see its TAP, so it
	// cannot use a prewarmed pool VMM (spawned outside any netns). Spawn it
	// directly in the netns instead.
	var h *vmmHandle
	var err error
	if spec.Netns != "" {
		h, err = d.pool.spawnInNetns(ctx, spec.Netns)
	} else {
		h, err = d.pool.get(ctx)
	}
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
		netns:  spec.Netns,
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
// Each clone restores from its own overlay snapshot (hardlinked files +
// rewritten vsock path), so the source VM and any number of concurrent
// clones never contend on a socket path.
func (d *Driver) Restore(ctx context.Context, snapshotPath string) (sandbox.Instance, error) {
	h, err := d.pool.get(ctx)
	if err != nil {
		return nil, err
	}

	vsockPath := filepath.Join(h.dir, "vsock.sock")
	overlay, err := prepareCloneSnapshot(snapshotPath, h.dir, vsockPath)
	if err != nil {
		h.kill()
		return nil, err
	}
	snapshotPath = overlay

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

	// OnDemand restore (userfaultfd) keeps latency independent of template
	// memory size. Older CH rejects the mode; fall back to eager Copy once.
	restoreCfg := &RestoreConfig{
		SourceURL:         "file://" + snapshotPath,
		MemoryRestoreMode: d.cfg.RestoreMode,
		Resume:            true,
	}
	err = inst.api.RestoreVM(ctx, restoreCfg)
	if err != nil && restoreCfg.MemoryRestoreMode == MemoryRestoreOnDemand {
		restoreCfg.MemoryRestoreMode = "" // CH default: Copy
		err = inst.api.RestoreVM(ctx, restoreCfg)
	}
	if err != nil {
		return fail(fmt.Errorf("restore: %w", err))
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
	netns  string    // pod netns path ("" = no pod networking)
	netCfg *netSetup // populated by setupNetwork when netns is set
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
	// containerd hands rootfs as a directory; a microVM needs a block image.
	// Pack it once per instance (erofs if available, else ext4). A rootfs
	// that is already a file (the configured default image, or a template)
	// is used as-is.
	if rootfs != "" {
		if info, err := os.Stat(rootfs); err == nil && info.IsDir() {
			res, err := image.PackDir(rootfs, filepath.Join(i.dir, "rootfs.img"), image.Options{})
			if err != nil {
				return fmt.Errorf("pack rootfs %s: %w", rootfs, err)
			}
			rootfs = res.Path
		}
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
	// Pod networking: create the TAP in the netns and attach it as
	// virtio-net (the guest adopts the pod IP after its agent is up).
	if i.netns != "" {
		if err := i.setupNetwork(vmCfg); err != nil {
			return fmt.Errorf("pod network: %w", err)
		}
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
	// With the agent up, the guest adopts the pod's network identity.
	if err := i.applyGuestNetwork(); err != nil {
		return fmt.Errorf("guest network config: %w", err)
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
	i.teardownNetwork()
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

// prepareCloneSnapshot builds a per-clone view of a template snapshot in
// cloneDir/snapshot: every file is hardlinked (zero-copy, memory-ranges can
// be GBs) except config.json, which is rewritten so the vsock device's host
// socket points into cloneDir. This gives each clone its own socket path,
// so N clones can be restored from one template concurrently.
//
// The rewrite goes through a generic map to preserve every VmConfig field
// CH recorded, including ones this driver doesn't model.
func prepareCloneSnapshot(snapshotPath, cloneDir, vsockPath string) (string, error) {
	overlay := filepath.Join(cloneDir, "snapshot")
	if err := os.MkdirAll(overlay, 0o755); err != nil {
		return "", err
	}
	entries, err := os.ReadDir(snapshotPath)
	if err != nil {
		return "", fmt.Errorf("read snapshot dir: %w", err)
	}
	for _, e := range entries {
		if e.IsDir() || e.Name() == "config.json" {
			continue
		}
		src := filepath.Join(snapshotPath, e.Name())
		dst := filepath.Join(overlay, e.Name())
		if err := os.Link(src, dst); err != nil {
			// Cross-device etc.: fall back to copying.
			data, rerr := os.ReadFile(src)
			if rerr != nil {
				return "", fmt.Errorf("link/copy %s: %v / %w", e.Name(), err, rerr)
			}
			if werr := os.WriteFile(dst, data, 0o600); werr != nil {
				return "", werr
			}
		}
	}

	data, err := os.ReadFile(filepath.Join(snapshotPath, "config.json"))
	if err != nil {
		return "", fmt.Errorf("read snapshot config.json: %w", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		return "", fmt.Errorf("parse snapshot config.json: %w", err)
	}
	if vs, ok := cfg["vsock"].(map[string]any); ok {
		vs["socket"] = vsockPath
	}
	out, err := json.Marshal(cfg)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(overlay, "config.json"), out, 0o600); err != nil {
		return "", err
	}
	return overlay, nil
}

// startVMM launches cloud-hypervisor with its stdout/stderr captured to
// <dir>/vmm.log so boot failures are diagnosable per instance.
func startVMM(binary, apiSock, dir string) (*exec.Cmd, error) {
	return startVMMPrefixed(nil, binary, apiSock, dir)
}

// startVMMPrefixed prepends an entry command (e.g. nsenter --net=<path>)
// so the VMM can run inside a pod network namespace.
func startVMMPrefixed(prefix []string, binary, apiSock, dir string) (*exec.Cmd, error) {
	logf, err := os.Create(filepath.Join(dir, "vmm.log"))
	if err != nil {
		return nil, err
	}
	argv := append(append([]string{}, prefix...), binary, "--api-socket", apiSock)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdout, cmd.Stderr = logf, logf
	// Safety net: if the daemon dies without a clean Shutdown (crash, SIGKILL),
	// the kernel sends the child SIGKILL too, so no cloud-hypervisor is
	// orphaned. The clean path (Manager.Shutdown -> Stop/Close) still runs
	// first; this only covers the ungraceful case.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("start cloud-hypervisor (%v): %w", argv, err)
	}
	return cmd, nil
}

// waitFor polls fn until success, ctx cancellation, or BootWait elapses.
func (i *Instance) waitFor(ctx context.Context, fn func() error) error {
	return waitReady(ctx, i.cfgDrv.BootWait, fn)
}
