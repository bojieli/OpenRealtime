package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

// ResolveAttachmentDescriptor is an explicit, bounded adapter from an opaque
// handle to untrusted attachment bytes. It does not parse, narrate, or promote
// content authority; those are separate graph choices downstream.
func ResolveAttachmentDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "media.ResolveAttachment",
		Revision:      1,
		Ports: []element.Port{
			{Name: "resolve", Direction: element.Input, Type: resolveRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "fetched", Direction: element.Input, Type: fetchResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: storeCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "fetch", Direction: element.Output, Type: fetchRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "store_cancel", Direction: element.Output, Type: storeCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "return_lease", Direction: element.Output, Type: returnLeaseRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "resolved", Direction: element.Output, Type: resolvedAttachmentType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: resolverOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers: []string{"resolve", "fetched"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"fetch", "store_cancel", "return_lease", "resolved", "outcome"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/media/attachment-resolver-state/v1",
		ConfigSchema: "schema://openrealtime/media/attachment-resolver-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
	}
}

type ResolveAttachmentConfig struct {
	MaxPending       int      `json:"max_pending,omitempty"`
	MaxBytes         int      `json:"max_bytes,omitempty"`
	MaxMetadataBytes int      `json:"max_metadata_bytes,omitempty"`
	AllowedMIMETypes []string `json:"allowed_mime_types,omitempty"`
	TerminalMemory   int      `json:"terminal_memory,omitempty"`
}

func decodeResolveAttachmentConfig(source json.RawMessage) (ResolveAttachmentConfig, error) {
	config := ResolveAttachmentConfig{
		MaxPending: 32, MaxBytes: 32 << 20, MaxMetadataBytes: 64 << 10,
		AllowedMIMETypes: []string{"*/*"}, TerminalMemory: 512,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return ResolveAttachmentConfig{}, err
	}
	if config.MaxPending < 1 || config.MaxPending > 1_000_000 {
		return ResolveAttachmentConfig{}, errors.New("max_pending must be between 1 and 1000000")
	}
	if config.MaxBytes < 1 || config.MaxBytes > maximumConfigurationByteSize {
		return ResolveAttachmentConfig{}, fmt.Errorf("max_bytes must be between 1 and %d", maximumConfigurationByteSize)
	}
	if config.MaxMetadataBytes < 1 || config.MaxMetadataBytes > 1<<20 {
		return ResolveAttachmentConfig{}, errors.New("max_metadata_bytes must be between 1 and 1048576")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return ResolveAttachmentConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	canonical, err := canonicalMIMEPatterns(config.AllowedMIMETypes)
	if err != nil {
		return ResolveAttachmentConfig{}, err
	}
	config.AllowedMIMETypes = canonical
	return config, nil
}

type resolveAttachmentFactory struct{}

func (resolveAttachmentFactory) Descriptor() element.Descriptor { return ResolveAttachmentDescriptor() }

func (resolveAttachmentFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeResolveAttachmentConfig(source)
	return err
}

func (resolveAttachmentFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeResolveAttachmentConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("media.ResolveAttachment %s config: %w", mount.InstanceID, err)
	}
	clock, err := runtimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	sequences, err := sequenceDependency(mount.Services)
	if err != nil {
		return nil, err
	}
	resolveInput, err := mount.Ports.Input("resolve")
	if err != nil {
		return nil, err
	}
	fetchedInput, err := mount.Ports.Input("fetched")
	if err != nil {
		return nil, err
	}
	cancelInput, err := mount.Ports.Input("cancel")
	if err != nil {
		return nil, err
	}
	fetchOutput, err := mount.Ports.Output("fetch")
	if err != nil {
		return nil, err
	}
	storeCancelOutput, err := mount.Ports.Output("store_cancel")
	if err != nil {
		return nil, err
	}
	returnLeaseOutput, err := mount.Ports.Output("return_lease")
	if err != nil {
		return nil, err
	}
	resolvedOutput, err := mount.Ports.Output("resolved")
	if err != nil {
		return nil, err
	}
	outcomeOutput, err := mount.Ports.Output("outcome")
	if err != nil {
		return nil, err
	}
	return &resolveAttachmentRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		resolution: mount.Resolution, resolveInput: resolveInput,
		fetchedInput: fetchedInput, cancelInput: cancelInput,
		fetchOutput: fetchOutput, storeCancelOutput: storeCancelOutput,
		returnLeaseOutput: returnLeaseOutput,
		resolvedOutput:    resolvedOutput, outcomeOutput: outcomeOutput,
		pending: make(map[string]pendingResolution), byReply: make(map[string]string),
		terminal: make(map[string]struct{}), preCanceled: make(map[string]string),
	}, nil
}

type pendingResolution struct {
	cause           element.Envelope
	request         ResolveRequest
	fetchID         string
	expectedReplyID string
	canceled        bool
	reason          string
}

type resolveAttachmentRunner struct {
	instance   string
	config     ResolveAttachmentConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter

	resolveInput, fetchedInput, cancelInput           element.InputPort
	fetchOutput, storeCancelOutput, returnLeaseOutput element.OutputPort
	resolvedOutput, outcomeOutput                     element.OutputPort

	pending        map[string]pendingResolution
	byReply        map[string]string
	terminal       map[string]struct{}
	terminalOrder  []string
	preCanceled    map[string]string
	preCancelOrder []string
}

func (runner *resolveAttachmentRunner) Run(parent context.Context) error {
	if err := reportLiveResolution(runner.resolution, attachmentResolverRuntimeID, nil); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan retentionInput)
	failures := make(chan error, 3)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{{"resolve", runner.resolveInput}, {"fetched", runner.fetchedInput}, {"cancel", runner.cancelInput}} {
		wait.Add(1)
		go receiveRetentionInputs(ctx, source.kind, source.port, false, inputs, failures, &wait)
	}
	defer func() { cancel(nil); wait.Wait() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case "resolve":
				err = runner.begin(ctx, input.envelope)
			case "fetched":
				err = runner.complete(ctx, input.envelope)
			case "cancel":
				err = runner.cancel(ctx, input.envelope)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *resolveAttachmentRunner) begin(ctx context.Context, envelope element.Envelope) error {
	request, ok := resolveRequestPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, ResolvedAttachment{Kind: ResultRefused, Code: "invalid_payload", Message: fmt.Sprintf("resolve payload has type %T", envelope.Payload)})
	}
	requestID := envelope.ItemID
	if err := validateIdentifier("resolve request ID", requestID, true); err != nil {
		return runner.finishDiagnostic(ctx, envelope, ResolvedAttachment{
			Kind: ResultRefused, Handle: request.Handle,
			Code: "invalid_request_id", Message: err.Error(),
		})
	}
	if _, exists := runner.pending[requestID]; exists {
		return runner.finishDiagnostic(ctx, envelope, ResolvedAttachment{Kind: ResultRefused, Handle: request.Handle, Code: "duplicate_pending", Message: "resolve request ID is already pending"})
	}
	if _, terminal := runner.terminal[requestID]; terminal {
		return runner.finishDiagnostic(ctx, envelope, ResolvedAttachment{Kind: ResultRefused, Handle: request.Handle, Code: "duplicate_request", Message: "resolve request ID is already terminal"})
	}
	if reason, canceled := runner.takePreCancel(requestID); canceled {
		return runner.finish(ctx, envelope, ResolvedAttachment{Kind: ResultCanceled, Handle: request.Handle, Code: "canceled", Message: reason})
	}
	if len(runner.pending) >= runner.config.MaxPending {
		return runner.finish(ctx, envelope, ResolvedAttachment{Kind: ResultRefused, Handle: request.Handle, Code: "pending_limit", Message: "attachment resolver pending bound reached"})
	}
	if err := runner.validateRequest(request); err != nil {
		return runner.finish(ctx, envelope, ResolvedAttachment{Kind: ResultRefused, Handle: request.Handle, Code: "invalid_request", Message: err.Error()})
	}
	fetchID, err := runner.nextItemID(requestID, "fetch")
	if err != nil {
		return err
	}
	expectedReplyID := fetchID + ":fetched"
	runner.pending[requestID] = pendingResolution{
		cause: envelope.Clone(), request: request, fetchID: fetchID,
		expectedReplyID: expectedReplyID,
	}
	runner.byReply[expectedReplyID] = requestID
	fetchEnvelope := envelope.Clone()
	fetchEnvelope.Type = fetchRequestType
	fetchEnvelope.ItemID = fetchID
	fetchEnvelope.CausalParents = []string{envelope.ItemID}
	fetchEnvelope.Payload = FetchRequest{
		Handle: request.Handle, ExpectedSHA256: request.Handle.SHA256,
		ExpectedSourceRevision: firstRevision(request.ExpectedSourceRevision, request.Handle.SourceRevision),
	}
	if _, err := runner.fetchOutput.Broadcast(ctx, fetchEnvelope); err != nil {
		delete(runner.pending, requestID)
		delete(runner.byReply, expectedReplyID)
		return err
	}
	return nil
}

func (runner *resolveAttachmentRunner) complete(ctx context.Context, envelope element.Envelope) error {
	requestID, found := runner.byReply[envelope.ItemID]
	if !found {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultIgnored, Code: "unknown_fetch_reply", Message: "fetch reply has no pending resolution"})
	}
	pending, found := runner.pending[requestID]
	if !found || pending.expectedReplyID != envelope.ItemID {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultIgnored, RequestID: requestID, Code: "stale_fetch_reply", Message: "fetch reply no longer has matching pending state"})
	}
	if envelope.SessionID != pending.cause.SessionID {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "cross_session_fetch_reply", Message: "fetch reply crossed the pending request session boundary"})
	}
	if len(envelope.CausalParents) != 1 || envelope.CausalParents[0] != pending.fetchID {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "invalid_fetch_parent", Message: "fetch reply does not name the exact immediate fetch request"})
	}
	result, ok := fetchResultPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "invalid_fetch_reply", Message: fmt.Sprintf("fetch reply payload has type %T", envelope.Payload)})
	}
	if result.Handle != pending.request.Handle {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "fetch_handle_mismatch", Message: "fetch reply does not carry the exact requested attachment handle"})
	}
	if err := validateResultKind(result.Kind); err != nil {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "invalid_fetch_result", Message: err.Error()})
	}
	if result.Kind == ResultSucceeded {
		if err := runner.validateFetched(envelope.SessionID, pending.request, result); err != nil {
			return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "fetched_content_mismatch", Message: err.Error()})
		}
	} else if result.Lease.state != nil || result.Lease.LeaseID != "" {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "unexpected_fetch_lease", Message: "non-successful fetch reply carried a blob lease"})
	}
	delete(runner.pending, requestID)
	delete(runner.byReply, pending.expectedReplyID)
	if pending.canceled {
		if result.Kind == ResultSucceeded {
			if err := runner.returnLease(ctx, envelope, result.Lease, "resolution canceled"); err != nil {
				return err
			}
		}
		return runner.finish(ctx, pending.cause, ResolvedAttachment{Kind: ResultCanceled, Handle: pending.request.Handle, Code: "canceled", Message: pending.reason})
	}
	if result.Kind != ResultSucceeded {
		return runner.finish(ctx, pending.cause, ResolvedAttachment{Kind: result.Kind, Handle: pending.request.Handle, Code: result.Code, Message: result.Message})
	}
	return runner.finish(ctx, pending.cause, ResolvedAttachment{Kind: ResultSucceeded, Handle: result.Handle, Lease: result.Lease})
}

func (runner *resolveAttachmentRunner) cancel(ctx context.Context, envelope element.Envelope) error {
	request, ok := storeCancelPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload)})
	}
	requestID := firstNonempty(request.RequestID, envelope.RunID, envelope.CancellationScope)
	if requestID == "" {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, Code: "missing_request_id", Message: "cancel requires an addressed resolve request ID"})
	}
	if err := validateIdentifier("cancel request ID", requestID, true); err != nil {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultRefused, Code: "invalid_request_id", Message: err.Error()})
	}
	request.Reason = boundedReason(request.Reason)
	if _, terminal := runner.terminal[requestID]; terminal {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultIgnored, RequestID: requestID, Code: "already_terminal", Message: "resolve request is already terminal"})
	}
	pending, found := runner.pending[requestID]
	if !found {
		runner.recordPreCancel(requestID, request.Reason)
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultSucceeded, RequestID: requestID, Code: "cancel_recorded", Message: request.Reason})
	}
	if pending.canceled {
		return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultIgnored, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "already_canceling"})
	}
	pending.canceled = true
	pending.reason = request.Reason
	runner.pending[requestID] = pending
	cancelID, err := runner.nextItemID(envelope.ItemID, "store_cancel")
	if err != nil {
		return err
	}
	cancelEnvelope := envelope.Clone()
	cancelEnvelope.Type = storeCancelType
	cancelEnvelope.ItemID = cancelID
	cancelEnvelope.RunID = pending.fetchID
	cancelEnvelope.CancellationScope = pending.fetchID
	cancelEnvelope.CausalParents = []string{envelope.ItemID}
	cancelEnvelope.Payload = StoreCancel{RequestID: pending.fetchID, Reason: request.Reason}
	if _, err := runner.storeCancelOutput.Broadcast(ctx, cancelEnvelope); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, envelope, ResolverOutcome{Kind: ResultSucceeded, RequestID: requestID, Handle: pending.request.Handle.Handle, Code: "cancel_forwarded", Message: request.Reason})
}

func (runner *resolveAttachmentRunner) validateRequest(request ResolveRequest) error {
	handle := request.Handle
	if err := errors.Join(
		validateIdentifier("handle", handle.Handle, true),
		validateIdentifier("capability", handle.Capability, true),
		validateIdentifier("attachment ID", handle.AttachmentID, true),
		validateIdentifier("source", handle.Source, true),
		validateIdentifier("scope", handle.Scope, true),
	); err != nil {
		return err
	}
	if handle.MIMEType == "" {
		return errors.New("MIME type is required")
	}
	if err := handle.Kind.validate(); err != nil {
		return err
	}
	if handle.SourceRevision == 0 {
		return errors.New("attachment handle source revision must be positive")
	}
	canonicalMIME, err := canonicalMIMEType(handle.MIMEType)
	if err != nil || canonicalMIME != handle.MIMEType {
		if err != nil {
			return err
		}
		return errors.New("attachment handle MIME type is not canonical")
	}
	metadataBytes := len(handle.Handle) + len(handle.Capability) + len(handle.AttachmentID) +
		len(handle.Name) + len(handle.MIMEType) + len(handle.SHA256) + len(handle.Source) + len(handle.Scope)
	for _, pattern := range request.AcceptMIMETypes {
		metadataBytes += len(pattern)
	}
	if metadataBytes > runner.config.MaxMetadataBytes {
		return fmt.Errorf("resolve metadata has %d bytes; max_metadata_bytes is %d",
			metadataBytes, runner.config.MaxMetadataBytes)
	}
	if canonical, err := canonicalSHA256(handle.SHA256); err != nil || canonical == "" || canonical != handle.SHA256 {
		if err != nil {
			return err
		}
		return errors.New("attachment handle requires a canonical content digest")
	}
	if handle.Bytes < 1 || handle.Bytes > runner.config.MaxBytes {
		return fmt.Errorf("attachment declares %d bytes; resolver max_bytes is %d", handle.Bytes, runner.config.MaxBytes)
	}
	if !matchesAnyMIME(handle.MIMEType, runner.config.AllowedMIMETypes) {
		return fmt.Errorf("MIME type %q is not allowed by resolver configuration", handle.MIMEType)
	}
	if request.MaxBytes < 0 || request.MaxBytes > runner.config.MaxBytes {
		return fmt.Errorf("request max_bytes must be between 0 and %d", runner.config.MaxBytes)
	}
	if request.MaxBytes > 0 && handle.Bytes > request.MaxBytes {
		return fmt.Errorf("attachment declares %d bytes; request max_bytes is %d", handle.Bytes, request.MaxBytes)
	}
	patterns, err := canonicalMIMEPatterns(request.AcceptMIMETypes)
	if err != nil {
		return err
	}
	if len(patterns) > 0 && !matchesAnyMIME(handle.MIMEType, patterns) {
		return fmt.Errorf("MIME type %q is not accepted by this request", handle.MIMEType)
	}
	if request.ExpectedSourceRevision != 0 && request.ExpectedSourceRevision != handle.SourceRevision {
		return errors.New("requested source revision does not match the handle")
	}
	if handle.Width < 0 || handle.Height < 0 {
		return errors.New("attachment dimensions cannot be negative")
	}
	if handle.Kind == AttachmentImage && (handle.Width <= 0 || handle.Height <= 0) {
		return errors.New("image attachment handle requires positive dimensions")
	}
	return nil
}

func (runner *resolveAttachmentRunner) validateFetched(sessionID string, request ResolveRequest, result FetchResult) error {
	handle := request.Handle
	if result.Handle != handle || result.Lease.Handle != handle {
		return errors.New("retained handle identity drifted during resolution")
	}
	if err := result.Lease.validateFor(sessionID, handle); err != nil {
		return fmt.Errorf("invalid live blob lease: %w", err)
	}
	size, err := result.Lease.Size()
	if err != nil {
		return fmt.Errorf("inspect live blob lease: %w", err)
	}
	if size != handle.Bytes || size > runner.config.MaxBytes {
		return errors.New("resolved byte length does not match the bounded handle metadata")
	}
	if request.MaxBytes > 0 && size > request.MaxBytes {
		return errors.New("resolved content exceeds request max_bytes")
	}
	return nil
}

func validateResultKind(kind ResultKind) error {
	switch kind {
	case ResultSucceeded, ResultCanceled, ResultRefused, ResultFailed, ResultExpired, ResultIgnored:
		return nil
	default:
		return fmt.Errorf("unsupported result kind %q", kind)
	}
}

func (runner *resolveAttachmentRunner) finish(ctx context.Context, cause element.Envelope, result ResolvedAttachment) error {
	runner.rememberTerminal(cause.ItemID)
	return runner.publishResolved(ctx, cause, result, false)
}

func (runner *resolveAttachmentRunner) finishDiagnostic(ctx context.Context, cause element.Envelope, result ResolvedAttachment) error {
	return runner.publishResolved(ctx, cause, result, true)
}

func (runner *resolveAttachmentRunner) publishResolved(ctx context.Context, cause element.Envelope, result ResolvedAttachment, diagnostic bool) error {
	itemID := cause.ItemID + ":resolved"
	if diagnostic {
		var err error
		itemID, err = runner.nextItemID(itemID, "duplicate")
		if err != nil {
			return err
		}
	}
	envelope := cause.Clone()
	envelope.Type = resolvedAttachmentType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = result
	if _, err := runner.resolvedOutput.Broadcast(ctx, envelope); err != nil {
		return err
	}
	bytes := 0
	if result.Kind == ResultSucceeded {
		bytes = result.Handle.Bytes
	}
	return runner.publishOutcome(ctx, cause, ResolverOutcome{Kind: result.Kind, RequestID: cause.ItemID, Handle: result.Handle.Handle, Bytes: bytes, Code: result.Code, Message: result.Message})
}

func (runner *resolveAttachmentRunner) publishOutcome(ctx context.Context, cause element.Envelope, outcome ResolverOutcome) error {
	itemID, err := runner.nextItemID(cause.ItemID, "resolver_outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = resolverOutcomeType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = outcome
	_, err = runner.outcomeOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *resolveAttachmentRunner) returnLease(ctx context.Context, cause element.Envelope, lease BlobLease, reason string) error {
	itemID, err := runner.nextItemID(cause.ItemID, "return_lease")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = returnLeaseRequestType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = ReturnLeaseRequest{Lease: lease, Reason: reason}
	_, err = runner.returnLeaseOutput.Broadcast(ctx, envelope)
	return err
}

func (runner *resolveAttachmentRunner) nextItemID(parent, label string) (string, error) {
	sequence, err := runner.sequences.Next(runner.instance + "." + label)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%d", parent, label, sequence), nil
}

func (runner *resolveAttachmentRunner) rememberTerminal(requestID string) {
	if requestID == "" {
		return
	}
	if _, found := runner.terminal[requestID]; !found {
		runner.terminal[requestID] = struct{}{}
		runner.terminalOrder = append(runner.terminalOrder, requestID)
	}
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *resolveAttachmentRunner) recordPreCancel(requestID, reason string) {
	if _, found := runner.preCanceled[requestID]; !found {
		runner.preCancelOrder = append(runner.preCancelOrder, requestID)
	}
	runner.preCanceled[requestID] = boundedReason(reason)
	for len(runner.preCancelOrder) > runner.config.TerminalMemory {
		oldest := runner.preCancelOrder[0]
		runner.preCancelOrder = runner.preCancelOrder[1:]
		delete(runner.preCanceled, oldest)
	}
}

func (runner *resolveAttachmentRunner) takePreCancel(requestID string) (string, bool) {
	reason, found := runner.preCanceled[requestID]
	if !found {
		return "", false
	}
	delete(runner.preCanceled, requestID)
	if index := slices.Index(runner.preCancelOrder, requestID); index >= 0 {
		runner.preCancelOrder = slices.Delete(runner.preCancelOrder, index, index+1)
	}
	return reason, true
}

func resolveRequestPayload(payload any) (ResolveRequest, bool) {
	switch value := payload.(type) {
	case ResolveRequest:
		return cloneResolveRequest(value), true
	case *ResolveRequest:
		if value != nil {
			return cloneResolveRequest(*value), true
		}
	}
	return ResolveRequest{}, false
}

func fetchResultPayload(payload any) (FetchResult, bool) {
	switch value := payload.(type) {
	case FetchResult:
		return cloneFetchResult(value), true
	case *FetchResult:
		if value != nil {
			return cloneFetchResult(*value), true
		}
	}
	return FetchResult{}, false
}

func canonicalMIMEPatterns(source []string) ([]string, error) {
	if len(source) == 0 {
		return nil, nil
	}
	if len(source) > 256 {
		return nil, errors.New("MIME pattern list exceeds 256 entries")
	}
	result := make([]string, 0, len(source))
	seen := make(map[string]struct{}, len(source))
	for _, raw := range source {
		if len(raw) > 256 {
			return nil, errors.New("MIME pattern exceeds 256 bytes")
		}
		value := strings.ToLower(strings.TrimSpace(raw))
		parts := strings.Split(value, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" ||
			(parts[0] == "*" && parts[1] != "*") || strings.ContainsAny(value, " \r\n\t") {
			return nil, fmt.Errorf("invalid MIME pattern %q", raw)
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	slices.Sort(result)
	return result, nil
}

func matchesAnyMIME(mimeType string, patterns []string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	parts := strings.Split(mimeType, "/")
	if len(parts) != 2 {
		return false
	}
	for _, pattern := range patterns {
		patternParts := strings.Split(pattern, "/")
		if patternParts[0] != "*" && patternParts[0] != parts[0] {
			continue
		}
		if patternParts[1] == "*" || patternParts[1] == parts[1] {
			return true
		}
	}
	return false
}

func firstRevision(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}
