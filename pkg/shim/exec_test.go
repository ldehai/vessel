package shim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	"github.com/containerd/containerd/api/types/task"
	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
)

// capturePublisher records full event payloads (recordingPublisher in
// events_test.go only records topics).
type capturePublisher struct {
	mu     sync.Mutex
	events []struct {
		topic string
		msg   proto.Message
	}
}

func (c *capturePublisher) Publish(_ context.Context, topic string, msg proto.Message) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, struct {
		topic string
		msg   proto.Message
	}{topic, msg})
	return nil
}

func (c *capturePublisher) topics() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.events))
	for _, e := range c.events {
		out = append(out, e.topic)
	}
	return out
}

func (c *capturePublisher) sawExecExit(taskID, execID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.events {
		if e.topic != TopicTaskExit {
			continue
		}
		if exit, ok := e.msg.(*eventstypes.TaskExit); ok &&
			exit.ContainerID == taskID && exit.ID == execID {
			return true
		}
	}
	return false
}

// procSpec wraps an OCI process spec the way containerd delivers it: an
// Any whose Value is the raw JSON.
func procSpec(t *testing.T, args []string, terminal bool) *anypb.Any {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"args": args, "terminal": terminal})
	return &anypb.Any{TypeUrl: "types.containerd.io/opencontainers/runtime-spec/1/Process", Value: raw}
}

// runningTask creates and starts a task, returning the service.
func runningTask(t *testing.T, id string) *Service {
	t.Helper()
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)
	if _, err := s.Create(ctx, &taskapi.CreateTaskRequest{ID: id, Bundle: bundle}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Start(ctx, &taskapi.StartRequest{ID: id}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestExecFullFlow(t *testing.T) {
	s := runningTask(t, "pod")
	ctx := context.Background()
	stdout := filepath.Join(t.TempDir(), "stdout")

	// Register the exec.
	if _, err := s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "e1",
		Spec:   procSpec(t, []string{"echo", "hi"}, false),
		Stdout: stdout,
	}); err != nil {
		t.Fatal(err)
	}
	st, err := s.State(ctx, &taskapi.StateRequest{ID: "pod", ExecID: "e1"})
	if err != nil || st.Status != task.Status_CREATED || st.ExecID != "e1" {
		t.Fatalf("state after Exec = %+v err=%v", st, err)
	}

	// Start it; Wait must observe the exit.
	if _, err := s.Start(ctx, &taskapi.StartRequest{ID: "pod", ExecID: "e1"}); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	wr, err := s.Wait(wctx, &taskapi.WaitRequest{ID: "pod", ExecID: "e1"})
	if err != nil || wr.ExitStatus != 0 || wr.ExitedAt == nil {
		t.Fatalf("wait = %+v err=%v", wr, err)
	}

	// FakeInstance.Exec writes "fake-out" to stdout: it must land in the file.
	data, err := os.ReadFile(stdout)
	if err != nil || string(data) != "fake-out" {
		t.Fatalf("stdout file = %q err=%v", data, err)
	}

	// Reap it; a second delete is NotFound.
	dr, err := s.Delete(ctx, &taskapi.DeleteRequest{ID: "pod", ExecID: "e1"})
	if err != nil || dr.ExitedAt == nil {
		t.Fatalf("delete exec = %+v err=%v", dr, err)
	}
	_, err = s.Delete(ctx, &taskapi.DeleteRequest{ID: "pod", ExecID: "e1"})
	wantCode(t, err, codes.NotFound)

	// The task itself is untouched by exec lifecycle.
	ts, err := s.State(ctx, &taskapi.StateRequest{ID: "pod"})
	if err != nil || ts.Status != task.Status_RUNNING {
		t.Fatalf("task state = %+v err=%v", ts, err)
	}
}

func TestExecValidation(t *testing.T) {
	s := runningTask(t, "pod")
	ctx := context.Background()

	// Terminal is explicitly unimplemented, not silently accepted.
	_, err := s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "tty", Terminal: true, Spec: procSpec(t, []string{"sh"}, false)})
	wantCode(t, err, codes.Unimplemented)
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "tty2", Spec: procSpec(t, []string{"sh"}, true)})
	wantCode(t, err, codes.Unimplemented)

	// Stdin likewise.
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "in", Stdin: "/fifo", Spec: procSpec(t, []string{"sh"}, false)})
	wantCode(t, err, codes.Unimplemented)

	// Missing pieces are InvalidArgument.
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{ID: "pod", ExecID: "", Spec: procSpec(t, []string{"x"}, false)})
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{ID: "pod", ExecID: "nospec"})
	wantCode(t, err, codes.InvalidArgument)
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{ID: "pod", ExecID: "noargs", Spec: procSpec(t, nil, false)})
	wantCode(t, err, codes.InvalidArgument)

	// Duplicate exec id.
	if _, err := s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "dup", Spec: procSpec(t, []string{"x"}, false)}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "dup", Spec: procSpec(t, []string{"x"}, false)})
	wantCode(t, err, codes.AlreadyExists)

	// Kill on a known exec: honest Unimplemented (no in-guest signalling yet).
	_, err = s.Kill(ctx, &taskapi.KillRequest{ID: "pod", ExecID: "dup"})
	wantCode(t, err, codes.Unimplemented)

	// Start twice: second is FailedPrecondition.
	if _, err := s.Start(ctx, &taskapi.StartRequest{ID: "pod", ExecID: "dup"}); err != nil {
		t.Fatal(err)
	}
	_, err = s.Start(ctx, &taskapi.StartRequest{ID: "pod", ExecID: "dup"})
	wantCode(t, err, codes.FailedPrecondition)
}

func TestExecRequiresRunningTask(t *testing.T) {
	s, _ := newTestService(t, nil)
	ctx := context.Background()
	bundle := writeBundle(t, nil)
	_, _ = s.Create(ctx, &taskapi.CreateTaskRequest{ID: "created-only", Bundle: bundle})

	// CREATED (not started) task: exec refused.
	_, err := s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "created-only", ExecID: "e", Spec: procSpec(t, []string{"x"}, false)})
	wantCode(t, err, codes.FailedPrecondition)

	// Unknown task.
	_, err = s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "ghost", ExecID: "e", Spec: procSpec(t, []string{"x"}, false)})
	wantCode(t, err, codes.NotFound)
}

// Exec exits are published as TaskExit carrying the exec id, which is how
// containerd tells an exec's death apart from the task's own.
func TestExecExitPublishesEventWithExecID(t *testing.T) {
	s := runningTask(t, "pod")
	ctx := context.Background()

	events := &capturePublisher{}
	s.SetPublisher(events)

	_, _ = s.Exec(ctx, &taskapi.ExecProcessRequest{
		ID: "pod", ExecID: "e1", Spec: procSpec(t, []string{"true"}, false)})
	_, _ = s.Start(ctx, &taskapi.StartRequest{ID: "pod", ExecID: "e1"})
	wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, _ = s.Wait(wctx, &taskapi.WaitRequest{ID: "pod", ExecID: "e1"})

	if !events.sawExecExit("pod", "e1") {
		t.Fatalf("no TaskExit with exec id; events: %v", events.topics())
	}
}
