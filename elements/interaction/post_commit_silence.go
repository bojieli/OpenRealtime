package interaction

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	// PostCommitSilenceSchedulerService lets a host supply the same monotonic
	// scheduler used by a deterministic simulation. Production graphs normally
	// use the lifecycle-owned system scheduler selected by the element.
	PostCommitSilenceSchedulerService = "interaction.post-commit-silence.scheduler"
	maximumPostCommitSilenceDelayMS   = 600_000
)

var (
	postCommitSilenceStateType = element.State(
		element.Named("interaction.PostCommitSilenceState"),
	)
	postCommitSilenceOutcomeType = element.Event(
		element.Named("interaction.PostCommitSilenceOutcome"),
	)
)

func PostCommitSilenceStateType() element.Type { return postCommitSilenceStateType.Clone() }
func PostCommitSilenceOutcomeType() element.Type {
	return postCommitSilenceOutcomeType.Clone()
}

// PostCommitSilenceDescriptor is an explicit graph-owned wake-up for a period
// in which no new durable user observation arrives. It never interprets a
// request or speaks by itself. Once the configured interval elapses, it emits
// an ordinary ResponseCreate bound to the latest committed trajectory prefix;
// the connected invocation/model policy still decides what, if anything, the
// agent should say.
func PostCommitSilenceDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "interaction.PostCommitSilence",
		Revision:      1,
		Ports: []element.Port{
			{Name: "committed", Direction: element.Input, Type: stateelements.ObservationCommitOutcomeType(),
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 32},
			{Name: "create", Direction: element.Output, Type: policyelements.ResponseCreateType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "state", Direction: element.Output, Type: postCommitSilenceStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "outcome", Direction: element.Output, Type: postCommitSilenceOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"committed"}, Outcomes: []string{"create", "state", "outcome"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/interaction/post-commit-silence-state/v1",
		ConfigSchema: "schema://openrealtime/interaction/post-commit-silence-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName},
			{Name: graphruntime.SequenceServiceName},
			{Name: PostCommitSilenceSchedulerService, Optional: true},
		},
		Effects: []element.Effect{{Name: "interaction.post-commit-silence.timer", Reversible: true}},
	}
}

type PostCommitSilenceConfig struct {
	DelayMS int `json:"delay_ms"`
}

type PostCommitSilenceOutcomeKind string

const (
	PostCommitSilenceArmed   PostCommitSilenceOutcomeKind = "armed"
	PostCommitSilenceFired   PostCommitSilenceOutcomeKind = "fired"
	PostCommitSilenceIgnored PostCommitSilenceOutcomeKind = "ignored"
	PostCommitSilenceRefused PostCommitSilenceOutcomeKind = "refused"
)

type PostCommitSilenceOutcome struct {
	Kind           PostCommitSilenceOutcomeKind `json:"kind"`
	Generation     uint64                       `json:"generation,omitempty"`
	DeadlineNS     uint64                       `json:"deadline_ns,omitempty"`
	ContextVersion uint64                       `json:"context_version,omitempty"`
	ContextItemID  string                       `json:"context_item_id,omitempty"`
	TriggerItemID  string                       `json:"trigger_item_id,omitempty"`
	ResponseID     string                       `json:"response_id,omitempty"`
	Code           string                       `json:"code,omitempty"`
	Message        string                       `json:"message,omitempty"`
	FinishedNS     uint64                       `json:"finished_ns"`
}

type PostCommitSilenceState struct {
	Armed          bool   `json:"armed"`
	Generation     uint64 `json:"generation"`
	DeadlineNS     uint64 `json:"deadline_ns,omitempty"`
	ContextVersion uint64 `json:"context_version"`
	ContextItemID  string `json:"context_item_id,omitempty"`
	TriggerItemID  string `json:"trigger_item_id,omitempty"`
	ArmedCount     uint64 `json:"armed_count"`
	ResetCount     uint64 `json:"reset_count"`
	FiredCount     uint64 `json:"fired_count"`
	IgnoredCount   uint64 `json:"ignored_count"`
	RefusedCount   uint64 `json:"refused_count"`
}

func decodePostCommitSilenceConfig(source json.RawMessage) (PostCommitSilenceConfig, error) {
	var config PostCommitSilenceConfig
	if err := elementconfig.Decode(source, &config); err != nil {
		return PostCommitSilenceConfig{}, err
	}
	if config.DelayMS < 1 || config.DelayMS > maximumPostCommitSilenceDelayMS {
		return PostCommitSilenceConfig{}, fmt.Errorf(
			"delay_ms must be between 1 and %d", maximumPostCommitSilenceDelayMS,
		)
	}
	return config, nil
}

type postCommitSilenceFactory struct{}

var (
	_ element.Factory         = postCommitSilenceFactory{}
	_ element.ConfigValidator = postCommitSilenceFactory{}
)

func (postCommitSilenceFactory) Descriptor() element.Descriptor {
	return PostCommitSilenceDescriptor()
}

func (postCommitSilenceFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodePostCommitSilenceConfig(source)
	return err
}

func (postCommitSilenceFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodePostCommitSilenceConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("interaction.PostCommitSilence %s config: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("post-commit silence requires the runtime clock service")
	}
	runtimeClock, ok := clockValue.(graphruntime.Clock)
	if !ok || reflectedNilInterface(runtimeClock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("post-commit silence requires the runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	scheduler := clock.Scheduler(clock.NewSystem())
	if value, _, found := mount.Services.Lookup(PostCommitSilenceSchedulerService); found {
		scheduler, ok = value.(clock.Scheduler)
		if !ok || scheduler == nil {
			return nil, fmt.Errorf("post-commit silence scheduler service has type %T", value)
		}
	}
	committed, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	create, err := mount.Ports.Output("create")
	if err != nil {
		return nil, err
	}
	state, err := mount.Ports.Output("state")
	if err != nil {
		return nil, err
	}
	outcome, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &postCommitSilenceRunner{
		instance: mount.InstanceID, config: config, clock: runtimeClock,
		sequences: sequences, scheduler: scheduler, committed: committed,
		create: create, stateOutput: state, outcomeOutput: outcome,
		resolution: mount.Resolution,
	}, nil
}

type postCommitSilenceRunner struct {
	instance  string
	config    PostCommitSilenceConfig
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator
	scheduler clock.Scheduler

	committed     element.InputPort
	create        element.OutputPort
	stateOutput   element.OutputPort
	outcomeOutput element.OutputPort
	resolution    element.ResolutionReporter

	timer  clock.Timer
	cause  element.Envelope
	commit stateelements.ObservationCommitOutcome
	state  PostCommitSilenceState
}

func (runner *postCommitSilenceRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportInteractionResolution(runner.resolution, PostCommitSilenceDescriptor()); err != nil {
		return err
	}
	commits := make(chan element.Envelope)
	fired := make(chan uint64)
	failures := make(chan error, 1)
	var receivers sync.WaitGroup
	receivers.Add(1)
	go receivePostCommitSilence(ctx, runner.committed, commits, failures, &receivers)
	defer func() {
		if runner.timer != nil {
			runner.timer.Stop()
		}
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel(err)
			return err
		case envelope := <-commits:
			if err := runner.acceptCommit(ctx, envelope, fired); err != nil {
				cancel(err)
				return err
			}
		case generation := <-fired:
			if err := runner.fire(ctx, generation); err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *postCommitSilenceRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope, fired chan<- uint64,
) error {
	commit, ok := postCommitSilencePayload(envelope.Payload)
	if !ok {
		runner.state.RefusedCount++
		return runner.publishTerminal(ctx, envelope, PostCommitSilenceOutcome{
			Kind: PostCommitSilenceRefused, Generation: runner.state.Generation,
			Code: "invalid_payload", Message: fmt.Sprintf("observation commit payload has type %T", envelope.Payload),
		})
	}
	if commit.Kind != stateelements.ObservationCommitted {
		runner.state.IgnoredCount++
		return runner.publishTerminal(ctx, envelope, PostCommitSilenceOutcome{
			Kind: PostCommitSilenceIgnored, Generation: runner.state.Generation,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "observation_not_committed", Message: "only a successful durable observation resets silence",
		})
	}
	if err := validatePostCommitSilenceCommit(envelope, commit); err != nil {
		runner.state.RefusedCount++
		return runner.publishTerminal(ctx, envelope, PostCommitSilenceOutcome{
			Kind: PostCommitSilenceRefused, Generation: runner.state.Generation,
			ContextVersion: commit.StoreVersion, TriggerItemID: commit.TriggerItemID,
			Code: "invalid_commit", Message: err.Error(),
		})
	}
	if commit.StoreVersion <= runner.state.ContextVersion {
		runner.state.IgnoredCount++
		return runner.publishTerminal(ctx, envelope, PostCommitSilenceOutcome{
			Kind: PostCommitSilenceIgnored, Generation: runner.state.Generation,
			ContextVersion: commit.StoreVersion, ContextItemID: commit.Context.StateItemID,
			TriggerItemID: commit.TriggerItemID, Code: "stale_commit",
			Message: "silence timer already observed this or a newer trajectory prefix",
		})
	}
	if runner.timer != nil {
		runner.timer.Stop()
		runner.timer = nil
		runner.state.ResetCount++
	}
	delay := time.Duration(runner.config.DelayMS) * time.Millisecond
	now := runner.clock.NowNS()
	deadline := now + uint64(delay)
	if deadline < now {
		return errors.New("post-commit silence deadline overflow")
	}
	runner.state.Generation++
	runner.state.Armed = true
	runner.state.DeadlineNS = deadline
	runner.state.ContextVersion = commit.StoreVersion
	runner.state.ContextItemID = commit.Context.StateItemID
	runner.state.TriggerItemID = commit.TriggerItemID
	runner.state.ArmedCount++
	runner.cause = envelope.Clone()
	runner.commit = commit
	generation := runner.state.Generation
	runner.timer = runner.scheduler.AfterFunc(delay, func() {
		select {
		case fired <- generation:
		case <-ctx.Done():
		}
	})
	if err := runner.publishOutcome(ctx, envelope, PostCommitSilenceOutcome{
		Kind: PostCommitSilenceArmed, Generation: generation, DeadlineNS: deadline,
		ContextVersion: commit.StoreVersion, ContextItemID: commit.Context.StateItemID,
		TriggerItemID: commit.TriggerItemID,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *postCommitSilenceRunner) fire(ctx context.Context, generation uint64) error {
	if !runner.state.Armed || generation != runner.state.Generation {
		return nil
	}
	runner.timer = nil
	runner.state.Armed = false
	runner.state.FiredCount++
	sequence, err := runner.sequences.Next(runner.instance + ".post-commit-silence-response")
	if err != nil {
		return err
	}
	responseID := fmt.Sprintf("%s:post_commit_silence:%d", runner.instance, sequence)
	version := runner.commit.Context.Prefix.Version
	envelope := runner.cause.Clone()
	envelope.Type = policyelements.ResponseCreateType()
	envelope.ItemID = responseID
	envelope.CaptureNS = runner.clock.NowNS()
	envelope.CausalParents = appendPostCommitParent(envelope.CausalParents, runner.cause.ItemID)
	envelope.CausalParents = appendPostCommitParent(envelope.CausalParents, runner.commit.Context.StateItemID)
	envelope.Payload = policyelements.ResponseCreate{
		ResponseID: responseID, ExpectedContextVersion: &version,
		ExpectedContextItemID: runner.commit.Context.StateItemID,
	}
	delivery, err := runner.create.Broadcast(ctx, envelope)
	if err != nil {
		return err
	}
	if delivery.Delivered != 1 {
		return fmt.Errorf("post-commit silence response delivered to %d lanes", delivery.Delivered)
	}
	if err := runner.publishOutcome(ctx, envelope, PostCommitSilenceOutcome{
		Kind: PostCommitSilenceFired, Generation: generation,
		DeadlineNS: runner.state.DeadlineNS, ContextVersion: version,
		ContextItemID: runner.commit.Context.StateItemID,
		TriggerItemID: runner.commit.TriggerItemID, ResponseID: responseID,
	}); err != nil {
		return err
	}
	return runner.publishState(ctx, envelope)
}

func (runner *postCommitSilenceRunner) publishTerminal(
	ctx context.Context, cause element.Envelope, outcome PostCommitSilenceOutcome,
) error {
	if err := runner.publishOutcome(ctx, cause, outcome); err != nil {
		return err
	}
	return runner.publishState(ctx, cause)
}

func (runner *postCommitSilenceRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome PostCommitSilenceOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".post-commit-silence-outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = postCommitSilenceOutcomeType
	envelope.ItemID = fmt.Sprintf("%s:post_commit_silence_outcome:%d", runner.instance, sequence)
	envelope.CausalParents = appendPostCommitParent(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	_, err = runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *postCommitSilenceRunner) publishState(
	ctx context.Context, cause element.Envelope,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".post-commit-silence-state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = postCommitSilenceStateType
	envelope.ItemID = fmt.Sprintf("%s:post_commit_silence_state:%d", runner.instance, sequence)
	envelope.CausalParents = appendPostCommitParent(envelope.CausalParents, cause.ItemID)
	envelope.Payload = runner.state
	_, err = runner.stateOutput.Broadcast(ctx, envelope)
	return err
}

func receivePostCommitSilence(
	ctx context.Context, input element.InputPort, output chan<- element.Envelope,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, _, err := input.ReceiveAny(ctx)
		if terminalInteractionReceive(ctx, err) {
			return
		}
		if err != nil {
			sendInteractionFailure(ctx, failures, err)
			return
		}
		select {
		case output <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func postCommitSilencePayload(payload any) (stateelements.ObservationCommitOutcome, bool) {
	switch value := payload.(type) {
	case stateelements.ObservationCommitOutcome:
		return value, true
	case *stateelements.ObservationCommitOutcome:
		if value != nil {
			return *value, true
		}
	}
	return stateelements.ObservationCommitOutcome{}, false
}

func validatePostCommitSilenceCommit(
	envelope element.Envelope, commit stateelements.ObservationCommitOutcome,
) error {
	for label, value := range map[string]string{
		"envelope item ID": envelope.ItemID, "session ID": envelope.SessionID,
		"trigger item ID": commit.TriggerItemID, "trajectory item ID": commit.TrajectoryItemID,
		"stream ID": commit.StreamID, "context item ID": commit.Context.StateItemID,
	} {
		if !canonicalPostCommitSilenceIdentifier(value) {
			return fmt.Errorf("%s is not canonical", label)
		}
	}
	if commit.StoreVersion == 0 || commit.SourceRevision == 0 || commit.ObservationRevision == 0 {
		return errors.New("committed observation requires positive store, source, and observation revisions")
	}
	if commit.Context.Prefix.Version != commit.StoreVersion {
		return fmt.Errorf("committed context version %d differs from store version %d",
			commit.Context.Prefix.Version, commit.StoreVersion)
	}
	const prefix = "sha256:"
	digest := commit.Context.Prefix.Digest
	if !strings.HasPrefix(digest, prefix) || len(digest) != len(prefix)+sha256.Size*2 ||
		digest != strings.ToLower(digest) {
		return errors.New("committed context digest is not canonical SHA-256")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(digest, prefix)); err != nil {
		return errors.New("committed context digest is not canonical SHA-256")
	}
	if !slices.Contains(envelope.CausalParents, commit.Context.StateItemID) {
		return errors.New("commit envelope does not causally name its context State item")
	}
	return nil
}

func canonicalPostCommitSilenceIdentifier(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func appendPostCommitParent(source []string, value string) []string {
	if value == "" || slices.Contains(source, value) {
		return source
	}
	return append(source, value)
}
