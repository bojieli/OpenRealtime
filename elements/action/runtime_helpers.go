package action

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	legacyaction "github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/authority"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type runtimeDependencies struct {
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator
}

func reportActionResolution(
	reporter element.ResolutionReporter, descriptor element.Descriptor,
	capabilities []element.CapabilityResolution,
) error {
	if reporter == nil {
		return fmt.Errorf("%s has no live resolution reporter", descriptor.Name)
	}
	identity, err := descriptor.Identity()
	if err != nil {
		return fmt.Errorf("resolve %s runtime identity: %w", descriptor.Name, err)
	}
	if err := reporter.Runtime(
		"go://github.com/bojieli/OpenRealtime/elements/action/"+descriptor.Name,
		fmt.Sprintf("descriptor:%d", descriptor.Revision), identity.Digest,
	); err != nil {
		return err
	}
	return reporter.Capabilities(capabilities)
}

func actionCapability(
	name, contract, providerID string, serviceRevision uint64, digest string,
) element.CapabilityResolution {
	return element.CapabilityResolution{
		Name: name, Contract: contract, ProviderID: providerID,
		ProviderRevision: fmt.Sprintf("service:%d", serviceRevision), ProviderDigest: digest,
	}
}

// ledgerArchitectureCapability identifies the deployed ledger service without
// publishing its per-session capability identity. A fresh session receives a
// distinct HMAC key so actions cannot cross session boundaries; the public
// hash of that key belongs in executable action evidence, not in the immutable
// graph treatment compared across benchmark attempts.
func ledgerArchitectureCapability(reference string, serviceRevision uint64) element.CapabilityResolution {
	return actionCapability(
		"ledger", "action.Ledger/v1", "ledger://"+reference, serviceRevision, "",
	)
}

func resolveRuntimeDependencies(services element.Services) (runtimeDependencies, error) {
	clockService, _, found := services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return runtimeDependencies{}, errors.New("action element has no runtime clock service")
	}
	clock, ok := clockService.(graphruntime.Clock)
	if !ok || reflectedNil(clock) {
		return runtimeDependencies{}, fmt.Errorf("runtime clock service has type %T", clockService)
	}
	sequenceService, _, found := services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return runtimeDependencies{}, errors.New("action element has no runtime sequence service")
	}
	sequences, ok := sequenceService.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return runtimeDependencies{}, fmt.Errorf("runtime sequence service has type %T", sequenceService)
	}
	return runtimeDependencies{clock: clock, sequences: sequences}, nil
}

type emitter struct {
	instance  string
	clock     graphruntime.Clock
	sequences *graphruntime.SequenceAllocator
}

func (emitter emitter) envelope(parent element.Envelope, valueType element.Type, payload any, label string) (element.Envelope, error) {
	sequence, err := emitter.sequences.Next(emitter.instance + "." + label)
	if err != nil {
		return element.Envelope{}, err
	}
	result := parent.Clone()
	result.Type = valueType.Clone()
	result.ItemID = fmt.Sprintf("%s/%s/%d", emitter.instance, label, sequence)
	result.Payload = payload
	result.CausalParents = appendUnique(result.CausalParents, parent.ItemID)
	return result, nil
}

func (emitter emitter) resolution(value Resolution) (element.Envelope, error) {
	sequence, err := emitter.sequences.Next(emitter.instance + ".resolution")
	if err != nil {
		return element.Envelope{}, err
	}
	return element.Envelope{
		Type: resolutionType.Clone(), ItemID: fmt.Sprintf("%s/resolution/%d", emitter.instance, sequence),
		ReceiveNS: emitter.clock.NowNS(), Payload: value,
	}, nil
}

func broadcast(ctx context.Context, output element.OutputPort, envelope element.Envelope) error {
	_, err := output.Broadcast(ctx, envelope)
	return err
}

func publishResolution(ctx context.Context, emit emitter, output element.OutputPort, resolution Resolution) error {
	envelope, err := emit.resolution(resolution)
	if err != nil {
		return err
	}
	return broadcast(ctx, output, envelope)
}

func publishPayload(
	ctx context.Context, emit emitter, output element.OutputPort, parent element.Envelope,
	valueType element.Type, payload any, label string,
) error {
	envelope, err := emit.envelope(parent, valueType, payload, label)
	if err != nil {
		return err
	}
	return broadcast(ctx, output, envelope)
}

func publishOutcome(
	ctx context.Context, emit emitter, output element.OutputPort, parent element.Envelope, outcome Outcome,
) error {
	if outcome.FinishedNS == 0 {
		outcome.FinishedNS = emit.clock.NowNS()
	}
	return publishPayload(ctx, emit, output, parent, outcomeType, outcome, "outcome")
}

type receivedInput struct {
	kind     string
	envelope element.Envelope
}

func receiveInputs(
	ctx context.Context, kind string, port element.InputPort, destination chan<- receivedInput,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := port.Receive(ctx)
		if err != nil {
			if ctx.Err() == nil {
				select {
				case failures <- fmt.Errorf("receive %s: %w", kind, err):
				case <-ctx.Done():
				}
			}
			return
		}
		select {
		case destination <- receivedInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func proposalPayload(payload any) (cognitionelements.ToolProposal, bool) {
	switch value := payload.(type) {
	case cognitionelements.ToolProposal:
		return cloneProposal(value), true
	case *cognitionelements.ToolProposal:
		if value != nil {
			return cloneProposal(*value), true
		}
	}
	return cognitionelements.ToolProposal{}, false
}

func authorityCandidatePayload(payload any) (authority.Candidate, bool) {
	switch value := payload.(type) {
	case authority.Candidate:
		return value, true
	case *authority.Candidate:
		if value != nil {
			return *value, true
		}
	}
	return authority.Candidate{}, false
}

func provenancePayload(payload any) (Provenance, bool) {
	switch value := payload.(type) {
	case Provenance:
		return value, true
	case *Provenance:
		if value != nil {
			return *value, true
		}
	}
	return Provenance{}, false
}

func admittedPayload(payload any) (AdmittedProposal, bool) {
	switch value := payload.(type) {
	case AdmittedProposal:
		return cloneAdmitted(value), true
	case *AdmittedProposal:
		if value != nil {
			return cloneAdmitted(*value), true
		}
	}
	return AdmittedProposal{}, false
}

func declaredPayload(payload any) (DeclaredAction, bool) {
	switch value := payload.(type) {
	case DeclaredAction:
		return cloneDeclared(value), true
	case *DeclaredAction:
		if value != nil {
			return cloneDeclared(*value), true
		}
	}
	return DeclaredAction{}, false
}

func confirmedPayload(payload any) (ConfirmedAction, bool) {
	switch value := payload.(type) {
	case ConfirmedAction:
		return cloneConfirmed(value), true
	case *ConfirmedAction:
		if value != nil {
			return cloneConfirmed(*value), true
		}
	}
	return ConfirmedAction{}, false
}

func authorizedPayload(payload any) (AuthorizedAction, bool) {
	switch value := payload.(type) {
	case AuthorizedAction:
		return cloneAuthorized(value), true
	case *AuthorizedAction:
		if value != nil {
			return cloneAuthorized(*value), true
		}
	}
	return AuthorizedAction{}, false
}

func canonicalActionPayload(payload any) (CanonicalAction, bool) {
	switch value := payload.(type) {
	case CanonicalAction:
		return cloneCanonicalAction(value), true
	case *CanonicalAction:
		if value != nil {
			return cloneCanonicalAction(*value), true
		}
	}
	return CanonicalAction{}, false
}

func executablePayload(payload any) (ExecutableAction, bool) {
	switch value := payload.(type) {
	case ExecutableAction:
		return cloneExecutable(value), true
	case *ExecutableAction:
		if value != nil {
			return cloneExecutable(*value), true
		}
	}
	return ExecutableAction{}, false
}

func executionResultPayload(payload any) (ExecutionResult, bool) {
	switch value := payload.(type) {
	case ExecutionResult:
		return cloneExecutionResult(value), true
	case *ExecutionResult:
		if value != nil {
			return cloneExecutionResult(*value), true
		}
	}
	return ExecutionResult{}, false
}

func canonicalResultPayload(payload any) (CanonicalResult, bool) {
	switch value := payload.(type) {
	case CanonicalResult:
		return cloneCanonicalResult(value), true
	case *CanonicalResult:
		if value != nil {
			return cloneCanonicalResult(*value), true
		}
	}
	return CanonicalResult{}, false
}

func interruptAddress(envelope element.Envelope) (Interrupt, string, error) {
	var interrupt Interrupt
	switch value := envelope.Payload.(type) {
	case Interrupt:
		interrupt = value
	case *Interrupt:
		if value == nil {
			return Interrupt{}, "invalid_payload", errors.New("interrupt payload is nil")
		}
		interrupt = *value
	default:
		return Interrupt{}, "invalid_payload", fmt.Errorf("interrupt payload has type %T", envelope.Payload)
	}
	interrupt.CallID = strings.TrimSpace(interrupt.CallID)
	if interrupt.CallID == "" {
		return Interrupt{}, "missing_call_id", errors.New("interrupt payload requires an explicit call ID")
	}
	interrupt.Reason = strings.TrimSpace(interrupt.Reason)
	return interrupt, "", nil
}

// actionScopeKey is the internal identity of one proposed external effect.
// Provider call IDs are only idempotency keys within a cognition invocation;
// they are not deployment-global names. Length-prefixed hashing prevents an
// adversarial delimiter from aliasing a different session/run/call tuple.
func actionScopeKey(sessionID, runID, callID string) string {
	hash := sha256.New()
	for _, identity := range []string{sessionID, runID, callID} {
		_, _ = fmt.Fprintf(hash, "%d:", len(identity))
		_, _ = hash.Write([]byte(identity))
	}
	return fmt.Sprintf("sha256:%x", hash.Sum(nil))
}

func actionRunKey(sessionID, runID string) string {
	return actionScopeKey(sessionID, runID, "")
}

func actionSessionCallKey(sessionID, callID string) string {
	return actionScopeKey(sessionID, "", callID)
}

func actionIdentity(admitted AdmittedProposal) string {
	return actionScopeKey(admitted.SessionID, admitted.ModelRunID, admitted.Proposal.Call.CallID)
}

func actionCommitmentID(admitted AdmittedProposal) string {
	return "action:" + actionIdentity(admitted)
}

func interruptIdentity(envelope element.Envelope, interrupt Interrupt) (string, string, error) {
	sessionID := strings.TrimSpace(envelope.SessionID)
	runID := strings.TrimSpace(envelope.RunID)
	if sessionID == "" {
		return "", "missing_session", errors.New("action interrupt requires a non-empty session ID")
	}
	if runID == "" {
		return "", "missing_model_run", errors.New("action interrupt requires a non-empty cognition run ID")
	}
	if scope := strings.TrimSpace(envelope.CancellationScope); scope != "" && scope != runID {
		return "", "cancellation_scope_mismatch", fmt.Errorf(
			"action interrupt cancellation scope %q differs from cognition run %q", scope, runID)
	}
	return actionScopeKey(sessionID, runID, interrupt.CallID), "", nil
}

func validateToolCall(call trajectory.ToolCall) error {
	if strings.TrimSpace(call.CallID) == "" || strings.TrimSpace(call.Name) == "" {
		return errors.New("tool call ID and name are required")
	}
	if len(call.Arguments) == 0 || !json.Valid(call.Arguments) {
		return errors.New("tool call arguments must be valid JSON")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(call.Arguments, &object); err != nil || object == nil {
		return errors.New("tool call arguments must be one JSON object")
	}
	return nil
}

func validateProposal(proposal cognitionelements.ToolProposal) error {
	if err := validateToolCall(proposal.Call); err != nil {
		return err
	}
	if !proposal.Declared {
		return errors.New("provider emitted a tool absent from its invocation declaration")
	}
	switch proposal.ProviderAuthority {
	case continuation.ToolAuthorityPropose, continuation.ToolAuthorityExecute:
		return nil
	default:
		return fmt.Errorf("provider authority %q cannot emit a tool proposal", proposal.ProviderAuthority)
	}
}

func validateAdmittedProposal(value AdmittedProposal) error {
	if err := validateProposal(value.Proposal); err != nil {
		return err
	}
	for name, identity := range map[string]string{
		"proposal item":            value.ProposalItemID,
		"candidate item":           value.CandidateItemID,
		"result item":              value.ResultItemID,
		"model run":                value.ModelRunID,
		"session":                  value.SessionID,
		"activation item":          value.ActivationItemID,
		"activation cause":         value.ActivationCauseItemID,
		"authority item":           value.AuthorityItemID,
		"observation trigger item": value.ObservationTriggerItemID,
		"context envelope item":    value.ContextEnvelopeItemID,
		"context tail item":        value.ContextTailItem,
		"provider reference":       value.ProviderReference,
		"model result digest":      value.ModelResultDigest,
		"model provider":           value.ModelProducer.Provider,
		"model name":               value.ModelProducer.Model,
		"model reasoning effort":   value.ModelProducer.ReasoningEffort,
		"model speech authority":   value.ModelProducer.SpeechAuthority,
	} {
		if strings.TrimSpace(identity) == "" {
			return fmt.Errorf("admitted proposal requires a non-empty %s identity", name)
		}
	}
	if value.SourceRevision == 0 || value.ContextVersion == 0 {
		return errors.New("admitted proposal requires positive source and context revisions")
	}
	if err := validateModelProducerEvidence(value.ModelProducer); err != nil ||
		!strings.HasPrefix(value.ModelResultDigest, "sha256:") {
		return errors.New("admitted proposal requires exact model producer and result evidence")
	}
	if value.Authority != trajectory.AuthorityUser && value.Authority != trajectory.AuthoritySystem {
		return fmt.Errorf("authority %q cannot authorize an external effect", value.Authority)
	}
	return nil
}

func validateModelProducerEvidence(producer trajectory.Producer) error {
	if producer.Phase != trajectory.PhaseFast && producer.Phase != trajectory.PhaseSlow {
		return errors.New("model producer phase must be fast or slow")
	}
	if strings.TrimSpace(producer.Provider) == "" || strings.TrimSpace(producer.Model) == "" {
		return errors.New("model producer requires provider and model identities")
	}
	if _, err := continuation.ParseEffort(producer.ReasoningEffort); err != nil {
		return err
	}
	switch continuation.SpeechAuthority(producer.SpeechAuthority) {
	case continuation.SpeechAuthorityVoice, continuation.SpeechAuthoritySilent:
		return nil
	default:
		return errors.New("model producer requires explicit effective speech authority")
	}
}

func validateDeclaredAction(value DeclaredAction) error {
	if err := validateAdmittedProposal(value.Admitted); err != nil {
		return err
	}
	if strings.TrimSpace(value.RegistryReference) == "" || strings.TrimSpace(value.RegistryDigest) == "" ||
		strings.TrimSpace(value.DeclarationDigest) == "" {
		return errors.New("declared action has incomplete immutable registry resolution")
	}
	_, err := legacyaction.ParseConfirm(string(value.Confirmation))
	return err
}

func validateConfirmedAction(value ConfirmedAction) error {
	if err := validateDeclaredAction(value.Declared); err != nil {
		return err
	}
	confirm, err := legacyaction.ParseConfirm(string(value.Declared.Confirmation))
	if err != nil {
		return err
	}
	providerReference := strings.TrimSpace(value.ProviderReference)
	providerIdentity := strings.TrimSpace(value.ProviderIdentity)
	capability := strings.TrimSpace(value.ConfirmationCapability)
	if confirm == legacyaction.ConfirmNever {
		if value.ConfirmationNeeded || providerReference != "" || providerIdentity != "" || capability != "" {
			return errors.New("confirmation-free action carries unexpected provider evidence")
		}
		return nil
	}
	if !value.ConfirmationNeeded || providerReference == "" || providerIdentity == "" || capability == "" {
		return errors.New("required confirmation has no exact provider decision")
	}
	return nil
}

func validateActionEnvelopeIdentity(
	envelope element.Envelope, admitted AdmittedProposal,
) (string, error) {
	if strings.TrimSpace(admitted.SessionID) == "" || strings.TrimSpace(envelope.SessionID) == "" ||
		envelope.SessionID != admitted.SessionID {
		return "session_mismatch", errors.New("action envelope and admitted evidence must name one non-empty session")
	}
	if strings.TrimSpace(admitted.ModelRunID) == "" || strings.TrimSpace(envelope.RunID) == "" ||
		envelope.RunID != admitted.ModelRunID {
		return "model_run_mismatch", errors.New("action envelope and admitted evidence must name one cognition run")
	}
	return "", nil
}

// attestDeploymentAuthority re-resolves every deployment-owned decision just
// before ledger admission/effect dispatch. Typed ports prevent ordinary graph
// bypasses; this attestation additionally fails closed if a custom element or
// replayed payload forges declaration, confirmation, or target evidence.
func attestDeploymentAuthority(
	tools *ToolRegistries, targets *TargetRegistries, confirmations *ConfirmationProviders,
	authorized AuthorizedAction,
) error {
	if err := validateAuthorizedAction(authorized); err != nil {
		return err
	}
	declared := authorized.Confirmed.Declared
	call := callOfDeclared(declared)
	set, err := tools.resolve(declared.RegistryReference)
	if err != nil {
		return err
	}
	if set.digest != declared.RegistryDigest {
		return errors.New("declared tool registry digest does not match deployment resolution")
	}
	tool, found, err := set.lookup(call.Name)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("tool %q is not declared by the resolved registry", call.Name)
	}
	if tool.digest != declared.DeclarationDigest || tool.dispatcherIdentity != declared.DispatcherIdentity ||
		tool.spec.Confirm != declared.Confirmation || tool.spec.Target != declared.Target ||
		tool.spec.Background != declared.Background {
		return errors.New("declared action differs from the immutable deployment tool declaration")
	}
	target, err := targets.resolve(authorized.TargetReference)
	if err != nil {
		return err
	}
	if target.digest != authorized.TargetDigest {
		return errors.New("authorized target digest does not match deployment resolution")
	}
	if err := validateTargetAuthorization(target, authorized.Confirmed); err != nil {
		return err
	}
	confirm, err := legacyaction.ParseConfirm(string(declared.Confirmation))
	if err != nil {
		return err
	}
	if confirm == legacyaction.ConfirmNever {
		return nil
	}
	registration, err := confirmations.resolve(authorized.Confirmed.ProviderReference)
	if err != nil {
		return err
	}
	if !registration.verify(authorized.Confirmed) {
		return errors.New("confirmation decision capability does not verify")
	}
	return nil
}

func cloneProposal(proposal cognitionelements.ToolProposal) cognitionelements.ToolProposal {
	proposal.Call = cloneToolCall(proposal.Call)
	return proposal
}

func cloneToolCall(call trajectory.ToolCall) trajectory.ToolCall {
	call.Arguments = slices.Clone(call.Arguments)
	return call
}

func cloneToolResult(result trajectory.ToolResult) trajectory.ToolResult {
	result.Output = slices.Clone(result.Output)
	return result
}

func cloneAdmitted(value AdmittedProposal) AdmittedProposal {
	value.Proposal = cloneProposal(value.Proposal)
	return value
}

func cloneDeclared(value DeclaredAction) DeclaredAction {
	value.Admitted = cloneAdmitted(value.Admitted)
	return value
}

func cloneConfirmed(value ConfirmedAction) ConfirmedAction {
	value.Declared = cloneDeclared(value.Declared)
	return value
}

func cloneAuthorized(value AuthorizedAction) AuthorizedAction {
	value.Confirmed = cloneConfirmed(value.Confirmed)
	return value
}

func cloneCanonicalAction(value CanonicalAction) CanonicalAction {
	value.Authorized = cloneAuthorized(value.Authorized)
	return value
}

func cloneExecutable(value ExecutableAction) ExecutableAction {
	value.Canonical = cloneCanonicalAction(value.Canonical)
	return value
}

func cloneExecutionResult(value ExecutionResult) ExecutionResult {
	value.Executable = cloneExecutable(value.Executable)
	value.Result = cloneToolResult(value.Result)
	return value
}

func cloneCanonicalResult(value CanonicalResult) CanonicalResult {
	value.Execution = cloneExecutionResult(value.Execution)
	return value
}

func callOfAdmitted(value AdmittedProposal) trajectory.ToolCall { return value.Proposal.Call }
func callOfDeclared(value DeclaredAction) trajectory.ToolCall   { return value.Admitted.Proposal.Call }
func callOfConfirmed(value ConfirmedAction) trajectory.ToolCall {
	return callOfDeclared(value.Declared)
}
func callOfAuthorized(value AuthorizedAction) trajectory.ToolCall {
	return callOfConfirmed(value.Confirmed)
}
func callOfCanonical(value CanonicalAction) trajectory.ToolCall {
	return callOfAuthorized(value.Authorized)
}
func callOfExecutable(value ExecutableAction) trajectory.ToolCall {
	return callOfCanonical(value.Canonical)
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

type boundedSet struct {
	limit int
	order []string
	items map[string]struct{}
}

func newBoundedSet(limit int) *boundedSet {
	return &boundedSet{limit: limit, items: make(map[string]struct{})}
}

func (set *boundedSet) add(value string) {
	if _, found := set.items[value]; found {
		return
	}
	set.items[value] = struct{}{}
	set.order = append(set.order, value)
	for len(set.order) > set.limit {
		delete(set.items, set.order[0])
		set.order = set.order[1:]
	}
}

func (set *boundedSet) contains(value string) bool {
	_, found := set.items[value]
	return found
}
