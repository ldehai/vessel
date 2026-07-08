package shim

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"github.com/containerd/ttrpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ldehai/vessel/pkg/sandbox"
)

// TestServeRoundTrip exercises the real transport: the containerd Task
// ttrpc client drives the full lifecycle — including a template restore and
// a fail-fast unknown template — over a unix socket, exactly as containerd
// would.
func TestServeRoundTrip(t *testing.T) {
	d := sandbox.NewFakeDriver()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(d)
	d.Snapshots["/snap/py"] = []byte("tpl")
	svc := NewService(mgr, "fake", MapTemplates{"python-3.12": {Driver: "fake", Path: "/snap/py"}})

	sock := filepath.Join(t.TempDir(), "shim.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = Serve(ctx, l, svc) }()

	conn, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client := ttrpc.NewClient(conn)
	defer client.Close()
	tc := taskapi.NewTaskClient(client)

	// Fresh pod lifecycle.
	bundle := writeBundle(t, nil)
	if _, err := tc.Create(ctx, &taskapi.CreateTaskRequest{ID: "pod", Bundle: bundle}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tc.Start(ctx, &taskapi.StartRequest{ID: "pod"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	st, err := tc.State(ctx, &taskapi.StateRequest{ID: "pod"})
	if err != nil || st.Status != task.Status_RUNNING {
		t.Fatalf("State: %v status=%v", err, st.GetStatus())
	}
	if _, err := tc.Delete(ctx, &taskapi.DeleteRequest{ID: "pod"}); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Template-annotated pod restores over the wire.
	tb := writeBundle(t, map[string]string{TemplateAnnotation: "python-3.12"})
	if _, err := tc.Create(ctx, &taskapi.CreateTaskRequest{ID: "tpl-pod", Bundle: tb}); err != nil {
		t.Fatalf("Create from template: %v", err)
	}

	// Unknown template fails fast across ttrpc with NotFound intact.
	ub := writeBundle(t, map[string]string{TemplateAnnotation: "missing"})
	_, err = tc.Create(ctx, &taskapi.CreateTaskRequest{ID: "bad", Bundle: ub})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown template over ttrpc: %v (code %s), want NotFound", err, status.Code(err))
	}
}
