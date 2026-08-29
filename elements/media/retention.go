package media

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
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

// RetainedMediaDescriptor owns one bounded retention scope. Retain, fetch,
// release, and addressed cancellation are separate graph observations. Fetch
// is intentionally not folded into an ambient service so every raw-byte path
// remains visible in Graph IR and live traces.
func RetainedMediaDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "media.RetainedMedia",
		Revision:      1,
		Ports: []element.Port{
			{Name: "retain", Direction: element.Input, Type: retainRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "fetch", Direction: element.Input, Type: fetchRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "release", Direction: element.Input, Type: releaseRequestType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "return_lease", Direction: element.Input, Type: returnLeaseRequestType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 16},
			{Name: "cancel", Direction: element.Input, Type: storeCancelType,
				Cardinality: element.Variadic, Required: true, MinConnections: 1, DefaultDepth: 16},
			{Name: "retained", Direction: element.Output, Type: retainResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "fetched", Direction: element.Output, Type: fetchResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "released", Direction: element.Output, Type: releaseResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "lease_returned", Direction: element.Output, Type: returnLeaseResultType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "outcome", Direction: element.Output, Type: retentionOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "metrics", Direction: element.Output, Type: retentionMetricsType,
				Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"retain", "fetch", "release", "return_lease"}, Interrupts: []string{"cancel"},
			Outcomes:       []string{"retained", "fetched", "released", "lease_returned", "outcome", "metrics"},
			MaxConcurrency: 1, BreaksCycles: true,
		},
		StateSchema:  "schema://openrealtime/media/retained-media-state/v1",
		ConfigSchema: "schema://openrealtime/media/retained-media-config/v1",
		Dependencies: []element.Dependency{
			{Name: graphruntime.ClockServiceName}, {Name: graphruntime.SequenceServiceName},
		},
		Effects: []element.Effect{{Name: "media.retained.memory", Reversible: true}},
	}
}

type RetainedMediaConfig struct {
	Scope            string `json:"scope,omitempty"`
	MaxItems         int    `json:"max_items,omitempty"`
	MaxBytes         int    `json:"max_bytes,omitempty"`
	MaxItemBytes     int    `json:"max_item_bytes,omitempty"`
	MaxMetadataBytes int    `json:"max_metadata_bytes,omitempty"`
	MaxActiveLeases  int    `json:"max_active_leases,omitempty"`
	WindowMS         int64  `json:"window_ms,omitempty"`
	TerminalMemory   int    `json:"terminal_memory,omitempty"`
	CancelMemory     int    `json:"cancel_memory,omitempty"`
}

func decodeRetainedMediaConfig(source json.RawMessage) (RetainedMediaConfig, error) {
	config := RetainedMediaConfig{
		Scope: "session", MaxItems: 32, MaxBytes: 64 << 20,
		MaxItemBytes: 32 << 20, MaxMetadataBytes: 64 << 10,
		MaxActiveLeases: 256, TerminalMemory: 512, CancelMemory: 256,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return RetainedMediaConfig{}, err
	}
	if err := validateIdentifier("retained media scope", config.Scope, true); err != nil {
		return RetainedMediaConfig{}, err
	}
	if config.MaxItems < 1 || config.MaxItems > 1_000_000 {
		return RetainedMediaConfig{}, errors.New("max_items must be between 1 and 1000000")
	}
	if config.MaxBytes < 1 || config.MaxBytes > maximumConfigurationByteSize {
		return RetainedMediaConfig{}, fmt.Errorf("max_bytes must be between 1 and %d", maximumConfigurationByteSize)
	}
	if config.MaxItemBytes < 1 || config.MaxItemBytes > config.MaxBytes {
		return RetainedMediaConfig{}, errors.New("max_item_bytes must be positive and no larger than max_bytes")
	}
	if config.MaxMetadataBytes < 1 || config.MaxMetadataBytes > 1<<20 {
		return RetainedMediaConfig{}, errors.New("max_metadata_bytes must be between 1 and 1048576")
	}
	if len(config.Scope) > config.MaxMetadataBytes {
		return RetainedMediaConfig{}, errors.New("retention scope exceeds max_metadata_bytes")
	}
	if config.MaxActiveLeases < 1 || config.MaxActiveLeases > 1_000_000 {
		return RetainedMediaConfig{}, errors.New("max_active_leases must be between 1 and 1000000")
	}
	if config.WindowMS < 0 || config.WindowMS > 365*24*60*60*1000 {
		return RetainedMediaConfig{}, errors.New("window_ms must be between 0 and 31536000000")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return RetainedMediaConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	if config.CancelMemory < 1 || config.CancelMemory > 1_000_000 {
		return RetainedMediaConfig{}, errors.New("cancel_memory must be between 1 and 1000000")
	}
	return config, nil
}

type retainedMediaFactory struct{}

func (retainedMediaFactory) Descriptor() element.Descriptor { return RetainedMediaDescriptor() }

func (retainedMediaFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeRetainedMediaConfig(source)
	return err
}

func (retainedMediaFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeRetainedMediaConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("media.RetainedMedia %s config: %w", mount.InstanceID, err)
	}
	clock, err := runtimeDependencies(mount.Services)
	if err != nil {
		return nil, err
	}
	sequences, err := sequenceDependency(mount.Services)
	if err != nil {
		return nil, err
	}
	ports, err := retainedPortsFrom(mount.Ports)
	if err != nil {
		return nil, err
	}
	return &retainedMediaRunner{
		instance: mount.InstanceID, config: config, clock: clock, sequences: sequences,
		resolution: mount.Resolution, ports: ports,
		entries: make(map[string]*retainedEntry), sourceIndex: make(map[string]string),
		leases:    make(map[string]*leaseRecord),
		terminals: make(map[string]terminalRequest),
		canceled:  make(map[string]string),
	}, nil
}

type retainedPorts struct {
	retain, fetch, release, returnLease, cancel         element.InputPort
	retained, fetched, released, leaseReturned, outcome element.OutputPort
	metrics                                             element.OutputPort
}

func retainedPortsFrom(ports element.Ports) (retainedPorts, error) {
	var result retainedPorts
	inputs := []struct {
		name string
		set  *element.InputPort
	}{{"retain", &result.retain}, {"fetch", &result.fetch}, {"release", &result.release}, {"return_lease", &result.returnLease}, {"cancel", &result.cancel}}
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
	}{{"retained", &result.retained}, {"fetched", &result.fetched}, {"released", &result.released}, {"lease_returned", &result.leaseReturned}, {"outcome", &result.outcome}, {"metrics", &result.metrics}}
	for _, output := range outputs {
		port, err := ports.Output(output.name)
		if err != nil {
			return result, err
		}
		*output.set = port
	}
	return result, nil
}

type retainedEntry struct {
	handle       AttachmentHandle
	blob         *immutableBlob
	retainedNS   uint64
	sourceKey    string
	addressable  bool
	activeLeases int
}

type leaseRecord struct {
	lease BlobLease
	entry *retainedEntry
}

type terminalRequest struct {
	operation   string
	fingerprint string
	replied     bool
}

type retainedMediaRunner struct {
	instance   string
	config     RetainedMediaConfig
	clock      graphruntime.Clock
	sequences  *graphruntime.SequenceAllocator
	resolution element.ResolutionReporter
	ports      retainedPorts

	entries          map[string]*retainedEntry
	sourceIndex      map[string]string
	leases           map[string]*leaseRecord
	order            []string
	addressableItems int
	liveBytes        int
	leasedBytes      int
	terminals        map[string]terminalRequest
	terminalOrder    []string
	canceled         map[string]string
	cancelOrder      []string
	metrics          RetentionMetrics
}

type retentionInput struct {
	kind     string
	envelope element.Envelope
}

func (runner *retainedMediaRunner) Run(parent context.Context) error {
	if err := reportLiveResolution(runner.resolution, retainedMediaRuntimeID, nil); err != nil {
		return err
	}
	if err := runner.publishMetrics(parent, element.Envelope{ItemID: runner.instance + ":startup"}); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	inputs := make(chan retentionInput)
	failures := make(chan error, 4)
	var wait sync.WaitGroup
	for _, source := range []struct {
		kind string
		port element.InputPort
		any  bool
	}{{"retain", runner.ports.retain, false}, {"fetch", runner.ports.fetch, false}, {"release", runner.ports.release, false}, {"return_lease", runner.ports.returnLease, true}, {"cancel", runner.ports.cancel, true}} {
		wait.Add(1)
		go receiveRetentionInputs(ctx, source.kind, source.port, source.any, inputs, failures, &wait)
	}
	defer func() {
		cancel(nil)
		wait.Wait()
		runner.revokeAllLeases()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			return err
		case input := <-inputs:
			runner.expire()
			var err error
			switch input.kind {
			case "retain":
				err = runner.retain(ctx, input.envelope)
			case "fetch":
				err = runner.fetch(ctx, input.envelope)
			case "release":
				err = runner.release(ctx, input.envelope)
			case "return_lease":
				err = runner.returnLease(ctx, input.envelope)
			case "cancel":
				err = runner.cancel(ctx, input.envelope)
			default:
				err = fmt.Errorf("unknown retained-media input %q", input.kind)
			}
			if err != nil {
				return err
			}
		}
	}
}

func (runner *retainedMediaRunner) retain(ctx context.Context, envelope element.Envelope) error {
	requestID := envelope.ItemID
	if err := validateIdentifier("retain request ID", requestID, true); err != nil {
		return runner.finishRetain(ctx, envelope, RetainResult{
			Kind: ResultRefused, Code: "invalid_request_id", Message: err.Error(),
		}, false)
	}
	request, ok := retainRequestPayload(envelope.Payload)
	if !ok {
		if runner.duplicate(requestID) {
			return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "duplicate_request", Message: "retain request ID is already terminal"}, false)
		}
		runner.recordTerminal(requestID, "retain", "")
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "invalid_payload", Message: fmt.Sprintf("retain payload has type %T", envelope.Payload)}, false)
	}
	canonicalMIME, mimeErr := canonicalMIMEType(request.MIMEType)
	if mimeErr == nil && canonicalMIME != request.MIMEType {
		mimeErr = errors.New("retained MIME type must already be canonical")
	}
	canonicalDigest, digestErr := canonicalSHA256(request.SHA256)
	if digestErr == nil {
		request.SHA256 = canonicalDigest
	}
	if runner.duplicate(requestID) {
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "duplicate_request", Message: "retain request ID is already terminal"}, false)
	}
	if reason, canceled := runner.takeCancellation(requestID); canceled {
		runner.recordTerminal(requestID, "retain", "")
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultCanceled, Code: "canceled", Message: reason}, false)
	}
	if mimeErr != nil {
		runner.recordTerminal(requestID, "retain", "")
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "invalid_request", Message: mimeErr.Error()}, false)
	}
	if digestErr != nil {
		runner.recordTerminal(requestID, "retain", "")
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "invalid_request", Message: digestErr.Error()}, false)
	}
	if err := runner.validateRetain(request); err != nil {
		runner.recordTerminal(requestID, "retain", "")
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "invalid_request", Message: err.Error()}, false)
	}
	fingerprint := retainFingerprint(request)
	actualDigest := digestContent(request.Content)
	if request.SHA256 != "" && request.SHA256 != actualDigest {
		runner.recordTerminal(requestID, "retain", fingerprint)
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultRefused, Code: "digest_mismatch", Message: fmt.Sprintf("declared %s, computed %s", request.SHA256, actualDigest)}, false)
	}
	sourceKey := retainedSourceKey(request)
	if existingHandle := runner.sourceIndex[sourceKey]; existingHandle != "" {
		existing, found := runner.entries[existingHandle]
		if found && existing.handle.SHA256 == actualDigest && existing.handle.MIMEType == request.MIMEType &&
			existing.handle.Kind == request.Kind && existing.handle.Name == request.Name &&
			existing.handle.Width == request.Width && existing.handle.Height == request.Height {
			runner.recordTerminal(requestID, "retain", fingerprint)
			return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultSucceeded, Handle: existing.handle}, false)
		}
		runner.recordTerminal(requestID, "retain", fingerprint)
		return runner.finishRetain(ctx, envelope, RetainResult{
			Kind: ResultRefused, Code: "source_revision_conflict",
			Message: "the same source attachment revision was already retained with different content or metadata",
		}, false)
	}
	handleID, capability, err := newOpaqueIdentity()
	if err != nil {
		runner.recordTerminal(requestID, "retain", fingerprint)
		return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultFailed, Code: "identity_generation_failed", Message: err.Error()}, false)
	}
	handle := AttachmentHandle{
		Handle: handleID, Capability: capability, AttachmentID: request.AttachmentID,
		Kind: request.Kind, Name: request.Name, MIMEType: request.MIMEType,
		SHA256: actualDigest, Bytes: len(request.Content), Source: request.Source,
		Scope: request.Scope, SourceRevision: request.SourceRevision,
		CapturedNS: request.CapturedNS, Width: request.Width, Height: request.Height,
	}
	if !runner.makeRoomFor(len(request.Content)) {
		runner.recordTerminal(requestID, "retain", fingerprint)
		return runner.finishRetain(ctx, envelope, RetainResult{
			Kind: ResultRefused, Code: "lease_capacity",
			Message: "active leases pin the configured retention byte bound",
		}, false)
	}
	blob := newImmutableBlob(request.Content)
	entry := &retainedEntry{
		handle: handle, blob: blob, retainedNS: runner.clock.NowNS(),
		sourceKey: sourceKey, addressable: true,
	}
	runner.entries[handle.Handle] = entry
	runner.sourceIndex[sourceKey] = handle.Handle
	runner.order = append(runner.order, handle.Handle)
	runner.addressableItems++
	runner.liveBytes += len(blob.bytes)
	runner.metrics.Retained++
	runner.recordTerminal(requestID, "retain", fingerprint)
	return runner.finishRetain(ctx, envelope, RetainResult{Kind: ResultSucceeded, Handle: handle}, true)
}

func (runner *retainedMediaRunner) fetch(ctx context.Context, envelope element.Envelope) error {
	requestID := envelope.ItemID
	if err := validateIdentifier("fetch request ID", requestID, true); err != nil {
		return runner.finishFetch(ctx, envelope, FetchResult{
			Kind: ResultRefused, Code: "invalid_request_id", Message: err.Error(),
		})
	}
	request, ok := fetchRequestPayload(envelope.Payload)
	if !ok {
		if runner.duplicate(requestID) {
			return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Code: "duplicate_request", Message: "fetch request ID is already terminal"})
		}
		runner.recordTerminal(requestID, "fetch", "")
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Code: "invalid_payload", Message: fmt.Sprintf("fetch payload has type %T", envelope.Payload)})
	}
	canonicalDigest, digestErr := canonicalSHA256(request.ExpectedSHA256)
	if digestErr == nil {
		request.ExpectedSHA256 = canonicalDigest
	}
	fingerprint := fetchFingerprint(request)
	if runner.duplicate(requestID) {
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Code: "duplicate_request", Message: "fetch request ID is already terminal"})
	}
	if reason, canceled := runner.takeCancellation(requestID); canceled {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultCanceled, Handle: request.Handle, Code: "canceled", Message: reason})
	}
	if digestErr != nil {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Handle: request.Handle, Code: "invalid_request", Message: digestErr.Error()})
	}
	entry, result := runner.authorizeHandle(request.Handle)
	if result.Kind != ResultSucceeded {
		runner.metrics.Misses++
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, result)
	}
	if request.ExpectedSHA256 != "" && request.ExpectedSHA256 != entry.handle.SHA256 {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Handle: entry.handle, Code: "digest_mismatch", Message: "retained digest does not match the requested digest"})
	}
	if request.ExpectedSourceRevision != 0 && request.ExpectedSourceRevision != entry.handle.SourceRevision {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Handle: entry.handle, Code: "source_revision_mismatch", Message: "retained source revision does not match the requested revision"})
	}
	if len(runner.leases) >= runner.config.MaxActiveLeases {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultRefused, Handle: request.Handle, Code: "lease_limit", Message: "active blob lease bound reached"})
	}
	leaseID, err := newOpaqueToken("lease_")
	if err != nil {
		runner.recordTerminal(requestID, "fetch", fingerprint)
		return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultFailed, Handle: request.Handle, Code: "lease_generation_failed", Message: err.Error()})
	}
	lease := newBlobLease(leaseID, envelope.SessionID, entry.handle, entry.blob)
	runner.leases[leaseID] = &leaseRecord{lease: lease, entry: entry}
	entry.activeLeases++
	runner.leasedBytes += entry.handle.Bytes
	runner.metrics.Resolved++
	runner.metrics.Leases++
	runner.recordTerminal(requestID, "fetch", fingerprint)
	return runner.finishFetch(ctx, envelope, FetchResult{Kind: ResultSucceeded, Handle: entry.handle, Lease: lease})
}

func (runner *retainedMediaRunner) release(ctx context.Context, envelope element.Envelope) error {
	requestID := envelope.ItemID
	if err := validateIdentifier("release request ID", requestID, true); err != nil {
		return runner.finishRelease(ctx, envelope, ReleaseResult{
			Kind: ResultRefused, Code: "invalid_request_id", Message: err.Error(),
		}, false)
	}
	request, ok := releaseRequestPayload(envelope.Payload)
	if !ok {
		if runner.duplicate(requestID) {
			return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: ResultRefused, Code: "duplicate_request", Message: "release request ID is already terminal"}, false)
		}
		runner.recordTerminal(requestID, "release", "")
		return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: ResultRefused, Code: "invalid_payload", Message: fmt.Sprintf("release payload has type %T", envelope.Payload)}, false)
	}
	request.Reason = boundedReason(request.Reason)
	fingerprint := releaseFingerprint(request)
	if runner.duplicate(requestID) {
		return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: ResultRefused, Code: "duplicate_request", Message: "release request ID is already terminal"}, false)
	}
	if reason, canceled := runner.takeCancellation(requestID); canceled {
		runner.recordTerminal(requestID, "release", fingerprint)
		return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: ResultCanceled, Handle: request.Handle, Code: "canceled", Message: reason}, false)
	}
	entry, authorization := runner.authorize(request.Handle, request.Capability, request.Scope)
	if authorization.Kind != ResultSucceeded {
		runner.metrics.Misses++
		runner.recordTerminal(requestID, "release", fingerprint)
		return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: authorization.Kind, Handle: request.Handle, Code: authorization.Code, Message: authorization.Message}, false)
	}
	runner.remove(entry.handle.Handle, true)
	runner.recordTerminal(requestID, "release", fingerprint)
	return runner.finishRelease(ctx, envelope, ReleaseResult{Kind: ResultSucceeded, Handle: entry.handle.Handle}, true)
}

func (runner *retainedMediaRunner) returnLease(ctx context.Context, envelope element.Envelope) error {
	requestID := envelope.ItemID
	if err := validateIdentifier("return-lease request ID", requestID, true); err != nil {
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, Code: "invalid_request_id", Message: err.Error(),
		})
	}
	request, ok := returnLeaseRequestPayload(envelope.Payload)
	if !ok {
		if runner.duplicate(requestID) {
			return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
				Kind: ResultRefused, Code: "duplicate_request",
				Message: "return-lease request ID is already terminal",
			})
		}
		runner.recordTerminal(requestID, "return_lease", "")
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, Code: "invalid_payload",
			Message: fmt.Sprintf("return lease payload has type %T", envelope.Payload),
		})
	}
	request.Reason = boundedReason(request.Reason)
	if err := validateIdentifier("lease ID", request.Lease.LeaseID, true); err != nil {
		runner.recordTerminal(requestID, "return_lease", "")
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "invalid_lease", Message: err.Error(),
		})
	}
	if runner.duplicate(requestID) {
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "duplicate_request",
			Message: "return-lease request ID is already terminal",
		})
	}
	fingerprint := returnLeaseFingerprint(request)
	if reason, canceled := runner.takeCancellation(requestID); canceled {
		runner.recordTerminal(requestID, "return_lease", fingerprint)
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultCanceled, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "canceled", Message: reason,
		})
	}
	record, found := runner.leases[request.Lease.LeaseID]
	if !found {
		runner.recordTerminal(requestID, "return_lease", fingerprint)
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultIgnored, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "unknown_lease",
			Message: "blob lease is no longer active",
		})
	}
	if record.lease.state != request.Lease.state || record.lease.LeaseID != request.Lease.LeaseID ||
		record.lease.Handle != request.Lease.Handle {
		runner.recordTerminal(requestID, "return_lease", fingerprint)
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "lease_identity_mismatch",
			Message: "returned lease does not match the active lease identity",
		})
	}
	if err := request.Lease.validateFor(envelope.SessionID, record.entry.handle); err != nil {
		runner.recordTerminal(requestID, "return_lease", fingerprint)
		return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
			Kind: ResultRefused, LeaseID: request.Lease.LeaseID,
			Handle: request.Lease.Handle.Handle, Code: "invalid_lease",
			Message: err.Error(),
		})
	}
	runner.dropLease(record, false)
	runner.recordTerminal(requestID, "return_lease", fingerprint)
	return runner.finishReturnLease(ctx, envelope, ReturnLeaseResult{
		Kind: ResultSucceeded, LeaseID: request.Lease.LeaseID,
		Handle: request.Lease.Handle.Handle,
	})
}

func (runner *retainedMediaRunner) cancel(ctx context.Context, envelope element.Envelope) error {
	request, ok := storeCancelPayload(envelope.Payload)
	if !ok {
		return runner.publishOutcome(ctx, envelope, RetentionOutcome{Kind: ResultRefused, Operation: "cancel", Code: "invalid_payload", Message: fmt.Sprintf("cancel payload has type %T", envelope.Payload)})
	}
	requestID := firstNonempty(request.RequestID, envelope.RunID, envelope.CancellationScope)
	if requestID == "" {
		return runner.publishOutcome(ctx, envelope, RetentionOutcome{Kind: ResultRefused, Operation: "cancel", Code: "missing_request_id", Message: "cancel requires an addressed request ID"})
	}
	if err := validateIdentifier("cancel request ID", requestID, true); err != nil {
		return runner.publishOutcome(ctx, envelope, RetentionOutcome{Kind: ResultRefused, Operation: "cancel", Code: "invalid_request_id", Message: err.Error()})
	}
	request.Reason = boundedReason(request.Reason)
	if _, terminal := runner.terminals[requestID]; terminal {
		return runner.publishOutcome(ctx, envelope, RetentionOutcome{Kind: ResultIgnored, Operation: "cancel", RequestID: requestID, Crossed: true, Code: "already_terminal", Message: "the requested operation already crossed its retention boundary"})
	}
	runner.recordCancellation(requestID, request.Reason)
	return runner.publishOutcome(ctx, envelope, RetentionOutcome{Kind: ResultSucceeded, Operation: "cancel", RequestID: requestID, Code: "cancel_recorded", Message: request.Reason})
}

func (runner *retainedMediaRunner) validateRetain(request RetainRequest) error {
	if err := validateIdentifier("attachment_id", request.AttachmentID, true); err != nil {
		return err
	}
	if err := request.Kind.validate(); err != nil {
		return err
	}
	if strings.TrimSpace(request.MIMEType) == "" || strings.ContainsAny(request.MIMEType, "\r\n") {
		return errors.New("a canonical MIME type is required")
	}
	if len(request.Content) == 0 {
		return errors.New("retained content is empty")
	}
	if len(request.Content) > runner.config.MaxItemBytes {
		return fmt.Errorf("content has %d bytes; max_item_bytes is %d", len(request.Content), runner.config.MaxItemBytes)
	}
	if err := validateIdentifier("source", request.Source, true); err != nil {
		return err
	}
	if err := validateIdentifier("scope", request.Scope, true); err != nil {
		return err
	}
	metadataBytes := len(request.AttachmentID) + len(request.Name) + len(request.MIMEType) +
		len(request.SHA256) + len(request.Source) + len(request.Scope)
	if metadataBytes > runner.config.MaxMetadataBytes {
		return fmt.Errorf("retained metadata has %d bytes; max_metadata_bytes is %d",
			metadataBytes, runner.config.MaxMetadataBytes)
	}
	if request.Scope != runner.config.Scope {
		return fmt.Errorf("retention scope %q is not selected scope %q", request.Scope, runner.config.Scope)
	}
	if request.SourceRevision == 0 {
		return errors.New("source_revision must be positive")
	}
	if request.Width < 0 || request.Height < 0 {
		return errors.New("attachment dimensions cannot be negative")
	}
	if request.Kind == AttachmentImage && (request.Width <= 0 || request.Height <= 0) {
		return errors.New("image attachments require positive dimensions")
	}
	return nil
}

func (runner *retainedMediaRunner) authorize(handle, capability, scope string) (*retainedEntry, FetchResult) {
	if err := errors.Join(
		validateIdentifier("handle", handle, true),
		validateIdentifier("capability", capability, true),
		validateIdentifier("scope", scope, true),
	); err != nil {
		return nil, FetchResult{Kind: ResultRefused, Code: "invalid_authority", Message: err.Error()}
	}
	entry, found := runner.entries[handle]
	if !found || !entry.addressable {
		return nil, FetchResult{Kind: ResultExpired, Code: "not_retained", Message: "attachment is not retained in this scope"}
	}
	if scope != runner.config.Scope || scope != entry.handle.Scope {
		return nil, FetchResult{Kind: ResultRefused, Code: "scope_mismatch", Message: "attachment retention scope does not match"}
	}
	if !secureEqual(entry.handle.Capability, capability) {
		return nil, FetchResult{Kind: ResultRefused, Code: "invalid_capability", Message: "attachment capability is invalid"}
	}
	return entry, FetchResult{Kind: ResultSucceeded, Handle: entry.handle}
}

func (runner *retainedMediaRunner) authorizeHandle(handle AttachmentHandle) (*retainedEntry, FetchResult) {
	entry, result := runner.authorize(handle.Handle, handle.Capability, handle.Scope)
	if result.Kind != ResultSucceeded {
		result.Handle = handle
		return nil, result
	}
	if entry.handle != handle {
		return nil, FetchResult{
			Kind: ResultRefused, Handle: handle, Code: "handle_identity_mismatch",
			Message: "complete attachment handle identity does not match retained metadata",
		}
	}
	return entry, FetchResult{Kind: ResultSucceeded, Handle: handle}
}

func (runner *retainedMediaRunner) finishRetain(ctx context.Context, cause element.Envelope, result RetainResult, crossed bool) error {
	if err := runner.publishReply(ctx, runner.ports.retained, cause, retainResultType, "retained", result); err != nil {
		return err
	}
	if err := runner.publishMetrics(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, RetentionOutcome{Kind: result.Kind, Operation: "retain", RequestID: cause.ItemID, Handle: result.Handle.Handle, Crossed: crossed, Code: result.Code, Message: result.Message})
}

func (runner *retainedMediaRunner) finishFetch(ctx context.Context, cause element.Envelope, result FetchResult) error {
	if err := runner.publishReply(ctx, runner.ports.fetched, cause, fetchResultType, "fetched", cloneFetchResult(result)); err != nil {
		return err
	}
	if err := runner.publishMetrics(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, RetentionOutcome{Kind: result.Kind, Operation: "fetch", RequestID: cause.ItemID, Handle: result.Handle.Handle, Code: result.Code, Message: result.Message})
}

func (runner *retainedMediaRunner) finishRelease(ctx context.Context, cause element.Envelope, result ReleaseResult, crossed bool) error {
	if err := runner.publishReply(ctx, runner.ports.released, cause, releaseResultType, "released", result); err != nil {
		return err
	}
	if err := runner.publishMetrics(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, RetentionOutcome{Kind: result.Kind, Operation: "release", RequestID: cause.ItemID, Handle: result.Handle, Crossed: crossed, Code: result.Code, Message: result.Message})
}

func (runner *retainedMediaRunner) finishReturnLease(ctx context.Context, cause element.Envelope, result ReturnLeaseResult) error {
	if err := runner.publishReply(ctx, runner.ports.leaseReturned, cause, returnLeaseResultType, "lease_returned", result); err != nil {
		return err
	}
	if err := runner.publishMetrics(ctx, cause); err != nil {
		return err
	}
	return runner.publishOutcome(ctx, cause, RetentionOutcome{
		Kind: result.Kind, Operation: "return_lease", RequestID: cause.ItemID,
		Handle: result.Handle, Code: result.Code, Message: result.Message,
	})
}

func (runner *retainedMediaRunner) publishReply(ctx context.Context, output element.OutputPort, cause element.Envelope, typ element.Type, suffix string, payload any) error {
	// A joiner can predict the one legitimate terminal reply exactly. Replays
	// and late diagnostics must not reuse that evidence identity.
	itemID := cause.ItemID + ":" + suffix
	terminal, tracked := runner.terminals[cause.ItemID]
	if !tracked || terminal.replied {
		var err error
		label := "untracked"
		if tracked {
			label = "duplicate"
		}
		itemID, err = runner.nextItemID(itemID, label)
		if err != nil {
			return err
		}
	} else {
		terminal.replied = true
		runner.terminals[cause.ItemID] = terminal
	}
	envelope := cause.Clone()
	envelope.Type = typ
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = payload
	_, err := output.Broadcast(ctx, envelope)
	return err
}

func (runner *retainedMediaRunner) publishOutcome(ctx context.Context, cause element.Envelope, outcome RetentionOutcome) error {
	itemID, err := runner.nextItemID(cause.ItemID, "retention_outcome")
	if err != nil {
		return err
	}
	outcome.FinishedNS = runner.clock.NowNS()
	envelope := cause.Clone()
	envelope.Type = retentionOutcomeType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = outcome
	_, err = runner.ports.outcome.Broadcast(ctx, envelope)
	return err
}

func (runner *retainedMediaRunner) publishMetrics(ctx context.Context, cause element.Envelope) error {
	runner.metrics.LiveItems = runner.addressableItems
	runner.metrics.LiveBytes = runner.liveBytes
	runner.metrics.ActiveLeases = len(runner.leases)
	runner.metrics.LeasedBytes = runner.leasedBytes
	runner.metrics.MaxActiveLeases = runner.config.MaxActiveLeases
	runner.metrics.MaxItems = runner.config.MaxItems
	runner.metrics.MaxBytes = runner.config.MaxBytes
	runner.metrics.WindowMS = runner.config.WindowMS
	itemID, err := runner.nextItemID(cause.ItemID, "retention_metrics")
	if err != nil {
		return err
	}
	envelope := cause.Clone()
	envelope.Type = retentionMetricsType
	envelope.ItemID = itemID
	envelope.CausalParents = []string{cause.ItemID}
	envelope.Payload = runner.metrics
	_, err = runner.ports.metrics.Broadcast(ctx, envelope)
	return err
}

func (runner *retainedMediaRunner) nextItemID(parent, label string) (string, error) {
	sequence, err := runner.sequences.Next(runner.instance + "." + label)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%s:%d", parent, label, sequence), nil
}

func (runner *retainedMediaRunner) expire() {
	if runner.config.WindowMS <= 0 {
		return
	}
	now := runner.clock.NowNS()
	window := uint64(runner.config.WindowMS) * 1_000_000
	for len(runner.order) > 0 {
		entry, found := runner.entries[runner.order[0]]
		if !found {
			runner.order = runner.order[1:]
			continue
		}
		if now < entry.retainedNS || now-entry.retainedNS < window {
			return
		}
		runner.remove(entry.handle.Handle, false)
	}
}

func (runner *retainedMediaRunner) makeRoomFor(bytes int) bool {
	for runner.addressableItems+1 > runner.config.MaxItems || runner.liveBytes+bytes > runner.config.MaxBytes {
		if len(runner.order) == 0 {
			return false
		}
		handle := runner.order[0]
		entry, found := runner.entries[handle]
		if !found || !entry.addressable {
			runner.order = runner.order[1:]
			continue
		}
		runner.remove(handle, false)
	}
	return true
}

func (runner *retainedMediaRunner) remove(handle string, released bool) {
	entry, found := runner.entries[handle]
	if !found || !entry.addressable {
		return
	}
	entry.addressable = false
	runner.addressableItems--
	if runner.sourceIndex[entry.sourceKey] == handle {
		delete(runner.sourceIndex, entry.sourceKey)
	}
	if index := slices.Index(runner.order, handle); index >= 0 {
		runner.order = slices.Delete(runner.order, index, index+1)
	}
	if entry.activeLeases == 0 {
		delete(runner.entries, handle)
		if entry.blob != nil {
			runner.liveBytes -= len(entry.blob.bytes)
		}
		entry.blob = nil
	}
	if released {
		runner.metrics.Released++
	} else {
		runner.metrics.Evicted++
	}
}

func (runner *retainedMediaRunner) dropLease(record *leaseRecord, revoked bool) {
	if record == nil || record.entry == nil {
		return
	}
	if current := runner.leases[record.lease.LeaseID]; current != record {
		return
	}
	delete(runner.leases, record.lease.LeaseID)
	record.lease.revoke()
	if record.entry.activeLeases > 0 {
		record.entry.activeLeases--
	}
	if runner.leasedBytes >= record.entry.handle.Bytes {
		runner.leasedBytes -= record.entry.handle.Bytes
	} else {
		runner.leasedBytes = 0
	}
	if revoked {
		runner.metrics.Revoked++
	} else {
		runner.metrics.Returned++
	}
	if !record.entry.addressable && record.entry.activeLeases == 0 {
		delete(runner.entries, record.entry.handle.Handle)
		if record.entry.blob != nil {
			runner.liveBytes -= len(record.entry.blob.bytes)
		}
		record.entry.blob = nil
	}
}

func (runner *retainedMediaRunner) revokeAllLeases() {
	for _, record := range runner.leases {
		runner.dropLease(record, true)
	}
	for handle, entry := range runner.entries {
		if entry != nil {
			entry.addressable = false
			entry.blob = nil
		}
		delete(runner.entries, handle)
	}
	runner.sourceIndex = make(map[string]string)
	runner.order = nil
	runner.addressableItems = 0
	runner.liveBytes = 0
	runner.leasedBytes = 0
}

func (runner *retainedMediaRunner) duplicate(requestID string) bool {
	_, found := runner.terminals[requestID]
	if !found {
		return false
	}
	// Both matching replays and conflicting ID reuse are terminal refusals. The
	// operation and fingerprint remain recorded for future audit/debug views.
	return true
}

func (runner *retainedMediaRunner) recordTerminal(requestID, operation, fingerprint string) {
	if _, found := runner.terminals[requestID]; found {
		return
	}
	runner.terminals[requestID] = terminalRequest{operation: operation, fingerprint: fingerprint}
	runner.terminalOrder = append(runner.terminalOrder, requestID)
	for len(runner.terminalOrder) > runner.config.TerminalMemory {
		oldest := runner.terminalOrder[0]
		runner.terminalOrder = runner.terminalOrder[1:]
		delete(runner.terminals, oldest)
	}
}

func (runner *retainedMediaRunner) recordCancellation(requestID, reason string) {
	if _, found := runner.canceled[requestID]; !found {
		runner.cancelOrder = append(runner.cancelOrder, requestID)
	}
	runner.canceled[requestID] = boundedReason(reason)
	for len(runner.cancelOrder) > runner.config.CancelMemory {
		oldest := runner.cancelOrder[0]
		runner.cancelOrder = runner.cancelOrder[1:]
		delete(runner.canceled, oldest)
	}
}

func (runner *retainedMediaRunner) takeCancellation(requestID string) (string, bool) {
	reason, found := runner.canceled[requestID]
	if !found {
		return "", false
	}
	delete(runner.canceled, requestID)
	if index := slices.Index(runner.cancelOrder, requestID); index >= 0 {
		runner.cancelOrder = slices.Delete(runner.cancelOrder, index, index+1)
	}
	return reason, true
}

func receiveRetentionInputs(ctx context.Context, kind string, input element.InputPort, anyLane bool, output chan<- retentionInput, failures chan<- error, wait *sync.WaitGroup) {
	defer wait.Done()
	for {
		var (
			envelope element.Envelope
			err      error
		)
		if anyLane {
			envelope, _, err = input.ReceiveAny(ctx)
		} else {
			envelope, err = input.Receive(ctx)
		}
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			select {
			case failures <- fmt.Errorf("receive retained-media %s: %w", kind, err):
			case <-ctx.Done():
			}
			return
		}
		select {
		case output <- retentionInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func retainRequestPayload(payload any) (RetainRequest, bool) {
	switch value := payload.(type) {
	case RetainRequest:
		return cloneRetainRequest(value), true
	case *RetainRequest:
		if value != nil {
			return cloneRetainRequest(*value), true
		}
	}
	return RetainRequest{}, false
}

func fetchRequestPayload(payload any) (FetchRequest, bool) {
	switch value := payload.(type) {
	case FetchRequest:
		return value, true
	case *FetchRequest:
		if value != nil {
			return *value, true
		}
	}
	return FetchRequest{}, false
}

func releaseRequestPayload(payload any) (ReleaseRequest, bool) {
	switch value := payload.(type) {
	case ReleaseRequest:
		return value, true
	case *ReleaseRequest:
		if value != nil {
			return *value, true
		}
	}
	return ReleaseRequest{}, false
}

func returnLeaseRequestPayload(payload any) (ReturnLeaseRequest, bool) {
	switch value := payload.(type) {
	case ReturnLeaseRequest:
		return value, true
	case *ReturnLeaseRequest:
		if value != nil {
			return *value, true
		}
	}
	return ReturnLeaseRequest{}, false
}

func storeCancelPayload(payload any) (StoreCancel, bool) {
	switch value := payload.(type) {
	case StoreCancel:
		return value, true
	case *StoreCancel:
		if value != nil {
			return *value, true
		}
	}
	return StoreCancel{}, false
}

func retainFingerprint(request RetainRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s",
		request.AttachmentID, request.Kind, request.Name, request.MIMEType,
		request.Scope, request.SourceRevision, digestContent(request.Content))
}

func retainedSourceKey(request RetainRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%d", request.Scope, request.Source,
		request.AttachmentID, request.SourceRevision)
}

func fetchFingerprint(request FetchRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%d", attachmentHandleFingerprint(request.Handle),
		request.ExpectedSHA256, request.ExpectedSourceRevision)
}

func releaseFingerprint(request ReleaseRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s", request.Handle, request.Scope, request.Reason)
}

func returnLeaseFingerprint(request ReturnLeaseRequest) string {
	return fmt.Sprintf("%s\x00%s\x00%s", request.Lease.LeaseID,
		attachmentHandleFingerprint(request.Lease.Handle), request.Reason)
}

func attachmentHandleFingerprint(handle AttachmentHandle) string {
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00%d\x00%s\x00%s\x00%d\x00%d\x00%d\x00%d",
		handle.Handle, handle.Capability, handle.AttachmentID, handle.Kind,
		handle.Name, handle.MIMEType, handle.SHA256, handle.Bytes,
		handle.Source, handle.Scope, handle.SourceRevision, handle.CapturedNS,
		handle.Width, handle.Height)
}

func newOpaqueIdentity() (string, string, error) {
	bytes := make([]byte, 48)
	if _, err := rand.Read(bytes); err != nil {
		return "", "", fmt.Errorf("generate retained-media identity: %w", err)
	}
	handle := "media_" + base64.RawURLEncoding.EncodeToString(bytes[:16])
	capability := base64.RawURLEncoding.EncodeToString(bytes[16:])
	return handle, capability, nil
}

func newOpaqueToken(prefix string) (string, error) {
	if prefix == "" {
		return "", errors.New("opaque identity prefix is empty")
	}
	random := make([]byte, 24)
	if _, err := rand.Read(random); err != nil {
		return "", fmt.Errorf("generate opaque identity: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(random), nil
}

func secureEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

func appendUnique(values []string, value string) []string {
	if value == "" || slices.Contains(values, value) {
		return values
	}
	return append(values, value)
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}
