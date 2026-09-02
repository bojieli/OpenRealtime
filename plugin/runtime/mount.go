package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/plugin"
)

// Config supplies the immutable plan and separate private deployment/value
// planes. A missing implementation entry selects the descriptor's symbolic
// name; production profiles normally pin every entry explicitly.
type Config struct {
	Plan            plugin.Plan
	Registry        *Registry
	Implementations map[string]string
	Values          map[string]json.RawMessage
	Permissions     map[string][]plugin.Permission
	ShutdownTimeout time.Duration
}

type mountedEntry struct {
	plan           plugin.PlannedEntry
	factory        Factory
	implementation string
	artifact       inspect.ArtifactIdentity
	config         json.RawMessage
	permissions    permissionSet
	scope          *lifecycleScope
	snapshot       StateSnapshotter
	desired        bool
	active         bool
	state          string
	err            string
}

// Mounted is one plugin realm. Mutating lifecycle operations serialize through
// its mutex; service lookups remain concurrent and do not take that mutex.
type Mounted struct {
	plan     plugin.Plan
	registry *Registry
	entries  []*mountedEntry
	byID     map[string]*mountedEntry
	store    *serviceStore
	timeout  time.Duration
	// realm is the lifecycle owner supplied to Mount. Later activation and
	// replacement operations must not retain request-scoped values or outlive
	// cancellation of this owner.
	realm context.Context

	mu       sync.Mutex
	closed   bool
	sequence atomic.Uint64
}

// Mount validates the entire plan, deployment, and values before acquiring the
// first plugin resource, then mounts in deterministic dependency order.
func Mount(ctx context.Context, config Config) (*Mounted, error) {
	if ctx == nil {
		return nil, errors.New("mount plugin plan: nil context")
	}
	if err := config.Plan.Validate(); err != nil {
		return nil, fmt.Errorf("mount plugin plan: %w", err)
	}
	if config.Registry == nil {
		return nil, errors.New("mount plugin plan: implementation registry is required")
	}
	if config.ShutdownTimeout <= 0 {
		config.ShutdownTimeout = 5 * time.Second
	}
	known := make(map[string]struct{}, len(config.Plan.Entries))
	for _, entry := range config.Plan.Entries {
		known[entry.Entry.ID] = struct{}{}
	}
	for entry := range config.Implementations {
		if _, found := known[entry]; !found {
			return nil, fmt.Errorf("mount plugin plan: implementation selects unknown entry %q", entry)
		}
	}
	for entry := range config.Values {
		if _, found := known[entry]; !found {
			return nil, fmt.Errorf("mount plugin plan: values select unknown entry %q", entry)
		}
	}
	for entry := range config.Permissions {
		if _, found := known[entry]; !found {
			return nil, fmt.Errorf("mount plugin plan: permissions select unknown entry %q", entry)
		}
	}

	mounted := &Mounted{
		plan: config.Plan.Clone(), registry: config.Registry,
		byID:  make(map[string]*mountedEntry),
		store: newServiceStore(), timeout: config.ShutdownTimeout, realm: ctx,
	}
	// Resolve and validate every implementation and value first. Mounting a
	// preceding plugin must never be the cleanup path for a later typo.
	for _, planned := range mounted.plan.Entries {
		implementation := strings.TrimSpace(config.Implementations[planned.Entry.ID])
		factory, artifact, err := config.Registry.resolve(implementation, planned.Identity)
		if err != nil {
			return nil, fmt.Errorf("mount plugin plan entry %s: %w", planned.Entry.ID, err)
		}
		if implementation == "" {
			implementation = planned.Identity.Name
		}
		raw := slices.Clone(config.Values[planned.Entry.ID])
		canonical, _, err := graphvalues.Digest(raw)
		if err != nil {
			return nil, fmt.Errorf("mount plugin plan entry %s values: %w", planned.Entry.ID, err)
		}
		validator, validates := factory.(ConfigValidator)
		switch {
		case planned.Descriptor.ConfigSchema == nil && !bytes.Equal(canonical, []byte("{}")):
			return nil, fmt.Errorf("mount plugin plan entry %s has values but plugin %s declares no config schema",
				planned.Entry.ID, planned.Identity.Name)
		case planned.Descriptor.ConfigSchema != nil && !validates:
			return nil, fmt.Errorf("mount plugin plan entry %s declares a config schema but implementation %q has no validator",
				planned.Entry.ID, implementation)
		case validates:
			if err := validator.ValidateConfig(canonical); err != nil {
				return nil, fmt.Errorf("mount plugin plan entry %s values: %w", planned.Entry.ID, err)
			}
		}
		permissions, err := bindPermissions(planned.Descriptor, config.Permissions[planned.Entry.ID])
		if err != nil {
			return nil, fmt.Errorf("mount plugin plan entry %s permissions: %w", planned.Entry.ID, err)
		}
		entry := &mountedEntry{
			plan: planned, factory: factory, implementation: implementation,
			artifact: artifact, config: slices.Clone(canonical), permissions: permissions,
			desired: true, state: "inactive",
		}
		mounted.entries = append(mounted.entries, entry)
		mounted.byID[planned.Entry.ID] = entry
	}

	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	for _, entry := range mounted.entries {
		if err := mounted.mountEntryLocked(ctx, entry); err != nil {
			rollbackErr := mounted.unmountAllLocked(context.Background(), errors.New("mount rollback"), false)
			return nil, errors.Join(err, rollbackErr)
		}
	}
	return mounted, nil
}

func (mounted *Mounted) mountEntryLocked(ctx context.Context, entry *mountedEntry) error {
	return mounted.mountEntryWithCandidateLocked(ctx, entry, nil)
}

func (mounted *Mounted) mountEntryWithCandidateLocked(
	ctx context.Context, entry *mountedEntry, candidate *preparedCandidateMount,
) error {
	return mounted.mountEntryWithCandidateAndStateLocked(ctx, entry, candidate, nil)
}

func (mounted *Mounted) mountEntryWithCandidateAndStateLocked(
	ctx context.Context,
	entry *mountedEntry,
	candidate *preparedCandidateMount,
	restored json.RawMessage,
) error {
	if entry.active {
		return nil
	}
	if len(restored) > 0 && entry.plan.Descriptor.StateSchema == nil {
		return fmt.Errorf("mount plugin %s received state without a state schema", entry.plan.Entry.ID)
	}
	bindings := make(map[string]plugin.DependencyBinding, len(entry.plan.Dependencies))
	services := boundServices{store: mounted.store, bindings: bindings}
	for _, binding := range entry.plan.Dependencies {
		bindings[binding.Service.Name] = binding
		if binding.Optional {
			continue
		}
		if _, _, _, _, found := services.Lookup(binding.Service.Name); !found {
			entry.state = "pending"
			entry.err = "required service unavailable: " + binding.Service.Name
			return fmt.Errorf("mount plugin %s: required service %s from %s is unavailable",
				entry.plan.Entry.ID, binding.Service.Name, binding.Provider)
		}
	}
	entry.state = "mounting"
	entry.err = ""
	scope := newLifecycleScope(ctx, entry.plan.Entry.ID, func(failure error) {
		go mounted.handleWorkerFailure(entry.plan.Entry.ID, failure)
	})
	publisher := servicePublisher{
		entry: entry.plan.Entry.ID, descriptor: entry.plan.Descriptor,
		store: mounted.store, scope: scope,
	}
	var state *stateLifecycle
	if entry.plan.Descriptor.StateSchema != nil {
		state = newStateLifecycle(entry.plan.Descriptor, restored)
	}
	mount := MountContext{
		EntryID: entry.plan.Entry.ID, Identity: entry.plan.Identity,
		Config: slices.Clone(entry.config), Services: services,
		Publisher: publisher, Lifecycle: scope, State: state, Permissions: entry.permissions.Clone(),
		Descriptor: entry.plan.Descriptor.Clone(),
	}
	var err error
	if candidate == nil {
		err = entry.factory.Mount(scope.ctx, mount)
	} else {
		err = scope.adopt(candidate.scope)
		if err == nil {
			candidate.adopted = true
			err = candidate.mount.Activate(scope.ctx, mount)
		}
	}
	if err == nil {
		for _, contract := range entry.plan.Descriptor.Provides {
			if _, found := mounted.store.lookup(entry.plan.Entry.ID, contract); !found {
				err = fmt.Errorf("plugin returned without publishing declared service %s", contract.Name)
				break
			}
		}
	}
	var snapshot StateSnapshotter
	if err == nil && state != nil {
		snapshot, err = state.seal()
	}
	if err != nil {
		cleanup, cancel := context.WithTimeout(context.Background(), mounted.timeout)
		defer cancel()
		cleanupErr := scope.close(cleanup, err)
		entry.scope = nil
		entry.snapshot = nil
		entry.active = false
		entry.state = "failed"
		entry.err = err.Error()
		return errors.Join(fmt.Errorf("mount plugin %s: %w", entry.plan.Entry.ID, err), cleanupErr)
	}
	entry.scope = scope
	entry.snapshot = snapshot
	entry.active = true
	entry.state = "active"
	entry.err = ""
	mounted.sequence.Add(1)
	return nil
}

func (mounted *Mounted) unmountEntryLocked(
	ctx context.Context, entry *mountedEntry, cause error, final bool,
) error {
	if !entry.active {
		if final {
			entry.state = "closed"
		}
		return nil
	}
	entry.state = "unmounting"
	shutdown, cancel := context.WithTimeout(ctx, mounted.timeout)
	err := entry.scope.close(shutdown, cause)
	cancel()
	entry.scope = nil
	entry.snapshot = nil
	entry.active = false
	if final {
		entry.state = "closed"
	} else if err != nil {
		entry.state = "failed"
		entry.err = err.Error()
	} else {
		entry.state = "inactive"
		entry.err = ""
	}
	mounted.sequence.Add(1)
	return err
}

func (mounted *Mounted) unmountAllLocked(ctx context.Context, cause error, final bool) error {
	var failures []error
	for index := len(mounted.entries) - 1; index >= 0; index-- {
		if err := mounted.unmountEntryLocked(ctx, mounted.entries[index], cause, final); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// Close idempotently cancels all scoped work and disposes registrations in
// reverse dependency order.
func (mounted *Mounted) Close(ctx context.Context) error {
	if ctx == nil {
		return errors.New("close plugin plan: nil context")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return nil
	}
	mounted.closed = true
	return mounted.unmountAllLocked(ctx, errors.New("plugin realm closed"), true)
}

// Unmount removes an entry and every active transitive dependent. The target
// remains disabled; dependents remain desired and can reactivate when the
// provider is explicitly activated again.
func (mounted *Mounted) Unmount(ctx context.Context, entryID string) error {
	if ctx == nil {
		return errors.New("unmount plugin: nil context")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return errors.New("unmount plugin: realm is closed")
	}
	target, found := mounted.byID[entryID]
	if !found {
		return fmt.Errorf("unmount plugin: unknown entry %q", entryID)
	}
	target.desired = false
	affected := mounted.dependentClosureLocked(entryID)
	return mounted.unmountSetLocked(ctx, affected, errors.New("plugin dependency removed"))
}

// Activate enables one entry and then remounts every desired dependent whose
// required providers are active.
func (mounted *Mounted) Activate(ctx context.Context, entryID string) error {
	if ctx == nil {
		return errors.New("activate plugin: nil context")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return errors.New("activate plugin: realm is closed")
	}
	if err := mounted.realmErrorLocked("activate plugin"); err != nil {
		return err
	}
	target, found := mounted.byID[entryID]
	if !found {
		return fmt.Errorf("activate plugin: unknown entry %q", entryID)
	}
	target.desired = true
	if err := mounted.mountEntryForOperationLocked(ctx, target); err != nil {
		return err
	}
	return mounted.mountEligibleForOperationLocked(ctx, nil)
}

// Replace performs a leaf implementation swap under the same descriptor
// identity. It unwinds dependents first and restores the previous
// implementation if candidate mount fails.
func (mounted *Mounted) Replace(ctx context.Context, entryID, implementation string) error {
	if ctx == nil {
		return errors.New("replace plugin: nil context")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return errors.New("replace plugin: realm is closed")
	}
	if err := mounted.realmErrorLocked("replace plugin"); err != nil {
		return err
	}
	target, found := mounted.byID[entryID]
	if !found {
		return fmt.Errorf("replace plugin: unknown entry %q", entryID)
	}
	implementation = strings.TrimSpace(implementation)
	factory, artifact, err := mounted.registry.resolve(implementation, target.plan.Identity)
	if err != nil {
		return fmt.Errorf("replace plugin %s: %w", entryID, err)
	}
	if implementation == "" {
		implementation = target.plan.Identity.Name
	}
	if implementation == target.implementation {
		return nil
	}
	validator, validates := factory.(ConfigValidator)
	switch {
	case target.plan.Descriptor.ConfigSchema != nil && !validates:
		return fmt.Errorf("replace plugin %s: implementation %q has no config validator",
			entryID, implementation)
	case validates:
		if err := validator.ValidateConfig(slices.Clone(target.config)); err != nil {
			return fmt.Errorf("replace plugin %s values: %w", entryID, err)
		}
	}
	affected := mounted.dependentClosureLocked(entryID)
	for _, entry := range mounted.entries {
		if _, selected := affected[entry.plan.Entry.ID]; selected &&
			entry.plan.Descriptor.StateSchema != nil {
			return fmt.Errorf(
				"%w: replace plugin %s affects stateful entry %s schema %s; use Reconcile",
				ErrStateMigrationNeeded, entryID, entry.plan.Entry.ID,
				entry.plan.Descriptor.StateSchema.Name,
			)
		}
	}
	if err := mounted.unmountSetLocked(
		ctx, affected, errors.New("plugin implementation replacement"),
	); err != nil {
		return fmt.Errorf("replace plugin %s could not quiesce current implementation: %w", entryID, err)
	}
	previousFactory := target.factory
	previousImplementation := target.implementation
	previousArtifact := target.artifact
	target.factory = factory
	target.implementation = implementation
	target.artifact = artifact
	mounted.sequence.Add(1)

	candidateErr := mounted.mountEligibleForOperationLocked(ctx, affected)
	if candidateErr == nil {
		return nil
	}
	candidateCleanupErr := mounted.unmountSetLocked(
		mounted.realm, affected, errors.New("plugin replacement rollback"),
	)
	target.factory = previousFactory
	target.implementation = previousImplementation
	target.artifact = previousArtifact
	mounted.sequence.Add(1)
	rollbackErr := mounted.mountEligibleForOperationLocked(mounted.realm, affected)
	if rollbackErr != nil || candidateCleanupErr != nil {
		failures := []error{
			fmt.Errorf("replace plugin %s candidate failed: %w", entryID, candidateErr),
		}
		if candidateCleanupErr != nil {
			failures = append(failures, fmt.Errorf("replace plugin %s candidate cleanup failed: %w",
				entryID, candidateCleanupErr))
		}
		if rollbackErr != nil {
			failures = append(failures, fmt.Errorf("replace plugin %s rollback failed: %w",
				entryID, rollbackErr))
		}
		return errors.Join(failures...)
	}
	return fmt.Errorf("replace plugin %s candidate failed and previous implementation was restored: %w",
		entryID, candidateErr)
}

func (mounted *Mounted) dependentClosureLocked(providerID string) map[string]struct{} {
	affected := map[string]struct{}{providerID: {}}
	changed := true
	for changed {
		changed = false
		for _, entry := range mounted.entries {
			if _, already := affected[entry.plan.Entry.ID]; already {
				continue
			}
			for _, binding := range entry.plan.Dependencies {
				if binding.Optional {
					continue
				}
				if _, dependencyRemoved := affected[binding.Provider]; dependencyRemoved {
					affected[entry.plan.Entry.ID] = struct{}{}
					changed = true
					break
				}
			}
		}
	}
	return affected
}

func (mounted *Mounted) handleWorkerFailure(entryID string, failure error) {
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return
	}
	entry := mounted.byID[entryID]
	if entry == nil || !entry.active {
		return
	}
	affected := mounted.dependentClosureLocked(entryID)
	_ = mounted.unmountSetLocked(context.Background(), affected, failure)
	entry.state = "failed"
	entry.err = failure.Error()
}

func (mounted *Mounted) unmountSetLocked(
	ctx context.Context, entries map[string]struct{}, cause error,
) error {
	var failures []error
	for index := len(mounted.entries) - 1; index >= 0; index-- {
		entry := mounted.entries[index]
		if _, selected := entries[entry.plan.Entry.ID]; !selected {
			continue
		}
		if err := mounted.unmountEntryLocked(ctx, entry, cause, false); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (mounted *Mounted) mountEligibleLocked(ctx context.Context, allowed map[string]struct{}) error {
	return mounted.mountEligibleWithCandidatesLocked(ctx, allowed, nil)
}

func (mounted *Mounted) mountEligibleWithCandidatesLocked(
	ctx context.Context, allowed map[string]struct{}, candidates map[string]*preparedCandidateMount,
) error {
	return mounted.mountEligibleWithCandidatesAndStateLocked(ctx, allowed, candidates, nil)
}

func (mounted *Mounted) mountEligibleWithCandidatesAndStateLocked(
	ctx context.Context,
	allowed map[string]struct{},
	candidates map[string]*preparedCandidateMount,
	restored map[string]json.RawMessage,
) error {
	var failures []error
	for _, entry := range mounted.entries {
		if entry.active || !entry.desired {
			continue
		}
		if allowed != nil {
			if _, selected := allowed[entry.plan.Entry.ID]; !selected {
				continue
			}
		}
		ready := true
		for _, dependency := range entry.plan.Dependencies {
			if dependency.Optional {
				continue
			}
			provider := mounted.byID[dependency.Provider]
			if provider == nil || !provider.active {
				ready = false
				break
			}
		}
		if !ready {
			continue
		}
		if err := mounted.mountEntryWithCandidateAndStateLocked(
			ctx, entry, candidates[entry.plan.Entry.ID], restored[entry.plan.Entry.ID],
		); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (mounted *Mounted) realmErrorLocked(operation string) error {
	if mounted.realm == nil {
		return fmt.Errorf("%s: realm lifecycle context is unavailable", operation)
	}
	if cause := context.Cause(mounted.realm); cause != nil {
		return fmt.Errorf("%s: realm lifecycle ended: %w", operation, cause)
	}
	return nil
}

// mountForOperationLocked gives newly mounted scopes the realm's values and
// lifetime, while forwarding cancellation from an individual operation only
// until mounting returns. Stopping that forwarding is the commit point: a
// later request cancellation cannot silently cancel a live plugin scope.
func (mounted *Mounted) mountForOperationLocked(
	operation context.Context, mount func(context.Context) error,
) error {
	if operation == nil {
		return errors.New("mount plugin operation: nil context")
	}
	if err := mounted.realmErrorLocked("mount plugin operation"); err != nil {
		return err
	}
	parent, cancel := context.WithCancelCause(mounted.realm)
	stopOperation := context.AfterFunc(operation, func() {
		cancel(context.Cause(operation))
	})
	err := mount(parent)
	if !stopOperation() || operation.Err() != nil {
		err = errors.Join(err, context.Cause(operation))
	}
	if cause := context.Cause(mounted.realm); cause != nil {
		err = errors.Join(err, fmt.Errorf("realm lifecycle ended: %w", cause))
	}
	if err != nil && (operation.Err() != nil || context.Cause(mounted.realm) != nil) {
		cancel(err)
	}
	// On success parent is intentionally left alive. It is referenced only by
	// the mounted lifecycle scopes and remains a child of mounted.realm.
	return err
}

func (mounted *Mounted) mountEntryForOperationLocked(ctx context.Context, entry *mountedEntry) error {
	return mounted.mountForOperationLocked(ctx, func(parent context.Context) error {
		return mounted.mountEntryLocked(parent, entry)
	})
}

func (mounted *Mounted) mountEligibleForOperationLocked(
	ctx context.Context, allowed map[string]struct{},
) error {
	return mounted.mountForOperationLocked(ctx, func(parent context.Context) error {
		return mounted.mountEligibleLocked(parent, allowed)
	})
}

func (mounted *Mounted) mountEligibleWithCandidatesForOperationLocked(
	ctx context.Context, allowed map[string]struct{}, candidates map[string]*preparedCandidateMount,
) error {
	return mounted.mountEligibleWithCandidatesAndStateForOperationLocked(
		ctx, allowed, candidates, nil,
	)
}

func (mounted *Mounted) mountEligibleWithCandidatesAndStateForOperationLocked(
	ctx context.Context,
	allowed map[string]struct{},
	candidates map[string]*preparedCandidateMount,
	restored map[string]json.RawMessage,
) error {
	return mounted.mountForOperationLocked(ctx, func(parent context.Context) error {
		return mounted.mountEligibleWithCandidatesAndStateLocked(parent, allowed, candidates, restored)
	})
}

// Plan returns a recursively independent exact static composition.
func (mounted *Mounted) Plan() plugin.Plan {
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	return mounted.plan.Clone()
}
