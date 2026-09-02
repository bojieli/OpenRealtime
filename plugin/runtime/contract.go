// Package runtime mounts one immutable plugin plan with scoped services and
// reversible lifecycle effects.
package runtime

import (
	"context"
	"encoding/json"

	"github.com/bojieli/OpenRealtime/plugin"
)

// Services exposes only dependencies declared and bound by the compiled plan.
// Lookup remains live: an optional provider can appear after its consumer
// mounts, and a provider removed during reconciliation disappears immediately.
type Services interface {
	Lookup(name string) (
		value any, contract plugin.Contract, provider string, revision uint64, found bool,
	)
}

// Publisher admits only services declared by the mounted plugin descriptor.
// Every publication is owned by the plugin lifecycle scope and is removed on
// unmount.
type Publisher interface {
	Provide(contract plugin.Contract, value any) error
}

// Lifecycle owns every runtime effect started by a plugin.
type Lifecycle interface {
	Defer(name string, dispose func(context.Context) error) error
	Go(name string, worker func(context.Context) error) error
}

// CandidateLifecycle owns resources acquired while a replacement is prepared.
// It deliberately omits Go: a candidate may acquire reversible resources, but
// it cannot start background work before the live dependency closure reaches
// its safe point.
type CandidateLifecycle interface {
	Defer(name string, dispose func(context.Context) error) error
}

// StateSnapshotter returns one bounded strict-JSON object representing the
// complete restorable state of a mounted entry. The callback must take an
// internally consistent snapshot when invoked; payload bytes never leave the
// runtime reconciliation boundary.
type StateSnapshotter func(context.Context) (json.RawMessage, error)

// StateLifecycle is the state-transfer boundary available only to descriptors
// that declare a StateSchema. A restoring mount must consume Restored before it
// publishes services, and every snapshot-capable mount must register exactly
// one callback before Mount or Activate returns.
type StateLifecycle interface {
	Restored() (snapshot json.RawMessage, available bool, err error)
	Snapshot(StateSnapshotter) error
}

// Permissions is the deployment grant selected for one plugin instance. It is
// always a subset of the immutable descriptor ceiling. A plugin must check the
// grant at the effect boundary in addition to consuming any declared authority
// service.
type Permissions interface {
	Allows(kind, resource, operation string) bool
	Snapshot() []plugin.Permission
}

// MountContext is the complete scoped environment of one plugin instance.
type MountContext struct {
	EntryID     string
	Identity    plugin.Identity
	Config      json.RawMessage
	Services    Services
	Publisher   Publisher
	Lifecycle   Lifecycle
	State       StateLifecycle
	Permissions Permissions
	Descriptor  plugin.Descriptor
}

// CandidateContext is the effect-restricted environment for a replacement
// pre-mount. Services are the exact currently bound dependencies and remain
// read-only. Publication and worker APIs are unavailable until Activate.
type CandidateContext struct {
	EntryID     string
	Identity    plugin.Identity
	Config      json.RawMessage
	Services    Services
	Lifecycle   CandidateLifecycle
	Permissions Permissions
	Descriptor  plugin.Descriptor
}

// Factory mounts one implementation of an immutable plugin descriptor. Mount
// may register services and scoped work, but it must not start untracked work.
type Factory interface {
	Descriptor() plugin.Descriptor
	Mount(context.Context, MountContext) error
}

// CandidateMount is a successfully prepared replacement. Activate is called
// exactly once, after the affected live dependency closure has quiesced, with
// the ordinary publication and lifecycle capabilities. Resources acquired by
// PreMount remain runtime-owned through CandidateContext.Lifecycle.
type CandidateMount interface {
	Activate(context.Context, MountContext) error
}

// CandidatePreMounter prepares a replacement without publishing services,
// starting workers, or changing externally visible state. Reconcile refuses a
// changed factory that lacks this contract before it tears down live work.
type CandidatePreMounter interface {
	PreMount(context.Context, CandidateContext) (CandidateMount, error)
}

// StateMigration is the exact predecessor state presented to an explicit
// candidate migrator after the affected live dependency closure has retired.
// Snapshot is independently cloned and remains private to the runtime and the
// selected migrator.
type StateMigration struct {
	EntryID              string
	Schema               plugin.Contract
	SourceImplementation string
	Snapshot             json.RawMessage
}

// CandidateStateMigrator explicitly converts a predecessor snapshot for one
// changed stateful row. The returned strict-JSON object is validated and then
// supplied through MountContext.State when the prepared candidate activates.
type CandidateStateMigrator interface {
	CandidateMount
	MigrateState(context.Context, StateMigration) (json.RawMessage, error)
}

// ConfigValidator proves one separate values document before any plugin is
// mounted. A descriptor with ConfigSchema requires this interface.
type ConfigValidator interface {
	ValidateConfig(json.RawMessage) error
}
