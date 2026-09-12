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
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
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
		contexts:        make(map[semanticContextAddress]semanticContextSample),
		terminal:        make(map[string]struct{}),
		canceledStreams: make(map[cancellationAddress]string),
		pinboard:        &coreinteraction.Pinboard{},
	}, nil
}

type semanticAdmissionPorts struct {
	context, update, agentOutput, committed, create, quiet, cancel  element.InputPort
	release                                                         element.InputPort
	safeRelease                                                     element.OutputPort
	voiceCommitted, voiceCreate, decision, state, outcome, resolved element.OutputPort
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
		{"release", &result.release},
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
		{"voice_committed", &result.voiceCommitted}, {"voice_create", &result.voiceCreate},
		{"safe_release", &result.safeRelease},
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
	request         semanticRequest
	update          SessionInvocationUpdate
	digest          string
	sample          semanticContextSample
	prefix          trajectory.Snapshot
	choice          coreinteraction.Choice
	spokeOver       bool
	event           coreinteraction.TranscriptEventKind
	policy          string
	outcome         coreinteraction.Outcome
	stage           string
	standingBefore  []coreinteraction.StandingInstruction
	standingAfter   []coreinteraction.StandingInstruction
	standingPinned  int
	standingRevoked int
	evidence        string
	standing        *StandingReport
	heard           string
	questions       []coreinteraction.AskedQuestion
	started, ended  uint64
	err             error
	failureCode     string
	canceled        bool
	timedOut        bool
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
	// steps is the lockstep history: one entry per transcript event decided,
	// with what the agent then said when the choice invoked it.
	steps []semanticStep
	// hold is set while a voice generation admitted by the last step is
	// still being answered. No decision starts until it lifts, so every
	// step sees the model's answer to the one before, and no two
	// generations ever run at once.
	hold *semanticHold
}

// semanticStep is one turn of the lockstep loop.
type semanticStep struct {
	stream string
	event  coreinteraction.TranscriptEventKind
	heard  string
	choice coreinteraction.Choice
	// spoke says the voice was admitted on this step; said is what it then
	// said, one segment per utterance, empty when it answered with silence.
	spoke bool
	said  []string
}

type semanticHold struct {
	sinceNS uint64
	// startedAtGrant and finishedAtGrant are the output lifecycle's
	// generation counters when the voice was admitted. The hold lifts once a
	// generation started after the grant and every started generation has
	// finished - counts that survive the state lane coalescing away the
	// snapshots in between.
	startedAtGrant  uint64
	finishedAtGrant uint64
}

const (
	// maximumSemanticSteps bounds the history shown to the policy.
	maximumSemanticSteps = 12
	// semanticHoldGrace is how long a hold waits for the output lifecycle to
	// acknowledge the generation before concluding it was refused downstream.
	semanticHoldGrace = 2 * time.Second
	// semanticHoldTimeout bounds a hold whose generation never reports back.
	semanticHoldTimeout = 20 * time.Second
	// semanticStepTextLimit bounds the words shown per step; the end of a
	// partial is the part that changed, so the tail is kept.
	semanticStepTextLimit = 160
)

// holding reports whether the next decision must wait for the model, lifting
// a hold that the lifecycle never acknowledged or never resolved.
func (runner *semanticAdmissionRunner) holding() bool {
	if runner.hold == nil {
		return false
	}
	now := runner.clock.NowNS()
	elapsed := time.Duration(now - runner.hold.sinceNS)
	switch {
	case elapsed > semanticHoldTimeout:
		runner.state.HoldTimeouts++
		runner.hold = nil
		return false
	case runner.agentOutput.GenerationsStarted == runner.hold.startedAtGrant && elapsed > semanticHoldGrace:
		// Nothing ever reached the model: the grant was refused downstream.
		runner.hold = nil
		return false
	}
	return true
}

// releaseHoldIfAnswered lifts the hold once the lifecycle reports the
// admitted generation over.
func (runner *semanticAdmissionRunner) releaseHoldIfAnswered(output coreinteraction.AgentOutput) {
	if runner.hold == nil {
		return
	}
	if output.GenerationsStarted > runner.hold.startedAtGrant &&
		output.GenerationsFinished >= output.GenerationsStarted {
		runner.hold = nil
	}
}

// recordStep appends one decided transcript event to the history.
func (runner *semanticAdmissionRunner) recordStep(result semanticDecisionResult) int {
	runner.steps = append(runner.steps, semanticStep{
		stream: result.request.streamID, event: result.event,
		heard: strings.TrimSpace(result.heard), choice: result.choice,
	})
	if len(runner.steps) > maximumSemanticSteps {
		runner.steps = runner.steps[len(runner.steps)-maximumSemanticSteps:]
	}
	return len(runner.steps) - 1
}

// noteAgentSaying attributes the agent's audible text to the step that
// invoked it. In lockstep at most one generation is live at a time, so any
// new utterance belongs to the most recent step that spoke.
func (runner *semanticAdmissionRunner) noteAgentSaying(output coreinteraction.AgentOutput) {
	saying := strings.TrimSpace(output.Saying)
	if saying == "" || !output.Active {
		return
	}
	for index := len(runner.steps) - 1; index >= 0; index-- {
		step := &runner.steps[index]
		if !step.spoke {
			continue
		}
		if !slices.Contains(step.said, saying) {
			step.said = append(step.said, saying)
		}
		return
	}
}

// stepLines renders the history for the evidence.
func (runner *semanticAdmissionRunner) stepLines() []string {
	lines := make([]string, 0, len(runner.steps))
	for _, step := range runner.steps {
		event := string(step.event)
		if event == "" {
			event = "event"
		}
		line := event + " \"" + semanticStepText(step.heard) + "\" -> " + step.choice.Token()
		if step.spoke {
			if len(step.said) == 0 {
				line += "; agent said nothing"
			} else {
				line += "; agent said \"" + strings.Join(step.said, " ") + "\""
			}
		}
		lines = append(lines, line)
	}
	return lines
}

// previousStepHeard is what the last step on this utterance had heard, and
// whether there was one.
func (runner *semanticAdmissionRunner) previousStepHeard(streamID string) (string, bool) {
	if streamID == "" {
		return "", false
	}
	for index := len(runner.steps) - 1; index >= 0; index-- {
		if runner.steps[index].stream == streamID {
			return runner.steps[index].heard, true
		}
	}
	return "", false
}

// wordsAdded is what later says beyond earlier when it carries on from it,
// and all of later when it does not.
func wordsAdded(earlier, later string) string {
	was, now := strings.Fields(earlier), strings.Fields(later)
	if len(was) == 0 || len(now) < len(was) {
		return strings.TrimSpace(later)
	}
	for index := range was {
		if normalizeStepWord(was[index]) != normalizeStepWord(now[index]) {
			return strings.TrimSpace(later)
		}
	}
	return strings.Join(now[len(was):], " ")
}

func normalizeStepWord(word string) string {
	return strings.ToLower(strings.Trim(word, ".,!?;:\"'"))
}

func semanticStepText(text string) string {
	if len(text) <= semanticStepTextLimit {
		return text
	}
	cut := text[len(text)-semanticStepTextLimit:]
	if index := strings.IndexByte(cut, ' '); index >= 0 && index < 40 {
		cut = cut[index+1:]
	}
	return "…" + cut
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
	runner.decider = decider
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
		{kind: "release", port: runner.ports.release},
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
	case "release":
		if err := runner.acceptPlaybackRelease(ctx, input.envelope); err != nil {
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

// Playback completion permits another client request. Apply its causal output
// state in this actor before forwarding that permit; the sampled State lane
// can be delayed or coalesced independently.
func (runner *semanticAdmissionRunner) acceptPlaybackRelease(ctx context.Context, envelope element.Envelope) error {
	release, ok := envelope.Payload.(speechelements.PlaybackRelease)
	if !ok || !envelope.Type.Equal(speechelements.PlaybackReleaseType()) {
		return errors.New("semantic admission playback release has an invalid type")
	}
	if err := validateSemanticAgentOutput(release.AgentOutput); err != nil {
		return err
	}
	if !canonicalSemanticIdentity(release.AgentOutputItemID) ||
		!slices.Contains(envelope.CausalParents, release.AgentOutputItemID) ||
		!canonicalSemanticIdentity(envelope.ItemID) || !canonicalSemanticIdentity(envelope.SessionID) ||
		!canonicalSemanticIdentity(envelope.RunID) ||
		(runner.contextSession != "" && envelope.SessionID != runner.contextSession) ||
		envelope.Sequence == 0 ||
		release.Receipt.Kind != speechelements.PlaybackReleased || release.Receipt.Sequence == 0 ||
		!canonicalSemanticIdentity(release.Receipt.Utterance.ID) ||
		envelope.SourceID != release.Receipt.Utterance.ID ||
		envelope.CancellationScope != release.Receipt.Utterance.ID {
		return errors.New("semantic admission playback release has invalid state or receipt lineage")
	}
	if release.AgentOutput.Revision == runner.agentOutput.Revision &&
		!reflect.DeepEqual(release.AgentOutput, runner.agentOutput) {
		return errors.New("semantic admission playback release conflicts with its output revision")
	}
	if release.AgentOutput.Revision > runner.agentOutput.Revision {
		runner.agentOutput = cloneSemanticAgentOutput(release.AgentOutput)
	}
	forwarded := envelope.Clone()
	forwarded.Type = speechelements.PlaybackReceiptType()
	forwarded.ItemID = envelope.ItemID + ":policy-applied"
	forwarded.CausalParents = appendUnique(forwarded.CausalParents, envelope.ItemID)
	forwarded.Payload = release.Receipt
	_, err := runner.ports.safeRelease.Broadcast(ctx, forwarded)
	return err
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
	runner.releaseHoldIfAnswered(output)
	runner.noteAgentSaying(output)
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
	// A stream cancellation also covers evidence that has not arrived yet,
	// even when this interrupt found an active or pending decision.
	runner.recordPreCancel(cancellationAddress{streamID: cancel.StreamID, sessionID: envelope.SessionID}, reason)
	if runner.active != nil && semanticSameStream(runner.active.request, envelope.SessionID, cancel.StreamID) {
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
	runner.state.Ignored++
	return runner.publishOutcome(ctx, envelope, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionIgnored, Operation: "cancel", StreamID: cancel.StreamID,
		Code: "cancel_recorded", Message: reason,
	})
}

func (runner *semanticAdmissionRunner) startReadyDecision(
	parent context.Context, results chan<- semanticDecisionResult,
) error {
	if runner.active != nil || len(runner.pending) == 0 || runner.holding() {
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
		history := semanticStepHistory{lines: runner.stepLines(), answeredHeard: runner.answeredSoFar(request.streamID)}
		history.previousHeard, history.previousKnown = runner.previousStepHeard(request.streamID)
		runner.decisions.Add(1)
		go func() {
			defer runner.decisions.Done()
			runner.decide(
				decisionCtx, request, update, digest, sample, prefix, standing, agentOutput, history, results,
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
	// Keep the stream address until bounded FIFO eviction. Each revision has
	// its own terminal key, so consuming this address would reopen the stream.
	reason, found := runner.canceledStreams[address]
	return reason, found
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
		// The newest context that contains this commit, not the context at
		// the commit. A request decided after a hold - the person kept
		// talking while the model answered - is decided against what the
		// model said, and the generation it may admit is compiled from that
		// same context: measured, a count answered from the context at the
		// commit could not see the number it had just said and said it again.
		candidate := sample
		if runner.latest.envelope.SessionID == request.envelope.SessionID &&
			runner.latest.snapshot.Version >= request.version &&
			(!exact || runner.latest.snapshot.Version > sample.snapshot.Version) {
			candidate = runner.latest
		}
		if candidate.snapshot.Version < request.version {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, nil
		}
		if _, err := trajectory.Prefix(candidate.snapshot, request.commit.Context.Prefix); err != nil {
			return SessionInvocationUpdate{}, "", semanticContextSample{}, trajectory.Snapshot{}, false, err
		}
		return cloneSemanticUpdate(runner.invocation), runner.invocationDigest, candidate,
			cloneSemanticSnapshot(candidate.snapshot), true, nil
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
	history semanticStepHistory, results chan<- semanticDecisionResult,
) {
	started := runner.clock.NowNS()
	timeout := time.Duration(runner.entry.descriptor.DecisionTimeoutMS) * time.Millisecond
	decisionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	situation, err := runner.situationWithStanding(
		decisionCtx, request, update, prefix, standing, agentOutput, history,
	)
	failure := ""
	if err != nil {
		failure = "visual_evidence_failed"
	}
	if err == nil {
		if err = validateSemanticSituation(situation); err != nil {
			failure = "invalid_evidence"
		}
	}
	// Standing instructions are durable memory, extracted from settled speech.
	// This is not a second opinion on the choice: it records a policy the
	// person set so that later instants see it in force. The choice below is
	// taken against the instructions in force when the words arrived, which
	// is why a request that sets a rule up is not also its first trigger.
	standingAfter := slices.Clone(standing)
	standingPinned, standingRevoked := 0, 0
	var standingReport *StandingReport
	if err == nil && runner.extractor != nil && request.operation == "committed" {
		current := currentSemanticItem(request, prefix)
		if semanticExtractableObservation(current) {
			utterance := semanticStandingUtterance(situation, current)
			var extraction coreinteraction.Extraction
			extraction, err = runner.extractor.Extract(
				decisionCtx, slices.Clone(standing),
				semanticRecentBefore(prefix.Items, current.ID, runner.config.RecentLines),
				utterance,
			)
			standingReport = &StandingReport{Utterance: utterance, Calls: extraction.Calls}
			if err != nil {
				failure = "standing_extraction_failed"
				standingReport.Failure = err.Error()
			} else {
				standingAfter, standingPinned, standingRevoked, err = applySemanticExtraction(
					standing, extraction, request.sourceRev, runner.clock.NowNS(), runner.config.StandingMemory,
				)
				if err != nil {
					failure = "standing_memory_exhausted"
					standingReport.Failure = err.Error()
				}
				standingReport.Pinned = standingLines(extraction.Pins)
				standingReport.Revoked = slices.Clone(extraction.Revokes)
				for _, dropped := range extraction.Dropped {
					standingReport.Dropped = append(standingReport.Dropped, dropped.Text+" ("+dropped.Reason+")")
				}
				standingReport.InForce = standingLines(standingAfter)
			}
		}
	}
	// One question set, one constrained answer, whatever kind of event this
	// is. The one exception is not a decision the model could take: a clock
	// nobody asked for.
	choice := coreinteraction.Choice{Speaking: situation.AgentSpeaking}
	var outcome coreinteraction.Outcome
	stage := "policy"
	evidence := ""
	var questions []coreinteraction.AskedQuestion
	if err == nil {
		switch {
		case request.operation == "quiet" && !situation.Quiet:
			// PostCommitSilence is an unowned graph clock unless an exact, due
			// standing policy promoted it to evidence. Without one the tick
			// decides nothing, and it must not disturb active output either.
			outcome = coreinteraction.Outcome{Index: 0, Option: choice.Token()}
			stage = "clock"
		case len(coreinteraction.ChoiceOptionsFor(situation)) == 1:
			// Nothing to decide: the person is still talking and no standing
			// instruction can come due, so the voice cannot be invoked and the
			// one remaining option is taken from state. No model is asked,
			// which is what keeps a partial every 100 ms from costing a policy
			// round-trip every 100 ms.
			outcome = coreinteraction.Outcome{Index: 0, Option: choice.Token()}
			stage = "state"
		default:
			evidence = situation.RenderEvidence()
			choice, outcome, questions, err = runner.decideChoice(decisionCtx, situation)
			if errors.Is(err, errInvalidDeciderOutcome) {
				// The provider answered and the answer was not one of the
				// options. A provider that did not answer at all is reported as
				// its own failure, because the two are fixed in different places.
				failure = "invalid_decider_outcome"
			}
		}
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
		choice: choice, spokeOver: choice.Speak && situation.Speaking, event: situation.TranscriptEvent,
		policy: runner.decider.Descriptor().Model, outcome: outcome, stage: stage,
		standingBefore: standing, standingAfter: standingAfter,
		standingPinned: standingPinned, standingRevoked: standingRevoked,
		evidence: evidence, standing: standingReport, heard: situation.Heard, questions: questions,
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

// decideChoice asks the policy model the one question, constrained to the
// exact options for this instant. The rendered situation is the measured one;
// only its closing line - the options - depends on whether the agent is
// speaking, and the answer is refused unless it is one of them.
func (runner *semanticAdmissionRunner) decideChoice(
	ctx context.Context, situation coreinteraction.Situation,
) (coreinteraction.Choice, coreinteraction.Outcome, []coreinteraction.AskedQuestion, error) {
	inertia := coreinteraction.Choice{Speaking: situation.AgentSpeaking}
	rules := runner.config.Rules
	if rules == "" {
		rules = coreinteraction.ChoiceInstruction
	}
	answers := map[string]bool{}
	var asked []coreinteraction.AskedQuestion
	composite := coreinteraction.Outcome{Measured: true, Confidence: 1}
	ask := func(question coreinteraction.StepQuestion) (bool, error) {
		started := runner.clock.NowNS()
		outcome, err := runner.decider.Decide(ctx, coreinteraction.Decision{
			Prompt: rules, Options: coreinteraction.YesNo(), Evidence: situation.RenderForQuestion(question),
			Images: situation.Seeing, Question: question.Name, Speaking: situation.AgentSpeaking,
		})
		if err != nil {
			return false, err
		}
		if err := validateSemanticOutcome(outcome, coreinteraction.YesNo()); err != nil {
			return false, fmt.Errorf("%w: %w", errInvalidDeciderOutcome, err)
		}
		asked = append(asked, coreinteraction.AskedQuestion{
			Question: question.Name, Answer: outcome.Option, Confidence: outcome.Confidence,
			Measured: outcome.Measured, DurationMS: float64(runner.clock.NowNS()-started) / 1e6,
		})
		if !outcome.Measured {
			composite.Measured = false
		} else if outcome.Confidence < composite.Confidence {
			composite.Confidence = outcome.Confidence
		}
		answers[question.Name] = outcome.Option == coreinteraction.AnswerYes
		return answers[question.Name], nil
	}
	// A decision that could not be taken is not a decision to do something
	// drastic. Inertia keeps a failing policy model quiet rather than letting
	// it interrupt people.
	if situation.AgentSpeaking {
		if _, err := ask(coreinteraction.StopQuestion); err != nil {
			return inertia, coreinteraction.Outcome{}, asked, err
		}
	}
	due, err := ask(coreinteraction.OccurrenceQuestion)
	if err != nil {
		return inertia, coreinteraction.Outcome{}, asked, err
	}
	// A partial is answered only for an occurrence. Anything settled - a
	// final, an explicit request, a frame - may also be a request to answer.
	if !due && situation.TranscriptEvent != coreinteraction.TranscriptPartial {
		if _, err := ask(coreinteraction.RequestQuestion); err != nil {
			return inertia, coreinteraction.Outcome{}, asked, err
		}
	}
	choice := coreinteraction.ComposeChoice(situation.AgentSpeaking, answers)
	if !composite.Measured {
		composite.Confidence = 0
	}
	composite.Option = choice.Token()
	composite.Index = slices.Index(coreinteraction.ChoiceOptions(situation.AgentSpeaking), composite.Option)
	return choice, composite, asked, nil
}

// errInvalidDeciderOutcome marks an answer the provider gave that was not one
// of the options, as distinct from a provider that did not answer.
var errInvalidDeciderOutcome = errors.New("invalid decider outcome")

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
	history := semanticStepHistory{lines: runner.stepLines(), answeredHeard: runner.answeredSoFar(request.streamID)}
	history.previousHeard, history.previousKnown = runner.previousStepHeard(request.streamID)
	return runner.situationWithStanding(
		ctx, request, update, prefix, nil, cloneSemanticAgentOutput(runner.agentOutput), history,
	)
}

// semanticStepHistory is the lockstep history as one decision sees it.
type semanticStepHistory struct {
	lines         []string
	previousHeard string
	previousKnown bool
	answeredHeard string
}

// answeredSoFar is what the last step that spoke on this utterance had heard.
func (runner *semanticAdmissionRunner) answeredSoFar(streamID string) string {
	if streamID == "" {
		return ""
	}
	for index := len(runner.steps) - 1; index >= 0; index-- {
		if runner.steps[index].stream == streamID && runner.steps[index].spoke {
			return runner.steps[index].heard
		}
	}
	return ""
}

func (runner *semanticAdmissionRunner) situationWithStanding(
	ctx context.Context, request semanticRequest, update SessionInvocationUpdate, prefix trajectory.Snapshot,
	standing []coreinteraction.StandingInstruction, agentOutput coreinteraction.AgentOutput,
	history semanticStepHistory,
) (coreinteraction.Situation, error) {
	board := semanticPinboard(standing)
	currentID := ""
	if len(prefix.Items) > 0 {
		currentID = currentSemanticItem(request, prefix).ID
	}
	contract := update.Contract
	if strings.TrimSpace(contract) == "" {
		contract = update.Invocation.Instruction
	}
	state := coreinteraction.Situation{
		Contract:      contract,
		Recent:        semanticPolicyRecent(prefix.Items, currentID, runner.config.RecentLines),
		Steps:         history.lines,
		Pins:          board.Lines(semanticNowNS(runner.clock)),
		AgentSpeaking: agentOutput.Active,
		AgentSaying:   agentOutput.Saying,
		AgentOutputProtected: slices.Contains(
			agentOutput.ProtectedStreams, request.streamID,
		),
		InFlight: agentOutput.InFlight,
	}
	for _, tool := range update.Invocation.Tools {
		line := tool.Name
		if strings.TrimSpace(tool.Description) != "" {
			line += " - " + strings.TrimSpace(tool.Description)
		}
		state.Tools = append(state.Tools, line)
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
			state.SinceStepKnown = true
			state.HeardSinceStep = wordsAdded(history.previousHeard, state.Heard)
			state.AnsweredSoFar = history.answeredHeard
		}
	}
	return state, nil
}

// semanticPolicyRecent is the conversation the policy is shown: settled
// utterances and what the agent said, before the event being decided. Live
// partials are left out - the step history shows each one with the choice
// it got, and rendered as conversation they read as the person repeating
// themselves, which measured as a fast model counting the same animal on
// every revision - and a line two adjacent items repeat is shown once.
func semanticPolicyRecent(items []trajectory.Item, currentID string, maximum int) []string {
	end := len(items)
	if currentID != "" {
		for index, item := range items {
			if item.ID == currentID {
				end = index
				break
			}
		}
	}
	kept := make([]trajectory.Item, 0, end)
	for _, item := range items[:end] {
		if semanticSpokenObservation(item) && strings.HasSuffix(item.Event.Type, ".revision") {
			continue
		}
		kept = append(kept, item)
	}
	lines := coreinteraction.RecentLines(kept, maximum)
	deduped := lines[:0]
	for _, line := range lines {
		if len(deduped) > 0 && deduped[len(deduped)-1] == line {
			continue
		}
		deduped = append(deduped, line)
	}
	return deduped
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

// semanticHeardSince reconstructs the bounded completed speech added after
// the last completed assistant utterance at the playback boundary. A
// recognizer may endpoint one spoken thought at a breath and a later response
// may supersede a prepared-but-unheard answer; neither event makes the earlier
// clause old evidence. Silent cognition and canceled voice output likewise do
// not claim a conversational turn.
func semanticHeardSince(
	items []trajectory.Item, currentID, speaker string, maximum int,
) string {
	endpoints := semanticUnansweredEndpoints(items, currentID, speaker, maximum)
	parts := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parts = append(parts, strings.TrimSpace(endpoint.Content))
	}
	return strings.Join(parts, " ")
}

// Keep the canonical items that supply the utterance, so extraction can omit
// those same observations from recent history without matching their text.
func semanticUnansweredEndpoints(
	items []trajectory.Item, currentID, speaker string, maximum int,
) []trajectory.Item {
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
	parts := make([]trajectory.Item, 0, min(maximum, 8))
	for index := current; index >= 0 && len(parts) < maximum; index-- {
		item := items[index]
		switch item.Kind {
		case trajectory.KindAssistantState:
			if item.AssistantState != nil && item.AssistantState.Visibility == trajectory.VisibilityPlayed &&
				(item.AssistantState.Heard == nil || item.AssistantState.Heard.Complete()) {
				if _, spoken := audible[item.AssistantState.AssistantItemID]; spoken {
					return parts
				}
			}
		case trajectory.KindAssistant:
			if _, spoken := audible[item.ID]; spoken && item.Visibility == trajectory.VisibilityPlayed && !item.Interrupted {
				return parts
			}
		case trajectory.KindObservation:
			if !semanticExtractableObservation(item) {
				continue
			}
			if coreinteraction.SpeakerOf(item) != speaker {
				return parts
			}
			text := strings.TrimSpace(item.Content)
			if text != "" {
				parts = append([]trajectory.Item{item}, parts...)
			}
		}
	}
	return parts
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
	current := len(items)
	for index, item := range items {
		if item.ID == currentID {
			current = index
			break
		}
	}
	// The finished utterance already contains its earlier endpoint clauses.
	// Showing those clauses again as recent conversation invents repetition.
	// Exact item and recognizer-stream identities also let us omit the partial
	// hypotheses superseded by those endpoints, including corrected wording or
	// speaker attribution. Other turns with the same text remain history.
	consumed := make(map[string]struct{})
	type revisionStream struct{ source, correlation string }
	finalized := make(map[revisionStream]uint64)
	if current < len(items) && semanticExtractableObservation(items[current]) {
		for _, endpoint := range semanticUnansweredEndpoints(
			items[:current+1], currentID, coreinteraction.SpeakerOf(items[current]), maximum,
		) {
			consumed[endpoint.ID] = struct{}{}
			if endpoint.Event.CorrelationID != "" && endpoint.SourceRevision > 0 {
				stream := revisionStream{endpoint.Event.Source, endpoint.Event.CorrelationID}
				finalized[stream] = max(finalized[stream], endpoint.SourceRevision)
			}
		}
	}
	before := make([]trajectory.Item, 0, current)
	for _, item := range items[:current] {
		if _, included := consumed[item.ID]; included {
			continue
		}
		if semanticSpokenObservation(item) && strings.HasSuffix(item.Event.Type, ".revision") {
			stream := revisionStream{item.Event.Source, item.Event.CorrelationID}
			if revision, found := finalized[stream]; found && item.SourceRevision > 0 && item.SourceRevision < revision {
				continue
			}
		}
		before = append(before, item)
	}
	return coreinteraction.RecentLines(before, maximum)
}

// standingLines states policies the way the report shows them.
func standingLines(instructions []coreinteraction.StandingInstruction) []string {
	lines := make([]string, 0, len(instructions))
	for _, instruction := range instructions {
		line := instruction.Text + " (" + string(instruction.Scope)
		if instruction.Counting {
			line += ", counting"
		}
		if instruction.Restricting {
			line += ", restricting"
		}
		lines = append(lines, line+")")
	}
	return lines
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
	choice := result.choice
	stepIndex := -1
	if request.operation == "committed" {
		stepIndex = runner.recordStep(result)
	}
	decision := SemanticDecision{
		Operation: request.operation, Choice: choice, SpokeOver: result.spokeOver, Event: result.event,
		Policy: result.policy, EvidenceItemID: request.envelope.ItemID, StreamID: request.streamID,
		SourceRevision: request.sourceRev, ContextVersion: request.version,
		InvocationDigest: result.digest, Provider: runner.entry.descriptor.Provider,
		Model: runner.entry.descriptor.Model, Confidence: result.outcome.Confidence,
		Measured: result.outcome.Measured, DecisionStage: result.stage,
		StandingBefore: len(result.standingBefore), StandingAfter: len(result.standingAfter),
		StandingPinned: result.standingPinned, StandingRevoked: result.standingRevoked,
		StartedNS: result.started, FinishedNS: result.ended,
		Evidence: result.evidence, Standing: result.standing, Questions: result.questions,
	}
	decisionEnvelope := request.envelope.Clone()
	decisionEnvelope.Type = runner.ports.decision.Type()
	decisionEnvelope.ItemID = decisionItemID
	decisionEnvelope.Sequence = sequence
	decisionEnvelope.Payload = decision
	decisionEnvelope.CausalParents = appendUnique(decisionEnvelope.CausalParents, request.envelope.ItemID)
	decisionEnvelope.CausalParents = appendUnique(decisionEnvelope.CausalParents, result.sample.envelope.ItemID)
	// The decision goes out first, whatever it was. stop reaches the overlap
	// controller through it, so stop+speak cancels the old output and admits
	// the new one in one event rather than a retry after the old one retires.
	if _, err := runner.ports.decision.Broadcast(ctx, decisionEnvelope); err != nil {
		return err
	}
	if request.operation != "committed" && choice.Speaking && choice.Speak && !choice.Stop {
		// An explicit request cannot queue a second response behind output
		// that is still running; the transcript lane owns that. It can stop
		// the output and respond instead - stop+speak falls through below.
		runner.state.Refused++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionRefused, Operation: request.operation, Choice: &choice, Event: result.event,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: "speech_in_flight",
			Message: "explicit response creation cannot queue behind active voice output; the transcript lane owns in-flight control",
		})
	}
	if choice.Stop {
		runner.state.Stopped++
	}
	if !choice.Speak {
		code, message := "listen", "the interaction policy chose not to invoke the voice"
		switch {
		case choice.Stop:
			code, message = "stop", "the interaction policy stopped the active voice output and invoked nothing"
		case choice.Speaking:
			code, message = "keep", "the interaction policy kept the active voice output"
		}
		runner.state.Suppressed++
		return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
			Kind: SemanticAdmissionSuppressed, Operation: request.operation, Choice: &choice, Event: result.event,
			StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
			DecisionItemID: decisionItemID, Code: code, Message: message,
		})
	}
	branch := request.envelope.Clone()
	branch.CausalParents = appendUnique(branch.CausalParents, decisionItemID)
	output := runner.ports.voiceCreate
	if request.operation == "committed" {
		output = runner.ports.voiceCommitted
	}
	branch.Type = output.Type()
	if request.operation == "committed" {
		branch.Payload = SemanticGrant{
			Commit: rebasedSemanticCommit(request, result.sample), Choice: choice, DecisionItemID: decisionItemID,
			SpokeOver: result.spokeOver,
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
	runner.state.AdmittedVoice++
	// Lockstep: nothing is decided until this generation has answered.
	runner.hold = &semanticHold{
		sinceNS:        runner.clock.NowNS(),
		startedAtGrant: runner.agentOutput.GenerationsStarted, finishedAtGrant: runner.agentOutput.GenerationsFinished,
	}
	if stepIndex >= 0 {
		runner.steps[stepIndex].spoke = true
	}
	return runner.publishOutcome(ctx, request.envelope, SemanticAdmissionOutcome{
		Kind: SemanticAdmissionAdmitted, Operation: request.operation, Choice: &choice, Event: result.event,
		StreamID: request.streamID, SourceRevision: request.sourceRev, ContextVersion: request.version,
		DecisionItemID: decisionItemID,
	})
}

// rebasedSemanticCommit moves a grant's committed context to the context the
// decision was actually taken against, when that is newer than the commit's
// own: the generation it admits then sees everything the policy saw.
func rebasedSemanticCommit(request semanticRequest, sample semanticContextSample) stateelements.ObservationCommitOutcome {
	commit := request.commit
	if sample.snapshot.Version <= request.version || sample.envelope.ItemID == "" {
		return commit
	}
	identity, err := trajectory.IdentifyPrefix(sample.snapshot, sample.snapshot.Version)
	if err != nil {
		return commit
	}
	commit.StoreVersion = sample.snapshot.Version
	commit.Context = stateelements.CommittedContext{Prefix: identity, StateItemID: sample.envelope.ItemID}
	return commit
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
	runner.state.Holding = runner.hold != nil
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
		output.Generating != 0 || len(output.ProtectedStreams) != 0) {
		return errors.New("semantic admission inactive agent output carries active lifecycle state")
	}
	if output.Generating < 0 {
		return errors.New("semantic admission agent output generating count must not be negative")
	}
	if output.GenerationsFinished > output.GenerationsStarted {
		return errors.New("semantic admission agent output finished more generations than it started")
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
