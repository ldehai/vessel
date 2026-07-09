// Package shim implements the containerd Runtime v2 Task service backed by
// vessel's sandbox.Manager, so Kubernetes can schedule vessel microVMs via
// a RuntimeClass (runtimeClassName: vessel).
//
// The differentiator: a pod annotated vessel.dev/template=<id> is created
// through the restore path — restored from a cached template snapshot in
// tens of milliseconds instead of booted from scratch. The annotation is an
// explicit request: naming an unregistered template fails Create rather
// than silently serving a cold boot.
//
// Scope of this slice: the Task RPC surface mapped onto the Manager, served
// over ttrpc (see Serve), driven in tests by the real containerd Task
// client. The containerd shim process-lifecycle handshake (start/delete
// subcommands, event publisher) and real-cluster e2e are the next slice —
// see cmd/containerd-shim-vessel-v1 and docs/kubernetes.md.
package shim

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/ldehai/vessel/pkg/sandbox"
)

// TemplateAnnotation selects a registered template snapshot for a pod.
const TemplateAnnotation = "vessel.dev/template"

// Service implements taskapi.TaskService.
//
// Pid semantics: vessel tasks live inside a microVM, so there is no
// host-visible container init. Like other VM runtimes, the shim reports its
// own pid — the process containerd should associate with the task for
// lifecycle purposes.
type Service struct {
	mgr           *sandbox.Manager
	defaultDriver string
	templates     Templates
	pid           uint32
	publisher     Publisher // nil = no event forwarding (standalone/tests)
	onShutdown    func()    // cancels the serve loop; nil = ignore Shutdown

	mu    sync.Mutex
	tasks map[string]*taskState // containerd task ID -> state
}

// SetPublisher wires containerd event forwarding (TaskCreate/Start/Exit/
// Delete). Call before serving.
func (s *Service) SetPublisher(p Publisher) { s.publisher = p }

// SetShutdown installs the function the Shutdown RPC invokes so containerd
// can terminate an idle shim. Call before serving.
func (s *Service) SetShutdown(fn func()) { s.onShutdown = fn }

type taskState struct {
	inst       sandbox.Instance
	bundle     string
	status     task.Status
	exitStatus uint32
	exitedAt   time.Time
	waiters    []chan struct{}
	execs      map[string]*execState // exec id -> state (see exec.go)
}

func NewService(mgr *sandbox.Manager, defaultDriver string, templates Templates) *Service {
	if templates == nil {
		templates = MapTemplates{}
	}
	return &Service{
		mgr:           mgr,
		defaultDriver: defaultDriver,
		templates:     templates,
		pid:           uint32(os.Getpid()),
		tasks:         map[string]*taskState{},
	}
}

// Create maps a containerd task to a vessel sandbox. A bundle annotated
// with a registered template restores from its snapshot; an annotation
// naming an unknown template is an error; no annotation creates a fresh
// sandbox from the bundle rootfs.
func (s *Service) Create(ctx context.Context, r *taskapi.CreateTaskRequest) (*taskapi.CreateTaskResponse, error) {
	if r.ID == "" {
		return nil, status.Error(codes.InvalidArgument, "task id is required")
	}
	s.mu.Lock()
	if _, exists := s.tasks[r.ID]; exists {
		s.mu.Unlock()
		return nil, status.Errorf(codes.AlreadyExists, "task %q already exists", r.ID)
	}
	s.mu.Unlock()

	var (
		inst sandbox.Instance
		err  error
	)
	if tmplID := bundleAnnotation(r.Bundle, TemplateAnnotation); tmplID != "" {
		driver, path, ok := s.templates.Lookup(tmplID)
		if !ok {
			return nil, status.Errorf(codes.NotFound,
				"annotation %s=%q: template not registered with this shim", TemplateAnnotation, tmplID)
		}
		if driver == "" {
			driver = s.defaultDriver
		}
		inst, err = s.mgr.RestoreFrom(ctx, driver, path)
	} else {
		spec := &sandbox.Spec{Name: r.ID, Rootfs: filepath.Join(r.Bundle, "rootfs")}
		inst, err = s.mgr.Create(ctx, s.defaultDriver, spec)
	}
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	// Re-check: a concurrent Create for the same ID may have won the race.
	if _, exists := s.tasks[r.ID]; exists {
		s.mu.Unlock()
		_ = inst.Stop(ctx)
		return nil, status.Errorf(codes.AlreadyExists, "task %q already exists", r.ID)
	}
	s.tasks[r.ID] = &taskState{
		inst:   inst,
		bundle: r.Bundle,
		status: task.Status_CREATED,
		execs:  map[string]*execState{},
	}
	s.mu.Unlock()

	s.publishWarn(TopicTaskCreate, &eventstypes.TaskCreate{
		ContainerID: r.ID, Bundle: r.Bundle, Pid: s.pid,
	})
	return &taskapi.CreateTaskResponse{Pid: s.pid}, nil
}

func (s *Service) Start(_ context.Context, r *taskapi.StartRequest) (*taskapi.StartResponse, error) {
	if r.ExecID != "" {
		return s.startExec(r.ID, r.ExecID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[r.ID]
	if !ok {
		return nil, errNotFound(r.ID)
	}
	if ts.status != task.Status_CREATED {
		return nil, status.Errorf(codes.FailedPrecondition, "task %q is %s, not created", r.ID, ts.status)
	}
	ts.status = task.Status_RUNNING
	s.publishWarn(TopicTaskStart, &eventstypes.TaskStart{ContainerID: r.ID, Pid: s.pid})
	return &taskapi.StartResponse{Pid: s.pid}, nil
}

func (s *Service) State(_ context.Context, r *taskapi.StateRequest) (*taskapi.StateResponse, error) {
	if r.ExecID != "" {
		return s.execState(r.ID, r.ExecID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[r.ID]
	if !ok {
		return nil, errNotFound(r.ID)
	}
	return &taskapi.StateResponse{
		ID:         r.ID,
		Bundle:     ts.bundle,
		Pid:        s.pid,
		Status:     ts.status,
		ExitStatus: ts.exitStatus,
		ExitedAt:   tspb(ts.exitedAt),
	}, nil
}

// Kill stops the sandbox. vessel cannot yet deliver signals inside the
// guest (that lands with exec support), so any signal tears the sandbox
// down; the reported exit status is the conventional 128+signal so callers
// can still distinguish SIGTERM (143) from SIGKILL (137).
func (s *Service) Kill(ctx context.Context, r *taskapi.KillRequest) (*emptypb.Empty, error) {
	if r.ExecID != "" {
		if err := s.killExec(r.ID, r.ExecID); err != nil {
			return nil, err
		}
		return &emptypb.Empty{}, nil
	}
	s.mu.Lock()
	ts, ok := s.tasks[r.ID]
	s.mu.Unlock()
	if !ok {
		return nil, errNotFound(r.ID)
	}
	_ = ts.inst.Stop(ctx)
	sig := r.Signal
	if sig == 0 {
		sig = 9 // SIGKILL by convention when unspecified
	}
	s.markExited(r.ID, ts, 128+sig)
	return &emptypb.Empty{}, nil
}

// Delete tears the sandbox down and reports its exit record. The task
// stays visible (STOPPED) to concurrent State calls until teardown is
// complete, then is removed.
func (s *Service) Delete(ctx context.Context, r *taskapi.DeleteRequest) (*taskapi.DeleteResponse, error) {
	if r.ExecID != "" {
		return s.deleteExec(r.ID, r.ExecID)
	}
	s.mu.Lock()
	ts, ok := s.tasks[r.ID]
	s.mu.Unlock()
	if !ok {
		return nil, errNotFound(r.ID)
	}

	_ = ts.inst.Stop(ctx)
	s.markExited(r.ID, ts, 0) // no-op if Kill already recorded an exit

	s.mu.Lock()
	delete(s.tasks, r.ID)
	exitStatus, exitedAt := ts.exitStatus, ts.exitedAt
	s.mu.Unlock()

	s.publishWarn(TopicTaskDelete, &eventstypes.TaskDelete{
		ContainerID: r.ID, Pid: s.pid, ExitStatus: exitStatus, ExitedAt: tspb(exitedAt),
	})
	return &taskapi.DeleteResponse{Pid: s.pid, ExitStatus: exitStatus, ExitedAt: tspb(exitedAt)}, nil
}

// Wait blocks until the task exits, then returns its exit record.
func (s *Service) Wait(ctx context.Context, r *taskapi.WaitRequest) (*taskapi.WaitResponse, error) {
	if r.ExecID != "" {
		return s.waitExec(ctx, r.ID, r.ExecID)
	}
	s.mu.Lock()
	ts, ok := s.tasks[r.ID]
	if !ok {
		s.mu.Unlock()
		return nil, errNotFound(r.ID)
	}
	if ts.status == task.Status_STOPPED {
		defer s.mu.Unlock()
		return &taskapi.WaitResponse{ExitStatus: ts.exitStatus, ExitedAt: tspb(ts.exitedAt)}, nil
	}
	ch := make(chan struct{})
	ts.waiters = append(ts.waiters, ch)
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &taskapi.WaitResponse{ExitStatus: ts.exitStatus, ExitedAt: tspb(ts.exitedAt)}, nil
}

// markExited transitions to STOPPED exactly once, wakes waiters and
// publishes TaskExit — the event kubelet depends on to notice pod death.
func (s *Service) markExited(id string, ts *taskState, code uint32) {
	s.mu.Lock()
	if ts.status == task.Status_STOPPED {
		s.mu.Unlock()
		return
	}
	ts.status = task.Status_STOPPED
	ts.exitStatus = code
	ts.exitedAt = time.Now()
	exitedAt := ts.exitedAt
	for _, ch := range ts.waiters {
		close(ch)
	}
	ts.waiters = nil
	s.mu.Unlock()

	if err := s.publishTaskExit(id, code, exitedAt); err != nil {
		fmt.Fprintf(os.Stderr, "vessel-shim: publish TaskExit for %s: %v\n", id, err)
	}
}

// publishWarn emits best-effort: event failures must not fail the RPC that
// triggered them, but they are logged (stderr = the shim log).
func (s *Service) publishWarn(topic string, event proto.Message) {
	if err := s.publish(topic, event); err != nil {
		fmt.Fprintf(os.Stderr, "vessel-shim: publish %s: %v\n", topic, err)
	}
}

func (s *Service) Pids(_ context.Context, r *taskapi.PidsRequest) (*taskapi.PidsResponse, error) {
	s.mu.Lock()
	_, ok := s.tasks[r.ID]
	s.mu.Unlock()
	if !ok {
		return nil, errNotFound(r.ID)
	}
	return &taskapi.PidsResponse{Processes: []*task.ProcessInfo{{Pid: s.pid}}}, nil
}

// Connect reports the shim and task pids for containerd's bookkeeping.
func (s *Service) Connect(_ context.Context, _ *taskapi.ConnectRequest) (*taskapi.ConnectResponse, error) {
	return &taskapi.ConnectResponse{ShimPid: s.pid, TaskPid: s.pid}, nil
}

// Shutdown lets containerd terminate an idle shim. Only honored when no
// tasks remain, matching containerd's expectation that a shim with live
// tasks refuses to die under it.
func (s *Service) Shutdown(_ context.Context, _ *taskapi.ShutdownRequest) (*emptypb.Empty, error) {
	s.mu.Lock()
	idle := len(s.tasks) == 0
	s.mu.Unlock()
	if idle && s.onShutdown != nil {
		s.onShutdown()
	}
	return &emptypb.Empty{}, nil
}

func (s *Service) Stats(_ context.Context, r *taskapi.StatsRequest) (*taskapi.StatsResponse, error) {
	s.mu.Lock()
	_, ok := s.tasks[r.ID]
	s.mu.Unlock()
	if !ok {
		return nil, errNotFound(r.ID)
	}
	return &taskapi.StatsResponse{}, nil
}

// --- honestly unimplemented ---
//
// These return codes.Unimplemented (the canonical answer for optional
// capabilities a runtime lacks) instead of silently succeeding. Exec is
// implemented (non-interactive) in exec.go; pty/IO plumbing lands with
// streaming support; Pause/Resume will map onto CH vm.pause/vm.resume;
// Checkpoint onto vessel snapshots.

func (s *Service) ResizePty(context.Context, *taskapi.ResizePtyRequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("ResizePty")
}
func (s *Service) CloseIO(context.Context, *taskapi.CloseIORequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("CloseIO")
}
func (s *Service) Pause(context.Context, *taskapi.PauseRequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("Pause")
}
func (s *Service) Resume(context.Context, *taskapi.ResumeRequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("Resume")
}
func (s *Service) Checkpoint(context.Context, *taskapi.CheckpointTaskRequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("Checkpoint")
}
func (s *Service) Update(context.Context, *taskapi.UpdateTaskRequest) (*emptypb.Empty, error) {
	return nil, errUnimplemented("Update")
}

// --- helpers ---

func errNotFound(id string) error {
	return status.Errorf(codes.NotFound, "task %q not found", id)
}

func errUnimplemented(op string) error {
	return status.Errorf(codes.Unimplemented, "%s is not supported by the vessel shim yet", op)
}

func tspb(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// bundleAnnotation reads .annotations[key] from an OCI bundle's config.json.
func bundleAnnotation(bundle, key string) string {
	if bundle == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(bundle, "config.json"))
	if err != nil {
		return ""
	}
	var spec struct {
		Annotations map[string]string `json:"annotations"`
	}
	if json.Unmarshal(data, &spec) != nil {
		return ""
	}
	return spec.Annotations[key]
}
