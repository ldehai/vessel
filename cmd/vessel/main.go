// vessel: agent-native sandbox runtime.
//
// Usage:
//
//	vessel up                                         zero-to-daemon bootstrap (assets + serve)
//	vessel run [-driver process] -- <cmd> [args...]   run a command in a fresh sandbox
//	vessel serve [-addr :7070]                        start the REST API daemon
//	vessel agent [-listen unix:///path]               guest-side agent (used inside VMs)
//	vessel info                                       list available drivers
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ldehai/vessel/pkg/agent"
	"github.com/ldehai/vessel/pkg/api"
	"github.com/ldehai/vessel/pkg/bootstrap"
	"github.com/ldehai/vessel/pkg/driver/cloudhypervisor"
	"github.com/ldehai/vessel/pkg/driver/process"
	"github.com/ldehai/vessel/pkg/e2b"
	"github.com/ldehai/vessel/pkg/sandbox"
	"github.com/ldehai/vessel/pkg/vsock"
)

// httpHandler mounts vessel's native REST API plus the E2B-compatible
// control-plane routes (/sandboxes) on one mux. E2B SDK clients point their
// API base URL here; native clients keep using /v1/... and /healthz.
func httpHandler(mgr *sandbox.Manager, defaultDriver string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api.NewServer(mgr))
	mux.Handle("/sandboxes", e2b.NewHandler(mgr, defaultDriver))
	mux.Handle("/sandboxes/", e2b.NewHandler(mgr, defaultDriver))
	return mux
}

func newManager(kernel, rootfs string) (*sandbox.Manager, *cloudhypervisor.Driver) {
	poolSize := 2
	if v := os.Getenv("VESSEL_POOL"); v != "" {
		fmt.Sscanf(v, "%d", &poolSize)
	}
	if kernel == "" {
		kernel = os.Getenv("VESSEL_KERNEL")
	}
	if rootfs == "" {
		rootfs = os.Getenv("VESSEL_ROOTFS")
	}
	chDrv := cloudhypervisor.New(cloudhypervisor.Config{
		BinaryPath: os.Getenv("VESSEL_CH_BINARY"),
		KernelPath: kernel,
		RootfsPath: rootfs,
		PoolSize:   poolSize,
	})
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(process.New())
	mgr.RegisterDriver(chDrv)
	return mgr, chDrv
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "up":
		os.Exit(cmdUp(os.Args[2:]))
	case "run":
		mgr, _ := newManager("", "")
		os.Exit(cmdRun(mgr, os.Args[2:]))
	case "serve":
		mgr, chDrv := newManager("", "")
		chDrv.Warm() // prewarm the VMM pool for low-latency create/restore
		os.Exit(cmdServe(mgr, os.Args[2:]))
	case "agent":
		os.Exit(cmdAgent(os.Args[2:]))
	case "info":
		mgr, _ := newManager("", "")
		fmt.Println("drivers:", mgr.Drivers())
	default:
		usage()
		os.Exit(2)
	}
}

// cmdUp: assets + daemon + copy-pasteable first steps. The command a README
// puts on line one.
func cmdUp(args []string) int {
	fs := flag.NewFlagSet("up", flag.ExitOnError)
	addr := fs.String("addr", ":7070", "listen address")
	_ = fs.Parse(args)

	assets, err := bootstrap.Up(func(format string, a ...any) {
		fmt.Printf(format+"\n", a...)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vessel up:", err)
		return 1
	}

	driver := "cloudhypervisor"
	kernel, rootfs := "", ""
	if assets.KVM {
		kernel, rootfs = assets.KernelPath, assets.RootfsPath
		if os.Getenv("VESSEL_CH_BINARY") == "" {
			os.Setenv("VESSEL_CH_BINARY", assets.CHBinary)
		}
	} else {
		driver = "process"
	}
	mgr, chDrv := newManager(kernel, rootfs)
	if assets.KVM {
		chDrv.Warm()
	}

	fmt.Printf(`
vessel is up — API on %s (driver: %s)

Native API:

  curl -X POST localhost%s/v1/sandboxes -d '{"driver":"%s","spec":{}}'

E2B SDK drop-in — point the SDK at this URL, no code changes:

  export E2B_API_URL="http://localhost%s"
  export E2B_API_KEY="local"

`, *addr, driver, *addr, driver, *addr)

	return serveHTTP(*addr, httpHandler(mgr, driver), mgr)
}

// serveHTTP runs the API server until SIGINT/SIGTERM, then stops every
// sandbox so no cloud-hypervisor child outlives the daemon.
func serveHTTP(addr string, h http.Handler, mgr *sandbox.Manager) int {
	srv := &http.Server{Addr: addr, Handler: h}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		return 0
	case <-ctx.Done():
		fmt.Fprintln(os.Stderr, "\nshutting down: stopping sandboxes…")
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		mgr.Shutdown(sctx)
		_ = srv.Shutdown(sctx)
		return 0
	}
}

// cmdAgent runs the guest-side agent (same behavior as the standalone
// vessel-agent binary): vsock listener by default, unix socket for dev.
func cmdAgent(args []string) int {
	fs := flag.NewFlagSet("agent", flag.ExitOnError)
	listen := fs.String("listen", "", "unix:///path for dev; empty = vsock port 5000")
	_ = fs.Parse(args)

	if os.Getenv("PATH") == "" { // PID 1: kernel-provided env has no PATH
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	var (
		l   net.Listener
		err error
	)
	if *listen == "" {
		l, err = vsock.Listen(5000)
	} else if path, ok := strings.CutPrefix(*listen, "unix://"); ok {
		_ = os.Remove(path)
		l, err = net.Listen("unix", path)
	} else {
		err = fmt.Errorf("bad -listen %q (want unix://... or empty)", *listen)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vessel agent:", err)
		return 1
	}
	fmt.Println("vessel agent listening on", l.Addr())
	if err := agent.NewServer().Serve(l); err != nil {
		fmt.Fprintln(os.Stderr, "vessel agent:", err)
		return 1
	}
	return 0
}

func cmdRun(mgr *sandbox.Manager, args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	driver := fs.String("driver", "process", "isolation driver")
	rootfs := fs.String("rootfs", "", "path to root filesystem (optional)")
	_ = fs.Parse(args)
	cmd := fs.Args()
	if len(cmd) == 0 {
		fmt.Fprintln(os.Stderr, "usage: vessel run [-driver d] [-rootfs path] -- <cmd> [args...]")
		return 2
	}

	ctx := context.Background()
	inst, err := mgr.Create(ctx, *driver, &sandbox.Spec{Rootfs: *rootfs, Cmd: cmd})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create:", err)
		return 1
	}
	defer func() { _ = inst.Stop(ctx) }()

	code, err := inst.Exec(ctx, cmd, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "exec:", err)
		return 1
	}
	return code
}

func cmdServe(mgr *sandbox.Manager, args []string) int {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":7070", "listen address")
	_ = fs.Parse(args)

	fmt.Println("vessel API listening on", *addr, "(native /v1 + E2B-compatible /sandboxes)")
	return serveHTTP(*addr, httpHandler(mgr, "cloudhypervisor"), mgr)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vessel <up|run|serve|agent|info> [flags]")
}
