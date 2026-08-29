package element

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Envelope is the common immutable runtime wrapper for typed graph items.
// Large payloads should be immutable handles or reference-counted media rather
// than mutable buffers copied through every edge.
type Envelope struct {
	Type              Type     `json:"type"`
	ItemID            string   `json:"item_id"`
	SessionID         string   `json:"session_id,omitempty"`
	SourceID          string   `json:"source_id,omitempty"`
	OpportunityID     string   `json:"opportunity_id,omitempty"`
	RunID             string   `json:"run_id,omitempty"`
	Sequence          uint64   `json:"sequence,omitempty"`
	CaptureNS         uint64   `json:"capture_ns,omitempty"`
	ReceiveNS         uint64   `json:"receive_ns,omitempty"`
	TraceID           string   `json:"trace_id,omitempty"`
	CancellationScope string   `json:"cancellation_scope,omitempty"`
	CausalParents     []string `json:"causal_parents,omitempty"`
	Payload           any      `json:"-"`
}

// Clone copies envelope metadata. Payload remains shared by design and must
// obey the immutable payload contract.
func (envelope Envelope) Clone() Envelope {
	envelope.Type = envelope.Type.Clone()
	envelope.CausalParents = slices.Clone(envelope.CausalParents)
	return envelope
}

// ValidateFor verifies the identity and concrete edge type before enqueue.
func (envelope Envelope) ValidateFor(expected Type) error {
	if envelope.ItemID == "" {
		return errors.New("graph envelope requires an item ID")
	}
	if err := envelope.Type.ValidateConcretePort(); err != nil {
		return fmt.Errorf("graph envelope %s: %w", envelope.ItemID, err)
	}
	if !envelope.Type.Equal(expected) {
		return fmt.Errorf("graph envelope %s has type %s, edge requires %s",
			envelope.ItemID, envelope.Type.String(), expected.String())
	}
	return nil
}

// DeliveryResult distinguishes accepted items from legal best-effort drops.
type DeliveryResult string

const (
	Delivered DeliveryResult = "delivered"
	Dropped   DeliveryResult = "dropped"
)

// SendResult summarizes one fan-out operation.
type SendResult struct {
	Delivered int `json:"delivered"`
	Dropped   int `json:"dropped"`
}

// Sender is one concrete output lane.
type Sender interface {
	ID() string
	Type() Type
	Send(context.Context, Envelope) (DeliveryResult, error)
}

// Receiver is one concrete input lane.
type Receiver interface {
	ID() string
	Type() Type
	Receive(context.Context) (Envelope, error)
}

// OutputPort owns zero or more output lanes. Broadcast is atomic across all
// lossless lanes with respect to bounded-capacity admission; best-effort lanes
// may independently drop when full.
type OutputPort interface {
	Name() string
	Type() Type
	Lanes() []Sender
	Broadcast(context.Context, Envelope) (SendResult, error)
}

// InputPort owns zero or more input lanes. Receive is valid for a singular
// connected port; ReceiveAny performs fair arbitration over a variadic group.
type InputPort interface {
	Name() string
	Type() Type
	Lanes() []Receiver
	Receive(context.Context) (Envelope, error)
	ReceiveAny(context.Context) (Envelope, string, error)
}

// Ports exposes only the connections declared for one instance.
type Ports interface {
	Input(name string) (InputPort, error)
	Output(name string) (OutputPort, error)
}

// Services resolves lifecycle coeffects without conflating them with data or
// control ports.
type Services interface {
	Lookup(name string) (value any, revision uint64, found bool)
}

// Lifecycle owns mount-time registrations and resources. Defer registers a
// bounded disposer that runs in reverse order on unmount.
type Lifecycle interface {
	Defer(name string, dispose func(context.Context) error) error
}

// MountContext is the complete, scoped environment of one element instance.
type MountContext struct {
	InstanceID string
	Identity   Identity
	Config     json.RawMessage
	Ports      Ports
	Services   Services
	Lifecycle  Lifecycle
}

// Runnable owns the long-lived reaction loop of one mounted element. It must
// return promptly after ctx is cancelled and must not retain port handles
// after unmount.
type Runnable interface {
	Run(context.Context) error
}

type RunnableFunc func(context.Context) error

func (function RunnableFunc) Run(ctx context.Context) error { return function(ctx) }

// Factory mounts one implementation of an immutable descriptor. Mount must
// not start unsupervised work; long-lived activity belongs in Runnable.Run.
type Factory interface {
	Descriptor() Descriptor
	Mount(context.Context, MountContext) (Runnable, error)
}
