// containerd-shim-vessel-v1 is the containerd Runtime v2 shim for vessel.
//
// It lets Kubernetes schedule vessel microVMs via a RuntimeClass
// (runtimeClassName: vessel). A pod annotated vessel.dev/template=<id> is
// created through the restore path — restored from a cached template
// snapshot in tens of milliseconds rather than booted from scratch.
//
// containerd invokes this binary three ways (see pkg/shim/bootstrap.go):
//
//	shim -namespace <ns> -id <id> -address <sock> ... start   bootstrap: print daemon address
//	shim -namespace <ns> -id <id> -address <sock> ... delete  cleanup after a dead shim
//	shim -namespace <ns> -id <id> -address <sock>             the daemon itself (fd 3 = listener)
//
// Node configuration (VM assets, template registry) lives in
// /etc/vessel/shim.json — see pkg/shim/config.go and docs/kubernetes.md.
//
// A fourth mode, -standalone, serves the Task API on a fixed socket without
// containerd, for local validation.
package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/ldehai/vessel/pkg/shim"
)

func main() {
	var (
		namespace  = flag.String("namespace", "", "containerd namespace")
		id         = flag.String("id", "", "task id")
		address    = flag.String("address", "", "containerd main socket address")
		_          = flag.String("publish-binary", "", "accepted for containerd compatibility (events go over TTRPC_ADDRESS)")
		_          = flag.Bool("debug", false, "accepted for containerd compatibility")
		standalone = flag.Bool("standalone", false, "serve the Task service on -socket without containerd")
		socket     = flag.String("socket", "/run/vessel/shim.sock", "unix socket (standalone mode)")
		templates  = flag.String("templates", "", "template registry JSON (standalone mode; containerd mode uses /etc/vessel/shim.json)")
	)
	flag.Parse()

	switch flag.Arg(0) {
	case "start":
		addr, err := shim.RunStart(*namespace, *id, *address)
		if err != nil {
			fatal("start", err)
		}
		fmt.Print(addr) // containerd reads the address from stdout
	case "delete":
		resp, err := shim.RunDelete(*namespace, *id, *address)
		if err != nil {
			fatal("delete", err)
		}
		data, err := shim.MarshalDeleteResponse(resp)
		if err != nil {
			fatal("delete", err)
		}
		_, _ = os.Stdout.Write(data)
	case "":
		if *standalone {
			os.Exit(runStandalone(*socket, *templates))
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := shim.RunDaemon(ctx, *namespace); err != nil {
			fatal("daemon", err)
		}
	default:
		fatal("args", fmt.Errorf("unknown subcommand %q (want start, delete, or none)", flag.Arg(0)))
	}
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
	cfg, err := shim.LoadConfig("")
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		return 1
	}
	svc := shim.NewServiceFromConfig(cfg, tmpls)

	_ = os.Remove(socket)
	l, err := net.Listen("unix", socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		return 1
	}
	fmt.Printf("vessel shim: Task service on %s (driver: %s)\n", socket, cfg.DefaultDriver())

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := shim.Serve(ctx, l, svc); err != nil && ctx.Err() == nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		return 1
	}
	return 0
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "containerd-shim-vessel-v1 %s: %v\n", stage, err)
	os.Exit(1)
}
