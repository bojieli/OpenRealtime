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
	Permissions Permissions
	Descriptor  plugin.Descriptor
}

// Factory mounts one implementation of an immutable plugin descriptor. Mount
// may register services and scoped work, but it must not start untracked work.
type Factory interface {
	Descriptor() plugin.Descriptor
	Mount(context.Context, MountContext) error
}

// ConfigValidator proves one separate values document before any plugin is
// mounted. A descriptor with ConfigSchema requires this interface.
type ConfigValidator interface {
	ValidateConfig(json.RawMessage) error
}
