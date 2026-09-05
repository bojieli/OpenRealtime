package ingress

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

	"github.com/bojieli/OpenRealtime/element"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// UserContentDescriptor keeps modality and activation explicit while sharing
// only the state needed to join retention replies to their originating turn.
// It never parses files or narrates images; those choices remain downstream.
func UserContentDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "ingress.UserContent",
		Revision:      1,
		Ports: []element.Port{
			{Name: "text", Direction: element.Input, Type: userTextType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "image", Direction: element.Input, Type: userImageType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "file", Direction: element.Input, Type: userFileType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "attachment", Direction: element.Input, Type: userAttachmentType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "retained", Direction: element.Input, Type: mediaelements.RetainResultType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: contentCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "retain", Direction: element.Output, Type: mediaelements.RetainRequestType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "release", Direction: element.Output, Type: mediaelements.ReleaseRequestType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "store_cancel", Direction: element.Output, Type: mediaelements.StoreCancelType(),
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "observations", Direction: element.Output, Type: observationType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "handles", Direction: element.Output, Type: attachmentHandleType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: ingressOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:       []string{"text", "image", "file", "attachment", "retained"},
			Interrupts:     []string{"cancel"},
			Outcomes:       []string{"retain", "release", "store_cancel", "observations", "handles", "outcome"},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/ingress/user-content-state/v1",
		ConfigSchema: "schema://openrealtime/ingress/user-content-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "ingress.pending.memory", Reversible: true}},
	}
}

type UserContentConfig struct {
	Observer         string `json:"observer,omitempty"`
	TextSource       string `json:"text_source,omitempty"`
	AttachmentSource string `json:"attachment_source,omitempty"`
	RetentionScope   string `json:"retention_scope,omitempty"`
	MaxTextBytes     int    `json:"max_text_bytes,omitempty"`
	MaxMetadataBytes int    `json:"max_metadata_bytes,omitempty"`
	MaxInputBytes    int    `json:"max_input_bytes,omitempty"`
	MaxPending       int    `json:"max_pending,omitempty"`
	MaxPendingBytes  int    `json:"max_pending_bytes,omitempty"`
	MaxStreams       int    `json:"max_streams,omitempty"`
	TerminalMemory   int    `json:"terminal_memory,omitempty"`
}

func decodeUserContentConfig(source json.RawMessage) (UserContentConfig, error) {
	config := UserContentConfig{
		Observer: "client", TextSource: "text", AttachmentSource: "message",
		RetentionScope: "session", MaxTextBytes: 1 << 20,
		MaxMetadataBytes: 64 << 10, MaxInputBytes: 32 << 20,
		MaxPending: 32, MaxPendingBytes: 64 << 20, MaxStreams: 256, TerminalMemory: 512,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return UserContentConfig{}, err
	}
	for _, identity := range []struct {
		name  string
		value string
	}{
		{"observer", config.Observer}, {"text_source", config.TextSource},
		{"attachment_source", config.AttachmentSource}, {"retention_scope", config.RetentionScope},
	} {
		if _, err := canonicalID(identity.value, identity.name); err != nil {
			return UserContentConfig{}, err
		}
	}
	if config.MaxInputBytes < 1 || config.MaxInputBytes > maximumIngressBytes {
		return UserContentConfig{}, fmt.Errorf("max_input_bytes must be between 1 and %d", maximumIngressBytes)
	}
	if config.MaxTextBytes < 1 || config.MaxTextBytes > 64<<20 {
		return UserContentConfig{}, errors.New("max_text_bytes must be between 1 and 67108864")
	}
	if config.MaxMetadataBytes < 1 || config.MaxMetadataBytes > 1<<20 {
		return UserContentConfig{}, errors.New("max_metadata_bytes must be between 1 and 1048576")
	}
	if len(config.Observer)+len(config.TextSource)+len(config.AttachmentSource)+len(config.RetentionScope) > config.MaxMetadataBytes {
		return UserContentConfig{}, errors.New("configured ingress metadata exceeds max_metadata_bytes")
	}
	if config.MaxPending < 1 || config.MaxPending > 1_000_000 {
		return UserContentConfig{}, errors.New("max_pending must be between 1 and 1000000")
	}
	if config.MaxPendingBytes < config.MaxInputBytes || config.MaxPendingBytes > maximumIngressBytes {
		return UserContentConfig{}, fmt.Errorf("max_pending_bytes must be between max_input_bytes and %d", maximumIngressBytes)
	}
	if config.MaxStreams < 1 || config.MaxStreams > 1_000_000 {
		return UserContentConfig{}, errors.New("max_streams must be between 1 and 1000000")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return UserContentConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	return config, nil
}

type userContentFactory struct{}

func (userContentFactory) Descriptor() element.Descriptor { return UserContentDescriptor() }

func (userContentFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeUserContentConfig(source)
	return err
}

func (userContentFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeUserContentConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("ingress.UserContent %s config: %w", mount.InstanceID, err)
	}
	clockValue, _, found := mount.Services.Lookup(graphruntime.ClockServiceName)
	if !found {
		return nil, errors.New("user content ingress has no runtime clock service")
	}
	clock, ok := clockValue.(graphruntime.Clock)
	if !ok || clock == nil {
		return nil, fmt.Errorf("runtime clock service has type %T", clockValue)
	}
	sequenceValue, _, found := mount.Services.Lookup(graphruntime.SequenceServiceName)
	if !found {
		return nil, errors.New("user content ingress has no runtime sequence service")
	}
	sequences, ok := sequenceValue.(*graphruntime.SequenceAllocator)
	if !ok || sequences == nil {
		return nil, fmt.Errorf("runtime sequence service has type %T", sequenceValue)
	}
	ports, err := userContentPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &userContentRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		resolution: mount.Resolution, ports: ports,
		pending: make(map[string]pendingContent), pendingByReply: make(map[string]string),
		pendingByContent: make(map[string]string), pendingByItem: make(map[string]string),
		pendingByStream: make(map[string]string), streams: make(map[string]revisionState),
		terminal: make(map[string]struct{}), preCanceled: make(map[contentCancellationAddress]string),
	}, nil
}

type userContentPorts struct {
	text, image, file, attachment, retained, cancel element.InputPort
	retain, release, storeCancel                    element.OutputPort
	observations, handles, outcome                  element.OutputPort
}

func userContentPortsFrom(ports element.Ports) (userContentPorts, error) {
	var result userContentPorts
	inputs := []struct {
		name string
		set  *element.InputPort
	}{{"text", &result.text}, {"image", &result.image}, {"file", &result.file}, {"attachment", &result.attachment}, {"retained", &result.retained}, {"cancel", &result.cancel}}
	for _, input := range inputs {
		port, err := ports.Input(input.name)
		if err != nil {
			return result, err
		}
		*input.set = port
	}
	outputs := []struct {
		name string
		set  *element.OutputPort
	}{{"retain", &result.retain}, {"release", &result.release}, {"store_cancel", &result.storeCancel}, {"observations", &result.observations}, {"handles", &result.handles}, {"outcome", &result.outcome}}
	for _, output := range outputs {
		port, err := ports.Output(output.name)
		if err != nil {
			return result, err
		}
		*output.set = port
	}
	return result, nil
}

type pendingContent struct {
	cause           element.Envelope
	contentID       string
	streamID        string
	operation       string
	retainID        string
	expectedReplyID string
	description     string
	expectedHandle  mediaelements.AttachmentHandle
	bytes           int
	canceled        bool
	reason          string
}

type revisionState struct {
	revision uint64
	final    bool
}

type contentCancellationAddress struct {
	contentID string
	streamID  string
	sessionID string
}

type userContentRunner struct {
	instance   string
	config     UserContentConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      userContentPorts

	pending          map[string]pendingContent
	pendingByReply   map[string]string
	pendingByContent map[string]string
	pendingByItem    map[string]string
	pendingByStream  map[string]string
	pendingBytes     int
	streams          map[string]revisionState
	streamOrder      []string
	terminal         map[string]struct{}
	terminalOrder    []string
	preCanceled      map[contentCancellationAddress]string
	preCancelOrder   []contentCancellationAddress
}

type ingressInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *userContentRunner) Run(parent context.Context) error {
	if err := reportLiveResolution(runner.resolution); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan ingressInput)
	failures := make(chan error, 6)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
	}{{"text", runner.ports.text}, {"image", runner.ports.image}, {"file", runner.ports.file}, {"attachment", runner.ports.attachment}, {"retained", runner.ports.retained}, {"cancel", runner.ports.cancel}} {
		wait.Add(1)
		go receiveIngressInputs(ctx, source.kind, source.port, inputs, failures, &wait)
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
			case "text":
				err = runner.text(ctx, input.envelope)
			case "image", "file", "attachment":
				err = runner.attachment(ctx, input.kind, input.envelope)
			case "retained":
				err = runner.retained(ctx, input.envelope)
			case "cancel":
				err = runner.cancel(ctx, input.envelope)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *userContentRunner) text(ctx context.Context, envelope element.Envelope) error {
	input, ok := userTextPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", Code: "invalid_payload", Message: fmt.Sprintf("text payload has type %T", envelope.Payload)}, true)
	}
	contentID, streamID, err := runner.identities(envelope, input.ContentID, input.StreamID)
	if err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", Code: "invalid_identity", Message: err.Error()}, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, Code: "duplicate_request"}, false)
	}
	if pendingID := runner.pendingByItem[envelope.ItemID]; pendingID != "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, Code: "duplicate_pending", Message: fmt.Sprintf("request is already pending as %q", pendingID)}, false)
	}
	if reason, canceled := runner.takePreCancel(contentID, streamID, envelope.SessionID); canceled {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeCanceled, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "canceled", Message: reason}, true)
	}
	input.Text = strings.TrimSpace(input.Text)
	input.StableText = strings.TrimSpace(input.StableText)
	if input.Text == "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "empty_text", Message: "typed text is empty"}, true)
	}
	if len(input.Text) > runner.config.MaxTextBytes || len(input.StableText) > runner.config.MaxTextBytes {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "text_too_large", Message: fmt.Sprintf("typed text exceeds max_text_bytes %d", runner.config.MaxTextBytes)}, true)
	}
	if len(contentID)+len(streamID)+len(input.Language) > runner.config.MaxMetadataBytes {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "metadata_too_large", Message: fmt.Sprintf("typed-text metadata exceeds max_metadata_bytes %d", runner.config.MaxMetadataBytes)}, true)
	}
	if input.StableText != "" && !strings.HasPrefix(input.Text, input.StableText) {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "invalid_stable_prefix", Message: "stable_text is not a prefix of text"}, true)
	}
	if err := runner.validateRevision(streamID, input.Revision, input.Supersedes); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "invalid_revision", Message: err.Error()}, true)
	}
	if input.Final && input.StableText == "" {
		input.StableText = input.Text
	}
	observation := perception.Observation{
		Text: input.Text, StableText: input.StableText,
		Observer: runner.config.Observer, Source: runner.config.TextSource,
		Authority: trajectory.AuthorityUser, Revision: input.Revision,
		Supersedes: input.Supersedes, Provisional: !input.Final, Final: input.Final,
		OccurredNS: firstTime(envelope.CaptureNS, runner.clock.NowNS()),
	}
	if err := observation.Validate(); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision, Code: "invalid_observation", Message: err.Error()}, true)
	}
	if err := runner.publishObservation(ctx, envelope, streamID, observation, nil); err != nil {
		return err
	}
	runner.commitRevision(streamID, input.Revision, input.Final)
	return runner.finish(ctx, envelope, Outcome{Kind: OutcomeSucceeded, Operation: "text", ContentID: contentID, StreamID: streamID, SourceRevision: input.Revision}, true)
}

func (runner *userContentRunner) attachment(ctx context.Context, operation string, envelope element.Envelope) error {
	normalized, err := normalizeAttachment(operation, envelope.Payload)
	if err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, Code: "invalid_payload", Message: err.Error()}, true)
	}
	contentID, streamID, err := runner.identities(envelope, normalized.contentID, normalized.streamID)
	if err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, Code: "invalid_identity", Message: err.Error()}, true)
	}
	normalized.contentID, normalized.streamID = contentID, streamID
	canonicalMIME, mimeErr := canonicalMIMEType(normalized.mimeType)
	if mimeErr != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "invalid_attachment", Message: mimeErr.Error()}, true)
	}
	normalized.mimeType = canonicalMIME
	if err := validateDeclaredDigest(normalized.sha256); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "invalid_attachment", Message: err.Error()}, true)
	}
	if runner.isDuplicate(envelope.ItemID) {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, Code: "duplicate_request"}, false)
	}
	if pendingID := runner.pendingByItem[envelope.ItemID]; pendingID != "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "duplicate_pending", Message: fmt.Sprintf("request is already pending as %q", pendingID)}, false)
	}
	if reason, canceled := runner.takePreCancel(contentID, streamID, envelope.SessionID); canceled {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeCanceled, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "canceled", Message: reason}, true)
	}
	if err := runner.validateAttachment(normalized); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "invalid_attachment", Message: err.Error()}, true)
	}
	actualDigest := contentDigest(normalized.content)
	if normalized.sha256 != "" && normalized.sha256 != actualDigest {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "digest_mismatch", Message: fmt.Sprintf("declared %s, computed %s", normalized.sha256, actualDigest)}, true)
	}
	normalized.sha256 = actualDigest
	if existing := runner.pendingByContent[contentID]; existing != "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "content_pending", Message: fmt.Sprintf("content ID already has pending request %q", existing)}, true)
	}
	if err := runner.validateRevision(streamID, normalized.sourceRevision, 0); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "invalid_revision", Message: err.Error()}, true)
	}
	if existing := runner.pendingByStream[streamID]; existing != "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "stream_pending", Message: fmt.Sprintf("stream has pending request %q", existing)}, true)
	}
	if len(runner.pending) >= runner.config.MaxPending || runner.pendingBytes+len(normalized.content) > runner.config.MaxPendingBytes {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: operation, ContentID: contentID, StreamID: streamID, SourceRevision: normalized.sourceRevision, Code: "pending_limit", Message: "ingress pending retention bound reached"}, true)
	}
	retainID, err := runner.nextItemID(envelope.ItemID, "retain")
	if err != nil {
		return err
	}
	expectedReplyID := retainID + ":retained"
	description := attachmentDescription(operation, normalized.name, normalized.caption)
	capturedNS := firstTime(normalized.capturedNS, envelope.CaptureNS, runner.clock.NowNS())
	expectedHandle := mediaelements.AttachmentHandle{
		AttachmentID: contentID, Kind: normalized.kind, Name: normalized.name,
		MIMEType: normalized.mimeType, SHA256: normalized.sha256,
		Bytes: len(normalized.content), Source: runner.config.AttachmentSource,
		Scope: runner.config.RetentionScope, SourceRevision: normalized.sourceRevision,
		CapturedNS: capturedNS, Width: normalized.width, Height: normalized.height,
	}
	pending := pendingContent{
		cause: envelope.Clone(), contentID: contentID, streamID: streamID,
		operation: operation, retainID: retainID, expectedReplyID: expectedReplyID,
		description: description, expectedHandle: expectedHandle,
		bytes: len(normalized.content),
	}
	runner.pending[retainID] = pending
	runner.pendingByReply[expectedReplyID] = retainID
	runner.pendingByContent[contentID] = retainID
	runner.pendingByItem[envelope.ItemID] = retainID
	runner.pendingByStream[streamID] = retainID
	runner.pendingBytes += pending.bytes
	request := mediaelements.RetainRequest{
		AttachmentID: contentID, Kind: normalized.kind, Name: normalized.name,
		MIMEType: normalized.mimeType, Content: normalized.content,
		SHA256: normalized.sha256, Source: runner.config.AttachmentSource,
		Scope: runner.config.RetentionScope, SourceRevision: normalized.sourceRevision,
		CapturedNS: capturedNS, Width: normalized.width, Height: normalized.height,
	}
	retainEnvelope := envelope.Clone()
	retainEnvelope.Type = mediaelements.RetainRequestType()
	retainEnvelope.ItemID = retainID
	retainEnvelope.SourceID = streamID
	retainEnvelope.CancellationScope = contentID
	retainEnvelope.CausalParents = []string{envelope.ItemID}
	retainEnvelope.Payload = request
	if _, err := runner.ports.retain.Broadcast(ctx, retainEnvelope); err != nil {
		runner.removePending(retainID)
		return err
	}
	return nil
}

func (runner *userContentRunner) retained(ctx context.Context, envelope element.Envelope) error {
	retainID, found := runner.pendingByReply[envelope.ItemID]
	if !found {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeIgnored, Operation: "retained", Code: "unknown_reply", Message: "retention reply has no pending ingress request"}, false)
	}
	pending, found := runner.pending[retainID]
	if !found || pending.expectedReplyID != envelope.ItemID {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeIgnored, Operation: "retained", Code: "stale_reply", Message: "retention reply no longer has matching pending state"}, false)
	}
	if envelope.SessionID != pending.cause.SessionID {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "retained", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "cross_session_reply", Message: "retention reply crossed the pending ingress session boundary"}, false)
	}
	if len(envelope.CausalParents) != 1 || envelope.CausalParents[0] != pending.retainID {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "retained", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "invalid_reply_parent", Message: "retention reply does not name the exact immediate retain request"}, false)
	}
	result, ok := retainResultPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "retained", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "invalid_retention_reply", Message: fmt.Sprintf("retention reply payload has type %T", envelope.Payload)}, false)
	}
	if err := validateRetentionResultKind(result.Kind); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "retained", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "invalid_retention_result", Message: err.Error()}, false)
	}
	if result.Kind == mediaelements.ResultSucceeded {
		if err := validateRetainedHandle(pending.expectedHandle, result.Handle); err != nil {
			return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "retained", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "retained_handle_mismatch", Message: err.Error()}, false)
		}
	}
	runner.removePending(retainID)
	if pending.canceled {
		if result.Kind == mediaelements.ResultSucceeded {
			if err := runner.releaseLateHandle(ctx, pending, envelope, result.Handle); err != nil {
				return err
			}
		}
		return runner.finish(ctx, pending.cause, Outcome{Kind: OutcomeCanceled, Operation: pending.operation, ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Handle: result.Handle.Handle, Code: "canceled", Message: pending.reason}, true)
	}
	if result.Kind != mediaelements.ResultSucceeded {
		kind := OutcomeRefused
		if result.Kind == mediaelements.ResultFailed {
			kind = OutcomeFailed
		} else if result.Kind == mediaelements.ResultCanceled {
			kind = OutcomeCanceled
		}
		return runner.finish(ctx, pending.cause, Outcome{Kind: kind, Operation: pending.operation, ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: result.Code, Message: result.Message}, true)
	}
	observation := perception.Observation{
		Text: pending.description, StableText: pending.description,
		Observer: runner.config.Observer, Source: runner.config.AttachmentSource,
		Authority: trajectory.AuthorityUser, Media: []trajectory.MediaRef{result.Handle.MediaRef()},
		Revision: pending.expectedHandle.SourceRevision, Final: true, OccurredNS: pending.expectedHandle.CapturedNS,
	}
	if err := observation.Validate(); err != nil {
		if releaseErr := runner.releaseLateHandle(ctx, pending, envelope, result.Handle); releaseErr != nil {
			return releaseErr
		}
		return runner.finish(ctx, pending.cause, Outcome{Kind: OutcomeFailed, Operation: pending.operation, ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Handle: result.Handle.Handle, Code: "invalid_observation", Message: err.Error()}, true)
	}
	if err := runner.publishHandle(ctx, pending.cause, envelope, result.Handle); err != nil {
		return err
	}
	if err := runner.publishObservation(ctx, pending.cause, pending.streamID, observation, &envelope); err != nil {
		return err
	}
	runner.commitRevision(pending.streamID, pending.expectedHandle.SourceRevision, true)
	return runner.finish(ctx, pending.cause, Outcome{Kind: OutcomeSucceeded, Operation: pending.operation, ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Handle: result.Handle.Handle}, true)
}

func (runner *userContentRunner) cancel(ctx context.Context, envelope element.Envelope) error {
	request, ok := contentCancelPayload(envelope.Payload)
	if !ok {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload)}, false)
	}
	if _, err := canonicalID(envelope.ItemID, "cancel item_id"); err != nil {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_identity", Message: err.Error()}, false)
	}
	contentID, streamID := request.ContentID, request.StreamID
	if contentID == "" && streamID == "" {
		contentID = firstNonempty(envelope.RunID, envelope.CancellationScope)
	}
	if contentID == "" && streamID == "" {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "cancel", Code: "missing_address", Message: "cancel requires a content or stream ID"}, false)
	}
	if contentID != "" {
		var err error
		contentID, err = canonicalID(contentID, "content_id")
		if err != nil {
			return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_identity", Message: err.Error()}, false)
		}
	}
	if streamID != "" {
		var err error
		streamID, err = canonicalID(streamID, "stream_id")
		if err != nil {
			return runner.finish(ctx, envelope, Outcome{Kind: OutcomeRefused, Operation: "cancel", Code: "invalid_identity", Message: err.Error()}, false)
		}
	}
	request.Reason = boundedReason(request.Reason)
	retainID := runner.pendingByContent[contentID]
	if retainID == "" {
		retainID = runner.pendingByStream[streamID]
	}
	pending, found := runner.pending[retainID]
	found = found && pending.cause.SessionID == envelope.SessionID
	if !found && contentID != "" {
		runner.recordPreCancel(contentCancellationAddress{contentID: contentID, sessionID: envelope.SessionID}, request.Reason)
	}
	if streamID != "" {
		// Retain the whole stream even if this interrupt also cancels a
		// pending retention request. Its eventual reply cannot reopen it.
		runner.recordPreCancel(contentCancellationAddress{streamID: streamID, sessionID: envelope.SessionID}, request.Reason)
	}
	if !found {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeSucceeded, Operation: "cancel", ContentID: contentID, StreamID: streamID, Code: "cancel_recorded", Message: request.Reason}, false)
	}
	if pending.canceled {
		return runner.finish(ctx, envelope, Outcome{Kind: OutcomeIgnored, Operation: "cancel", ContentID: pending.contentID, StreamID: pending.streamID, Code: "already_canceling"}, false)
	}
	pending.canceled = true
	pending.reason = request.Reason
	runner.pending[retainID] = pending
	cancelID, err := runner.nextItemID(envelope.ItemID, "store_cancel")
	if err != nil {
		return err
	}
	cancelEnvelope := envelope.Clone()
	cancelEnvelope.Type = mediaelements.StoreCancelType()
	cancelEnvelope.ItemID = cancelID
	cancelEnvelope.RunID = retainID
	cancelEnvelope.CancellationScope = retainID
	cancelEnvelope.CausalParents = []string{envelope.ItemID}
	cancelEnvelope.Payload = mediaelements.StoreCancel{RequestID: retainID, Reason: request.Reason}
	if _, err := runner.ports.storeCancel.Broadcast(ctx, cancelEnvelope); err != nil {
		return err
	}
	return runner.finish(ctx, envelope, Outcome{Kind: OutcomeSucceeded, Operation: "cancel", ContentID: pending.contentID, StreamID: pending.streamID, SourceRevision: pending.expectedHandle.SourceRevision, Code: "cancel_forwarded", Message: request.Reason}, false)
}

func (runner *userContentRunner) identities(envelope element.Envelope, content, stream string) (string, string, error) {
	if _, err := canonicalID(envelope.ItemID, "item_id"); err != nil {
		return "", "", err
	}
	content = firstNonempty(content, envelope.ItemID)
	canonicalContent, err := canonicalID(content, "content_id")
	if err != nil {
		return "", "", err
	}
	stream = firstNonempty(stream, envelope.SourceID, canonicalContent)
	canonicalStream, err := canonicalID(stream, "stream_id")
	if err != nil {
		return "", "", err
	}
	return canonicalContent, canonicalStream, nil
}

func (runner *userContentRunner) validateAttachment(value normalizedAttachment) error {
	if value.sourceRevision == 0 {
		return errors.New("source_revision must be positive")
	}
	if strings.TrimSpace(value.mimeType) == "" || strings.ContainsAny(value.mimeType, "\r\n") {
		return errors.New("a canonical MIME type is required")
	}
	if len(value.content) == 0 {
		return errors.New("attachment content is empty")
	}
	if len(value.content) > runner.config.MaxInputBytes {
		return fmt.Errorf("attachment has %d bytes; max_input_bytes is %d", len(value.content), runner.config.MaxInputBytes)
	}
	metadataBytes := len(value.contentID) + len(value.streamID) + len(value.name) +
		len(value.caption) + len(value.mimeType) + len(value.sha256)
	if metadataBytes > runner.config.MaxMetadataBytes {
		return fmt.Errorf("attachment metadata has %d bytes; max_metadata_bytes is %d",
			metadataBytes, runner.config.MaxMetadataBytes)
	}
	if len(value.caption) > runner.config.MaxTextBytes {
		return fmt.Errorf("attachment caption has %d bytes; max_text_bytes is %d",
			len(value.caption), runner.config.MaxTextBytes)
	}
	if value.kind == mediaelements.AttachmentImage && (value.width <= 0 || value.height <= 0) {
		return errors.New("image requires positive dimensions")
	}
	if value.width < 0 || value.height < 0 {
		return errors.New("attachment dimensions cannot be negative")
	}
	if value.kind == mediaelements.AttachmentFile && strings.TrimSpace(value.name) == "" {
		return errors.New("file name is required")
	}
	return nil
}

func contentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func validateDeclaredDigest(value string) error {
	if value == "" {
		return nil
	}
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return errors.New("content digest must be a canonical SHA-256 digest")
	}
	if _, err := hex.DecodeString(strings.TrimPrefix(value, prefix)); err != nil {
		return fmt.Errorf("invalid content digest: %w", err)
	}
	if value != strings.ToLower(value) {
		return errors.New("content digest must use lowercase hexadecimal")
	}
	return nil
}

func validateRetentionResultKind(kind mediaelements.ResultKind) error {
	switch kind {
	case mediaelements.ResultSucceeded, mediaelements.ResultCanceled,
		mediaelements.ResultRefused, mediaelements.ResultFailed,
		mediaelements.ResultExpired, mediaelements.ResultIgnored:
		return nil
	default:
		return fmt.Errorf("unsupported retention result kind %q", kind)
	}
}

func validateRetainedHandle(expected, actual mediaelements.AttachmentHandle) error {
	if _, err := canonicalID(actual.Handle, "retained handle"); err != nil {
		return err
	}
	if _, err := canonicalID(actual.Capability, "retained capability"); err != nil {
		return err
	}
	authorityFree := actual
	authorityFree.Handle = ""
	authorityFree.Capability = ""
	if authorityFree != expected {
		return errors.New("successful retention reply metadata does not exactly match the admitted attachment")
	}
	return nil
}

func (runner *userContentRunner) validateRevision(streamID string, revision, supersedes uint64) error {
	if revision == 0 {
		return errors.New("revision must be positive")
	}
	if pending := runner.pendingByStream[streamID]; pending != "" {
		return fmt.Errorf("stream has pending request %q", pending)
	}
	state, found := runner.streams[streamID]
	if !found {
		if supersedes != 0 {
			return errors.New("first revision cannot supersede an unseen revision")
		}
		if err := runner.ensureStreamCapacity(); err != nil {
			return err
		}
		return nil
	}
	if state.final {
		return errors.New("stream is already final")
	}
	if revision <= state.revision {
		return fmt.Errorf("revision %d is not newer than %d", revision, state.revision)
	}
	if supersedes != state.revision {
		return fmt.Errorf("revision %d must supersede current revision %d", revision, state.revision)
	}
	return nil
}

func (runner *userContentRunner) ensureStreamCapacity() error {
	for len(runner.streams)+len(runner.pendingByStream) >= runner.config.MaxStreams {
		removed := false
		for index, streamID := range runner.streamOrder {
			if state := runner.streams[streamID]; state.final {
				delete(runner.streams, streamID)
				runner.streamOrder = slices.Delete(runner.streamOrder, index, index+1)
				removed = true
				break
			}
		}
		if !removed {
			return errors.New("active stream bound reached")
		}
	}
	return nil
}

func (runner *userContentRunner) commitRevision(streamID string, revision uint64, final bool) {
	if _, found := runner.streams[streamID]; !found {
		runner.streamOrder = append(runner.streamOrder, streamID)
	}
	runner.streams[streamID] = revisionState{revision: revision, final: final}
}

func (runner *userContentRunner) removePending(retainID string) {
	pending, found := runner.pending[retainID]
	if !found {
		return
	}
	delete(runner.pending, retainID)
	if runner.pendingByReply[pending.expectedReplyID] == retainID {
		delete(runner.pendingByReply, pending.expectedReplyID)
	}
	if runner.pendingByContent[pending.contentID] == retainID {
		delete(runner.pendingByContent, pending.contentID)
	}
	if runner.pendingByItem[pending.cause.ItemID] == retainID {
		delete(runner.pendingByItem, pending.cause.ItemID)
	}
	if runner.pendingByStream[pending.streamID] == retainID {
		delete(runner.pendingByStream, pending.streamID)
	}
	runner.pendingBytes -= pending.bytes
	if runner.pendingBytes < 0 {
		runner.pendingBytes = 0
	}
}

func (runner *userContentRunner) releaseLateHandle(ctx context.Context, pending pendingContent, reply element.Envelope, handle mediaelements.AttachmentHandle) error {
	itemID, err := runner.nextItemID(pending.retainID, "release_after_cancel")
	if err != nil {
		return err
	}
	envelope := pending.cause.Clone()
	envelope.Type = mediaelements.ReleaseRequestType()
	envelope.ItemID = itemID
	envelope.CausalParents = []string{reply.ItemID}
	envelope.Payload = mediaelements.ReleaseRequest{
		Handle: handle.Handle, Capability: handle.Capability, Scope: handle.Scope,
		Reason: "ingress canceled before retention completed",
	}
	_, err = runner.ports.release.Broadcast(ctx, envelope)
	return err
}

func (runner *userContentRunner) publishHandle(ctx context.Context, cause, reply element.Envelope, handle mediaelements.AttachmentHandle) error {
	itemID, err := runner.nextItemID(cause.ItemID, "handle")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = attachmentHandleType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID, reply.ItemID}
	envelope.Payload = handle
	_, err = runner.ports.handles.Broadcast(ctx, envelope)
	return err
}

func (runner *userContentRunner) publishObservation(ctx context.Context, cause element.Envelope, streamID string, observation perception.Observation, other *element.Envelope) error {
	itemID, err := runner.nextItemID(cause.ItemID, "observation")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = observationType
	envelope.ItemID = itemID
	envelope.SourceID = streamID
	envelope.CaptureNS = observation.OccurredNS
	envelope.CausalParents = []string{cause.ItemID}
	if other != nil {
		envelope.CausalParents = append(envelope.CausalParents, other.ItemID)
	}
	observation.Media = slices.Clone(observation.Media)
	envelope.Payload = observation
	_, err = runner.ports.observations.Broadcast(ctx, envelope)
	return err
}

func (runner *userContentRunner) finish(ctx context.Context, cause element.Envelope, outcome Outcome, remember bool) error {
	if remember {
		runner.rememberTerminal(cause.ItemID)
	}
	outcome.FinishedNS = runner.clock.NowNS()
	itemID, err := runner.nextItemID(cause.ItemID, "ingress_outcome")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = ingressOutcomeType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = outcome
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *userContentRunner) nextItemID(parent, label string) (string, error) {
	sequence, err := runner.sequences.Next(runner.instance + "." + label)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%d", parent, label, sequence), nil
}

func (runner *userContentRunner) isDuplicate(itemID string) bool {
	_, found := runner.terminal[itemID]
	return found
}

func (runner *userContentRunner) rememberTerminal(itemID string) {
	if itemID == "" {
		return
	}
	if _, found := runner.terminal[itemID]; !found {
		runner.terminal[itemID] = struct{}{}
		runner.terminalOrder = append(runner.terminalOrder, itemID)
	}
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminal, oldest)
	}
}

func (runner *userContentRunner) recordPreCancel(address contentCancellationAddress, reason string) {
	if _, found := runner.preCanceled[address]; !found {
		runner.preCancelOrder = append(runner.preCancelOrder, address)
	}
	runner.preCanceled[address] = boundedReason(reason)
	for len(runner.preCancelOrder) > runner.config.TerminalMemory {
		oldest := runner.preCancelOrder[0]
		runner.preCancelOrder = runner.preCancelOrder[1:]
		delete(runner.preCanceled, oldest)
	}
}

func (runner *userContentRunner) takePreCancel(contentID, streamID, sessionID string) (string, bool) {
	for _, address := range []contentCancellationAddress{
		{contentID: contentID, sessionID: sessionID},
		{streamID: streamID, sessionID: sessionID},
	} {
		reason, found := runner.preCanceled[address]
		if !found {
			continue
		}
		// An exact content request is consumed once. A stream covers every
		// later revision and remains in the bounded cancellation memory.
		if address.contentID != "" {
			delete(runner.preCanceled, address)
			if index := slices.Index(runner.preCancelOrder, address); index >= 0 {
				runner.preCancelOrder = slices.Delete(runner.preCancelOrder, index, index+1)
			}
		}
		return reason, true
	}
	return "", false
}

func normalizeAttachment(operation string, payload any) (normalizedAttachment, error) {
	switch operation {
	case "image":
		value, ok := userImagePayload(payload)
		if !ok {
			return normalizedAttachment{}, fmt.Errorf("image payload has type %T", payload)
		}
		return normalizedAttachment{contentID: value.ContentID, streamID: value.StreamID, caption: value.Caption, mimeType: value.MIMEType, content: value.Content, sha256: value.SHA256, kind: mediaelements.AttachmentImage, sourceRevision: value.SourceRevision, capturedNS: value.CapturedNS, width: value.Width, height: value.Height}, nil
	case "file":
		value, ok := userFilePayload(payload)
		if !ok {
			return normalizedAttachment{}, fmt.Errorf("file payload has type %T", payload)
		}
		return normalizedAttachment{contentID: value.ContentID, streamID: value.StreamID, name: value.Name, caption: value.Caption, mimeType: value.MIMEType, content: value.Content, sha256: value.SHA256, kind: mediaelements.AttachmentFile, sourceRevision: value.SourceRevision, capturedNS: value.CapturedNS}, nil
	case "attachment":
		value, ok := userAttachmentPayload(payload)
		if !ok {
			return normalizedAttachment{}, fmt.Errorf("attachment payload has type %T", payload)
		}
		return normalizedAttachment{contentID: value.ContentID, streamID: value.StreamID, name: value.Name, caption: value.Caption, mimeType: value.MIMEType, content: value.Content, sha256: value.SHA256, kind: mediaelements.AttachmentOther, sourceRevision: value.SourceRevision, capturedNS: value.CapturedNS}, nil
	default:
		return normalizedAttachment{}, fmt.Errorf("unknown attachment operation %q", operation)
	}
}

func attachmentDescription(operation, name, caption string) string {
	if strings.TrimSpace(caption) != "" {
		return caption
	}
	switch operation {
	case "image":
		return "The user attached an image."
	case "file":
		return fmt.Sprintf("The user attached a file named %q.", name)
	default:
		if strings.TrimSpace(name) != "" {
			return fmt.Sprintf("The user attached an item named %q.", name)
		}
		return "The user attached an item."
	}
}

func receiveIngressInputs(ctx context.Context, kind string, input element.InputPort, output chan<- ingressInput, failures chan<- error, wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive ingress %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- ingressInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func userTextPayload(payload any) (UserText, bool) {
	switch value := payload.(type) {
	case UserText:
		return value, true
	case *UserText:
		if value != nil {
			return *value, true
		}
	}
	return UserText{}, false
}

func userImagePayload(payload any) (UserImage, bool) {
	switch value := payload.(type) {
	case UserImage:
		return cloneUserImage(value), true
	case *UserImage:
		if value != nil {
			return cloneUserImage(*value), true
		}
	}
	return UserImage{}, false
}

func userFilePayload(payload any) (UserFile, bool) {
	switch value := payload.(type) {
	case UserFile:
		return cloneUserFile(value), true
	case *UserFile:
		if value != nil {
			return cloneUserFile(*value), true
		}
	}
	return UserFile{}, false
}

func userAttachmentPayload(payload any) (UserAttachment, bool) {
	switch value := payload.(type) {
	case UserAttachment:
		return cloneUserAttachment(value), true
	case *UserAttachment:
		if value != nil {
			return cloneUserAttachment(*value), true
		}
	}
	return UserAttachment{}, false
}

func retainResultPayload(payload any) (mediaelements.RetainResult, bool) {
	switch value := payload.(type) {
	case mediaelements.RetainResult:
		return value, true
	case *mediaelements.RetainResult:
		if value != nil {
			return *value, true
		}
	}
	return mediaelements.RetainResult{}, false
}

func contentCancelPayload(payload any) (ContentCancel, bool) {
	switch value := payload.(type) {
	case ContentCancel:
		return value, true
	case *ContentCancel:
		if value != nil {
			return *value, true
		}
	}
	return ContentCancel{}, false
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func firstTime(values ...uint64) uint64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}
