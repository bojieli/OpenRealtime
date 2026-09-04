package policy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	coreinteraction "github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type semanticAdmissionFactory struct{}

func (semanticAdmissionFactory) Descriptor() element.Descriptor { return SemanticAdmissionDescriptor() }

func (semanticAdmissionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeSemanticAdmissionConfig(source)
	return err
}

func (semanticAdmissionFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeSemanticAdmissionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("policy.SemanticAdmission %s config: %w", mount.InstanceID, err)
	}
	registryValue, registryRevision, found := mount.Services.Lookup(SemanticDeciderRegistryService)
	if !found {
		return nil, fmt.Errorf("policy.SemanticAdmission %s has no semantic decider registry service", mount.InstanceID)
	}
	registry, ok := registryValue.(*SemanticDeciderRegistry)
	if !ok || registry == nil {
		return nil, fmt.Errorf("semantic decider registry service has type %T", registryValue)
	}
	entry, err := registry.resolve(config.Decider)
	if err != nil {
		return nil, err
	}
	if config.StandingExtraction && !entry.descriptor.StandingExtraction {
		return nil, errors.New("semantic admission standing extraction requires a provider-declared extraction capability")
	}
	var media continuation.MediaResolver
	if service, _, available := mount.Services.Lookup(cognitionelements.MediaResolverService); available {
		switch typed := service.(type) {
		case continuation.MediaResolver:
			media = typed
		case func(string) (continuation.Media, error):
			media = continuation.MediaResolver(typed)
		default:
			return nil, fmt.Errorf("semantic admission media resolver service has type %T", service)
		}
		if media == nil {
			return nil, errors.New("semantic admission media resolver service is nil")
		}
	}
	if config.DirectVisualInput {
		if !entry.descriptor.Vision {
			return nil, errors.New("semantic admission direct visual input requires a vision-capable decider")
		}
		if media == nil {
			return nil, errors.New("semantic admission direct visual input requires a media resolver")
		}
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("semantic admission has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || semanticReflectedNil(clock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("semantic admission has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := semanticAdmissionPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	handle := &semanticDeciderHandle{}
	if err := mount.Lifecycle.Defer("close-semantic-decider", handle.close); err != nil {
		return nil, err
	}
	return &semanticAdmissionRunner{
		instance: mount.InstanceID, config: config, reference: config.Decider,
		entry: entry, registryRevision: registryRevision, handle: handle,
		clock: clock, sequences: sequences, resolution: mount.Resolution, ports: ports, media: media,
		contexts: make(map[semanticContextAddress]semanticContextSample),
		terminal: make(map[string]struct{}), canceledStreams: make(map[cancellationAddress]string),
		pinboard: &coreinteraction.Pinboard{},
	}, nil
}

type semanticAdmissionPorts struct {
	context, update, agentOutput, committed, create, quiet, cancel element.InputPort
	voiceCommitted, silentCommitted, voiceCreate, silentCreate     element.OutputPort
	decision, state, outcome, resolved                             element.OutputPort
}

func semanticAdmissionPortsFrom(ports element.Ports) (semanticAdmissionPorts, error) {
	if ports == nil {
		return semanticAdmissionPorts{}, errors.New("policy.SemanticAdmission has nil ports")
	}
	var result semanticAdmissionPorts
	for _, input := range []struct {
		name string
		set  *element.InputPort
	}{
		{"context", &result.context}, {"update", &result.update},
		{"agent_output", &result.agentOutput},
		{"committed", &result.committed}, {"create", &result.create},
		{"quiet", &result.quiet}, {"cancel", &result.cancel},
	} {
		port, err := ports.Input(input.name)
		if err != nil {
			return semanticAdmissionPorts{}, err
		}
		*input.set = port
	}
	for _, output := range []struct {
		name string
		set  *element.OutputPort
	}{
		{"voice_committed", &result.voiceCommitted}, {"silent_committed", &result.silentCommitted},
		{"voice_create", &result.voiceCreate}, {"silent_create", &result.silentCreate},
		{"decision", &result.decision}, {"state", &result.state},
		{"outcome", &result.outcome}, {"resolved", &result.resolved},
	} {
		port, err := ports.Output(output.name)
		if err != nil {
			return semanticAdmissionPorts{}, err
		}
		*output.set = port
	}
	return result, nil
}

type semanticContextAddress struct {
	session, item string
}

type semanticContextSample struct {
	envelope element.Envelope
	snapshot trajectory.Snapshot
}

type semanticRequest struct {
	operation string
	envelope  element.Envelope
	commit    stateelements.ObservationCommitOutcome
	create    ResponseCreate
	version   uint64
	stateItem string
	streamID  string
	sourceRev uint64
	context   *stateelements.CommittedContext
}

func (request semanticRequest) key() string {
	return request.operation + "\x00" + request.envelope.SessionID + "\x00" + request.envelope.ItemID
}

type semanticDecisionResult struct {
	request           semanticRequest
	update            SessionInvocationUpdate
	digest            string
	sample            semanticContextSample
	prefix            trajectory.Snapshot
	act               coreinteraction.Act
	policy            string
	outcome           coreinteraction.Outcome
	stage             string
	activation        string
	activationOutcome coreinteraction.Outcome
	standingCoverage  string
	coverageOutcome   coreinteraction.Outcome
	standingBefore    []coreinteraction.StandingInstruction
	standingAfter     []coreinteraction.StandingInstruction
	standingPinned    int
	standingRevoked   int
	started           uint64
	ended             uint64
	err               error
	failureCode       string
	canceled          bool
	timedOut          bool
}

type activeSemanticDecision struct {
	request     semanticRequest
	cancel      context.CancelCauseFunc
	disposition semanticDecisionDisposition
	message     string
}

type semanticDecisionDisposition string

const (
	semanticDecisionCanceled             semanticDecisionDisposition = "canceled"
	semanticDecisionEvidenceSuperseded   semanticDecisionDisposition = "evidence_superseded"
	semanticDecisionInvocationSuperseded semanticDecisionDisposition = "invocation_superseded"
)

var (
	errSemanticEvidenceSuperseded   = errors.New("semantic decision evidence superseded")
	errSemanticInvocationSuperseded = errors.New("semantic decision invocation superseded")
)

type semanticAdmissionInput struct {
	kind     string
	envelope element.Envelope
}

type semanticAdmissionRunner struct {
	instance         string
	config           SemanticAdmissionConfig
	reference        string
	entry            semanticDeciderEntry
	registryRevision uint64
	handle           *semanticDeciderHandle
	decider          SemanticDecider
	model            *coreinteraction.InteractionModel
	transcriptPolicy *coreinteraction.TranscriptEventPolicy
	extractor        coreinteraction.Extractor
	media            continuation.MediaResolver
	clock            graphruntime.Clock
	sequences        *graphruntime.SequenceAllocator
	resolution       element.ResolutionReporter
	ports            semanticAdmissionPorts

	invocation       SessionInvocationUpdate
	invocationDigest string
	contexts         map[semanticContextAddress]semanticContextSample
	contextOrder     []semanticContextAddress
	haveContext      bool
	contextSession   string
	latest           semanticContextSample
	pending          []semanticRequest
	active           *activeSemanticDecision
	terminal         map[string]struct{}
	terminalOrder    []string
	canceledStreams  map[cancellationAddress]string
	canceledOrder    []cancellationAddress
	pinboard         *coreinteraction.Pinboard
	agentOutput      coreinteraction.AgentOutput
	state            SemanticAdmissionState
	decisions        sync.WaitGroup
}

func (runner *semanticAdmissionRunner) Run(parent context.Context) error {
	decider, err := runner.entry.factory()
	if err != nil {
		return fmt.Errorf("create semantic decider %q: %w", runner.reference, err)
	}
	if semanticReflectedNil(decider) {
		return fmt.Errorf("semantic decider %q factory returned nil", runner.reference)
	}
	if err := decider.Descriptor().Validate(); err != nil {
		return errors.Join(fmt.Errorf("semantic decider %q returned invalid descriptor: %w", runner.reference, err), closeSemanticDecider(decider))
	}
	if !reflect.DeepEqual(decider.Descriptor(), runner.entry.descriptor) {
		return errors.Join(fmt.Errorf("semantic decider %q descriptor drifted", runner.reference), closeSemanticDecider(decider))
	}
	if err := runner.handle.set(decider); err != nil {
		return errors.Join(err, closeSemanticDecider(decider))
	}
	model, err := coreinteraction.NewInteractionModel(decider)
	if err != nil {
		return err
	}
	if runner.config.StandingExtraction {
		generator, ok := decider.(coreinteraction.Generator)
		if !ok || semanticReflectedNil(generator) {
			return errors.New("semantic decider declared standing extraction but does not implement interaction.Generator")
		}
		runner.extractor, err = coreinteraction.NewExtractor(generator)
		if err != nil {
			return fmt.Errorf("create semantic standing-policy extractor: %w", err)
		}
	}
	if runner.config.TranscriptEvents != nil {
		options, optionsErr := semanticTranscriptOptions(*runner.config.TranscriptEvents)
		if optionsErr != nil {
			return optionsErr
		}
		runner.transcriptPolicy, err = coreinteraction.NewTranscriptEventPolicy(decider, options)
		if err != nil {
			return fmt.Errorf("create semantic transcript-event policy: %w", err)
		}
	}
	runner.decider, runner.model = decider, model
	if err := runner.reportResolution(); err != nil {
		return err
	}
	if err := runner.publishResolved(parent); err != nil {
		return err
	}
	runner.state.TerminalMemory = runner.config.TerminalMemory
	runner.state.CancellationMemory = runner.config.CancelMemory
	runner.state.StandingMemory = runner.config.StandingMemory
	if err := runner.publishState(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}

	ctx, stop := context.WithCancelCause(parent)
	defer stop(nil)
	inputs := make(chan semanticAdmissionInput)
	failures := make(chan error, 6)
	results := make(chan semanticDecisionResult, 1)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind     string
		port     element.InputPort
		variadic bool
	}{
		{kind: "context", port: runner.ports.context}, {kind: "update", port: runner.ports.update},
		{kind: "agent_output", port: runner.ports.agentOutput},
		{kind: "committed", port: runner.ports.committed, variadic: true},
		{kind: "create", port: runner.ports.create}, {kind: "quiet", port: runner.ports.quiet},
		{kind: "cancel", port: runner.ports.cancel},
	} {
		receivers.Add(1)
		go receiveSemanticAdmission(ctx, source.kind, source.port, source.variadic, inputs, failures, &receivers)
	}
	defer func() {
		if runner.active != nil {
			runner.active.cancel(errors.New("semantic admission stopped"))
		}
		stop(nil)
		receivers.Wait()
		runner.decisions.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case result := <-results:
			active := runner.active
			runner.active = nil
			runner.state.Active = false
			if active == nil || active.request.key() != result.request.key() {
				return errors.New("semantic admission received a result without its exact active decision")
			}
			if err := runner.finishDecision(ctx, result, *active); err != nil {
				return err
			}
			if err := runner.startReadyDecision(ctx, results); err != nil {
				return err
			}
			if err := runner.publishState(ctx, result.request.envelope); err != nil {
				return err
			}
		case input := <-inputs:
			if err := runner.acceptInput(ctx, input, results); err != nil {
				return err
			}
		}
	}
}

func receiveSemanticAdmission(
	ctx context.Context, kind string, port element.InputPort, variadic bool,
	inputs chan<- semanticAdmissionInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		var envelope element.Envelope
		var err error
		if variadic {
			envelope, _, err = port.ReceiveAny(ctx)
		} else {
			envelope, err = port.Receive(ctx)
		}
		if err != nil {
			if ctx.Err() == nil && !errors.Is(err, graphruntime.ErrChannelClosed) {
				select {
				case failures <- err:
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case inputs <- semanticAdmissionInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func (runner *semanticAdmissionRunner) acceptInput(
	ctx context.Context, input semanticAdmissionInput, results chan<- semanticDecisionResult,
) error {
	switch input.kind {
	case "context":
		if err := runner.acceptContext(ctx, input.envelope); err != nil {
			return err
		}
	case "update":
		if err := runner.acceptUpdate(ctx, input.envelope); err != nil {
			return err
		}
	case "agent_output":
		if err := runner.acceptAgentOutput(ctx, input.envelope); err != nil {
			return err
		}
	case "committed":
		if err := runner.enqueueCommit(ctx, input.envelope); err != nil {
			return err
		}
	case "create", "quiet":
		if err := runner.enqueueCreate(ctx, input.kind, input.envelope); err != nil {
			return err
		}
	case "cancel":
		if err := runner.acceptCancel(ctx, input.envelope); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown semantic admission input %q", input.kind)
	}
	if err := runner.startReadyDecision(ctx, results); err != nil {
		return err
	}
	return runner.publishState(ctx, input.envelope)
}

func (runner *semanticAdmissionRunner) acceptAgentOutput(
	ctx context.Context, envelope element.Envelope,
) error {
	output, ok := semanticAgentOutputPayload(envelope.Payload)
	if !ok {
		return runner.publishRefusal(ctx, envelope, "agent_output", "invalid_agent_output",
			fmt.Sprintf("semantic admission agent output payload has type %T", envelope.Payload))
	}
	if err := validateSemanticAgentOutput(output); err != nil {
		return runner.publishRefusal(ctx, envelope, "agent_output", "invalid_agent_output", err.Error())
	}
	if output.Revision <= runner.agentOutput.Revision {
		runner.state.Ignored++
		return nil
	}
	runner.agentOutput = cloneSemanticAgentOutput(output)
	return nil
}

func (runner *semanticAdmissionRunner) acceptContext(ctx context.Context, envelope element.Envelope) error {
	snapshot, ok := semanticSnapshotPayload(envelope.Payload)
	if !ok {
		return errors.New("semantic admission received an invalid trajectory State envelope")
	}
	if snapshot.Version != uint64(len(snapshot.Items)) {
		return errors.New("semantic admission trajectory State has inconsistent version")
	}
	bootstrap := snapshot.Version == 0 && envelope.SessionID == ""
	if err := validatePolicyIdentifier("semantic State item ID", envelope.ItemID, true); err != nil {
		return err
	}
	if !bootstrap {
		if err := validatePolicyIdentifier("semantic State session ID", envelope.SessionID, true); err != nil {
			return err
		}
	}
	if runner.haveContext {
		switch {
		case runner.contextSession != "" && envelope.SessionID != runner.contextSession:
			return errors.New("semantic admission trajectory State changed session")
		case runner.contextSession == "" && envelope.SessionID == "" && snapshot.Version != 0:
			return errors.New("semantic admission non-empty trajectory State has no session")
		case snapshot.Version < runner.latest.snapshot.Version:
			return fmt.Errorf("semantic admission trajectory State regressed from %d to %d",
				runner.latest.snapshot.Version, snapshot.Version)
		case snapshot.Version == runner.latest.snapshot.Version &&
			envelope.ItemID != runner.latest.envelope.ItemID:
			return errors.New("semantic admission trajectory State changed identity at the same version")
		}
	}
	copy := semanticContextSample{envelope: envelope.Clone(), snapshot: cloneSemanticSnapshot(snapshot)}
	address := semanticContextAddress{session: envelope.SessionID, item: envelope.ItemID}
	if existing, duplicate := runner.contexts[address]; duplicate {
		if !reflect.DeepEqual(existing.snapshot, copy.snapshot) {
			return errors.New("semantic admission trajectory State equivocated at one identity")
		}
	} else {
		runner.contextOrder = append(runner.contextOrder, address)
	}
	runner.contexts[address] = copy
	runner.haveContext = true
	if envelope.SessionID != "" {
		runner.contextSession = envelope.SessionID
	}
	runner.latest = copy
	runner.state.ContextVersion = max(runner.state.ContextVersion, snapshot.Version)
	for len(runner.contextOrder) > runner.config.TerminalMemory {
		oldest := runner.contextOrder[0]
		runner.contextOrder = runner.contextOrder[1:]
		delete(runner.contexts, oldest)
	}
	return nil
}

func (runner *semanticAdmissionRunner) acceptUpdate(ctx context.Context, envelope element.Envelope) error {
	update, ok := sessionInvocationUpdatePayload(envelope.Payload)
	if !ok {
		return runner.publishRefusal(ctx, envelope, "update", "invalid_update", fmt.Sprintf("semantic admission update payload has type %T", envelope.Payload))
	}
	validated, digest, err := validateSessionInvocationUpdate(update)
	if err != nil {
		return runner.publishRefusal(ctx, envelope, "update", "invalid_update", err.Error())
	}
	if validated.Revision <= runner.invocation.Revision {
		runner.state.Ignored++
		return runner.publishOutcome(ctx, envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: "update", Code: "stale_update",
			Message: "semantic admission invocation revision must increase",
		})
	}
	runner.invocation = cloneSemanticUpdate(validated)
	runner.invocationDigest = digest
	runner.state.InvocationRevision = validated.Revision
	runner.state.InvocationDigest = digest
	if runner.active != nil {
		runner.active.disposition = semanticDecisionInvocationSuperseded
		runner.active.message = "newer session invocation superseded the active semantic decision"
		runner.active.cancel(errSemanticInvocationSuperseded)
	}
	return nil
}

func (runner *semanticAdmissionRunner) enqueueCommit(ctx context.Context, envelope element.Envelope) error {
	commit, ok := observationCommitPayload(envelope.Payload)
	if !ok {
		return runner.publishRefusal(ctx, envelope, "committed", "invalid_commit", fmt.Sprintf("semantic admission commit payload has type %T", envelope.Payload))
	}
	if commit.Kind != stateelements.ObservationCommitted {
		runner.state.Ignored++
		return runner.publishOutcome(ctx, envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: "committed", StreamID: commit.StreamID,
			SourceRevision: commit.SourceRevision, ContextVersion: commit.StoreVersion,
			Code: "observation_not_committed", Message: "semantic admission accepts only canonical observation commits",
		})
	}
	if err := validatePolicyIdentifier("semantic commit session ID", envelope.SessionID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, "committed", "invalid_commit_envelope", err.Error())
	}
	if err := validatePolicyIdentifier("semantic commit item ID", envelope.ItemID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, "committed", "invalid_commit_envelope", err.Error())
	}
	if err := validateCommit(commit); err != nil {
		return runner.publishRefusal(ctx, envelope, "committed", "invalid_commit", err.Error())
	}
	request := semanticRequest{
		operation: "committed", envelope: envelope.Clone(), commit: commit,
		version: commit.StoreVersion, stateItem: commit.Context.StateItemID,
		streamID: commit.StreamID, sourceRev: commit.SourceRevision,
	}
	return runner.enqueue(ctx, request)
}

func (runner *semanticAdmissionRunner) enqueueCreate(
	ctx context.Context, operation string, envelope element.Envelope,
) error {
	create, ok := responseCreatePayload(envelope.Payload)
	if !ok {
		return runner.publishRefusal(ctx, envelope, operation, "invalid_create", fmt.Sprintf("semantic admission create payload has type %T", envelope.Payload))
	}
	if _, err := responseCreateIdentifier(create); err != nil {
		return runner.publishRefusal(ctx, envelope, operation, "invalid_create", err.Error())
	}
	if create.TrustedPurpose != "" {
		return runner.publishRefusal(ctx, envelope, operation, "invalid_create",
			"transport response create cannot supply a trusted runtime purpose")
	}
	if err := validatePolicyIdentifier("semantic response session ID", envelope.SessionID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, operation, "invalid_create_envelope", err.Error())
	}
	if err := validatePolicyIdentifier("semantic response item ID", envelope.ItemID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, operation, "invalid_create_envelope", err.Error())
	}
	request := semanticRequest{
		operation: operation, envelope: envelope.Clone(), create: create,
		version: *create.ExpectedContextVersion, stateItem: create.ExpectedContextItemID,
		context: cloneResponseCreateContext(create.CommittedContext),
	}
	return runner.enqueue(ctx, request)
}

func (runner *semanticAdmissionRunner) enqueue(ctx context.Context, request semanticRequest) error {
	if runner.isTerminal(request.key()) {
		runner.state.Ignored++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			Code: "terminal_replay", Message: "semantic admission trigger is already terminal",
		})
	}
	if runner.requestInFlight(request.key()) {
		runner.state.Ignored++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			Code: "duplicate_pending", Message: "semantic admission trigger is already pending or active",
		})
	}
	if reason, canceled := runner.takePreCancel(request); canceled {
		runner.rememberTerminal(request.key())
		runner.state.Canceled++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionCanceled, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			Code: "canceled_before_decision", Message: reason,
		})
	}
	if err := runner.supersedeOlderRequests(ctx, request); err != nil {
		return err
	}
	if len(runner.pending) >= runner.config.MaxPending {
		runner.rememberTerminal(request.key())
		runner.state.Refused++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionRefused, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			Code: "pending_capacity", Message: "semantic admission pending capacity is exhausted",
		})
	}
	runner.pending = append(runner.pending, request)
	runner.state.Pending = len(runner.pending)
	return nil
}

func (runner *semanticAdmissionRunner) acceptCancel(ctx context.Context, envelope element.Envelope) error {
	cancel, ok := generationCancelPayload(envelope.Payload)
	if !ok || (cancel.StreamID == "") == (cancel.GenerationID == "") {
		return runner.publishRefusal(ctx, envelope, "cancel", "invalid_cancel", "semantic admission cancel requires exactly one stream_id or generation_id")
	}
	reason := boundedPolicyReason(cancel.Reason)
	if reason == "" {
		reason = "semantic decision canceled"
	}
	if cancel.StreamID == "" {
		runner.state.Ignored++
		return runner.publishOutcome(ctx, envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: "cancel", Code: "generation_not_owned",
			Message: "semantic admission owns pre-generation stream decisions, not emitted generation IDs",
		})
	}
	if err := validatePolicyIdentifier("semantic cancel session ID", envelope.SessionID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, "cancel", "invalid_cancel", err.Error())
	}
	if err := validatePolicyIdentifier("semantic cancel stream ID", cancel.StreamID, true); err != nil {
		return runner.publishRefusal(ctx, envelope, "cancel", "invalid_cancel", err.Error())
	}
	matched := false
	if runner.active != nil && semanticSameStream(runner.active.request, envelope.SessionID, cancel.StreamID) {
		matched = true
		runner.active.disposition = semanticDecisionCanceled
		runner.active.message = reason
		runner.active.cancel(errors.New(reason))
	}
	kept := runner.pending[:0]
	for _, pending := range runner.pending {
		if !semanticSameStream(pending, envelope.SessionID, cancel.StreamID) {
			kept = append(kept, pending)
			continue
		}
		matched = true
		runner.rememberTerminal(pending.key())
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, pending.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionCanceled, Operation: pending.operation,
			StreamID: pending.streamID, SourceRevision: pending.sourceRev, ContextVersion: pending.version,
			Code: "canceled_before_decision", Message: reason,
		}); err != nil {
			return err
		}
	}
	runner.pending = kept
	runner.state.Pending = len(kept)
	if !matched {
		runner.recordPreCancel(cancellationAddress{streamID: cancel.StreamID, sessionID: envelope.SessionID}, reason)
	}
	runner.state.Ignored++
	return runner.publishOutcome(ctx, envelope, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionIgnored, Operation: "cancel", StreamID: cancel.StreamID,
		Code: "cancel_recorded", Message: reason,
	})
}

func (runner *semanticAdmissionRunner) startReadyDecision(
	parent context.Context, results chan<- semanticDecisionResult,
) error {
	if runner.active != nil || len(runner.pending) == 0 {
		return nil
	}
	for index, request := range runner.pending {
		update, digest, sample, prefix, ready, err := runner.inputsFor(request)
		if err != nil {
			runner.pending = append(runner.pending[:index], runner.pending[index+1:]...)
			runner.state.Pending = len(runner.pending)
			runner.rememberTerminal(request.key())
			runner.state.Refused++
			return runner.publishOutcome(parent, request.envelope, SemanticAdmissionOutcome{
				Kind: SemanticAdmissionRefused, Operation: request.operation,
				StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
				Code: "invalid_context", Message: err.Error(),
			})
		}
		if !ready {
			continue
		}
		runner.pending = append(runner.pending[:index], runner.pending[index+1:]...)
		runner.state.Pending = len(runner.pending)
		decisionCtx, cancel := context.WithCancelCause(parent)
		runner.active = &activeSemanticDecision{request: request, cancel: cancel}
		runner.state.Active = true
		standing := runner.pinboard.InForce()
		agentOutput := cloneSemanticAgentOutput(runner.agentOutput)
		runner.decisions.Add(1)
		go func() {
			defer runner.decisions.Done()
			runner.decide(
				decisionCtx, request, update, digest, sample, prefix, standing, agentOutput, results,
			)
		}()
		return nil
	}
	return nil
}

func (runner *semanticAdmissionRunner) requestInFlight(key string) bool {
	if runner.active != nil && runner.active.request.key() == key {
		return true
	}
	for _, pending := range runner.pending {
		if pending.key() == key {
			return true
		}
	}
	return false
}

func semanticSameStream(request semanticRequest, sessionID, streamID string) bool {
	return streamID != "" && request.streamID == streamID && request.envelope.SessionID == sessionID
}

func (runner *semanticAdmissionRunner) supersedeOlderRequests(
	ctx context.Context, request semanticRequest,
) error {
	if request.streamID == "" || request.sourceRev == 0 {
		return nil
	}
	if runner.active != nil && semanticSameStream(
		runner.active.request, request.envelope.SessionID, request.streamID,
	) && runner.active.request.sourceRev < request.sourceRev {
		runner.active.disposition = semanticDecisionEvidenceSuperseded
		runner.active.message = "newer semantic evidence superseded the active decision"
		runner.active.cancel(errSemanticEvidenceSuperseded)
	}
	kept := runner.pending[:0]
	for _, pending := range runner.pending {
		if !semanticSameStream(pending, request.envelope.SessionID, request.streamID) ||
			pending.sourceRev >= request.sourceRev {
			kept = append(kept, pending)
			continue
		}
		runner.rememberTerminal(pending.key())
		runner.state.Canceled++
		if err := runner.publishOutcome(ctx, pending.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionCanceled, Operation: pending.operation,
			StreamID: pending.streamID, SourceRevision: pending.sourceRev,
			ContextVersion: pending.version, Code: "decision_superseded",
			Message: "newer semantic evidence superseded the pending decision",
		}); err != nil {
			return err
		}
	}
	runner.pending = kept
	runner.state.Pending = len(kept)
	return nil
}

func (runner *semanticAdmissionRunner) recordPreCancel(address cancellationAddress, reason string) {
	if _, found := runner.canceledStreams[address]; !found {
		runner.canceledOrder = append(runner.canceledOrder, address)
	}
	runner.canceledStreams[address] = reason
	for len(runner.canceledOrder) > runner.config.CancelMemory {
		oldest := runner.canceledOrder[0]
		runner.canceledOrder = runner.canceledOrder[1:]
		delete(runner.canceledStreams, oldest)
	}
}

func (runner *semanticAdmissionRunner) takePreCancel(request semanticRequest) (string, bool) {
	if request.streamID == "" {
		return "", false
	}
	address := cancellationAddress{
		streamID: request.streamID, sessionID: request.envelope.SessionID,
	}
	reason, found := runner.canceledStreams[address]
	if !found {
		return "", false
	}
	delete(runner.canceledStreams, address)
	if index := slices.Index(runner.canceledOrder, address); index >= 0 {
		runner.canceledOrder = slices.Delete(runner.canceledOrder, index, index+1)
	}
	return reason, true
}

func (runner *semanticAdmissionRunner) inputsFor(
	request semanticRequest,
) (SessionInvocationUpdate, string, semanticContextSample, trajectory.Snapshot, bool, error) {
	if runner.invocation.Revision == 0 {
		return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, nil
	}
	address := semanticContextAddress{session: request.envelope.SessionID, item: request.stateItem}
	sample, exact := runner.contexts[address]
	if request.operation == "committed" {
		candidate := sample
		if !exact && runner.latest.envelope.SessionID == request.envelope.SessionID &&
			runner.latest.snapshot.Version >= request.version {
			candidate = runner.latest
		}
		if candidate.snapshot.Version < request.version {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, nil
		}
		prefix, err := trajectory.Prefix(candidate.snapshot, request.commit.Context.Prefix)
		if err != nil {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, err
		}
		return cloneSemanticUpdate(runner.invocation), runner.invocationDigest, candidate, prefix, true, nil
	}
	if !exact {
		if request.context == nil || runner.latest.envelope.SessionID != request.envelope.SessionID ||
			runner.latest.snapshot.Version < request.context.Prefix.Version {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, nil
		}
		prefix, err := trajectory.Prefix(runner.latest.snapshot, request.context.Prefix)
		if err != nil {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false,
				fmt.Errorf("response committed context: %w", err)
		}
		return cloneSemanticUpdate(runner.invocation), runner.invocationDigest, runner.latest,
			prefix, true, nil
	}
	if sample.snapshot.Version != request.version {
		return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false,
			fmt.Errorf("response context version %d disagrees with exact State version %d", request.version, sample.snapshot.Version)
	}
	prefix := cloneSemanticSnapshot(sample.snapshot)
	if request.context != nil {
		var err error
		prefix, err = trajectory.Prefix(sample.snapshot, request.context.Prefix)
		if err != nil {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false,
				fmt.Errorf("response committed context: %w", err)
		}
	}
	return cloneSemanticUpdate(runner.invocation), runner.invocationDigest, sample, prefix, true, nil
}

func (runner *semanticAdmissionRunner) decide(
	ctx context.Context, request semanticRequest, update SessionInvocationUpdate, digest string,
	sample semanticContextSample, prefix trajectory.Snapshot,
	standing []coreinteraction.StandingInstruction, agentOutput coreinteraction.AgentOutput,
	results chan<- semanticDecisionResult,
) {
	started := runner.clock.NowNS()
	timeout := time.Duration(runner.entry.descriptor.DecisionTimeoutMS) * time.Millisecond
	decisionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	situation, err := runner.situationWithStanding(
		decisionCtx, request, update, prefix, standing, agentOutput,
	)
	failure := ""
	if err != nil {
		failure = "visual_evidence_failed"
	}
	var act coreinteraction.Act
	var outcome coreinteraction.Outcome
	policyName := runner.model.Name()
	if runner.transcriptPolicy != nil &&
		(situation.TranscriptEvent == coreinteraction.TranscriptPartial ||
			situation.TranscriptEvent == coreinteraction.TranscriptFinal) {
		policyName = runner.transcriptPolicy.Name()
	}
	stage := "primary"
	activation := ""
	var activationOutcome coreinteraction.Outcome
	standingCoverage := ""
	var coverageOutcome coreinteraction.Outcome
	standingAfter := slices.Clone(standing)
	standingPinned, standingRevoked := 0, 0
	if err == nil {
		err = validateSemanticSituation(situation)
		if err != nil {
			failure = "invalid_evidence"
		}
	}
	// A completed utterance can arrive after every prior voice run is already
	// terminal, so the overlap policy has no active work to classify or cancel.
	// Screen addressing before the utterance can acquire either generation or
	// tool authority, and before an extractor can mutate durable policy memory.
	// A distinct result is required: ordinary wait also describes legitimate
	// policy setup and cannot safely prove that the words belong to somebody
	// else's conversation.
	activationChecked := false
	skipStandingMutation := false
	if err == nil && request.operation == "committed" &&
		runner.config.VerifyVoiceActivation && semanticActivationEvidence(situation) {
		current := currentSemanticItem(request, prefix)
		if semanticExtractableObservation(current) {
			activationOutcome, err = runner.verifyVoiceActivation(decisionCtx, situation)
			if err != nil {
				failure = "voice_activation_failed"
			} else {
				activationChecked = true
				activation = strings.TrimSpace(activationOutcome.Option)
				confident := semanticActivationConfident(
					activationOutcome, runner.config.MinimumActivationConfidence,
				)
				if activation == semanticVoiceAddressedElsewhere {
					// Even an uncertain other-addressee verdict cannot authorize a
					// durable pin or revocation. Confidence only decides whether this
					// guard may also veto an otherwise strong immediate answer.
					skipStandingMutation = true
					if confident {
						act = coreinteraction.ActStaySilent
						stage = "voice_addressing"
					}
				}
			}
		}
	}
	if err == nil && stage == "primary" && !skipStandingMutation &&
		runner.extractor != nil && request.operation == "committed" {
		current := currentSemanticItem(request, prefix)
		if semanticExtractableObservation(current) {
			utterance := semanticStandingUtterance(situation, current)
			var extraction coreinteraction.Extraction
			extraction, err = runner.extractor.Extract(
				decisionCtx, slices.Clone(standing),
				semanticRecentBefore(prefix.Items, current.ID, runner.config.RecentLines),
				utterance,
			)
			if err != nil {
				failure = "standing_extraction_failed"
			} else {
				standingAfter, standingPinned, standingRevoked, err = applySemanticExtraction(
					standing, extraction, request.sourceRev, runner.clock.NowNS(), runner.config.StandingMemory,
				)
				if err != nil {
					failure = "standing_memory_exhausted"
				} else if len(extraction.Pins) > 0 {
					coverageOutcome, err = runner.verifyStandingCoverage(decisionCtx, utterance, extraction.Pins)
					if err != nil {
						failure = "standing_coverage_failed"
					} else {
						standingCoverage = strings.TrimSpace(coverageOutcome.Option)
						if standingCoverage == semanticStandingCovered ||
							standingCoverage == semanticStandingAdditional && coverageOutcome.Measured &&
								coverageOutcome.Confidence < runner.config.MinimumActivationConfidence {
							act = coreinteraction.ActStaySilent
							stage = "standing_coverage"
						}
					}
				}
			}
		}
	}
	if err == nil && stage == "primary" {
		act, outcome, err = runner.decideAct(decisionCtx, request, situation)
		if err == nil {
			options := runner.semanticActOptions(request, situation)
			err = validateSemanticOutcome(outcome, options)
			if err != nil {
				failure = "invalid_decider_outcome"
			}
		}
	}
	// Speak-through is the act for carrying out a standing arrangement while
	// another person keeps the floor. Without an arrangement in force, a live
	// transcript that merely describes one is not authority to speak: the model
	// repeatedly selected this act halfway through "count the animals as I
	// mention them" and made the setup sentence the first count. Interrupt is
	// deliberately not constrained here; a deployment contract can itself
	// authorize an immediate correction without a policy spoken in-session.
	if err == nil && stage == "primary" && act == coreinteraction.ActSpeakThrough {
		switch {
		case len(standing) == 0:
			act = coreinteraction.ActStaySilent
			stage = "standing_authority"
		case runner.extractor != nil && situation.TranscriptEvent == coreinteraction.TranscriptPartial &&
			!runner.extractor.HasArrived(decisionCtx, slices.Clone(standing), situation.Heard):
			// A standing policy grants authority to react to its future trigger,
			// not to speak while the user is still refining that policy. Ask the
			// extractor's deliberately narrow trigger question against only the
			// current partial, rather than letting earlier setup words satisfy it.
			act = coreinteraction.ActStaySilent
			stage = "standing_trigger"
		}
	}
	if err == nil && stage == "primary" && runner.config.VerifyVoiceActivation {
		answerAvailable := slices.Contains(situation.AvailableActs(), coreinteraction.ActAnswer)
		verify := act == coreinteraction.ActAnswer ||
			act == coreinteraction.ActStaySilent && len(standing) > 0 && answerAvailable
		if verify && semanticActivationEvidence(situation) {
			if !activationChecked {
				activationOutcome, err = runner.verifyVoiceActivation(decisionCtx, situation)
				if err != nil {
					failure = "voice_activation_failed"
				} else {
					activationChecked = true
					activation = strings.TrimSpace(activationOutcome.Option)
				}
			}
			if err == nil {
				activation = strings.TrimSpace(activationOutcome.Option)
				confident := semanticActivationConfident(
					activationOutcome, runner.config.MinimumActivationConfidence,
				)
				switch {
				case activation == semanticVoiceAddressedElsewhere && confident &&
					act == coreinteraction.ActAnswer:
					act = coreinteraction.ActStaySilent
					stage = "voice_addressing"
				case activation == semanticVoiceWait && confident && act == coreinteraction.ActAnswer:
					act = coreinteraction.ActStaySilent
					stage = "voice_activation"
				case activation == semanticVoiceConditionMet && confident &&
					act == coreinteraction.ActStaySilent && answerAvailable:
					// A final transcript is the bounded recovery point for a standing
					// condition the primary act missed. The guard can only select an
					// already executable Answer; it cannot generate content or widen
					// the graph's act set.
					act = coreinteraction.ActAnswer
					stage = "voice_activation"
				}
			}
		}
	}
	if err == nil && stage == "primary" && runner.config.VerifySilentAction &&
		act == coreinteraction.ActActSilently {
		current := currentSemanticItem(request, prefix)
		if semanticSpokenObservation(current) {
			activationOutcome, err = runner.verifySilentAction(decisionCtx, situation)
			if err != nil {
				failure = "silent_action_activation_failed"
			} else {
				activation = strings.TrimSpace(activationOutcome.Option)
				confident := semanticActivationConfident(
					activationOutcome, runner.config.MinimumActivationConfidence,
				)
				if activation != semanticSilentActionReady || !confident {
					act = coreinteraction.ActStaySilent
					stage = "silent_action_activation"
				}
			}
		}
	}
	if err == nil && stage == "primary" && act != coreinteraction.ActStaySilent &&
		runner.config.MinimumActivationConfidence > 0 && outcome.Measured &&
		outcome.Confidence < runner.config.MinimumActivationConfidence {
		act = coreinteraction.ActStaySilent
		stage = "confidence_guard"
	}
	canceled := errors.Is(decisionCtx.Err(), context.Canceled)
	timedOut := errors.Is(decisionCtx.Err(), context.DeadlineExceeded)
	if canceled || timedOut {
		if cause := context.Cause(decisionCtx); cause != nil {
			err = cause
		} else {
			err = decisionCtx.Err()
		}
	}
	result := semanticDecisionResult{
		request: request, update: update, digest: digest, sample: sample, prefix: prefix,
		act: act, policy: policyName, outcome: outcome, stage: stage, activation: activation,
		activationOutcome: activationOutcome, standingCoverage: standingCoverage,
		coverageOutcome: coverageOutcome, standingBefore: standing, standingAfter: standingAfter,
		standingPinned: standingPinned, standingRevoked: standingRevoked,
		started: started, ended: runner.clock.NowNS(), err: err,
		failureCode: failure, canceled: canceled, timedOut: timedOut,
	}
	select {
	case results <- result:
	case <-ctx.Done():
		select {
		case results <- result:
		default:
		}
	}
}

func validateSemanticSituation(situation coreinteraction.Situation) error {
	total := len(situation.Render())
	for _, image := range situation.Seeing {
		if len(image.Bytes) > maximumSemanticTextBytes-total {
			return fmt.Errorf("semantic decision evidence exceeds %d bytes", maximumSemanticTextBytes)
		}
		total += len(image.Bytes)
	}
	if total > maximumSemanticTextBytes {
		return fmt.Errorf("semantic decision evidence exceeds %d bytes", maximumSemanticTextBytes)
	}
	return nil
}

func (runner *semanticAdmissionRunner) decideAct(
	ctx context.Context, request semanticRequest, situation coreinteraction.Situation,
) (coreinteraction.Act, coreinteraction.Outcome, error) {
	if request.operation == "committed" && runner.transcriptPolicy != nil &&
		(situation.TranscriptEvent == coreinteraction.TranscriptPartial ||
			situation.TranscriptEvent == coreinteraction.TranscriptFinal) {
		return runner.transcriptPolicy.Decide(ctx, situation.TranscriptEvent, situation)
	}
	if situation.Decidable() || request.operation == "committed" {
		return runner.model.Decide(ctx, situation)
	}
	// Explicit response.create and PostCommitSilence triggers are themselves
	// new control evidence even when the latest durable item is plumbing, such
	// as a tool result, rather than an observation that Situation.Decidable
	// recognizes. The enumerated decider must inspect the bounded recent
	// conversation and choose the branch; treating these triggers as inertia
	// silently drops tool-result continuations and explicit client requests.
	// Calling the narrow Decider directly bypasses only InteractionModel's
	// cheap "no evidence" short-circuit. It preserves the same rendered
	// situation, prompt, and executable act set, and still cannot generate text
	// or tools.
	acts := situation.AvailableActs()
	switch len(acts) {
	case 0:
		return coreinteraction.ActStaySilent, coreinteraction.Outcome{},
			errors.New("semantic admission situation has no executable act")
	case 1:
		return acts[0], coreinteraction.Outcome{Index: 0, Option: string(acts[0])}, nil
	}
	options := make([]string, len(acts))
	for index, act := range acts {
		options[index] = string(act)
	}
	outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
		Prompt: coreinteraction.Instruction, Options: options, Evidence: situation.Render(),
	})
	if err != nil {
		return coreinteraction.ActStaySilent, coreinteraction.Outcome{}, err
	}
	chosen := coreinteraction.Act(strings.TrimSpace(outcome.Option))
	for _, act := range acts {
		if chosen == act {
			return chosen, outcome, nil
		}
	}
	return coreinteraction.ActStaySilent, outcome,
		fmt.Errorf("interaction model chose %q, which is not available here", outcome.Option)
}

func (runner *semanticAdmissionRunner) semanticActOptions(
	request semanticRequest, situation coreinteraction.Situation,
) []string {
	if request.operation == "committed" && runner.transcriptPolicy != nil &&
		(situation.TranscriptEvent == coreinteraction.TranscriptPartial ||
			situation.TranscriptEvent == coreinteraction.TranscriptFinal) {
		situation.AllowedActs = runner.transcriptPolicy.AllowedActs(situation.TranscriptEvent)
	}
	return semanticActOptions(situation)
}

func semanticActOptions(situation coreinteraction.Situation) []string {
	acts := situation.AvailableActs()
	options := make([]string, len(acts))
	for index, act := range acts {
		options[index] = string(act)
	}
	return options
}

func validateSemanticOutcome(outcome coreinteraction.Outcome, options []string) error {
	chosen := strings.TrimSpace(outcome.Option)
	index := slices.Index(options, chosen)
	if index < 0 {
		return fmt.Errorf("semantic policy chose %q, which is not one of the exact options", outcome.Option)
	}
	if outcome.Index != index {
		return fmt.Errorf(
			"semantic policy option %q reports index %d, want %d", chosen, outcome.Index, index,
		)
	}
	if math.IsNaN(outcome.Confidence) || math.IsInf(outcome.Confidence, 0) ||
		outcome.Confidence < 0 || outcome.Confidence > 1 {
		return errors.New("semantic policy confidence must be finite and between 0 and 1")
	}
	return nil
}

func semanticActivationConfident(outcome coreinteraction.Outcome, minimum float64) bool {
	return !outcome.Measured || minimum == 0 || outcome.Confidence >= minimum
}

const (
	semanticStandingCovered         = "covered"
	semanticStandingAdditional      = "additional-work"
	semanticVoiceConditionMet       = "condition-met"
	semanticVoiceDirectRequest      = "direct-request"
	semanticVoiceAddressedElsewhere = "addressed-elsewhere"
	semanticVoiceWait               = "wait"
	semanticSilentActionReady       = "action-ready"
	semanticSilentActionWait        = "wait"
)

const semanticStandingCoverageInstruction = "The policy extractor listed the standing policies established by one utterance. " +
	"Decide whether the utterance contains any separate request due now OUTSIDE those listed policies. " +
	"covered means every request in the utterance is one of the listed standing policies or merely setup, preference, or context for them; " +
	"describing the trigger inside a listed policy does not make that trigger happen. " +
	"additional-work means there is also a separate complete question, immediate command, or report of an already established trigger that is not part of a listed policy. " +
	"Reply with one label only. Examples: 'I want fish tonight; order when the waiter names something that fits' is covered by the listed ordering policy. " +
	"'From now on answer briefly; what is the capital of France?' has additional-work outside the brevity policy."

const semanticVoiceActivationInstruction = "You are an activation guard, not a conversational agent. " +
	"Classify whether the CURRENT EVIDENCE creates a reason for a voice assistant to answer now under the AGENT CONTRACT and any STANDING POLICIES. " +
	"Current evidence may be a completed utterance, an image or visual observation, or elapsed silence explicitly named by a standing policy. " +
	"condition-met means the contract or a standing policy says to answer when some fact occurs, and the current evidence proves that fact now. " +
	"direct-request means the current evidence directly asks a complete question or requests work that should start now, not later. " +
	"addressed-elsewhere means the current speech is explicitly addressed to another person by name, title, or other vocative, whether it is a question, " +
	"request, answer, or statement. A role or title used as a vocative, especially at the start of an utterance, identifies its recipient just as a personal name does; " +
	"do not reinterpret that person's question as addressed to the assistant. Before choosing addressed-elsewhere, inspect the AGENT CONTRACT for the assistant's explicit identity. " +
	"If it says 'You are X' or otherwise names the assistant as X, speech addressed to X is addressed to this assistant and a complete request is direct-request; " +
	"this explicit identity rule takes precedence over the name or title vocative rule. Otherwise choose addressed-elsewhere unless recent conversation establishes that addressee as this assistant. " +
	"A vocative directly calls to a recipient and is often a name, role, or title phrase set off by a comma at the beginning: 'Officer, ...' and 'Doctor Smith, ...' are addressed-elsewhere when the contract does not identify the assistant that way, even though a question follows. " +
	"Merely mentioning a person is not a vocative. A name used as the object of a verb remains part of a request to the current assistant: 'Can you tell Tim the printer is jammed?' is direct-request. " +
	"Never assume or adopt a named person's identity merely because " +
	"the current utterance addresses them. wait means neither of the other labels: a future condition is merely being described or requested, an applicable " +
	"condition has not occurred, the evidence is narration, or the current evidence only continues or refines the setup of a standing policy without satisfying it. " +
	"A direct topic change with no other addressee remains direct-request. " +
	"Reply with one label only. Examples: contract 'correct a date that contradicts the third'; current 'we do design review next week' is wait; " +
	"the same contract with current 'ship by the thirteenth' is condition-met. Standing policy 'count animals as they are mentioned'; " +
	"current 'say the count out loud' is wait, while current 'a heron landed' is condition-met. Standing policy 'tell me when the build finishes'; " +
	"an image still showing the build in progress is wait, while an image proving it finished is condition-met. Contract 'answer briefly'; " +
	"current 'what is the capital of France' is direct-request. Contract 'You are Alex, a support assistant'; current 'Alex, please help with the printer' is direct-request. " +
	"Standing policy 'translate everything a Mandarin-speaking colleague says into English'; current colleague speech '你好，很高兴见到你' is condition-met, not a direct request and not wait. " +
	"Current 'Tim, the printer is jammed again - help?', 'Officer, is this the right form?', and 'Doctor Smith, could you check this?' are addressed-elsewhere when those are other people; " +
	"current 'Can we talk about something else?' is direct-request."

const semanticSilentActionInstruction = "You are a silent-action activation guard, not an agent and not a tool chooser. " +
	"Decide whether the CURRENT instant fully grounds some action using an AVAILABLE SILENT TOOL now. " +
	"action-ready means the current evidence supplies the event, option, or parameters needed to use a listed tool now under the AGENT CONTRACT, " +
	"standing policies, and recent conversation. wait means it does not: the person is still describing a goal, a recording has not offered a matching option, " +
	"or an offered option conflicts with the requested goal. Never invent a missing option or parameter. Reply with one label only. " +
	"Examples: tool 'press_key'; person says 'call support and find my order' is wait. The recording says 'press one for billing' while the goal is order status is wait. " +
	"The recording says 'press two for order status' while that goal stands is action-ready."

func (runner *semanticAdmissionRunner) verifyStandingCoverage(
	ctx context.Context, utterance string, policies []coreinteraction.StandingInstruction,
) (coreinteraction.Outcome, error) {
	var evidence strings.Builder
	evidence.WriteString("UTTERANCE:\n")
	evidence.WriteString(strings.TrimSpace(utterance))
	evidence.WriteString("\n\nEXTRACTED STANDING POLICIES:\n")
	for _, policy := range policies {
		if text := strings.TrimSpace(policy.Text); text != "" {
			evidence.WriteString("- ")
			evidence.WriteString(text)
			evidence.WriteByte('\n')
		}
	}
	options := []string{semanticStandingCovered, semanticStandingAdditional}
	outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
		Prompt:   semanticStandingCoverageInstruction,
		Options:  options,
		Evidence: evidence.String(),
	})
	if err == nil {
		err = validateSemanticOutcome(outcome, options)
	}
	return outcome, err
}

func (runner *semanticAdmissionRunner) verifyVoiceActivation(
	ctx context.Context, situation coreinteraction.Situation,
) (coreinteraction.Outcome, error) {
	options := []string{
		semanticVoiceConditionMet, semanticVoiceDirectRequest,
		semanticVoiceAddressedElsewhere, semanticVoiceWait,
	}
	activation := semanticVoiceActivationSituation(situation)
	outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
		Prompt: semanticVoiceActivationInstruction, Options: options,
		Evidence: activation.Render(), Images: cloneSemanticImages(activation.Seeing),
	})
	if err == nil {
		err = validateSemanticOutcome(outcome, options)
	}
	return outcome, err
}

// semanticVoiceActivationSituation makes the activation guard's evidence
// boundary match its prompt. Recent conversation is useful to the primary
// interaction decision, but an earlier occurrence must never satisfy a
// condition for the current event. Standing instructions and the agent
// contract already carry the durable context this guard is authorized to
// enforce; the current heard/seen/quiet evidence is kept intact.
func semanticVoiceActivationSituation(situation coreinteraction.Situation) coreinteraction.Situation {
	activation := situation
	activation.Recent = nil
	if strings.TrimSpace(activation.Heard) != "" {
		activation.HeardSince = activation.Heard
	}
	return activation
}

func (runner *semanticAdmissionRunner) verifySilentAction(
	ctx context.Context, situation coreinteraction.Situation,
) (coreinteraction.Outcome, error) {
	options := []string{semanticSilentActionReady, semanticSilentActionWait}
	outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
		Prompt: semanticSilentActionInstruction, Options: options, Evidence: situation.Render(),
	})
	if err == nil {
		err = validateSemanticOutcome(outcome, options)
	}
	return outcome, err
}

func applySemanticExtraction(
	existing []coreinteraction.StandingInstruction, extraction coreinteraction.Extraction,
	turn, now uint64, maximum int,
) ([]coreinteraction.StandingInstruction, int, int, error) {
	if turn == 0 {
		return nil, 0, 0, errors.New("semantic standing extraction requires a positive source revision")
	}
	board := semanticPinboard(existing)
	// Existing turn-scoped policies govern this decision and then expire.
	// Policies extracted from the current completed utterance are installed
	// afterwards, so a turn-scoped instruction can govern the next turn once
	// without disappearing at the instant it was noticed.
	board.EndTurn()
	beforeRevocation := len(board.InForce())
	for _, revoked := range extraction.Revokes {
		board.Revoke(revoked)
	}
	revoked := beforeRevocation - len(board.InForce())
	pins := slices.Clone(extraction.Pins)
	for index := range pins {
		pins[index].SetNS = now
	}
	pinned := board.SetForTurn(turn, pins)
	result := board.InForce()
	if len(result) > maximum {
		return nil, 0, 0, fmt.Errorf(
			"semantic standing-policy memory requires %d entries, maximum is %d", len(result), maximum,
		)
	}
	return result, pinned, revoked, nil
}

func (runner *semanticAdmissionRunner) situation(
	ctx context.Context, request semanticRequest, update SessionInvocationUpdate, prefix trajectory.Snapshot,
) (coreinteraction.Situation, error) {
	return runner.situationWithStanding(
		ctx, request, update, prefix, nil, cloneSemanticAgentOutput(runner.agentOutput),
	)
}

func (runner *semanticAdmissionRunner) situationWithStanding(
	ctx context.Context, request semanticRequest, update SessionInvocationUpdate, prefix trajectory.Snapshot,
	standing []coreinteraction.StandingInstruction, agentOutput coreinteraction.AgentOutput,
) (coreinteraction.Situation, error) {
	board := semanticPinboard(standing)
	state := coreinteraction.Situation{
		Contract: update.Invocation.Instruction,
		Recent:   coreinteraction.RecentLines(prefix.Items, runner.config.RecentLines),
		Pins:     board.Lines(semanticNowNS(runner.clock)),
		AllowedActs: []coreinteraction.Act{
			coreinteraction.ActStaySilent, coreinteraction.ActAnswer,
		},
		AgentSpeaking: agentOutput.Active,
		AgentSaying:   agentOutput.Saying,
		AgentOutputProtected: slices.Contains(
			agentOutput.ProtectedStreams, request.streamID,
		),
		InFlight: agentOutput.InFlight,
	}
	for _, policy := range standing {
		state.Restricted = state.Restricted || policy.Restricting
	}
	for _, tool := range update.Invocation.Tools {
		line := tool.Name
		if strings.TrimSpace(tool.Description) != "" {
			line += " - " + strings.TrimSpace(tool.Description)
		}
		state.Tools = append(state.Tools, line)
	}
	if len(state.Tools) > 0 {
		state.AllowedActs = append(state.AllowedActs, coreinteraction.ActActSilently)
	}
	if request.operation == "quiet" {
		state.Silence = "15s"
		const quiet = 15 * time.Second
		for _, policy := range standing {
			if policy.After > 0 && policy.Due(quiet) {
				state.Quiet = true
				break
			}
		}
		if !state.Quiet {
			// PostCommitSilence is a generic graph clock: it fires after every
			// durable observation and carries no authority to invent a periodic
			// turn. Only a pinned, due silence policy turns that tick into
			// evidence. With none, silence is the sole executable outcome.
			state.AllowedActs = []coreinteraction.Act{coreinteraction.ActStaySilent}
		}
		return state, nil
	}
	if len(prefix.Items) == 0 {
		return state, nil
	}
	if runner.config.DirectVisualInput {
		selected := continuation.LatestMediaHandles(prefix.Items)
		resolved := make(map[string]struct{}, len(selected))
		for _, item := range prefix.Items {
			if item.Kind != trajectory.KindObservation || item.Observation == nil {
				continue
			}
			for _, reference := range item.Observation.Media {
				if _, latest := selected[reference.Handle]; !latest {
					continue
				}
				if _, duplicate := resolved[reference.Handle]; duplicate {
					continue
				}
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(reference.MIMEType)), "image/") {
					continue
				}
				media, err := resolveSemanticMedia(ctx, runner.media, reference.Handle)
				if err != nil {
					return coreinteraction.Situation{}, fmt.Errorf(
						"resolve semantic visual evidence %q: %w", reference.Handle, err,
					)
				}
				if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(media.MIMEType)), "image/") {
					return coreinteraction.Situation{}, fmt.Errorf(
						"resolved semantic visual evidence %q has MIME type %q", reference.Handle, media.MIMEType,
					)
				}
				state.Seeing = append(state.Seeing, coreinteraction.Image{
					MIMEType: media.MIMEType, Bytes: media.Bytes,
				})
				resolved[reference.Handle] = struct{}{}
			}
		}
	}
	current := currentSemanticItem(request, prefix)
	if current.Kind == trajectory.KindObservation {
		if trajectory.AuthorityOf(current) == trajectory.AuthorityObserver {
			state.Seen = strings.TrimSpace(current.Content)
		} else {
			state.Speaker = coreinteraction.SpeakerOf(current)
			state.Heard = strings.TrimSpace(current.Content)
			state.HeardSince = state.Heard
			if semanticExtractableObservation(current) {
				state.HeardSince = semanticHeardSince(
					prefix.Items, current.ID, state.Speaker, runner.config.RecentLines,
				)
			}
			if current.Event != nil && strings.HasSuffix(current.Event.Type, ".revision") {
				state.TranscriptEvent = coreinteraction.TranscriptPartial
				state.Speaking = true
			} else {
				state.TranscriptEvent = coreinteraction.TranscriptFinal
			}
		}
	}
	return state, nil
}

func currentSemanticItem(request semanticRequest, prefix trajectory.Snapshot) trajectory.Item {
	if len(prefix.Items) == 0 {
		return trajectory.Item{}
	}
	current := prefix.Items[len(prefix.Items)-1]
	if request.commit.TrajectoryItemID == "" {
		return current
	}
	for index := len(prefix.Items) - 1; index >= 0; index-- {
		if prefix.Items[index].ID == request.commit.TrajectoryItemID {
			return prefix.Items[index]
		}
	}
	return current
}

func semanticExtractableObservation(item trajectory.Item) bool {
	return semanticSpokenObservation(item) && strings.HasSuffix(item.Event.Type, ".endpoint")
}

// semanticSpokenObservation reports whether an item is current speech evidence
// that a transcript policy may act on. Standing extraction still waits for an
// endpoint, but a silent action can be selected from a partial and therefore
// must be verified there too. Otherwise "Press two for" can open the tool lane
// before the recording has named the option the user wanted.
func semanticSpokenObservation(item trajectory.Item) bool {
	return item.Kind == trajectory.KindObservation &&
		trajectory.AuthorityOf(item) == trajectory.AuthorityUser &&
		item.Event != nil &&
		(strings.HasSuffix(item.Event.Type, ".revision") || strings.HasSuffix(item.Event.Type, ".endpoint")) &&
		strings.TrimSpace(item.Content) != ""
}

// semanticStandingUtterance returns the whole unanswered stretch that the
// current endpoint completes. Deepgram may endpoint at a breath, splitting
// "if I am quiet for fifteen seconds" from "ask whether I am still there".
// The extractor can reconstruct that policy from recent context, but grounding
// it against only the last fragment rejects the correct reconstruction. The
// situation already carries the exact same-speaker, post-playback stretch; use
// that one value for extraction and grounding so those two checks cannot
// disagree about what was said.
func semanticStandingUtterance(situation coreinteraction.Situation, current trajectory.Item) string {
	if heard := strings.TrimSpace(situation.HeardSince); heard != "" {
		return heard
	}
	return strings.TrimSpace(current.Content)
}

func semanticActivationEvidence(situation coreinteraction.Situation) bool {
	return situation.TranscriptEvent == coreinteraction.TranscriptFinal ||
		situation.Seen != "" || len(situation.Seeing) > 0 || situation.Quiet
}

// semanticHeardSince reconstructs the bounded completed speech added after
// the last assistant audio that actually crossed the playback boundary. A
// recognizer may endpoint one spoken thought at a breath and a later response
// may supersede a prepared-but-unheard answer; neither event makes the earlier
// clause old evidence. Silent cognition and canceled voice output likewise do
// not claim a conversational turn.
func semanticHeardSince(
	items []trajectory.Item, currentID, speaker string, maximum int,
) string {
	if maximum <= 0 {
		maximum = defaultSemanticRecentLines
	}
	current := len(items) - 1
	if currentID != "" {
		for index := len(items) - 1; index >= 0; index-- {
			if items[index].ID == currentID {
				current = index
				break
			}
		}
	}
	audible := make(map[string]struct{})
	for index := 0; index <= current && index < len(items); index++ {
		item := items[index]
		if item.Kind == trajectory.KindAssistant &&
			item.Producer.SpeechAuthority != string(continuation.SpeechAuthoritySilent) &&
			strings.TrimSpace(item.Content) != "" && strings.TrimSpace(item.Content) != coreinteraction.WaitToken {
			audible[item.ID] = struct{}{}
		}
	}
	parts := make([]string, 0, min(maximum, 8))
	for index := current; index >= 0 && len(parts) < maximum; index-- {
		item := items[index]
		switch item.Kind {
		case trajectory.KindAssistantState:
			if item.AssistantState != nil && item.AssistantState.Visibility == trajectory.VisibilityPlayed {
				if _, spoken := audible[item.AssistantState.AssistantItemID]; spoken {
					return strings.Join(parts, " ")
				}
			}
		case trajectory.KindAssistant:
			if _, spoken := audible[item.ID]; spoken && item.Visibility == trajectory.VisibilityPlayed {
				return strings.Join(parts, " ")
			}
		case trajectory.KindObservation:
			if !semanticExtractableObservation(item) {
				continue
			}
			if coreinteraction.SpeakerOf(item) != speaker {
				return strings.Join(parts, " ")
			}
			text := strings.TrimSpace(item.Content)
			if text != "" {
				parts = append([]string{text}, parts...)
			}
		}
	}
	return strings.Join(parts, " ")
}

func cloneSemanticImages(source []coreinteraction.Image) []coreinteraction.Image {
	result := make([]coreinteraction.Image, len(source))
	for index := range source {
		result[index] = coreinteraction.Image{
			MIMEType: source[index].MIMEType, Bytes: slices.Clone(source[index].Bytes),
		}
	}
	return result
}

func semanticRecentBefore(items []trajectory.Item, currentID string, maximum int) []string {
	if currentID == "" {
		return coreinteraction.RecentLines(items, maximum)
	}
	before := make([]trajectory.Item, 0, len(items))
	for _, item := range items {
		if item.ID == currentID {
			break
		}
		before = append(before, item)
	}
	return coreinteraction.RecentLines(before, maximum)
}

func semanticPinboard(instructions []coreinteraction.StandingInstruction) *coreinteraction.Pinboard {
	board := &coreinteraction.Pinboard{}
	for _, instruction := range instructions {
		board.Pin(instruction)
	}
	return board
}

func semanticNowNS(clock graphruntime.Clock) uint64 {
	if clock == nil || semanticReflectedNil(clock) {
		return 0
	}
	return clock.NowNS()
}

type semanticMediaResult struct {
	media continuation.Media
	err   error
}

// MediaResolver predates context-aware provider calls. Isolate it behind the
// exact semantic-decision deadline so a stalled retained-media plug-in cannot
// delay the policy outcome. The resolver itself remains owned by the bounded
// session media bridge and may finish after that outcome; the buffered result
// lets it return its lease without blocking on an abandoned decision.
func resolveSemanticMedia(
	ctx context.Context, resolver continuation.MediaResolver, handle string,
) (continuation.Media, error) {
	if resolver == nil {
		return continuation.Media{}, errors.New("semantic visual media resolver is unavailable")
	}
	result := make(chan semanticMediaResult, 1)
	go func() {
		media, err := resolver(handle)
		result <- semanticMediaResult{media: media, err: err}
	}()
	select {
	case resolved := <-result:
		resolved.media.Bytes = slices.Clone(resolved.media.Bytes)
		return resolved.media, resolved.err
	case <-ctx.Done():
		return continuation.Media{}, context.Cause(ctx)
	}
}

func (runner *semanticAdmissionRunner) finishDecision(
	ctx context.Context, result semanticDecisionResult, active activeSemanticDecision,
) error {
	request := result.request
	if active.disposition == semanticDecisionInvocationSuperseded {
		runner.state.Ignored++
		if err := runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionIgnored, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev,
			ContextVersion: request.version, Code: "invocation_superseded",
			Message: active.message,
		}); err != nil {
			return err
		}
		return runner.enqueue(ctx, request)
	}
	if active.disposition == semanticDecisionCanceled ||
		active.disposition == semanticDecisionEvidenceSuperseded {
		runner.rememberTerminal(request.key())
		runner.state.Canceled++
		code := "decision_canceled"
		if active.disposition == semanticDecisionEvidenceSuperseded {
			code = "decision_superseded"
		}
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionCanceled, Operation: request.operation,
			StreamID: request.streamID, SourceRevision: request.sourceRev,
			ContextVersion: request.version, Code: code, Message: active.message,
		})
	}
	runner.rememberTerminal(request.key())
	if result.err != nil {
		kind, code := SemanticAdmissionFailed, "decider_failed"
		if result.failureCode != "" {
			code = result.failureCode
		}
		message := result.err.Error()
		if result.timedOut || errors.Is(result.err, context.DeadlineExceeded) {
			kind, code = SemanticAdmissionFailed, "decision_timeout"
			runner.state.Failed++
		} else if result.canceled || errors.Is(result.err, context.Canceled) {
			kind, code = SemanticAdmissionCanceled, "decision_canceled"
			runner.state.Canceled++
		} else {
			runner.state.Failed++
		}
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: kind, Operation: request.operation, StreamID: request.streamID,
			SourceRevision: request.sourceRev, ContextVersion: request.version,
			Code: code, Message: boundedPolicyReason(message),
		})
	}
	runner.pinboard = semanticPinboard(result.standingAfter)
	runner.state.StandingPolicies = len(result.standingAfter)
	sequence, err := runner.sequences.Next(runner.instance + ".semantic_decision")
	if err != nil {
		return err
	}
	decisionItemID := fmt.Sprintf("%s:decision:%d", runner.instance, sequence)
	confidence := result.outcome
	if result.stage == "standing_coverage" {
		confidence = result.coverageOutcome
	} else if result.stage == "voice_activation" || result.stage == "voice_addressing" ||
		result.stage == "silent_action_activation" {
		confidence = result.activationOutcome
	}
	decision := SemanticDecision{
		Operation: request.operation, Act: result.act, Policy: result.policy,
		EvidenceItemID: request.envelope.ItemID, StreamID: request.streamID,
		SourceRevision: request.sourceRev, ContextVersion: request.version,
		InvocationDigest: result.digest, Provider: runner.entry.descriptor.Provider,
		Model: runner.entry.descriptor.Model, Confidence: confidence.Confidence,
		Measured: confidence.Measured, DecisionStage: result.stage,
		Activation: result.activation, ActivationConfidence: result.activationOutcome.Confidence,
		ActivationMeasured: result.activationOutcome.Measured,
		StandingCoverage:   result.standingCoverage, CoverageConfidence: result.coverageOutcome.Confidence,
		CoverageMeasured: result.coverageOutcome.Measured,
		StandingBefore:   len(result.standingBefore), StandingAfter: len(result.standingAfter),
		StandingPinned: result.standingPinned, StandingRevoked: result.standingRevoked,
		StartedNS: result.started, FinishedNS: result.ended,
	}
	decisionEnvelope := request.envelope.Clone()
	decisionEnvelope.Type = runner.ports.decision.Type()
	decisionEnvelope.ItemID = decisionItemID
	decisionEnvelope.Sequence = sequence
	decisionEnvelope.Payload = decision
	decisionEnvelope.CausalParents = appendUnique(decisionEnvelope.CausalParents, request.envelope.ItemID)
	decisionEnvelope.CausalParents = appendUnique(decisionEnvelope.CausalParents, result.sample.envelope.ItemID)
	if _, err := runner.ports.decision.Broadcast(ctx, decisionEnvelope); err != nil {
		return err
	}
	branch := request.envelope.Clone()
	branch.CausalParents = appendUnique(branch.CausalParents, decisionItemID)
	var output element.OutputPort
	switch result.act {
	case coreinteraction.ActStaySilent:
		runner.state.Suppressed++
		code := "listen"
		message := "semantic policy selected no generation"
		if result.stage == "voice_addressing" &&
			result.activation == semanticVoiceAddressedElsewhere {
			code = "addressed_elsewhere"
			message = "current evidence is addressed to another person"
		}
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionSuppressed, Operation: request.operation, Act: result.act,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: code, Message: message,
		})
	case coreinteraction.ActActSilently:
		if request.operation == "committed" {
			output = runner.ports.silentCommitted
		} else {
			output = runner.ports.silentCreate
		}
		runner.state.AdmittedSilent++
	case coreinteraction.ActAnswer, coreinteraction.ActSpeakThrough, coreinteraction.ActInterrupt:
		if request.operation == "committed" {
			output = runner.ports.voiceCommitted
		} else {
			output = runner.ports.voiceCreate
		}
		runner.state.AdmittedVoice++
	case coreinteraction.ActKeepSpeaking, coreinteraction.ActStopSpeaking:
		code, message, refused := semanticControlDisposition(request.operation, result.act)
		if refused {
			runner.state.Refused++
			return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
				Kind: SemanticAdmissionRefused, Operation: request.operation, Act: result.act,
				StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
				DecisionItemID: decisionItemID, Code: code, Message: message,
			})
		}
		runner.state.Suppressed++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionSuppressed, Operation: request.operation, Act: result.act,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: code, Message: message,
		})
	default:
		runner.state.Refused++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionRefused, Operation: request.operation, Act: result.act,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: "unsupported_act",
			Message: "semantic admission final/quiet path accepts only listen, answer, or act-silently",
		})
	}
	branch.Type = output.Type()
	if request.operation == "committed" {
		branch.Payload = SemanticGrant{
			Commit: request.commit, Act: result.act, DecisionItemID: decisionItemID,
		}
	} else {
		create := request.create
		if request.operation == "quiet" {
			create.TrustedPurpose = ResponseCreatePurposePostCommitSilence
		}
		branch.Payload = create
	}
	if _, err := output.Broadcast(ctx, branch); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionAdmitted, Operation: request.operation, Act: result.act,
		StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
		DecisionItemID: decisionItemID,
	})
}

func semanticControlDisposition(
	operation string, act coreinteraction.Act,
) (code, message string, refused bool) {
	if operation != "committed" {
		return "unsupported_act",
			"explicit response creation cannot claim an in-flight speech control act", true
	}
	if act == coreinteraction.ActStopSpeaking {
		return "stop_speaking",
			"semantic policy delegated cancellation of existing output to the overlap controller", false
	}
	return "keep_speaking", "semantic policy kept the existing deliberate output active", false
}

func (runner *semanticAdmissionRunner) reportResolution() error {
	digest, err := semanticDescriptorDigest(runner.entry.descriptor)
	if err != nil {
		return err
	}
	provider := liveidentity.Artifact{
		ID:       "interaction-model://" + runner.entry.descriptor.Provider,
		Revision: runner.entry.descriptor.Model + "@" + runner.entry.descriptor.Revision,
		Digest:   digest,
	}
	adapter := liveidentity.Artifact{
		ID:       "builtin://openrealtime/adapters/policy.SemanticAdmission-interaction.Decider",
		Revision: semanticAdmissionRuntimeRevision,
	}
	capabilities := []element.CapabilityResolution{
		liveidentity.Capability("interaction.semantic-acts", "openrealtime.interaction/Decider-v1", provider, adapter),
	}
	if runner.config.StandingExtraction {
		capabilities = append(capabilities, liveidentity.Capability(
			"interaction.standing-extraction", "openrealtime.interaction/Extractor-v1", provider, adapter,
		))
	}
	if runner.config.VerifyVoiceActivation {
		capabilities = append(capabilities, liveidentity.Capability(
			"interaction.voice-activation", "openrealtime.interaction/Decider-v1", provider, adapter,
		))
	}
	if runner.config.VerifySilentAction {
		capabilities = append(capabilities, liveidentity.Capability(
			"interaction.silent-action-activation", "openrealtime.interaction/Decider-v1", provider, adapter,
		))
	}
	return liveidentity.Report(runner.resolution, liveidentity.Artifact{
		ID: semanticAdmissionRuntimeID, Revision: semanticAdmissionRuntimeRevision,
	}, capabilities)
}

func (runner *semanticAdmissionRunner) publishResolved(ctx context.Context) error {
	digest, err := semanticDescriptorDigest(runner.entry.descriptor)
	if err != nil {
		return err
	}
	sequence, err := runner.sequences.Next(runner.instance + ".semantic_resolution")
	if err != nil {
		return err
	}
	envelope := element.Envelope{
		Type: runner.ports.resolved.Type(), ItemID: fmt.Sprintf("%s:resolved:%d", runner.instance, sequence),
		Sequence: sequence, Payload: SemanticDeciderResolution{
			Reference: runner.reference, Descriptor: runner.entry.descriptor,
			DescriptorDigest: digest, RegistryRevision: runner.registryRevision,
		},
	}
	_, err = runner.ports.resolved.Broadcast(ctx, envelope)
	return err
}

func (runner *semanticAdmissionRunner) publishRefusal(
	ctx context.Context, cause element.Envelope, operation, code, message string,
) error {
	runner.state.Refused++
	return runner.publishOutcome(ctx, cause, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionRefused, Operation: operation, Code: code,
		Message: boundedPolicyReason(message),
	})
}

func (runner *semanticAdmissionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome SemanticAdmissionOutcome,
) error {
	sequence, err := runner.sequences.Next(runner.instance + ".semantic_outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = runner.ports.outcome.Type()
	envelope.ItemID = fmt.Sprintf("%s:outcome:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.Payload = outcome
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *semanticAdmissionRunner) publishState(ctx context.Context, cause element.Envelope) error {
	sequence, err := runner.sequences.Next(runner.instance + ".semantic_state")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = runner.ports.state.Type()
	envelope.ItemID = fmt.Sprintf("%s:state:%d", runner.instance, sequence)
	envelope.Sequence = sequence
	envelope.Payload = runner.state
	envelope.CausalParents = appendUnique(envelope.CausalParents, cause.ItemID)
	_, err = runner.ports.state.Broadcast(ctx, envelope)
	return err
}

func (runner *semanticAdmissionRunner) rememberTerminal(key string) {
	if key == "" {
		return
	}
	if _, found := runner.terminal[key]; found {
		return
	}
	runner.terminal[key] = struct{}{}
	runner.terminalOrder = append(runner.terminalOrder, key)
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *semanticAdmissionRunner) isTerminal(key string) bool {
	_, found := runner.terminal[key]
	return found
}

func semanticSnapshotPayload(payload any) (trajectory.Snapshot, bool) {
	switch value := payload.(type) {
	case trajectory.Snapshot:
		return cloneSemanticSnapshot(value), true
	case *trajectory.Snapshot:
		if value != nil {
			return cloneSemanticSnapshot(*value), true
		}
	}
	return trajectory.Snapshot{}, false
}

func semanticAgentOutputPayload(payload any) (coreinteraction.AgentOutput, bool) {
	switch value := payload.(type) {
	case coreinteraction.AgentOutput:
		return cloneSemanticAgentOutput(value), true
	case *coreinteraction.AgentOutput:
		if value != nil {
			return cloneSemanticAgentOutput(*value), true
		}
	}
	return coreinteraction.AgentOutput{}, false
}

func cloneSemanticAgentOutput(output coreinteraction.AgentOutput) coreinteraction.AgentOutput {
	output.ProtectedStreams = slices.Clone(output.ProtectedStreams)
	return output
}

func validateSemanticAgentOutput(output coreinteraction.AgentOutput) error {
	if output.Revision == 0 {
		return errors.New("semantic admission agent output revision must be positive")
	}
	if output.Audible && (!output.Active || output.Queued) {
		return errors.New("semantic admission audible agent output must be active and not queued")
	}
	if !output.Active && (output.Queued || output.Audible || output.Saying != "" || output.InFlight != "" ||
		len(output.ProtectedStreams) != 0) {
		return errors.New("semantic admission inactive agent output carries active lifecycle state")
	}
	for name, value := range map[string]string{"saying": output.Saying, "in_flight": output.InFlight} {
		if value != strings.TrimSpace(value) || !utf8.ValidString(value) || len(value) > maximumSemanticTextBytes {
			return fmt.Errorf("semantic admission agent output %s is not bounded canonical text", name)
		}
	}
	if len(output.ProtectedStreams) > maximumSemanticAgentOutputStreams {
		return fmt.Errorf("semantic admission agent output carries %d protected streams, maximum is %d",
			len(output.ProtectedStreams), maximumSemanticAgentOutputStreams)
	}
	previous := ""
	for _, streamID := range output.ProtectedStreams {
		if err := validatePolicyIdentifier("semantic admission protected stream ID", streamID, true); err != nil {
			return err
		}
		if previous != "" && streamID <= previous {
			return errors.New("semantic admission protected stream IDs must be sorted and unique")
		}
		previous = streamID
	}
	return nil
}

func cloneSemanticSnapshot(source trajectory.Snapshot) trajectory.Snapshot {
	result := source
	result.Items = make([]trajectory.Item, len(source.Items))
	for index := range source.Items {
		item := source.Items[index]
		item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
		item.ProviderState = slices.Clone(item.ProviderState)
		if item.ToolCall != nil {
			copy := *item.ToolCall
			copy.Arguments = slices.Clone(item.ToolCall.Arguments)
			item.ToolCall = &copy
		}
		if item.ToolCallDerivation != nil {
			copy := *item.ToolCallDerivation
			copy.Rewrites = slices.Clone(item.ToolCallDerivation.Rewrites)
			item.ToolCallDerivation = &copy
		}
		if item.ToolResult != nil {
			copy := *item.ToolResult
			copy.Output = slices.Clone(item.ToolResult.Output)
			item.ToolResult = &copy
		}
		if item.ToolPlaceholder != nil {
			copy := *item.ToolPlaceholder
			item.ToolPlaceholder = &copy
		}
		if item.Observation != nil {
			copy := *item.Observation
			copy.Media = slices.Clone(item.Observation.Media)
			item.Observation = &copy
		}
		if item.AssistantState != nil {
			copy := *item.AssistantState
			item.AssistantState = &copy
		}
		if item.Repair != nil {
			copy := *item.Repair
			item.Repair = &copy
		}
		if item.Event != nil {
			copy := *item.Event
			item.Event = &copy
		}
		result.Items[index] = item
	}
	return result
}

type semanticDeciderHandle struct {
	mu      sync.Mutex
	decider SemanticDecider
}

func (handle *semanticDeciderHandle) set(decider SemanticDecider) error {
	handle.mu.Lock()
	defer handle.mu.Unlock()
	if handle.decider != nil {
		return errors.New("semantic decider handle is already initialized")
	}
	handle.decider = decider
	return nil
}

func (handle *semanticDeciderHandle) close(context.Context) error {
	handle.mu.Lock()
	decider := handle.decider
	handle.decider = nil
	handle.mu.Unlock()
	return closeSemanticDecider(decider)
}

func closeSemanticDecider(decider SemanticDecider) error {
	if semanticReflectedNil(decider) {
		return nil
	}
	if closer, ok := decider.(io.Closer); ok {
		return closer.Close()
	}
	return nil
}

var (
	_ element.Factory         = semanticAdmissionFactory{}
	_ element.ConfigValidator = semanticAdmissionFactory{}
)
