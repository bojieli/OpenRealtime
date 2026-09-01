package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/plugin"
)

var (
	ErrReconcileStale               = errors.New("plugin reconciliation candidate is stale")
	ErrReconcileNoChanges           = errors.New("plugin reconciliation candidate has no changes")
	ErrCandidatePreMountUnsupported = errors.New("plugin reconciliation candidate cannot be pre-mounted")
	ErrStateMigrationNeeded         = errors.New("plugin reconciliation requires unsupported state migration")
)

const ReconcileReceiptFormatVersion = 1

// EntryUpdate is one explicit row replacement. Set fields distinguish an
// omitted plane from selecting its zero value (for example, the descriptor's
// default implementation or an empty permission set).
type EntryUpdate struct {
	Entry             string
	SetImplementation bool
	Implementation    string
	SetConfig         bool
	Config            json.RawMessage
	SetPermissions    bool
	Permissions       []plugin.Permission
}

// ReconcileCandidate is bound to both the immutable plan and one observed
// lifecycle sequence. This prevents a UI or operator from applying a review
// made before a provider failure, replacement, or manual unmount.
type ReconcileCandidate struct {
	ExpectedPlanFingerprint string
	ExpectedSequence        uint64
	Updates                 []EntryUpdate
}

type EntryTransition struct {
	Entry                string                   `json:"entry"`
	BeforeImplementation string                   `json:"before_implementation"`
	AfterImplementation  string                   `json:"after_implementation"`
	BeforeRuntime        inspect.ArtifactIdentity `json:"before_runtime"`
	AfterRuntime         inspect.ArtifactIdentity `json:"after_runtime"`
	BeforeConfigDigest   string                   `json:"before_config_digest"`
	AfterConfigDigest    string                   `json:"after_config_digest"`
	BeforePermissions    []plugin.Permission      `json:"before_permissions,omitempty"`
	AfterPermissions     []plugin.Permission      `json:"after_permissions,omitempty"`
}

type ReconcileReceipt struct {
	FormatVersion   uint64            `json:"format_version"`
	PlanFingerprint string            `json:"plan_fingerprint"`
	BeforeSequence  uint64            `json:"before_sequence"`
	AfterSequence   uint64            `json:"after_sequence"`
	Transitions     []EntryTransition `json:"transitions"`
}

type preparedUpdate struct {
	entry          *mountedEntry
	factory        Factory
	preMounter     CandidatePreMounter
	implementation string
	artifact       inspect.ArtifactIdentity
	config         json.RawMessage
	permissions    permissionSet
	transition     EntryTransition
	candidate      *preparedCandidateMount
}

type preparedCandidateMount struct {
	mount   CandidateMount
	scope   *lifecycleScope
	adopted bool
}

type candidateLifecycleView struct{ scope *lifecycleScope }

func (lifecycle candidateLifecycleView) Defer(
	name string, dispose func(context.Context) error,
) error {
	return lifecycle.scope.Defer(name, dispose)
}

type previousEntry struct {
	factory        Factory
	implementation string
	artifact       inspect.ArtifactIdentity
	config         json.RawMessage
	permissions    permissionSet
}

// Reconcile atomically applies implementation, values, and permission changes
// under the same immutable plan. It validates and effect-restricted pre-mounts
// every changed row before quiescing the affected dependency closure, rolls the
// complete set back on an activation failure, and refuses stateful rows until
// an explicit state-migration service exists. Topology/descriptor changes
// require a future plan-level reconciliation API and cannot be smuggled through
// this method.
func (mounted *Mounted) Reconcile(
	ctx context.Context, candidate ReconcileCandidate,
) (ReconcileReceipt, error) {
	if ctx == nil {
		return ReconcileReceipt{}, errors.New("reconcile plugin plan: nil context")
	}
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	if mounted.closed {
		return ReconcileReceipt{}, errors.New("reconcile plugin plan: realm is closed")
	}
	before := mounted.sequence.Load()
	if candidate.ExpectedPlanFingerprint != mounted.plan.Fingerprint || candidate.ExpectedSequence != before {
		return ReconcileReceipt{}, fmt.Errorf(
			"%w: expected plan %s sequence %d, live plan is %s sequence %d",
			ErrReconcileStale, candidate.ExpectedPlanFingerprint, candidate.ExpectedSequence,
			mounted.plan.Fingerprint, before,
		)
	}
	if err := mounted.realmErrorLocked("reconcile plugin plan"); err != nil {
		return ReconcileReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		return ReconcileReceipt{}, fmt.Errorf("reconcile plugin plan: %w", err)
	}
	prepared, err := mounted.prepareReconciliationLocked(candidate.Updates)
	if err != nil {
		return ReconcileReceipt{}, err
	}
	// Do not begin a lifecycle transition for a request that expired while it
	// was being preflighted.
	if err := ctx.Err(); err != nil {
		return ReconcileReceipt{}, fmt.Errorf("reconcile plugin plan: %w", err)
	}
	if len(prepared) == 0 {
		return ReconcileReceipt{}, ErrReconcileNoChanges
	}
	if err := mounted.preMountReconciliationLocked(ctx, prepared); err != nil {
		return ReconcileReceipt{}, err
	}
	if err := ctx.Err(); err != nil {
		cleanupErr := mounted.disposePreparedCandidatesLocked(
			prepared, errors.New("plugin reconciliation canceled after candidate pre-mount"),
		)
		return ReconcileReceipt{}, errors.Join(
			fmt.Errorf("reconcile plugin plan: %w", err), cleanupErr,
		)
	}
	if err := mounted.realmErrorLocked("reconcile plugin plan after candidate pre-mount"); err != nil {
		cleanupErr := mounted.disposePreparedCandidatesLocked(
			prepared, errors.New("plugin reconciliation realm ended after candidate pre-mount"),
		)
		return ReconcileReceipt{}, errors.Join(err, cleanupErr)
	}
	affected := make(map[string]struct{})
	previous := make(map[string]previousEntry, len(prepared))
	for _, update := range prepared {
		for id := range mounted.dependentClosureLocked(update.entry.plan.Entry.ID) {
			affected[id] = struct{}{}
		}
		previous[update.entry.plan.Entry.ID] = previousEntry{
			factory: update.entry.factory, implementation: update.entry.implementation,
			artifact: update.entry.artifact, config: slices.Clone(update.entry.config),
			permissions: update.entry.permissions.Clone(),
		}
	}
	if err := mounted.unmountSetLocked(ctx, affected, errors.New("plugin reconciliation safe point")); err != nil {
		// Restoration is an integrity obligation, not optional work owned by the
		// caller's request context. A canceled request must not strand the realm
		// inactive or make restored lifecycle scopes inherit cancellation.
		cleanupErr := mounted.disposePreparedCandidatesLocked(
			prepared, errors.New("plugin reconciliation did not reach its safe point"),
		)
		restoreErr := mounted.mountEligibleForOperationLocked(mounted.realm, affected)
		return ReconcileReceipt{}, errors.Join(
			fmt.Errorf("reconcile plugin plan could not quiesce affected entries: %w", err),
			wrapOptional("restore after quiesce failure", restoreErr),
			wrapOptional("dispose pre-mounted candidate", cleanupErr),
		)
	}
	for _, update := range prepared {
		update.entry.factory = update.factory
		update.entry.implementation = update.implementation
		update.entry.artifact = update.artifact
		update.entry.config = slices.Clone(update.config)
		update.entry.permissions = update.permissions.Clone()
	}
	mounted.sequence.Add(1)
	// Successful scopes inherit the realm owner, never request-scoped values;
	// request cancellation is forwarded only until mounting reaches its commit
	// point.
	candidates := make(map[string]*preparedCandidateMount, len(prepared))
	for index := range prepared {
		candidates[prepared[index].entry.plan.Entry.ID] = prepared[index].candidate
	}
	candidateErr := mounted.mountEligibleWithCandidatesForOperationLocked(ctx, affected, candidates)
	if candidateErr == nil {
		for index := range prepared {
			if !prepared[index].entry.active || !prepared[index].candidate.adopted {
				candidateErr = errors.Join(candidateErr, fmt.Errorf(
					"reconcile plugin candidate entry %s did not activate",
					prepared[index].entry.plan.Entry.ID,
				))
			}
		}
	}
	if candidateErr != nil {
		recoveryContext := mounted.realm
		cleanupErr := mounted.unmountSetLocked(
			recoveryContext, affected, errors.New("plugin reconciliation rollback"),
		)
		preMountCleanupErr := mounted.disposePreparedCandidatesLocked(
			prepared, errors.New("plugin reconciliation candidate rollback"),
		)
		for id, value := range previous {
			entry := mounted.byID[id]
			entry.factory = value.factory
			entry.implementation = value.implementation
			entry.artifact = value.artifact
			entry.config = slices.Clone(value.config)
			entry.permissions = value.permissions.Clone()
		}
		mounted.sequence.Add(1)
		rollbackErr := mounted.mountEligibleForOperationLocked(recoveryContext, affected)
		if rollbackErr != nil || cleanupErr != nil || preMountCleanupErr != nil {
			return ReconcileReceipt{}, errors.Join(
				fmt.Errorf("reconcile plugin candidate failed: %w", candidateErr),
				wrapOptional("candidate cleanup", cleanupErr),
				wrapOptional("pre-mounted candidate cleanup", preMountCleanupErr),
				wrapOptional("rollback", rollbackErr),
			)
		}
		return ReconcileReceipt{}, fmt.Errorf(
			"reconcile plugin candidate failed and previous composition was restored: %w", candidateErr,
		)
	}
	transitions := make([]EntryTransition, len(prepared))
	for index, update := range prepared {
		transitions[index] = cloneTransition(update.transition)
	}
	sort.Slice(transitions, func(left, right int) bool { return transitions[left].Entry < transitions[right].Entry })
	return ReconcileReceipt{
		FormatVersion: ReconcileReceiptFormatVersion, PlanFingerprint: mounted.plan.Fingerprint,
		BeforeSequence: before, AfterSequence: mounted.sequence.Load(), Transitions: transitions,
	}, nil
}

func (mounted *Mounted) prepareReconciliationLocked(updates []EntryUpdate) ([]preparedUpdate, error) {
	if len(updates) == 0 || len(updates) > len(mounted.entries) {
		return nil, fmt.Errorf("reconcile plugin plan: candidate must update 1..%d entries", len(mounted.entries))
	}
	seen := make(map[string]struct{}, len(updates))
	prepared := make([]preparedUpdate, 0, len(updates))
	for _, update := range updates {
		if update.Entry == "" || update.Entry != strings.TrimSpace(update.Entry) {
			return nil, errors.New("reconcile plugin plan: update has an invalid entry ID")
		}
		if _, duplicate := seen[update.Entry]; duplicate {
			return nil, fmt.Errorf("reconcile plugin plan: entry %s is repeated", update.Entry)
		}
		seen[update.Entry] = struct{}{}
		entry := mounted.byID[update.Entry]
		if entry == nil {
			return nil, fmt.Errorf("reconcile plugin plan: unknown entry %q", update.Entry)
		}
		if !entry.desired || !entry.active {
			return nil, fmt.Errorf("reconcile plugin plan: entry %s is not active and desired", update.Entry)
		}
		if !update.SetImplementation && !update.SetConfig && !update.SetPermissions {
			return nil, fmt.Errorf("reconcile plugin plan: entry %s selects no plane", update.Entry)
		}

		factory, implementation, artifact := entry.factory, entry.implementation, entry.artifact
		if update.SetImplementation {
			selected := strings.TrimSpace(update.Implementation)
			if selected != update.Implementation {
				return nil, fmt.Errorf("reconcile plugin plan: entry %s implementation is not canonical", update.Entry)
			}
			var err error
			factory, artifact, err = mounted.registry.resolve(selected, entry.plan.Identity)
			if err != nil {
				return nil, fmt.Errorf("reconcile plugin plan entry %s: %w", update.Entry, err)
			}
			implementation = selected
			if implementation == "" {
				implementation = entry.plan.Identity.Name
			}
		}
		config := slices.Clone(entry.config)
		if update.SetConfig {
			canonical, _, err := graphvalues.Digest(update.Config)
			if err != nil {
				return nil, fmt.Errorf("reconcile plugin plan entry %s values: %w", update.Entry, err)
			}
			config = canonical
		}
		validator, validates := factory.(ConfigValidator)
		switch {
		case entry.plan.Descriptor.ConfigSchema == nil && !bytes.Equal(config, []byte("{}")):
			return nil, fmt.Errorf("reconcile plugin plan entry %s has values but declares no config schema", update.Entry)
		case entry.plan.Descriptor.ConfigSchema != nil && !validates:
			return nil, fmt.Errorf("reconcile plugin plan entry %s implementation has no config validator", update.Entry)
		case validates:
			if err := validator.ValidateConfig(slices.Clone(config)); err != nil {
				return nil, fmt.Errorf("reconcile plugin plan entry %s values: %w", update.Entry, err)
			}
		}
		permissions := entry.permissions.Clone()
		if update.SetPermissions {
			var err error
			permissions, err = bindPermissions(entry.plan.Descriptor, update.Permissions)
			if err != nil {
				return nil, fmt.Errorf("reconcile plugin plan entry %s permissions: %w", update.Entry, err)
			}
		}
		changed := implementation != entry.implementation || !bytes.Equal(config, entry.config) ||
			!permissionSetsEqual(permissions, entry.permissions)
		if !changed {
			continue
		}
		if entry.plan.Descriptor.StateSchema != nil {
			return nil, fmt.Errorf("%w: entry %s has state schema %s",
				ErrStateMigrationNeeded, update.Entry, entry.plan.Descriptor.StateSchema.Name)
		}
		_, beforeDigest, _ := graphvalues.Digest(entry.config)
		_, afterDigest, _ := graphvalues.Digest(config)
		prepared = append(prepared, preparedUpdate{
			entry: entry, factory: factory, implementation: implementation,
			artifact: artifact, config: config, permissions: permissions,
			transition: EntryTransition{
				Entry:                update.Entry,
				BeforeImplementation: entry.implementation, AfterImplementation: implementation,
				BeforeRuntime: entry.artifact, AfterRuntime: artifact,
				BeforeConfigDigest: beforeDigest, AfterConfigDigest: afterDigest,
				BeforePermissions: entry.permissions.Snapshot(), AfterPermissions: permissions.Snapshot(),
			},
		})
	}
	order := make(map[string]int, len(mounted.entries))
	for index, entry := range mounted.entries {
		order[entry.plan.Entry.ID] = index
	}
	sort.Slice(prepared, func(left, right int) bool {
		return order[prepared[left].entry.plan.Entry.ID] < order[prepared[right].entry.plan.Entry.ID]
	})
	for index := range prepared {
		preMounter, supportsPreMount := prepared[index].factory.(CandidatePreMounter)
		if !supportsPreMount {
			return nil, fmt.Errorf("%w: entry %s implementation %q",
				ErrCandidatePreMountUnsupported, prepared[index].entry.plan.Entry.ID,
				prepared[index].implementation)
		}
		prepared[index].preMounter = preMounter
	}
	return prepared, nil
}

func (mounted *Mounted) preMountReconciliationLocked(
	operation context.Context, prepared []preparedUpdate,
) error {
	for index := range prepared {
		update := &prepared[index]
		entry := update.entry
		bindings := make(map[string]plugin.DependencyBinding, len(entry.plan.Dependencies))
		services := boundServices{store: mounted.store, bindings: bindings}
		for _, binding := range entry.plan.Dependencies {
			bindings[binding.Service.Name] = binding
			if binding.Optional {
				continue
			}
			if _, _, _, _, found := services.Lookup(binding.Service.Name); !found {
				cleanupErr := mounted.disposePreparedCandidatesLocked(
					prepared, errors.New("plugin reconciliation pre-mount dependency unavailable"),
				)
				return errors.Join(fmt.Errorf(
					"reconcile plugin candidate entry %s requires unavailable service %s from %s",
					entry.plan.Entry.ID, binding.Service.Name, binding.Provider,
				), cleanupErr)
			}
		}

		scope := newLifecycleScope(mounted.realm, entry.plan.Entry.ID+":candidate", nil)
		stopOperation := context.AfterFunc(operation, func() {
			scope.cancel(context.Cause(operation))
		})
		candidate, err := update.preMounter.PreMount(scope.ctx, CandidateContext{
			EntryID: entry.plan.Entry.ID, Identity: entry.plan.Identity,
			Config: slices.Clone(update.config), Services: services,
			Lifecycle: candidateLifecycleView{scope: scope}, Permissions: update.permissions.Clone(),
			Descriptor: entry.plan.Descriptor.Clone(),
		})
		if !stopOperation() || operation.Err() != nil {
			err = errors.Join(err, context.Cause(operation))
		}
		if cause := context.Cause(mounted.realm); cause != nil {
			err = errors.Join(err, fmt.Errorf("realm lifecycle ended: %w", cause))
		}
		if nilServiceValue(candidate) {
			err = errors.Join(err, errors.New("candidate pre-mount returned a nil activation"))
		}
		update.candidate = &preparedCandidateMount{mount: candidate, scope: scope}
		if err != nil {
			cleanupErr := mounted.disposePreparedCandidatesLocked(
				prepared, errors.New("plugin reconciliation candidate pre-mount failed"),
			)
			return errors.Join(fmt.Errorf(
				"reconcile plugin candidate entry %s pre-mount: %w", entry.plan.Entry.ID, err,
			), cleanupErr)
		}
	}
	return nil
}

func (mounted *Mounted) disposePreparedCandidatesLocked(
	prepared []preparedUpdate, cause error,
) error {
	var failures []error
	for index := len(prepared) - 1; index >= 0; index-- {
		candidate := prepared[index].candidate
		if candidate == nil || candidate.scope == nil {
			continue
		}
		cleanup, cancel := context.WithTimeout(context.Background(), mounted.timeout)
		err := candidate.scope.close(cleanup, cause)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Errorf(
				"dispose pre-mounted plugin %s: %w", prepared[index].entry.plan.Entry.ID, err,
			))
		}
	}
	return errors.Join(failures...)
}

func permissionSetsEqual(left, right permissionSet) bool {
	return slices.EqualFunc(left.grants, right.grants, func(a, b plugin.Permission) bool {
		return a.Kind == b.Kind && a.Resource == b.Resource && a.Authority == b.Authority &&
			slices.Equal(a.Operations, b.Operations)
	})
}

func cloneTransition(source EntryTransition) EntryTransition {
	result := source
	result.BeforePermissions = clonePermissions(source.BeforePermissions)
	result.AfterPermissions = clonePermissions(source.AfterPermissions)
	return result
}

func clonePermissions(source []plugin.Permission) []plugin.Permission {
	result := make([]plugin.Permission, len(source))
	for index, permission := range source {
		result[index] = permission
		result[index].Operations = slices.Clone(permission.Operations)
	}
	return result
}

func wrapOptional(label string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", label, err)
}
