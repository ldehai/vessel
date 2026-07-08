//go:build unix

package shim

import (
	"context"
	"crypto/sha256"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ldehai/vessel/pkg/driver/cloudhypervisor"
	"github.com/ldehai/vessel/pkg/driver/process"
	"github.com/ldehai/vessel/pkg/sandbox"
)

// This file implements the containerd Runtime v2 shim process lifecycle
// ("the handshake"), matching what containerd's runtime/v2 code does with
// the runc shim:
//
//	containerd exec:  shim -namespace <ns> -id <id> -address <sock> ... start   (cwd = bundle)
//	  start: listen on a derived unix socket, spawn the daemon (re-exec
//	         self without the subcommand, listener passed as fd 3), write
//	         bundle/address + bundle/shim.pid, print the address to stdout,
//	         exit. containerd connects to the printed address.
//	containerd exec:  shim ... delete                                           (cwd = bundle)
//	  delete: best-effort cleanup of a dead task's resources; print a
//	          protobuf task.DeleteResponse to stdout.
//
// Environment provided by containerd: TTRPC_ADDRESS (events endpoint),
// GRPC_ADDRESS, NAMESPACE, MAX_SHIM_VERSION=2.

// socketFD is the fd number at which the daemon inherits its listener
// (stdin/out/err are 0-2, first ExtraFile is 3 — same as the runc shim).
const socketFD = 3

// socketRoot mirrors containerd's convention (/run/containerd/s/<hash>).
// Overridable for tests via VESSEL_SHIM_SOCKET_ROOT.
func socketRoot() string {
	if v := os.Getenv("VESSEL_SHIM_SOCKET_ROOT"); v != "" {
		return v
	}
	return "/run/containerd/s"
}

// SocketAddress derives a deterministic, short unix socket path from the
// containerd address, namespace and task id. unix socket paths cap at ~108
// bytes, so the triple is hashed and truncated to 16 hex chars — ample
// uniqueness for per-node task counts while leaving room for deep socket
// roots (containerd's own full-sha256 convention overflows outside
// /run/containerd/s).
func SocketAddress(containerdAddress, namespace, id string) string {
	d := sha256.Sum256([]byte(filepath.Join(containerdAddress, namespace, id)))
	return "unix://" + filepath.Join(socketRoot(), fmt.Sprintf("%x", d[:8]))
}

func socketPath(address string) string { return strings.TrimPrefix(address, "unix://") }

// RunStart implements the `start` subcommand. cwd must be the bundle.
// Returns the address containerd should connect to (the caller prints it
// to stdout).
func RunStart(namespace, id, containerdAddress string) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", err
	}
	bundle, err := os.Getwd()
	if err != nil {
		return "", err
	}

	address := SocketAddress(containerdAddress, namespace, id)
	path := socketPath(address)
	if err := os.MkdirAll(filepath.Dir(path), 0o711); err != nil {
		return "", err
	}
	_ = os.Remove(path) // a dead shim's stale socket must not block restart
	l, err := net.Listen("unix", path)
	if err != nil {
		return "", fmt.Errorf("listen %s: %w", address, err)
	}
	ul := l.(*net.UnixListener)
	// The daemon owns the socket's lifecycle (it serves on the inherited
	// fd and RunDelete unlinks the path). The parent must NOT unlink on
	// close, or it would delete the path out from under the daemon.
	ul.SetUnlinkOnClose(false)
	f, err := ul.File()
	if err != nil {
		l.Close()
		return "", err
	}
	// The parent's listener copy closes with this process; the daemon owns
	// the dup'd fd.
	defer l.Close()
	defer f.Close()

	cmd := exec.Command(self, "-namespace", namespace, "-id", id, "-address", containerdAddress)
	cmd.Dir = bundle
	cmd.Env = os.Environ() // TTRPC_ADDRESS etc. propagate to the daemon
	cmd.ExtraFiles = []*os.File{f}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // survive containerd's process-group signals
	logf, err := os.OpenFile(filepath.Join(bundle, "shim.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}
	defer logf.Close()
	cmd.Stdout, cmd.Stderr = logf, logf

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("spawn shim daemon: %w", err)
	}
	// The daemon is intentionally not reaped here: start exits immediately
	// and the daemon reparents to init.

	if err := writeFileAtomic(filepath.Join(bundle, "address"), address); err != nil {
		_ = cmd.Process.Kill()
		return "", err
	}
	if err := writeFileAtomic(filepath.Join(bundle, "shim.pid"), strconv.Itoa(cmd.Process.Pid)); err != nil {
		_ = cmd.Process.Kill()
		return "", err
	}
	return address, nil
}

// RunDaemon implements the daemon (no subcommand): serve the Task API on
// the inherited fd-3 listener until containerd asks for Shutdown.
func RunDaemon(ctx context.Context, namespace string) error {
	f := os.NewFile(socketFD, "shim-listener")
	if f == nil {
		return fmt.Errorf("no listener at fd %d (daemon must be spawned by `start`)", socketFD)
	}
	l, err := net.FileListener(f)
	f.Close()
	if err != nil {
		return fmt.Errorf("fd %d is not a listener: %w", socketFD, err)
	}

	cfg, err := LoadConfig("")
	if err != nil {
		return err
	}
	svc := NewService(newManagerFromConfig(cfg), cfg.DefaultDriver(), cfg.Templates)

	// Events: containerd hands us its ttrpc endpoint via env. Absence is
	// tolerated (standalone/testing) but noted.
	if ttrpcAddr := os.Getenv("TTRPC_ADDRESS"); ttrpcAddr != "" {
		pub, err := NewRemotePublisher(ttrpcAddr, namespace)
		if err != nil {
			fmt.Fprintf(os.Stderr, "vessel-shim: events disabled: %v\n", err)
		} else {
			defer pub.Close()
			svc.SetPublisher(pub)
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	svc.SetShutdown(cancel)

	err = Serve(ctx, l, svc)
	if ctx.Err() != nil {
		return nil // clean Shutdown-triggered exit
	}
	return err
}

// RunDelete implements the `delete` subcommand: containerd invokes it
// (cwd = bundle) to clean up after a dead or unresponsive shim. Kill the
// daemon if it still runs, remove its socket, and return the exit record.
func RunDelete(namespace, id, containerdAddress string) (*taskapi.DeleteResponse, error) {
	bundle, err := os.Getwd()
	if err != nil {
		return nil, err
	}

	if data, err := os.ReadFile(filepath.Join(bundle, "shim.pid")); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 1 {
			// Negative pid: signal the daemon's whole process group so
			// stray cloud-hypervisor children die with it.
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	_ = os.Remove(socketPath(SocketAddress(containerdAddress, namespace, id)))
	_ = os.Remove(filepath.Join(bundle, "address"))
	_ = os.Remove(filepath.Join(bundle, "shim.pid"))

	return &taskapi.DeleteResponse{
		Pid:        uint32(os.Getpid()),
		ExitStatus: 137,
		ExitedAt:   timestamppb.New(time.Now()),
	}, nil
}

// MarshalDeleteResponse encodes the delete result the way containerd reads
// it from stdout.
func MarshalDeleteResponse(r *taskapi.DeleteResponse) ([]byte, error) {
	return proto.Marshal(r)
}

// NewServiceFromConfig builds a Service from node config. Explicit
// templates (the standalone -templates flag) take precedence over the
// config file's registry.
func NewServiceFromConfig(cfg *Config, templates Templates) *Service {
	if templates == nil && cfg.Templates != nil {
		templates = cfg.Templates
	}
	return NewService(newManagerFromConfig(cfg), cfg.DefaultDriver(), templates)
}

func newManagerFromConfig(cfg *Config) *sandbox.Manager {
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(process.New())
	mgr.RegisterDriver(cloudhypervisor.New(cloudhypervisor.Config{
		BinaryPath: cfg.CHBinary,
		KernelPath: cfg.Kernel,
		RootfsPath: cfg.Rootfs,
		PoolSize:   cfg.Pool,
	}))
	return mgr
}

func writeFileAtomic(path, content string) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
