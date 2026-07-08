package shim

import (
	"context"
	"net"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/ttrpc"
)

// Serve registers the Task service on a ttrpc server and serves it on l
// until the listener closes or ctx is cancelled. This is the transport
// layer containerd's shim protocol runs over.
//
// The containerd shim binary lifecycle (the start/delete subcommand
// handshake that hands containerd a socket address, plus the event
// publisher) is intentionally not wired here — that is the shim binary's
// job and the subject of real-cluster e2e. Keeping Serve transport-only
// makes the RPC mapping testable with the real containerd Task client
// over a plain socket.
func Serve(ctx context.Context, l net.Listener, svc taskapi.TaskService) error {
	srv, err := ttrpc.NewServer()
	if err != nil {
		return err
	}
	taskapi.RegisterTaskService(srv, svc)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx, l) }()

	select {
	case <-ctx.Done():
		_ = srv.Close()
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}
