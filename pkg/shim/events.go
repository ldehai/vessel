package shim

import (
	"context"
	"fmt"
	"net"
	"time"

	eventstypes "github.com/containerd/containerd/api/events"
	eventsapi "github.com/containerd/containerd/api/services/ttrpc/events/v1"
	"github.com/containerd/ttrpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Publisher forwards runtime events to containerd. kubelet learns about pod
// exits through the TaskExit event — without it Kubernetes never notices a
// dead sandbox and restart policies never fire.
type Publisher interface {
	Publish(ctx context.Context, topic string, event proto.Message) error
}

// Event topics containerd expects from a runtime.
const (
	TopicTaskCreate = "/tasks/create"
	TopicTaskStart  = "/tasks/start"
	TopicTaskExit   = "/tasks/exit"
	TopicTaskDelete = "/tasks/delete"
)

// RemotePublisher publishes over containerd's ttrpc events service (the
// TTRPC_ADDRESS the shim receives in its environment).
type RemotePublisher struct {
	namespace string
	client    *ttrpc.Client
	events    eventsapi.EventsService
}

// NewRemotePublisher connects to containerd's ttrpc endpoint.
func NewRemotePublisher(ttrpcAddress, namespace string) (*RemotePublisher, error) {
	conn, err := net.DialTimeout("unix", ttrpcAddress, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial containerd ttrpc %s: %w", ttrpcAddress, err)
	}
	client := ttrpc.NewClient(conn)
	return &RemotePublisher{
		namespace: namespace,
		client:    client,
		events:    eventsapi.NewEventsClient(client),
	}, nil
}

func (p *RemotePublisher) Publish(ctx context.Context, topic string, event proto.Message) error {
	body, err := anypb.New(event)
	if err != nil {
		return err
	}
	_, err = p.events.Forward(ctx, &eventsapi.ForwardRequest{
		Envelope: &eventsapi.Envelope{
			Timestamp: timestamppb.Now(),
			Namespace: p.namespace,
			Topic:     topic,
			Event:     body,
		},
	})
	return err
}

func (p *RemotePublisher) Close() error { return p.client.Close() }

// publish is the Service's nil-safe, best-effort emit: event delivery
// failures are not allowed to fail the RPC that triggered them (containerd
// may be restarting), but they must not be silent either — the caller logs.
func (s *Service) publish(topic string, event proto.Message) error {
	if s.publisher == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.publisher.Publish(ctx, topic, event)
}

func (s *Service) publishTaskExit(id string, exitStatus uint32, exitedAt time.Time) error {
	return s.publish(TopicTaskExit, &eventstypes.TaskExit{
		ContainerID: id,
		ID:          id,
		Pid:         s.pid,
		ExitStatus:  exitStatus,
		ExitedAt:    timestamppb.New(exitedAt),
	})
}
