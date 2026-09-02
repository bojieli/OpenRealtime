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

// Lifecycle owns mount-time registrations, caller-blocking work, and
// background workers. Defer registers a bounded disposer that runs in reverse
// order on unmount. Do runs work in the caller while making it visible to
// lifecycle cancellation and join; Go starts the same kind of owned work in a
// supervised goroutine. Worker names are unique within one mount lifecycle.
type Lifecycle interface {
	Defer(name string, dispose func(context.Context) error) error
	Do(name string, work func(context.Context) error) error
	Go(name string, worker func(context.Context) error) error
}

// StateSnapshotter returns one bounded strict-JSON object representing the
// complete restorable state of an element instance. Snapshot payloads remain
// private to graph reconciliation and are never inspection-plane evidence.
type StateSnapshotter func(context.Context) (json.RawMessage, error)

// StateResumer reopens mutation admission after a state capture is refused
// before lifecycle teardown. It must be idempotent and honor its context.
type StateResumer func(context.Context) error

// StateQuiescer closes mutation admission and drains already-admitted
// mutations before a snapshot. A successful reconciliation retires the
// quiesced lifecycle; refusal invokes the returned resumer.
type StateQuiescer func(context.Context) (StateResumer, error)

// StateLifecycle is present only when the immutable descriptor explicitly
// declares StateTransfer capabilities. Restored may be consumed once while
// mounting when restore is declared. Snapshot and Quiesce register at most one
// callback each before Factory.Mount returns and only when their corresponding
// capabilities are declared. A StateSchema alone does not grant or imply any
// live-transfer operation.
type StateLifecycle interface {
	Restored() (snapshot json.RawMessage, available bool, err error)
	Quiesce(StateQuiescer) error
	Snapshot(StateSnapshotter) error
}

// ResolutionReporter publishes immutable live runtime/provider selection to
// the management plane. Elements call it only after a real startup handshake
// or provider construction; deployment declarations are not live evidence.
// An empty capability list is a complete, attested statement that the node
// selected no provider capabilities.
type ResolutionReporter interface {
	Runtime(artifactID, revision, digest string) error
	Capabilities([]CapabilityResolution) error
}

// CapabilityResolution is intentionally independent of provider packages.
// Provider and adapter identities remain separate so operational evidence can
// distinguish a model change from a transport/adapter change.
type CapabilityResolution struct {
	Name             string
	Contract         string
	ProviderID       string
	ProviderRevision string
	ProviderDigest   string
	AdapterID        string
	AdapterRevision  string
	AdapterDigest    string
}

// MountContext is the complete, scoped environment of one element instance.
type MountContext struct {
	InstanceID string
	Identity   Identity
	Config     json.RawMessage
	Ports      Ports
	Services   Services
	Lifecycle  Lifecycle
	State      StateLifecycle
	Resolution ResolutionReporter
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

// ConfigValidator proves one resolved node value before any factory in the
// graph is mounted. A factory whose descriptor declares ConfigSchema must
// implement this interface; an implementation without a config schema may
// accept only the empty object.
type ConfigValidator interface {
	ValidateConfig(json.RawMessage) error
}
