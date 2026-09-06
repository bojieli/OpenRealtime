package policy

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	observationInvocationRuntimeID       = "builtin://openrealtime/elements/policy.ObservationInvocation"
	observationInvocationRuntimeRevision = "implementation:1"
)

// ObservationInvocationDescriptor owns settings and response creation for
// committed participant content. Unlike SessionInvocation, it accepts ordinary
// observation commits and confers no semantic decision or speech-over authority.
// GenerateOnCommit selects automatic activation in the values plane; the default
// records the committed context and waits for an explicit response.create.
func ObservationInvocationDescriptor() element.Descriptor {
	descriptor := SessionInvocationDescriptor()
	descriptor.Name = "policy.ObservationInvocation"
	descriptor.ConfigSchema = "schema://openrealtime/policy/observation-invocation-config/v1"
	descriptor.Effects = []element.Effect{{Name: "policy.observation-invocation.memory", Reversible: true}}
	for index := range descriptor.Ports {
		if descriptor.Ports[index].Name == "committed" {
			descriptor.Ports[index].Type = observationCommitType.Clone()
		}
	}
	return descriptor
}

type ObservationInvocationConfig struct {
	SessionInvocationConfig
	GenerateOnCommit bool `json:"generate_on_commit,omitempty"`
}

func decodeObservationInvocationConfig(source json.RawMessage) (ObservationInvocationConfig, error) {
	config := ObservationInvocationConfig{SessionInvocationConfig: SessionInvocationConfig{
		TerminalMemory: defaultSessionInvocationTerminalMax, CancelMemory: defaultGenerationCancelMemory,
	}}
	if err := elementconfig.Decode(source, &config); err != nil {
		return ObservationInvocationConfig{}, err
	}
	base, err := json.Marshal(config.SessionInvocationConfig)
	if err != nil {
		return ObservationInvocationConfig{}, err
	}
	if _, err := decodeSessionInvocationConfig(base); err != nil {
		return ObservationInvocationConfig{}, err
	}
	return config, nil
}

type observationInvocationFactory struct{}

func (observationInvocationFactory) Descriptor() element.Descriptor {
	return ObservationInvocationDescriptor()
}

func (observationInvocationFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeObservationInvocationConfig(source)
	return err
}

func (observationInvocationFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeObservationInvocationConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.ObservationInvocation %s config: %w", mount.InstanceID, err)
	}
	core, err := mountSessionInvocation(mount, config.SessionInvocationConfig)
	if err != nil {
		return nil, err
	}
	return &observationInvocationRunner{sessionInvocationRunner: core, automatic: config.GenerateOnCommit}, nil
}

type observationInvocationRunner struct {
	*sessionInvocationRunner
	automatic bool
}

func (runner *observationInvocationRunner) Run(ctx context.Context) error {
	return runner.run(ctx, liveidentity.Artifact{
		ID: observationInvocationRuntimeID, Revision: observationInvocationRuntimeRevision,
	}, runner.acceptObservationCommit, runner.acceptResponseCreate)
}

func (runner *observationInvocationRunner) acceptObservationCommit(ctx context.Context, envelope element.Envelope) error {
	commit, ok := observationCommitPayload(envelope.Payload)
	if !ok {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit",
			fmt.Sprintf("observation commit payload has type %T", envelope.Payload))
	}
	if err := runner.validateEnvelope(envelope, "observation commit"); err != nil {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit", err.Error())
	}
	if commit.Kind != stateelements.ObservationCommitted {
		return runner.ignoreCommit(ctx, envelope, commit, "observation_not_committed")
	}
	if err := validateCommit(commit); err != nil {
		return runner.refuse(ctx, envelope, "committed", "", "invalid_commit", err.Error())
	}
	if !runner.automatic {
		runner.state.ContextVersion = max(runner.state.ContextVersion, commit.StoreVersion)
		return runner.ignoreCommit(ctx, envelope, commit, "explicit_response_required")
	}
	return runner.emitCommit(ctx, envelope, commit, "")
}

func (runner *observationInvocationRunner) ignoreCommit(
	ctx context.Context, envelope element.Envelope, commit stateelements.ObservationCommitOutcome, code string,
) error {
	runner.state.Ignored++
	if err := runner.publishOutcome(ctx, envelope, SessionInvocationOutcome{
		Kind: SessionInvocationIgnored, Operation: "committed", Role: runner.config.Role,
		InvocationRevision: runner.invocation.Revision, InvocationDigest: runner.invocationDigest,
		StreamID: commit.StreamID, ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
		Code: code,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *observationInvocationRunner) acceptResponseCreate(ctx context.Context, envelope element.Envelope) error {
	if create, ok := responseCreatePayload(envelope.Payload); ok && create.TrustedPurpose != "" {
		return runner.refuse(ctx, envelope, "create", "", "invalid_create",
			"observation response creation cannot supply a trusted semantic purpose")
	}
	return runner.acceptCreate(ctx, envelope)
}
