package interaction

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type modelResultCommitFactory struct{}

var (
	_ element.Factory         = modelResultCommitFactory{}
	_ element.ConfigValidator = modelResultCommitFactory{}
)

func (modelResultCommitFactory) Descriptor() element.Descriptor {
	return ModelResultCommitDescriptor()
}

func (modelResultCommitFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeCommitConfig(source)
	return err
}

func (modelResultCommitFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeCommitConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("interaction.ModelResultCommit %s config: %w", mount.InstanceID, err)
	}
	clockService, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("interaction model commit has no runtime clock service")
	}
	clock, ok := clockService.(graphruntime.Clock)
	if !ok || reflectedNilInterface(clock) {
		return nil, fmt.Errorf("runtime clock service has type %T", clockService)
	}
	sequenceService, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("interaction model commit has no runtime sequence service")
	}
	sequences, ok := sequenceService.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceService)
	}
	resultInput, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	commitInput, err := mount.Ports.Input("committed")
	if err != nil {
		return nil, err
	}
	rejectionInput, err := mount.Ports.Input("rejected")
	if err != nil {
		return nil, err
	}
	appendOutput, err := mount.Ports.Output("append")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &modelResultCommitRunner{
		instance:       mount.InstanceID,
		config:         config,
		clock:          clock,
		sequences:      sequences,
		resultInput:    resultInput,
		commitInput:    commitInput,
		rejectionInput: rejectionInput,
		appendOutput:   appendOutput,
		outcomeOutput:  outcomeOutput,
		pending:        make(map[string]pendingModelCommit, config.MaxPending),
		pendingRun:     make(map[string]string, config.MaxPending),
		resolvedRuns:   make(map[string]struct{}, config.MaxPending),
		resolution:     mount.Resolution,
	}, nil
}

type modelCommitInputKind uint8

const (
	modelResultInput modelCommitInputKind = iota
	modelCommittedInput
	modelRejectedInput
)

type modelCommitInput struct {
	kind     modelCommitInputKind
	envelope element.Envelope
}

type pendingModelCommit struct {
	cause           element.Envelope
	runID           string
	expectedVersion uint64
	contextTailID   string
	itemIDs         []string
	itemsDigest     [sha256.Size]byte
	prefix          trajectory.PrefixIdentity
	history         bool
	items           []trajectory.Item
	historyResult   *cognitionelements.Result
}

type modelResultCommitRunner struct {
	instance  string
	config    ModelResultCommitConfig
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator

	resultInput    element.InputPort
	commitInput    element.InputPort
	rejectionInput element.InputPort
	appendOutput   element.OutputPort
	outcomeOutput  element.OutputPort

	pending       map[string]pendingModelCommit
	pendingRun    map[string]string
	resolvedRuns  map[string]struct{}
	resolvedOrder []string
	resolution    element.ResolutionReporter
}

func (runner *modelResultCommitRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportInteractionResolution(runner.resolution, ModelResultCommitDescriptor()); err != nil {
		return err
	}
	inputs := make(chan modelCommitInput)
	failures := make(chan error, 3)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind modelCommitInputKind
		port element.InputPort
	}{
		{modelResultInput, runner.resultInput},
		{modelCommittedInput, runner.commitInput},
		{modelRejectedInput, runner.rejectionInput},
	} {
		receivers.Add(1)
		go receiveModelCommitInput(ctx, source.kind, source.port, inputs, failures, &receivers)
	}
	defer func() {
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
		case input := <-inputs:
			var err error
			switch input.kind {
			case modelResultInput:
				err = runner.acceptResult(ctx, input.envelope)
			case modelCommittedInput:
				err = runner.acceptCommit(ctx, input.envelope)
			case modelRejectedInput:
				err = runner.acceptRejection(ctx, input.envelope)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *modelResultCommitRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	result, valid := cognitionResultPayload(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if valid {
		if runID == "" {
			runID = strings.TrimSpace(result.RunID)
		}
	}
	if !valid {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelRefused, RunID: runID, Code: "invalid_payload",
			Message: fmt.Sprintf("cognition result payload has type %T", envelope.Payload),
		})
	}
	if strings.TrimSpace(result.RunID) == "" || runID == "" {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelRefused, RunID: runID, Code: "missing_run_id",
			Message: "cognition result requires a run ID",
		})
	}
	if result.RunID != runID || (strings.TrimSpace(envelope.RunID) != "" && envelope.RunID != result.RunID) {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelRefused, RunID: runID, Code: "conflicting_run_id",
			Message: "result payload and envelope name different run IDs",
		})
	}
	if _, resolved := runner.resolvedRuns[runID]; resolved {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelIgnored, RunID: runID, Code: "already_resolved",
		})
	}
	if requestID := runner.pendingRun[runID]; requestID != "" {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelIgnored, RunID: runID, RequestID: requestID, Code: "already_pending",
		})
	}
	if len(runner.pending) >= runner.config.MaxPending {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelRefused, RunID: runID, Code: "pending_capacity",
			Message: fmt.Sprintf("model commit retains at most %d requests", runner.config.MaxPending),
		})
	}
	if err := validateCognitionResult(result); err != nil {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelRefused, RunID: runID, Code: "invalid_result", Message: err.Error(),
		})
	}
	return runner.startResultCommit(ctx, envelope, result, false)
}

func (runner *modelResultCommitRunner) startResultCommit(
	ctx context.Context, envelope element.Envelope, result cognitionelements.Result, history bool,
) error {
	runID := result.RunID
	items, err := runner.buildResultItems(result)
	if err != nil {
		return err
	}
	requestSequence, err := runner.sequences.Next(runner.instance + ".request")
	if err != nil {
		return err
	}
	requestID := fmt.Sprintf("%s:model-append:%d", runner.instance, requestSequence)
	itemIDs := make([]string, len(items))
	for index := range items {
		itemIDs[index] = items[index].ID
	}
	pending := pendingModelCommit{
		cause: envelope.Clone(), runID: runID, expectedVersion: result.ContextVersion,
		contextTailID: result.ContextTailID, itemIDs: slices.Clone(itemIDs),
		itemsDigest: digestTrajectoryItems(items), prefix: result.ContextPrefix,
		history: history,
	}
	if history {
		pending.items = cloneTrajectoryItems(items)
	} else if runner.config.RetainRejectedSpeech &&
		result.Descriptor.EffectiveSpeechAuthority() == continuation.SpeechAuthorityVoice &&
		strings.TrimSpace(result.AssistantText) != "" && result.ContextPrefix.Digest != "" {
		// Retain only the sanitized speech surface. Native provider state can
		// contain tool calls or reasoning and cannot be relabeled as speech.
		speech := result
		speech.Outputs = nil
		for _, output := range result.Outputs {
			if output.Kind == cognitionelements.PreparedAssistant {
				speech.Outputs = append(speech.Outputs, output)
			}
		}
		speech.ReasoningText, speech.ReasoningRetained = "", false
		speech.ToolProposals, speech.Completion = nil, continuation.Completion{}
		pending.historyResult = &speech
	}
	runner.pending[requestID] = pending
	runner.pendingRun[runID] = requestID
	appendEnvelope := envelope.Clone()
	appendEnvelope.Type = appendType
	appendEnvelope.ItemID = requestID
	appendEnvelope.RunID = runID
	appendEnvelope.CausalParents = appendUniqueString(appendEnvelope.CausalParents, envelope.ItemID)
	request := stateelements.Append{
		Compare: true, ExpectedVersion: result.ContextVersion, Items: cloneTrajectoryItems(items),
	}
	if history {
		prefix := result.ContextPrefix
		request.Compare, request.Prefix = false, &prefix
	}
	appendEnvelope.Payload = request
	delivery, err := runner.appendOutput.Broadcast(ctx, appendEnvelope)
	if err != nil {
		runner.removePending(requestID, pending)
		return err
	}
	if delivery.Delivered != 1 {
		runner.removePending(requestID, pending)
		return fmt.Errorf("model append %s delivered to %d lanes", requestID, delivery.Delivered)
	}
	return nil
}

func validateCognitionResult(result cognitionelements.Result) error {
	if err := continuation.ValidateDescriptor(result.Descriptor); err != nil {
		return fmt.Errorf("invalid provider descriptor: %w", err)
	}
	if err := continuation.ValidateInvocation(result.Invocation, result.Descriptor); err != nil {
		return fmt.Errorf("invalid invocation: %w", err)
	}
	if result.ContextVersion == 0 && strings.TrimSpace(result.ContextTailID) != "" {
		return errors.New("empty context cannot name a tail item")
	}
	if result.ContextVersion > 0 && strings.TrimSpace(result.ContextTailID) == "" {
		return errors.New("non-empty context requires its tail item ID")
	}
	if result.ContextPrefix.Digest != "" && result.ContextPrefix.Version != result.ContextVersion {
		return errors.New("context prefix disagrees with the result context version")
	}
	var assistant strings.Builder
	var reasoning strings.Builder
	var proposals []cognitionelements.ToolProposal
	for index, output := range result.Outputs {
		switch output.Kind {
		case cognitionelements.PreparedReasoning:
			if output.Proposal != nil || output.Text == "" {
				return fmt.Errorf("reasoning output %d requires text and no proposal", index)
			}
			reasoning.WriteString(output.Text)
		case cognitionelements.PreparedAssistant:
			if output.Proposal != nil || output.Text == "" {
				return fmt.Errorf("assistant output %d requires text and no proposal", index)
			}
			assistant.WriteString(output.Text)
		case cognitionelements.PreparedTool:
			if output.Proposal == nil || output.Text != "" {
				return fmt.Errorf("tool output %d requires a proposal and no text", index)
			}
			proposals = append(proposals, cloneCognitionProposal(*output.Proposal))
		default:
			return fmt.Errorf("output %d has unknown kind %q", index, output.Kind)
		}
	}
	if assistant.String() != result.AssistantText || reasoning.String() != result.ReasoningText {
		return errors.New("ordered outputs disagree with aggregate assistant or reasoning text")
	}
	if !result.ReasoningRetained && reasoning.Len() != 0 {
		return errors.New("result claims unretained reasoning but contains reasoning output")
	}
	// Zero tool proposals have one semantic representation even though the
	// producer's defensive clone materializes an empty slice. Nil-versus-empty
	// is not an ordered-output disagreement.
	if len(proposals) != len(result.ToolProposals) ||
		len(proposals) != 0 && !reflect.DeepEqual(proposals, result.ToolProposals) {
		return errors.New("ordered outputs disagree with aggregate tool proposals")
	}
	return nil
}

func (runner *modelResultCommitRunner) buildResultItems(
	result cognitionelements.Result,
) ([]trajectory.Item, error) {
	producer := trajectory.Producer{
		Phase:           result.Descriptor.Phase,
		Provider:        result.Descriptor.Provider,
		Model:           result.Descriptor.Model,
		ReasoningEffort: string(result.Descriptor.Effort),
		// Retained strictly as provider provenance. Speech routing never reads it.
		SpeechAuthority: string(result.Descriptor.EffectiveSpeechAuthority()),
	}
	instruction, err := runner.newTrajectoryItem(result, trajectory.KindInstruction,
		trajectory.Producer{Phase: trajectory.PhaseRuntime})
	if err != nil {
		return nil, err
	}
	instruction.Content = result.Invocation.Instruction
	if result.ContextTailID != "" {
		instruction.CausalParentIDs = []string{result.ContextTailID}
	}
	items := []trajectory.Item{instruction}
	parentID := instruction.ID
	hasProposal := slices.ContainsFunc(result.Outputs, func(output cognitionelements.PreparedOutput) bool {
		return output.Kind == cognitionelements.PreparedTool
	})
	providerStateAttached := false
	for _, output := range result.Outputs {
		if output.Kind == cognitionelements.PreparedTool && result.Interrupted {
			// Interrupted action intent has no safe point. In particular, it must
			// never be upgraded from proposal data into an executable call.
			continue
		}
		item, itemErr := runner.newTrajectoryItem(result, trajectory.KindReasoning, producer)
		if itemErr != nil {
			return nil, itemErr
		}
		item.CausalParentIDs = []string{parentID}
		item.Interrupted = result.Interrupted
		switch output.Kind {
		case cognitionelements.PreparedReasoning:
			item.Kind = trajectory.KindReasoning
			item.Content = output.Text
		case cognitionelements.PreparedAssistant:
			item.Kind = trajectory.KindAssistant
			item.Content = output.Text
			item.Visibility = trajectory.VisibilityPrepared
		case cognitionelements.PreparedTool:
			item.Kind = trajectory.KindToolProposal
			call := cloneTrajectoryToolCall(output.Proposal.Call)
			item.ToolCall = &call
		}
		if !hasProposal && !providerStateAttached && len(result.Completion.ProviderState) > 0 {
			item.ProviderStateType = result.Completion.ProviderStateType
			item.ProviderState = slices.Clone(result.Completion.ProviderState)
			providerStateAttached = true
		}
		items = append(items, item)
		parentID = item.ID
	}
	if !hasProposal && !providerStateAttached && len(result.Completion.ProviderState) > 0 {
		item, itemErr := runner.newTrajectoryItem(result, trajectory.KindReasoning, producer)
		if itemErr != nil {
			return nil, itemErr
		}
		item.CausalParentIDs = []string{parentID}
		item.Interrupted = result.Interrupted
		item.ProviderStateType = result.Completion.ProviderStateType
		item.ProviderState = slices.Clone(result.Completion.ProviderState)
		items = append(items, item)
	}
	return items, nil
}

func (runner *modelResultCommitRunner) newTrajectoryItem(
	result cognitionelements.Result, kind trajectory.Kind, producer trajectory.Producer,
) (trajectory.Item, error) {
	sequence, err := runner.sequences.Next(runner.instance + ".item")
	if err != nil {
		return trajectory.Item{}, err
	}
	return trajectory.Item{
		ID:   fmt.Sprintf("%s:model-item:%d", runner.instance, sequence),
		Kind: kind, MonotonicNS: runner.clock.NowNS(),
		SourceRevision: result.Invocation.SourceRevision,
		InvocationID:   result.RunID, Producer: producer,
	}, nil
}

func (runner *modelResultCommitRunner) acceptCommit(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, pending, found, correlationErr := runner.pendingForReply(envelope)
	if correlationErr != nil {
		return correlationErr
	}
	if !found {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelIgnored, RunID: strings.TrimSpace(envelope.RunID),
			Code: "unknown_commit_reply", Message: "trajectory commit has no pending request",
		})
	}
	commit, valid := trajectoryCommitPayload(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory commit reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if !slices.Equal(commit.AppendedIDs, pending.itemIDs) {
		return fmt.Errorf("trajectory commit reply %s attests %v, want %v",
			envelope.ItemID, commit.AppendedIDs, pending.itemIDs)
	}
	if uint64(len(pending.itemIDs)) > ^uint64(0)-pending.expectedVersion {
		return fmt.Errorf("trajectory commit reply %s overflows the expected version", envelope.ItemID)
	}
	wantVersion := pending.expectedVersion + uint64(len(pending.itemIDs))
	versionValid := commit.Version == wantVersion
	if pending.history {
		versionValid = commit.Version >= wantVersion
		if err := trajectory.VerifyPrefix(commit.Snapshot, pending.prefix); err != nil {
			return fmt.Errorf("speech history commit changed its source prefix: %w", err)
		}
	}
	if !versionValid || commit.Snapshot.Version != commit.Version ||
		uint64(len(commit.Snapshot.Items)) != commit.Snapshot.Version {
		return fmt.Errorf("trajectory commit reply %s has inconsistent version/snapshot: got %d/%d/%d, want %d",
			envelope.ItemID, commit.Version, commit.Snapshot.Version,
			len(commit.Snapshot.Items), wantVersion)
	}
	if len(commit.Snapshot.Items) < len(pending.itemIDs) {
		return fmt.Errorf("trajectory commit reply %s snapshot omits appended items", envelope.ItemID)
	}
	if pending.expectedVersion == 0 {
		if pending.contextTailID != "" {
			return fmt.Errorf("trajectory commit reply %s binds an empty prefix to tail %q",
				envelope.ItemID, pending.contextTailID)
		}
	} else if pending.expectedVersion > uint64(len(commit.Snapshot.Items)) ||
		commit.Snapshot.Items[pending.expectedVersion-1].ID != pending.contextTailID {
		return fmt.Errorf("trajectory commit reply %s does not attest context tail %q at version %d",
			envelope.ItemID, pending.contextTailID, pending.expectedVersion)
	}
	committedTail := commit.Snapshot.Items[len(commit.Snapshot.Items)-len(pending.itemIDs):]
	for index, item := range committedTail {
		if item.ID != pending.itemIDs[index] {
			return fmt.Errorf("trajectory commit reply %s snapshot item %d is %q, want %q",
				envelope.ItemID, index, item.ID, pending.itemIDs[index])
		}
	}
	wantDigest := pending.itemsDigest
	if pending.history {
		// The history append owns canonical insertion time. Reconstruct its
		// exact timestamp normalization; every other source byte must match.
		expected := cloneTrajectoryItems(pending.items)
		boundary := uint64(0)
		start := len(commit.Snapshot.Items) - len(expected)
		if start > 0 {
			boundary = commit.Snapshot.Items[start-1].MonotonicNS
		}
		for index := range expected {
			expected[index].MonotonicNS = max(expected[index].MonotonicNS, boundary)
			boundary = expected[index].MonotonicNS
		}
		wantDigest = digestTrajectoryItems(expected)
	}
	if got := digestTrajectoryItems(committedTail); got != wantDigest {
		return fmt.Errorf("trajectory commit reply %s changed appended item contents", envelope.ItemID)
	}
	runner.removePending(requestID, pending)
	runner.rememberResolved(pending.runID)
	kind, code := ModelCommitted, ""
	if pending.history {
		kind, code = ModelSpeechRetained, "stale_speech_history"
	}
	return runner.publishModelCommitOutcome(ctx,
		modelCommitReplyCause(pending, requestID, envelope), ModelCommitOutcome{
			Kind: kind, Code: code, RunID: pending.runID, RequestID: requestID,
			StoreVersion: commit.Version, ItemIDs: slices.Clone(pending.itemIDs),
		})
}

func (runner *modelResultCommitRunner) acceptRejection(
	ctx context.Context, envelope element.Envelope,
) error {
	requestID, pending, found, correlationErr := runner.pendingForReply(envelope)
	if correlationErr != nil {
		return correlationErr
	}
	if !found {
		return runner.publishModelCommitOutcome(ctx, envelope, ModelCommitOutcome{
			Kind: ModelIgnored, RunID: strings.TrimSpace(envelope.RunID),
			Code: "unknown_rejection_reply", Message: "trajectory rejection has no pending request",
		})
	}
	rejection, valid := trajectoryRejectionPayload(envelope.Payload)
	if !valid {
		return fmt.Errorf("trajectory rejection reply %s has payload %T", envelope.ItemID, envelope.Payload)
	}
	if rejection.ExpectedVersion != pending.expectedVersion {
		return fmt.Errorf("trajectory rejection reply %s names expected version %d, want %d",
			envelope.ItemID, rejection.ExpectedVersion, pending.expectedVersion)
	}
	runner.removePending(requestID, pending)
	if !pending.history && pending.historyResult != nil && rejection.Code == "version_conflict" &&
		rejection.CurrentVersion > pending.expectedVersion {
		// Freshness refusal remains the cause of this separate history
		// transaction. Its committed payload contains no stale proposal.
		return runner.startResultCommit(ctx, modelCommitReplyCause(pending, requestID, envelope),
			*pending.historyResult, true)
	}
	runner.rememberResolved(pending.runID)
	return runner.publishModelCommitOutcome(ctx,
		modelCommitReplyCause(pending, requestID, envelope), ModelCommitOutcome{
			Kind: ModelRejected, RunID: pending.runID, RequestID: requestID,
			StoreVersion: rejection.CurrentVersion, ItemIDs: slices.Clone(pending.itemIDs),
			Code: rejection.Code, Message: rejection.Message,
		})
}

func modelCommitReplyCause(
	pending pendingModelCommit, requestID string, reply element.Envelope,
) element.Envelope {
	cause := pending.cause.Clone()
	cause.CausalParents = appendUniqueString(cause.CausalParents, requestID)
	cause.CausalParents = appendUniqueString(cause.CausalParents, reply.ItemID)
	return cause
}

func (runner *modelResultCommitRunner) pendingForReply(
	envelope element.Envelope,
) (string, pendingModelCommit, bool, error) {
	candidates := slices.Clone(envelope.CausalParents)
	for _, suffix := range []string{":committed", ":rejected"} {
		if strings.HasSuffix(envelope.ItemID, suffix) {
			candidates = append(candidates, strings.TrimSuffix(envelope.ItemID, suffix))
		}
	}
	var matchedID string
	var matched pendingModelCommit
	for _, candidate := range candidates {
		if pending, found := runner.pending[candidate]; found {
			if matchedID != "" && candidate != matchedID {
				return "", pendingModelCommit{}, false,
					fmt.Errorf("trajectory reply %s ambiguously names requests %s and %s",
						envelope.ItemID, matchedID, candidate)
			}
			matchedID, matched = candidate, pending
		}
	}
	if matchedID == "" {
		return "", pendingModelCommit{}, false, nil
	}
	if envelope.RunID != "" && envelope.RunID != matched.runID {
		return "", pendingModelCommit{}, false,
			fmt.Errorf("trajectory reply %s names run %q, want %q",
				envelope.ItemID, envelope.RunID, matched.runID)
	}
	if envelope.SessionID != matched.cause.SessionID {
		return "", pendingModelCommit{}, false,
			fmt.Errorf("trajectory reply %s crossed session boundary", envelope.ItemID)
	}
	return matchedID, matched, true, nil
}

func digestTrajectoryItems(items []trajectory.Item) [sha256.Size]byte {
	encoded, err := json.Marshal(items)
	if err != nil {
		// trajectory.Item contains only JSON-marshalable fields. Treating an
		// impossible encoder failure as a distinct digest still makes a forged
		// reply fail closed; validation of the append itself reports the cause.
		return sha256.Sum256([]byte("marshal-error:" + err.Error()))
	}
	return sha256.Sum256(encoded)
}

func (runner *modelResultCommitRunner) removePending(
	requestID string, pending pendingModelCommit,
) {
	delete(runner.pending, requestID)
	if runner.pendingRun[pending.runID] == requestID {
		delete(runner.pendingRun, pending.runID)
	}
}

func (runner *modelResultCommitRunner) rememberResolved(runID string) {
	if _, exists := runner.resolvedRuns[runID]; exists {
		return
	}
	if len(runner.resolvedOrder) == runner.config.MaxPending {
		oldest := runner.resolvedOrder[0]
		delete(runner.resolvedRuns, oldest)
		copy(runner.resolvedOrder, runner.resolvedOrder[1:])
		runner.resolvedOrder = runner.resolvedOrder[:len(runner.resolvedOrder)-1]
	}
	runner.resolvedRuns[runID] = struct{}{}
	runner.resolvedOrder = append(runner.resolvedOrder, runID)
}

func (runner *modelResultCommitRunner) publishModelCommitOutcome(
	ctx context.Context, cause element.Envelope, outcome ModelCommitOutcome,
) error {
	outcome.ItemIDs = slices.Clone(outcome.ItemIDs)
	envelope := cause.Clone()
	envelope.Type = modelCommitOutcomeType
	envelope.ItemID = cause.ItemID + ":model-commit-outcome:" + string(outcome.Kind)
	envelope.RunID = outcome.RunID
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.Payload = outcome
	return broadcastInteraction(ctx, runner.outcomeOutput, envelope)
}

func receiveModelCommitInput(
	ctx context.Context, kind modelCommitInputKind, input element.InputPort,
	output chan<- modelCommitInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalInteractionReceive(ctx, err) {
			return
		}
		if err != nil {
			sendInteractionFailure(ctx, failures, err)
			return
		}
		select {
		case output <- modelCommitInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func cognitionResultPayload(payload any) (cognitionelements.Result, bool) {
	var result cognitionelements.Result
	switch typed := payload.(type) {
	case cognitionelements.Result:
		result = typed
	case *cognitionelements.Result:
		if typed == nil {
			return cognitionelements.Result{}, false
		}
		result = *typed
	default:
		return cognitionelements.Result{}, false
	}
	result.Invocation.Capabilities = slices.Clone(result.Invocation.Capabilities)
	result.Invocation.Tools = slices.Clone(result.Invocation.Tools)
	for index := range result.Invocation.Tools {
		result.Invocation.Tools[index].Parameters = slices.Clone(result.Invocation.Tools[index].Parameters)
	}
	result.Outputs = slices.Clone(result.Outputs)
	for index := range result.Outputs {
		if result.Outputs[index].Proposal != nil {
			copy := cloneCognitionProposal(*result.Outputs[index].Proposal)
			result.Outputs[index].Proposal = &copy
		}
	}
	result.ToolProposals = slices.Clone(result.ToolProposals)
	for index := range result.ToolProposals {
		result.ToolProposals[index] = cloneCognitionProposal(result.ToolProposals[index])
	}
	result.Completion.ProviderState = slices.Clone(result.Completion.ProviderState)
	return result, true
}

func cloneCognitionProposal(proposal cognitionelements.ToolProposal) cognitionelements.ToolProposal {
	proposal.Call = cloneTrajectoryToolCall(proposal.Call)
	return proposal
}

func cloneTrajectoryToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func trajectoryCommitPayload(payload any) (stateelements.Commit, bool) {
	switch typed := payload.(type) {
	case stateelements.Commit:
		typed.AppendedIDs = slices.Clone(typed.AppendedIDs)
		return typed, true
	case *stateelements.Commit:
		if typed != nil {
			copy := *typed
			copy.AppendedIDs = slices.Clone(typed.AppendedIDs)
			return copy, true
		}
	}
	return stateelements.Commit{}, false
}

func trajectoryRejectionPayload(payload any) (stateelements.Rejection, bool) {
	switch typed := payload.(type) {
	case stateelements.Rejection:
		return typed, true
	case *stateelements.Rejection:
		if typed != nil {
			return *typed, true
		}
	}
	return stateelements.Rejection{}, false
}

func cloneTrajectoryItems(items []trajectory.Item) []trajectory.Item {
	result := make([]trajectory.Item, len(items))
	for index, item := range items {
		item.CausalParentIDs = slices.Clone(item.CausalParentIDs)
		item.ProviderState = slices.Clone(item.ProviderState)
		if item.ToolCall != nil {
			copy := cloneTrajectoryToolCall(*item.ToolCall)
			item.ToolCall = &copy
		}
		if item.ToolCallDerivation != nil {
			copy := *item.ToolCallDerivation
			copy.Rewrites = slices.Clone(item.ToolCallDerivation.Rewrites)
			item.ToolCallDerivation = &copy
		}
		result[index] = item
	}
	return result
}

func reflectedNilInterface(value any) bool {
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
