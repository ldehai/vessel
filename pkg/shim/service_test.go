package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ldehai/vessel/pkg/sandbox"
)

func newTestService(t *testing.T, templates Templates) (*Service, *sandbox.FakeDriver) {
	t.Helper()
	d := sandbox.NewFakeDriver()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(d)
	return NewService(mgr, "fake", templates), d
}

// writeBundle creates an OCI bundle dir whose config.json carries the given
// annotations.
func writeBundle(t *testing.T, annotations map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	writeBundleConfig(t, dir, annotations)
	return dir
}

// writeBundleConfig writes an OCI config.json into an existing bundle dir.
func writeBundleConfig(t *testing.T, dir string, annotations map[string]string) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"annotations": annotations})
	if err := os.WriteFile(filepath.Join(dir, "config.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func wantCode(t *testing.T, err error, want codes.Code) {
	t.Helper()
	if status.Code(err) != want {
		t.Fatalf("error = %v (code %s), want code %s", err, status.Code(err), want)
	}
}

func TestLifecycle(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)
	shimPid := uint32(os.Getpid())

	cr, err := s.Create(ctx, &taskapi.CreateTaskRequest{ID: "pod1", Bundle: bundle})
	if err != nil {
		t.Fatal(err)
	}
	if cr.Pid != shimPid {
		t.Fatalf("create pid = %d, want shim pid %d", cr.Pid, shimPid)
	}

	if _, err := s.Start(ctx, &taskapi.StartRequest{ID: "pod1"}); err != nil {
		t.Fatal(err)
	}
	// Start is not idempotent: a second Start must fail.
	_, err = s.Start(ctx, &taskapi.StartRequest{ID: "pod1"})
	wantCode(t, err, codes.FailedPrecondition)

	st, err := s.State(ctx, &taskapi.StateRequest{ID: "pod1"})
	if err != nil || st.Status != task.Status_RUNNING || st.Pid != shimPid || st.Bundle != bundle {
		t.Fatalf("state = %+v err=%v", st, err)
	}

	conn, err := s.Connect(ctx, &taskapi.ConnectRequest{ID: "pod1"})
	if err != nil || conn.ShimPid != shimPid || conn.TaskPid != shimPid {
		t.Fatalf("connect = %+v err=%v", conn, err)
	}

	dr, err := s.Delete(ctx, &taskapi.DeleteRequest{ID: "pod1"})
	if err != nil || dr.ExitedAt == nil || dr.ExitStatus != 0 {
		t.Fatalf("delete = %+v err=%v", dr, err)
	}
	_, err = s.State(ctx, &taskapi.StateRequest{ID: "pod1"})
	wantCode(t, err, codes.NotFound)
}

func TestCreateValidation(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)

	_, err := s.Create(ctx, &taskapi.CreateTaskRequest{ID: "", Bundle: bundle})
	wantCode(t, err, codes.InvalidArgument)

	if _, err := s.Create(ctx, &taskapi.CreateTaskRequest{ID: "dup", Bundle: bundle}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Create(ctx, &taskapi.CreateTaskRequest{ID: "dup", Bundle: bundle})
	wantCode(t, err, codes.AlreadyExists)
}

func TestTemplateRestore(t *testing.T) {
	d := sandbox.NewFakeDriver()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(d)
	d.Snapshots["/snap/py"] = []byte("tpl")
	s := NewService(mgr, "fake", MapTemplates{"python-3.12": {Driver: "fake", Path: "/snap/py"}})

	bundle := writeBundle(t, map[string]string{TemplateAnnotation: "python-3.12"})
	if _, err := s.Create(context.Background(), &taskapi.CreateTaskRequest{ID: "p", Bundle: bundle}); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	inst := s.tasks["p"].inst.(*sandbox.FakeInstance)
	s.mu.Unlock()
	if inst.RestoredFrom() != "/snap/py" {
		t.Fatalf("expected restore from template snapshot, got %q", inst.RestoredFrom())
	}
}

// An annotation naming an unregistered template is an explicit request the
// shim cannot honor: fail fast, never silently cold-boot.
func TestUnknownTemplateFailsCreate(t *testing.T) {
	s, _ := newTestService(t, nil)
	bundle := writeBundle(t, map[string]string{TemplateAnnotation: "missing"})
	_, err := s.Create(context.Background(), &taskapi.CreateTaskRequest{ID: "p", Bundle: bundle})
	wantCode(t, err, codes.NotFound)
}

func TestKillSignalSemantics(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()

	mk := func(id string) {
		t.Helper()
		bundle := writeBundle(t, nil)
		if _, err := s.Create(ctx, &taskapi.CreateTaskRequest{ID: id, Bundle: bundle}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Start(ctx, &taskapi.StartRequest{ID: id}); err != nil {
			t.Fatal(err)
		}
	}

	// SIGTERM -> 143, SIGKILL -> 137, unspecified -> 137.
	for _, tc := range []struct {
		id     string
		signal uint32
		want   uint32
	}{{"term", 15, 143}, {"kill", 9, 137}, {"default", 0, 137}} {
		mk(tc.id)
		if _, err := s.Kill(ctx, &taskapi.KillRequest{ID: tc.id, Signal: tc.signal}); err != nil {
			t.Fatal(err)
		}
		st, _ := s.State(ctx, &taskapi.StateRequest{ID: tc.id})
		if st.Status != task.Status_STOPPED || st.ExitStatus != tc.want {
			t.Fatalf("%s: status=%v exit=%d, want STOPPED/%d", tc.id, st.Status, st.ExitStatus, tc.want)
		}
	}

	// Delete after Kill must preserve the kill's exit record.
	dr, err := s.Delete(ctx, &taskapi.DeleteRequest{ID: "term"})
	if err != nil || dr.ExitStatus != 143 {
		t.Fatalf("delete after kill: exit=%d err=%v, want 143", dr.GetExitStatus(), err)
	}
}

func TestExecIDRejected(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)
	_, _ = s.Create(ctx, &taskapi.CreateTaskRequest{ID: "p", Bundle: bundle})

	_, err := s.Kill(ctx, &taskapi.KillRequest{ID: "p", ExecID: "e1"})
	wantCode(t, err, codes.NotFound)
	_, err = s.Start(ctx, &taskapi.StartRequest{ID: "p", ExecID: "e1"})
	wantCode(t, err, codes.NotFound)
	_, err = s.Wait(ctx, &taskapi.WaitRequest{ID: "p", ExecID: "e1"})
	wantCode(t, err, codes.NotFound)
}

func TestUnimplementedAreExplicit(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	_, err := s.Exec(ctx, &taskapi.ExecProcessRequest{ID: "p"})
	wantCode(t, err, codes.Unimplemented)
	_, err = s.Pause(ctx, &taskapi.PauseRequest{ID: "p"})
	wantCode(t, err, codes.Unimplemented)
}

func TestWaitUnblocksOnKill(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)
	_, _ = s.Create(ctx, &taskapi.CreateTaskRequest{ID: "w", Bundle: bundle})
	_, _ = s.Start(ctx, &taskapi.StartRequest{ID: "w"})

	done := make(chan *taskapi.WaitResponse, 1)
	go func() {
		resp, _ := s.Wait(ctx, &taskapi.WaitRequest{ID: "w"})
		done <- resp
	}()
	time.Sleep(20 * time.Millisecond) // let Wait register its waiter

	if _, err := s.Kill(ctx, &taskapi.KillRequest{ID: "w", Signal: 15}); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-done:
		if resp.ExitStatus != 143 {
			t.Fatalf("exit = %d, want 143", resp.ExitStatus)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not unblock after Kill")
	}

	// Wait on an already-stopped task returns immediately.
	resp, err := s.Wait(ctx, &taskapi.WaitRequest{ID: "w"})
	if err != nil || resp.ExitStatus != 143 {
		t.Fatalf("wait after stop: %+v err=%v", resp, err)
	}
}
