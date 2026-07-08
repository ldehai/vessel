// vessel: agent-native sandbox runtime (M1 skeleton).
//
// Usage:
//
//	vessel run [-driver process] -- <cmd> [args...]   run a command in a fresh sandbox
//	vessel serve [-addr :7070]                        start the REST API daemon
//	vessel info                                       list available drivers
package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/ldehai/vessel/pkg/api"
	"github.com/ldehai/vessel/pkg/driver/cloudhypervisor"
	"github.com/ldehai/vessel/pkg/driver/process"
	"github.com/ldehai/vessel/pkg/sandbox"
)

func main() {
	poolSize := 2
	if v := os.Getenv("VESSEL_POOL"); v != "" {
		fmt.Sscanf(v, "%d", &poolSize)
	}
	chDrv := cloudhypervisor.New(cloudhypervisor.Config{
		KernelPath: os.Getenv("VESSEL_KERNEL"),
		RootfsPath: os.Getenv("VESSEL_ROOTFS"),
		PoolSize:   poolSize,
	})
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(process.New())
	mgr.RegisterDriver(chDrv)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	switch os.Args[1] {
	case "run":
		os.Exit(cmdRun(mgr, os.Args[2:]))
	case "serve":
		chDrv.Warm() // prewarm the VMM pool for low-latency create/restore
		os.Exit(cmdServe(mgr, os.Args[2:]))
	case "info":
		fmt.Println("drivers:", mgr.Drivers())
	default:
		usage()
		os.Exit(2)
	}
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

	fmt.Println("vessel API listening on", *addr)
	if err := http.ListenAndServe(*addr, api.NewServer(mgr)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: vessel <run|serve|info> [flags]")
}
