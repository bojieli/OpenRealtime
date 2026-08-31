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
	context, update, committed, create, quiet, cancel          element.InputPort
	voiceCommitted, silentCommitted, voiceCreate, silentCreate element.OutputPort
	decision, state, outcome, resolved                         element.OutputPort
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
	state            SemanticAdmissionState
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
		go runner.decide(decisionCtx, request, update, digest, sample, prefix, standing, results)
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
	standing []coreinteraction.StandingInstruction, results chan<- semanticDecisionResult,
) {
	started := runner.clock.NowNS()
	timeout := time.Duration(runner.entry.descriptor.DecisionTimeoutMS) * time.Millisecond
	decisionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	situation, err := runner.situationWithStanding(decisionCtx, request, update, prefix, standing)
	failure := ""
	if err != nil {
		failure = "visual_evidence_failed"
	}
	var act coreinteraction.Act
	var outcome coreinteraction.Outcome
	stage := "primary"
	activation := ""
	var activationOutcome coreinteraction.Outcome
	standingCoverage := ""
	var coverageOutcome coreinteraction.Outcome
	standingAfter := slices.Clone(standing)
	standingPinned, standingRevoked := 0, 0
	if err == nil && runner.extractor != nil && request.operation == "committed" {
		current := currentSemanticItem(request, prefix)
		if semanticExtractableObservation(current) {
			var extraction coreinteraction.Extraction
			extraction, err = runner.extractor.Extract(
				decisionCtx, slices.Clone(standing),
				semanticRecentBefore(prefix.Items, current.ID, runner.config.RecentLines),
				strings.TrimSpace(current.Content),
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
					coverageOutcome, err = runner.verifyStandingCoverage(decisionCtx, current.Content, extraction.Pins)
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
	if err == nil {
		err = validateSemanticSituation(situation)
		if err != nil {
			failure = "invalid_evidence"
		}
	}
	if err == nil && stage == "primary" {
		act, outcome, err = runner.decideAct(decisionCtx, request.operation, situation)
		if err == nil {
			options := semanticActOptions(situation)
			err = validateSemanticOutcome(outcome, options)
			if err != nil {
				failure = "invalid_decider_outcome"
			}
		}
	}
	if err == nil && stage == "primary" && runner.config.VerifyVoiceActivation &&
		act == coreinteraction.ActAnswer && len(standing) == 0 {
		current := currentSemanticItem(request, prefix)
		if semanticExtractableObservation(current) {
			activationOutcome, err = runner.verifyVoiceActivation(
				decisionCtx, update.Invocation.Instruction, current.Content,
			)
			if err != nil {
				failure = "voice_activation_failed"
			} else {
				activation = strings.TrimSpace(activationOutcome.Option)
				if activation == semanticVoiceWait {
					act = coreinteraction.ActStaySilent
					stage = "voice_activation"
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
		act: act, outcome: outcome, stage: stage, activation: activation,
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
	ctx context.Context, operation string, situation coreinteraction.Situation,
) (coreinteraction.Act, coreinteraction.Outcome, error) {
	if situation.Decidable() || operation == "committed" {
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

const (
	semanticStandingCovered    = "covered"
	semanticStandingAdditional = "additional-work"
	semanticVoiceConditionMet  = "condition-met"
	semanticVoiceDirectRequest = "direct-request"
	semanticVoiceWait          = "wait"
)

const semanticStandingCoverageInstruction = "The policy extractor listed the standing policies established by one utterance. " +
	"Decide whether the utterance contains any separate request due now OUTSIDE those listed policies. " +
	"covered means every request in the utterance is one of the listed standing policies or merely setup, preference, or context for them; " +
	"describing the trigger inside a listed policy does not make that trigger happen. " +
	"additional-work means there is also a separate complete question, immediate command, or report of an already established trigger that is not part of a listed policy. " +
	"Reply with one label only. Examples: 'I want fish tonight; order when the waiter names something that fits' is covered by the listed ordering policy. " +
	"'From now on answer briefly; what is the capital of France?' has additional-work outside the brevity policy."

const semanticVoiceActivationInstruction = "You are an activation guard, not a conversational agent. " +
	"Classify whether the CURRENT UTTERANCE creates a reason for a voice assistant to answer now under the AGENT CONTRACT. " +
	"condition-met means the contract says to answer when some fact occurs, and the current utterance provides that fact now. " +
	"direct-request means the current utterance directly asks a complete question or requests work that should start now, not later. " +
	"wait means neither: a future condition is merely being described or requested, an applicable condition has not occurred, or the utterance is narration. " +
	"Reply with one label only. Examples: contract 'correct a date that contradicts the third'; current 'we do design review next week' is wait; " +
	"the same contract with current 'ship by the thirteenth' is condition-met. Contract 'answer briefly'; current 'what is the capital of France' is direct-request."

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
	ctx context.Context, contract, utterance string,
) (coreinteraction.Outcome, error) {
	options := []string{semanticVoiceConditionMet, semanticVoiceDirectRequest, semanticVoiceWait}
	outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
		Prompt:  semanticVoiceActivationInstruction,
		Options: options,
		Evidence: "AGENT CONTRACT:\n" + strings.TrimSpace(contract) +
			"\n\nCURRENT UTTERANCE:\n" + strings.TrimSpace(utterance),
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
	return runner.situationWithStanding(ctx, request, update, prefix, nil)
}

func (runner *semanticAdmissionRunner) situationWithStanding(
	ctx context.Context, request semanticRequest, update SessionInvocationUpdate, prefix trajectory.Snapshot,
	standing []coreinteraction.StandingInstruction,
) (coreinteraction.Situation, error) {
	board := semanticPinboard(standing)
	state := coreinteraction.Situation{
		Contract: update.Invocation.Instruction,
		Recent:   coreinteraction.RecentLines(prefix.Items, runner.config.RecentLines),
		Pins:     board.Lines(semanticNowNS(runner.clock)),
		AllowedActs: []coreinteraction.Act{
			coreinteraction.ActStaySilent, coreinteraction.ActAnswer,
		},
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
		state.Quiet = true
		state.Silence = "15s"
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
	return item.Kind == trajectory.KindObservation &&
		trajectory.AuthorityOf(item) == trajectory.AuthorityUser &&
		item.Event != nil && strings.HasSuffix(item.Event.Type, ".endpoint") &&
		strings.TrimSpace(item.Content) != ""
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
	} else if result.stage == "voice_activation" {
		confidence = result.activationOutcome
	}
	decision := SemanticDecision{
		Operation: request.operation, Act: result.act, Policy: runner.model.Name(),
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
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionSuppressed, Operation: request.operation, Act: result.act,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: "listen", Message: "semantic policy selected no generation",
		})
	case coreinteraction.ActActSilently:
		if request.operation == "committed" {
			output = runner.ports.silentCommitted
		} else {
			output = runner.ports.silentCreate
		}
		runner.state.AdmittedSilent++
	case coreinteraction.ActAnswer:
		if request.operation == "committed" {
			output = runner.ports.voiceCommitted
		} else {
			output = runner.ports.voiceCreate
		}
		runner.state.AdmittedVoice++
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
	if _, err := output.Broadcast(ctx, branch); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionAdmitted, Operation: request.operation, Act: result.act,
		StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
		DecisionItemID: decisionItemID,
	})
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
