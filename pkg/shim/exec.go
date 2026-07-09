package shim

// Exec-process support: `ctr task exec` / `kubectl exec` land here. The
// guest side has existed since the vsock agent (M2); this file wires
// containerd's exec semantics onto it.
//
// Scope (v1, non-interactive): args from the OCI process spec are executed
// in the sandbox via Instance.Exec; buffered stdout/stderr are written to
// the FIFOs containerd provides when the process finishes. Explicitly NOT
// supported yet, and rejected rather than faked: Terminal (needs streaming
// pty plumbing), stdin, per-exec env/cwd (Instance.Exec does not carry
// them), and signalling a running exec (agent protocol has no kill op).

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"
)

type execState struct {
	args           []string
	stdout, stderr string // FIFO paths from containerd ("" = discard)
	status         task.Status
	exitStatus     uint32
	exitedAt       time.Time
	waiters        []chan struct{}
}

// processSpec is the subset of the OCI runtime-spec Process we honor.
type processSpec struct {
	Terminal bool     `json:"terminal"`
	Args     []string `json:"args"`
	Env      []string `json:"env"`
	Cwd      string   `json:"cwd"`
}

// Exec registers an exec process on a task (containerd calls Start with
// the same ExecID afterwards to actually run it).
func (s *Service) Exec(_ context.Context, r *taskapi.ExecProcessRequest) (*emptypb.Empty, error) {
	if r.ExecID == "" {
		return nil, status.Error(codes.InvalidArgument, "exec id is required")
	}
	if r.Spec == nil {
		return nil, status.Error(codes.InvalidArgument, "process spec is required")
	}
	var spec processSpec
	if err := json.Unmarshal(r.Spec.GetValue(), &spec); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse process spec: %v", err)
	}
	if r.Terminal || spec.Terminal {
		return nil, status.Error(codes.Unimplemented,
			"terminal exec is not supported yet (needs streaming pty plumbing)")
	}
	if len(spec.Args) == 0 {
		return nil, status.Error(codes.InvalidArgument, "process spec has no args")
	}
	if r.Stdin != "" {
		return nil, status.Error(codes.Unimplemented, "exec stdin is not supported yet")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[r.ID]
	if !ok {
		return nil, errNotFound(r.ID)
	}
	if ts.status != task.Status_RUNNING {
		return nil, status.Errorf(codes.FailedPrecondition, "task %q is %s, not running", r.ID, ts.status)
	}
	if _, exists := ts.execs[r.ExecID]; exists {
		return nil, status.Errorf(codes.AlreadyExists, "exec %q already exists", r.ExecID)
	}
	ts.execs[r.ExecID] = &execState{
		args:   spec.Args,
		stdout: r.Stdout,
		stderr: r.Stderr,
		status: task.Status_CREATED,
	}
	return &emptypb.Empty{}, nil
}

// startExec transitions a registered exec to RUNNING and executes it in
// the sandbox asynchronously (containerd expects Start to return fast).
func (s *Service) startExec(taskID, execID string) (*taskapi.StartResponse, error) {
	s.mu.Lock()
	ts, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return nil, errNotFound(taskID)
	}
	es, ok := ts.execs[execID]
	if !ok {
		s.mu.Unlock()
		return nil, errExecNotFound(execID)
	}
	if st := es.status; st != task.Status_CREATED {
		s.mu.Unlock()
		return nil, status.Errorf(codes.FailedPrecondition, "exec %q is %s, not created", execID, st)
	}
	es.status = task.Status_RUNNING
	// Snapshot everything the goroutine needs while still under the lock:
	// after Unlock these fields may be read concurrently with
	// markExecExited's writes.
	inst := ts.inst
	args, stdoutPath, stderrPath := es.args, es.stdout, es.stderr
	s.mu.Unlock()

	go func() {
		stdout, stderr := openOutput(stdoutPath), openOutput(stderrPath)
		defer closeOutput(stdout)
		defer closeOutput(stderr)

		code, err := inst.Exec(context.Background(), args, stdout, stderr)
		if err != nil {
			fmt.Fprintf(stderr, "vessel exec: %v\n", err)
			code = 255
		}
		s.markExecExited(taskID, execID, es, uint32(code))
	}()
	return &taskapi.StartResponse{Pid: s.pid}, nil
}

func (s *Service) execState(taskID, execID string) (*taskapi.StateResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[taskID]
	if !ok {
		return nil, errNotFound(taskID)
	}
	es, ok := ts.execs[execID]
	if !ok {
		return nil, errExecNotFound(execID)
	}
	return &taskapi.StateResponse{
		ID:         execID,
		Bundle:     ts.bundle,
		Pid:        s.pid,
		Status:     es.status,
		Stdout:     es.stdout,
		Stderr:     es.stderr,
		ExitStatus: es.exitStatus,
		ExitedAt:   tspb(es.exitedAt),
		ExecID:     execID,
	}, nil
}

func (s *Service) waitExec(ctx context.Context, taskID, execID string) (*taskapi.WaitResponse, error) {
	s.mu.Lock()
	ts, ok := s.tasks[taskID]
	if !ok {
		s.mu.Unlock()
		return nil, errNotFound(taskID)
	}
	es, ok := ts.execs[execID]
	if !ok {
		s.mu.Unlock()
		return nil, errExecNotFound(execID)
	}
	if es.status == task.Status_STOPPED {
		defer s.mu.Unlock()
		return &taskapi.WaitResponse{ExitStatus: es.exitStatus, ExitedAt: tspb(es.exitedAt)}, nil
	}
	ch := make(chan struct{})
	es.waiters = append(es.waiters, ch)
	s.mu.Unlock()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-ch:
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return &taskapi.WaitResponse{ExitStatus: es.exitStatus, ExitedAt: tspb(es.exitedAt)}, nil
}

// deleteExec reaps a finished exec record and returns its exit record.
func (s *Service) deleteExec(taskID, execID string) (*taskapi.DeleteResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[taskID]
	if !ok {
		return nil, errNotFound(taskID)
	}
	es, ok := ts.execs[execID]
	if !ok {
		return nil, errExecNotFound(execID)
	}
	if es.status == task.Status_RUNNING {
		return nil, status.Errorf(codes.FailedPrecondition, "exec %q is still running", execID)
	}
	delete(ts.execs, execID)
	return &taskapi.DeleteResponse{Pid: s.pid, ExitStatus: es.exitStatus, ExitedAt: tspb(es.exitedAt)}, nil
}

// killExec: the agent protocol cannot signal an in-guest process yet, so
// this is an honest Unimplemented for known execs (NotFound for unknown).
func (s *Service) killExec(taskID, execID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ts, ok := s.tasks[taskID]
	if !ok {
		return errNotFound(taskID)
	}
	if _, ok := ts.execs[execID]; !ok {
		return errExecNotFound(execID)
	}
	return status.Error(codes.Unimplemented,
		"signalling an exec process inside the guest is not supported yet")
}

// markExecExited transitions an exec to STOPPED exactly once, wakes
// waiters and publishes TaskExit with the exec id (how containerd
// distinguishes exec exits from the task's own).
func (s *Service) markExecExited(taskID, execID string, es *execState, code uint32) {
	s.mu.Lock()
	if es.status == task.Status_STOPPED {
		s.mu.Unlock()
		return
	}
	es.status = task.Status_STOPPED
	es.exitStatus = code
	es.exitedAt = time.Now()
	exitedAt := es.exitedAt
	for _, ch := range es.waiters {
		close(ch)
	}
	es.waiters = nil
	s.mu.Unlock()

	if err := s.publish(TopicTaskExit, &eventstypes.TaskExit{
		ContainerID: taskID,
		ID:          execID,
		Pid:         s.pid,
		ExitStatus:  code,
		ExitedAt:    tspb(exitedAt),
	}); err != nil {
		fmt.Fprintf(os.Stderr, "vessel-shim: publish TaskExit for %s/%s: %v\n", taskID, execID, err)
	}
}

func errExecNotFound(execID string) error {
	return status.Errorf(codes.NotFound, "exec %q not found", execID)
}

// openOutput opens a containerd-provided output path (a FIFO in
// production, a regular file in tests). Empty path or open failure
// degrades to discard — output plumbing must not break the exec itself.
func openOutput(path string) io.WriteCloser {
	if path == "" {
		return nopWriteCloser{io.Discard}
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(os.Stderr, "vessel-shim: open exec output %s: %v\n", path, err)
		return nopWriteCloser{io.Discard}
	}
	return f
}

func closeOutput(w io.WriteCloser) { _ = w.Close() }

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
