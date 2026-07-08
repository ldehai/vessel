package shim

import (
	"context"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	taskapi "github.com/containerd/containerd/api/runtime/task/v2"
	eventsapi "github.com/containerd/containerd/api/services/ttrpc/events/v1"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/ldehai/vessel/pkg/sandbox"
)

// recordingPublisher captures Service emissions in-process.
type recordingPublisher struct {
	mu     sync.Mutex
	topics []string
}

func (r *recordingPublisher) Publish(_ context.Context, topic string, _ proto.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.topics = append(r.topics, topic)
	return nil
}

func (r *recordingPublisher) got() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.topics...)
}

// The Service must emit the four lifecycle events kubelet's view of a pod
// depends on, in order.
func TestServicePublishesLifecycleEvents(t *testing.T) {
	s, _ := newTestService(t, nil)
	rec := &recordingPublisher{}
	s.SetPublisher(rec)
	ctx := context.Background()
	bundle := writeBundle(t, nil)

	_, _ = s.Create(ctx, &taskapi.CreateTaskRequest{ID: "e", Bundle: bundle})
	_, _ = s.Start(ctx, &taskapi.StartRequest{ID: "e"})
	_, _ = s.Kill(ctx, &taskapi.KillRequest{ID: "e", Signal: 15})
	_, _ = s.Delete(ctx, &taskapi.DeleteRequest{ID: "e"})

	want := []string{TopicTaskCreate, TopicTaskStart, TopicTaskExit, TopicTaskDelete}
	got := rec.got()
	if len(got) != len(want) {
		t.Fatalf("topics = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("topic[%d] = %s, want %s", i, got[i], want[i])
		}
	}
}

// fakeEventsServer implements containerd's ttrpc events endpoint.
type fakeEventsServer struct {
	mu        sync.Mutex
	envelopes []*eventsapi.Envelope
}

func (f *fakeEventsServer) Forward(_ context.Context, r *eventsapi.ForwardRequest) (*emptypb.Empty, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.envelopes = append(f.envelopes, r.Envelope)
	return &emptypb.Empty{}, nil
}

// RemotePublisher must deliver a correctly-enveloped event to containerd's
// real ttrpc Events service (here: its generated server stub).
func TestRemotePublisherForward(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "containerd.sock.ttrpc")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := ttrpc.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeEventsServer{}
	eventsapi.RegisterEventsService(srv, fake)
	go func() { _ = srv.Serve(context.Background(), l) }()
	defer srv.Close()

	pub, err := NewRemotePublisher(sock, "k8s.io")
	if err != nil {
		t.Fatal(err)
	}
	defer pub.Close()

	// Publish through a Service so the envelope carries a real TaskExit.
	d := sandbox.NewFakeDriver()
	mgr := sandbox.NewManager()
	mgr.RegisterDriver(d)
	s := NewService(mgr, "fake", nil)
	s.SetPublisher(pub)

	if err := s.publishTaskExit("pod-x", 143, time.Now()); err != nil {
		t.Fatal(err)
	}

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.envelopes) != 1 {
		t.Fatalf("forwarded envelopes = %d, want 1", len(fake.envelopes))
	}
	env := fake.envelopes[0]
	if env.Topic != TopicTaskExit || env.Namespace != "k8s.io" || env.Event == nil {
		t.Fatalf("envelope = %+v", env)
	}
}
