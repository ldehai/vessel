// containerd-shim-vessel-v1 is the containerd Runtime v2 shim for vessel.
//
// It lets Kubernetes schedule vessel microVMs via a RuntimeClass
// (runtimeClassName: vessel). A pod annotated vessel.dev/template=<id> is
// created through the restore path — restored from a cached template
// snapshot in tens of milliseconds rather than booted from scratch.
//
// Status: this build serves the Task RPC surface over ttrpc. The containerd
// process-lifecycle handshake (the start/delete subcommands that hand
// containerd a socket address, plus the event publisher) is the next slice;
// invoking the shim the containerd way fails loudly rather than pretending.
// For local validation run -standalone with a template registry:
//
//	containerd-shim-vessel-v1 -standalone \
//	    -socket /run/vessel/shim.sock \
//	    -templates /etc/vessel/templates.json
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/ldehai/vessel/pkg/driver/cloudhypervisor"
	"github.com/ldehai/vessel/pkg/driver/process"
	"github.com/ldehai/vessel/pkg/sandbox"
	"github.com/ldehai/vessel/pkg/shim"
)

func main() {
	var (
		standalone = flag.Bool("standalone", false, "serve the Task service on -socket (local validation, not the containerd handshake)")
		socket     = flag.String("socket", "/run/vessel/shim.sock", "unix socket to serve on (standalone mode)")
		templates  = flag.String("templates", "", "JSON template registry: id -> {driver, path} (see docs/kubernetes.md)")
		id         = flag.String("id", "", "task id (containerd invocation)")
		address    = flag.String("address", "", "containerd address (containerd invocation)")
	)
	flag.Parse()

	if !*standalone {
		fmt.Fprintln(os.Stderr, "containerd-shim-vessel-v1: the containerd handshake is not implemented in this build")
		fmt.Fprintln(os.Stderr, "  run with -standalone to serve the Task service directly (see docs/kubernetes.md)")
		_, _ = id, address
		os.Exit(1)
	}
	os.Exit(runStandalone(*socket, *templates))
}

func runStandalone(socket, templatesPath string) int {
	var tmpls shim.Templates
	if templatesPath != "" {
		m, err := shim.LoadTemplates(templatesPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "templates:", err)
			return 1
		}
		tmpls = m
		fmt.Printf("loaded %d template(s) from %s\n", len(m), templatesPath)
	}

	mgr := sandbox.NewManager()
	mgr.RegisterDriver(process.New())
	mgr.RegisterDriver(cloudhypervisor.New(cloudhypervisor.Config{
		BinaryPath: os.Getenv("VESSEL_CH_BINARY"),
		KernelPath: os.Getenv("VESSEL_KERNEL"),
		RootfsPath: os.Getenv("VESSEL_ROOTFS"),
	}))

	defaultDriver := "cloudhypervisor"
	if os.Getenv("VESSEL_KERNEL") == "" {
		defaultDriver = "process" // no VM assets: stay usable for API validation
	}
	svc := shim.NewService(mgr, defaultDriver, tmpls)

	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	fmt.Printf("vessel shim: Task service on %s (driver: %s)\n", socket, defaultDriver)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := shim.Serve(ctx, l, svc); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	return 0
}
