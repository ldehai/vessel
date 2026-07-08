// vessel-agent runs inside the guest VM as a lightweight init/control
// process. It listens on vsock (production) or a unix socket (dev) and
// executes commands sent by the host's vessel daemon.
//
// Usage:
//
//	vessel-agent                      # vsock port 5000 (inside a VM)
//	vessel-agent -listen unix:///tmp/agent.sock   # dev mode
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/ldehai/vessel/pkg/agent"
	"github.com/ldehai/vessel/pkg/vsock"
)

const defaultVsockPort = 5000

func main() {
	listen := flag.String("listen", "", "unix:///path for dev; empty = vsock port 5000")
	flag.Parse()

	// As PID 1 the kernel-provided environment has no PATH; exec.LookPath
	// would then fail for anything not given as an absolute path.
	if os.Getenv("PATH") == "" {
		_ = os.Setenv("PATH", "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}

	var (
		l   net.Listener
		err error
	)
	if *listen == "" {
		l, err = vsock.Listen(defaultVsockPort)
	} else if path, ok := strings.CutPrefix(*listen, "unix://"); ok {
		_ = os.Remove(path)
		l, err = net.Listen("unix", path)
	} else {
		err = fmt.Errorf("bad -listen %q (want unix://... or empty)", *listen)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "vessel-agent:", err)
		os.Exit(1)
	}

	fmt.Println("vessel-agent listening on", l.Addr())
	if err := agent.NewServer().Serve(l); err != nil {
		fmt.Fprintln(os.Stderr, "vessel-agent:", err)
		os.Exit(1)
	}
}
