package architecture

import (
	"context"
	"fmt"

	"github.com/bojieli/OpenRealtime/binding"
)

// Bind attaches an immutable architecture definition to the concrete binding
// that realises it. The inner binding name remains visible for adapter-level
// evidence; the architecture identity is added to every live session status.
func Bind(definition Definition, inner binding.Binding) (binding.Binding, error) {
	if inner == nil {
		return nil, fmt.Errorf("architecture %s needs a concrete binding", definition.Ref())
	}
	if err := definition.Validate(); err != nil {
		return nil, err
	}
	if inner.Ownership().Effective() != definition.Ownership.Effective() {
		return nil, fmt.Errorf("architecture %s ownership cannot be realised by binding %q", definition.Ref(), inner.Name())
	}
	// Static capabilities can prove a requirement, but an absent optional bit
	// may be discovered only in a sidecar ready frame. The definitive check is
	// therefore performed after Start against the live status.
	return &resolvedBinding{definition: definition, inner: inner}, nil
}

type resolvedBinding struct {
	definition Definition
	inner      binding.Binding
}

func (resolved *resolvedBinding) Name() string { return resolved.inner.Name() }

func (resolved *resolvedBinding) Ownership() binding.Ownership {
	return resolved.definition.Ownership
}

func (resolved *resolvedBinding) Capabilities() binding.Capabilities {
	return resolved.inner.Capabilities()
}

func (resolved *resolvedBinding) Start(
	ctx context.Context, options binding.Options,
) (binding.Runtime, error) {
	runtime, err := resolved.inner.Start(ctx, options)
	if err != nil {
		return nil, err
	}
	status := runtime.Status()
	if err := resolved.definition.ValidateStatus(status); err != nil {
		_ = runtime.Close(ctx, err)
		return nil, err
	}
	return &resolvedRuntime{Runtime: runtime, identity: resolved.definition.Identity()}, nil
}

type resolvedRuntime struct {
	binding.Runtime
	identity binding.ArchitectureIdentity
}

func (runtime *resolvedRuntime) Status() binding.Status {
	status := runtime.Runtime.Status()
	status.Architecture = runtime.identity
	return status
}

var _ binding.Binding = (*resolvedBinding)(nil)
var _ binding.Runtime = (*resolvedRuntime)(nil)
