package scenarioconversation

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
)

// mediaResolverBridge is a capability-minimizing rendezvous between the
// synchronous continuation.MediaResolver contract and the graph-visible
// media.RetainedMedia -> media.ResolveAttachment lease protocol. It owns no
// media bytes. The only byte authority remains the mounted RetainedMedia node.
type mediaResolverBridge struct {
	mu        sync.Mutex
	sessionID string
	limits    MediaLimits

	resolvePort element.OutputPort
	returnPort  element.OutputPort
	runCtx      context.Context
	ready       chan struct{}
	done        chan struct{}
	started     bool
	closed      bool
	closeCause  error
	sequence    uint64

	handles       map[string]mediaHandleRecord
	handleOrder   []string
	revisions     map[string]uint64
	activeHandles map[string]int
	pending       map[string]*pendingMediaResolution
	byReply       map[string]string
	returns       map[string]*pendingLeaseReturn
}

type mediaHandleRecord struct {
	handle mediaelements.AttachmentHandle
	itemID string
}

type pendingMediaResolution struct {
	requestID string
	replyID   string
	handle    mediaelements.AttachmentHandle
	reply     chan mediaResolutionDelivery
}

type mediaResolutionDelivery struct {
	envelope element.Envelope
	result   mediaelements.ResolvedAttachment
	err      error
	consumed chan struct{}
}

type pendingLeaseReturn struct {
	requestID string
	replyID   string
	parentID  string
	leaseID   string
	handle    string
	reply     chan error
}

func newMediaResolverBridge(sessionID string, source MediaLimits) (*mediaResolverBridge, error) {
	if !canonicalIdentity(sessionID) {
		return nil, errors.New("scenario conversation media resolver requires a canonical session ID")
	}
	limits, err := source.normalized()
	if err != nil {
		return nil, err
	}
	return &mediaResolverBridge{
		sessionID: sessionID, limits: limits, ready: make(chan struct{}), done: make(chan struct{}),
		handles: make(map[string]mediaHandleRecord), revisions: make(map[string]uint64),
		activeHandles: make(map[string]int), pending: make(map[string]*pendingMediaResolution),
		byReply: make(map[string]string), returns: make(map[string]*pendingLeaseReturn),
	}, nil
}

// Bind installs only mounted graph boundary ports. It is called by the
// session adapter factory after Mount and before either graph component runs.
func (bridge *mediaResolverBridge) Bind(resolve, returnLease element.OutputPort) error {
	if bridge == nil || resolve == nil || returnLease == nil {
		return errors.New("bind scenario conversation media resolver: bridge and ports are required")
	}
	if !resolve.Type().Equal(mediaelements.ResolveRequestType()) {
		return fmt.Errorf("bind scenario conversation media resolver: resolve port has type %s", resolve.Type().String())
	}
	if !returnLease.Type().Equal(mediaelements.ReturnLeaseRequestType()) {
		return fmt.Errorf("bind scenario conversation media resolver: return-lease port has type %s", returnLease.Type().String())
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed || bridge.resolvePort != nil || bridge.returnPort != nil {
		return errors.New("bind scenario conversation media resolver: bridge is closed or already bound")
	}
	bridge.resolvePort = resolve
	bridge.returnPort = returnLease
	return nil
}

// Start supplies the supervised session lifetime. Provider construction can
// happen during graph mount, but resolution cannot send before adapter Run.
func (bridge *mediaResolverBridge) Start(ctx context.Context) error {
	if bridge == nil || ctx == nil {
		return errors.New("start scenario conversation media resolver: bridge and context are required")
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed || bridge.started {
		return errors.New("start scenario conversation media resolver: bridge is closed or already started")
	}
	if bridge.resolvePort == nil || bridge.returnPort == nil {
		return errors.New("start scenario conversation media resolver: graph ports are not bound")
	}
	bridge.started = true
	bridge.runCtx = ctx
	close(bridge.ready)
	return nil
}

// RegisterHandle accepts the complete capability-bearing handle emitted by
// ingress.UserContent. Trajectory items retain only AttachmentHandle.MediaRef,
// so cognition can later name a handle without receiving its capability.
func (bridge *mediaResolverBridge) RegisterHandle(envelope element.Envelope) error {
	if bridge == nil {
		return errors.New("scenario conversation media handle: nil bridge")
	}
	handle, ok := attachmentHandlePayload(envelope.Payload)
	if !ok {
		return fmt.Errorf("scenario conversation media handle has payload %T", envelope.Payload)
	}
	if envelope.SessionID != bridge.sessionID || !canonicalIdentity(envelope.ItemID) ||
		len(envelope.CausalParents) != 2 || !canonicalIdentity(envelope.CausalParents[0]) ||
		!canonicalIdentity(envelope.CausalParents[1]) || envelope.CausalParents[0] == envelope.CausalParents[1] {
		return errors.New("scenario conversation media handle crossed or drifted from its exact causal session identity")
	}
	if err := validateAttachmentHandle(handle, bridge.limits.MaxBytes); err != nil {
		return fmt.Errorf("scenario conversation media handle: %w", err)
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed {
		return bridge.closedErrorLocked()
	}
	if _, duplicate := bridge.handles[handle.Handle]; duplicate {
		return fmt.Errorf("scenario conversation media handle %q was emitted twice", handle.Handle)
	}
	revisionKey := handle.Source + "\x00" + handle.AttachmentID
	if previous := bridge.revisions[revisionKey]; previous != 0 && handle.SourceRevision <= previous {
		return fmt.Errorf("scenario conversation media handle %q revision %d did not advance past %d",
			handle.Handle, handle.SourceRevision, previous)
	}
	if err := bridge.makeHandleRoomLocked(); err != nil {
		return err
	}
	bridge.handles[handle.Handle] = mediaHandleRecord{handle: handle, itemID: envelope.ItemID}
	bridge.handleOrder = append(bridge.handleOrder, handle.Handle)
	bridge.revisions[revisionKey] = handle.SourceRevision
	return nil
}

func (bridge *mediaResolverBridge) Resolve(handleID string) (media continuation.Media, err error) {
	if bridge == nil || !canonicalIdentity(handleID) {
		return continuation.Media{}, errors.New("resolve scenario conversation media: invalid handle")
	}
	select {
	case <-bridge.ready:
	case <-bridge.done:
		return continuation.Media{}, bridge.closedError()
	}

	pending, port, runCtx, err := bridge.beginResolution(handleID)
	if err != nil {
		return continuation.Media{}, err
	}
	request := mediaelements.ResolveRequest{
		Handle: pending.handle, AcceptMIMETypes: []string{pending.handle.MIMEType},
		MaxBytes: bridge.limits.MaxBytes, ExpectedSourceRevision: pending.handle.SourceRevision,
	}
	delivery, sendErr := port.Broadcast(runCtx, element.Envelope{
		Type: port.Type(), ItemID: pending.requestID, SessionID: bridge.sessionID,
		SourceID: pending.handle.Source, Sequence: bridge.sequenceValue(),
		TraceID: pending.requestID, CausalParents: []string{bridge.handleItemID(handleID)},
		Payload: request,
	})
	if sendErr != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		bridge.abortResolution(pending)
		if sendErr != nil {
			return continuation.Media{}, fmt.Errorf("resolve scenario conversation media: send request: %w", sendErr)
		}
		return continuation.Media{}, fmt.Errorf("resolve scenario conversation media: request delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}

	var resolved mediaResolutionDelivery
	select {
	case resolved = <-pending.reply:
	case <-bridge.done:
		bridge.abortResolution(pending)
		return continuation.Media{}, bridge.closedError()
	}
	if resolved.consumed != nil {
		defer close(resolved.consumed)
	}
	acquired := resolved.result.Lease.LeaseID != ""
	if acquired {
		defer func() {
			returnErr := bridge.returnLease(resolved.envelope, resolved.result.Lease)
			err = errors.Join(err, returnErr)
		}()
	}
	if resolved.err != nil {
		return continuation.Media{}, resolved.err
	}
	if resolved.result.Kind != mediaelements.ResultSucceeded {
		return continuation.Media{}, fmt.Errorf("resolve scenario conversation media %q: %s/%s: %s",
			handleID, resolved.result.Kind, resolved.result.Code, resolved.result.Message)
	}
	if !acquired {
		return continuation.Media{}, errors.New("resolve scenario conversation media: successful result has no live lease")
	}
	content, readErr := readExactLease(resolved.result.Lease, pending.handle, bridge.limits.MaxBytes)
	if readErr != nil {
		return continuation.Media{}, readErr
	}
	return continuation.Media{MIMEType: pending.handle.MIMEType, Bytes: content}, nil
}

func (bridge *mediaResolverBridge) beginResolution(handleID string) (
	*pendingMediaResolution, element.OutputPort, context.Context, error,
) {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if bridge.closed {
		return nil, nil, nil, bridge.closedErrorLocked()
	}
	record, found := bridge.handles[handleID]
	if !found {
		return nil, nil, nil, fmt.Errorf("resolve scenario conversation media: handle %q is not admitted in this session", handleID)
	}
	if len(bridge.pending) >= bridge.limits.MaxPending {
		return nil, nil, nil, errors.New("resolve scenario conversation media: pending bound reached")
	}
	requestID, sequence, err := bridge.nextIDLocked("mediaresolve")
	if err != nil {
		return nil, nil, nil, err
	}
	pending := &pendingMediaResolution{
		requestID: requestID, replyID: requestID + ":resolved", handle: record.handle,
		reply: make(chan mediaResolutionDelivery, 1),
	}
	bridge.pending[requestID] = pending
	bridge.byReply[pending.replyID] = requestID
	bridge.activeHandles[handleID]++
	bridge.sequence = sequence
	return pending, bridge.resolvePort, bridge.runCtx, nil
}

// AcceptResolved is called only by the adapter's exact output-boundary
// drainer. Known integrity failures are acknowledged after Resolve has had a
// chance to return any attached live lease, then fail the session closed.
func (bridge *mediaResolverBridge) AcceptResolved(envelope element.Envelope) error {
	if bridge == nil {
		return errors.New("accept scenario conversation media resolution: nil bridge")
	}
	bridge.mu.Lock()
	requestID, found := bridge.byReply[envelope.ItemID]
	pending := bridge.pending[requestID]
	if !found || pending == nil || pending.replyID != envelope.ItemID {
		bridge.mu.Unlock()
		return fmt.Errorf("scenario conversation media resolution %q has no exact pending request", envelope.ItemID)
	}
	delete(bridge.byReply, envelope.ItemID)
	delete(bridge.pending, requestID)
	bridge.dropActiveLocked(pending.handle.Handle)
	bridge.mu.Unlock()

	result, payloadOK := resolvedAttachmentPayload(envelope.Payload)
	var integrityErr error
	if !payloadOK {
		integrityErr = fmt.Errorf("scenario conversation media resolution %q has payload %T", envelope.ItemID, envelope.Payload)
	} else if envelope.SessionID != bridge.sessionID {
		integrityErr = errors.New("scenario conversation media resolution crossed a session boundary")
	} else if len(envelope.CausalParents) != 1 || envelope.CausalParents[0] != pending.requestID {
		integrityErr = errors.New("scenario conversation media resolution has a non-exact immediate parent")
	} else if result.Handle != pending.handle {
		integrityErr = errors.New("scenario conversation media resolution handle identity drifted")
	} else if err := validateMediaResult(result); err != nil {
		integrityErr = err
	}
	delivery := mediaResolutionDelivery{envelope: envelope.Clone(), result: result, err: integrityErr}
	if integrityErr != nil {
		delivery.consumed = make(chan struct{})
	}
	select {
	case pending.reply <- delivery:
	case <-bridge.done:
		return bridge.closedError()
	}
	if delivery.consumed != nil {
		select {
		case <-delivery.consumed:
		case <-bridge.done:
		}
		return integrityErr
	}
	return nil
}

func (bridge *mediaResolverBridge) returnLease(cause element.Envelope, lease mediaelements.BlobLease) error {
	if lease.LeaseID == "" {
		return errors.New("return scenario conversation media lease: missing lease ID")
	}
	bridge.mu.Lock()
	if bridge.closed {
		err := bridge.closedErrorLocked()
		bridge.mu.Unlock()
		return err
	}
	requestID, sequence, err := bridge.nextIDLocked("mediareturn")
	if err != nil {
		bridge.mu.Unlock()
		return err
	}
	pending := &pendingLeaseReturn{
		requestID: requestID, replyID: requestID + ":lease_returned", parentID: cause.ItemID,
		leaseID: lease.LeaseID, handle: lease.Handle.Handle, reply: make(chan error, 1),
	}
	bridge.returns[pending.replyID] = pending
	port, runCtx := bridge.returnPort, bridge.runCtx
	bridge.sequence = sequence
	bridge.mu.Unlock()

	delivery, sendErr := port.Broadcast(runCtx, element.Envelope{
		Type: port.Type(), ItemID: requestID, SessionID: bridge.sessionID,
		RunID: lease.LeaseID, Sequence: sequence, TraceID: requestID,
		CausalParents: []string{cause.ItemID}, Payload: mediaelements.ReturnLeaseRequest{
			Lease: lease, Reason: "continuation media projection completed",
		},
	})
	if sendErr != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		bridge.removeLeaseReturn(pending)
		if sendErr != nil {
			return fmt.Errorf("return scenario conversation media lease: %w", sendErr)
		}
		return fmt.Errorf("return scenario conversation media lease: request delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	select {
	case resultErr := <-pending.reply:
		return resultErr
	case <-bridge.done:
		bridge.removeLeaseReturn(pending)
		return bridge.closedError()
	}
}

func (bridge *mediaResolverBridge) AcceptLeaseReturned(envelope element.Envelope) error {
	if bridge == nil {
		return errors.New("accept scenario conversation media lease return: nil bridge")
	}
	bridge.mu.Lock()
	pending := bridge.returns[envelope.ItemID]
	if pending != nil {
		delete(bridge.returns, envelope.ItemID)
	}
	bridge.mu.Unlock()
	if pending == nil {
		return fmt.Errorf("scenario conversation media lease return %q has no exact pending request", envelope.ItemID)
	}
	result, ok := returnLeaseResultPayload(envelope.Payload)
	var resultErr error
	if !ok {
		resultErr = fmt.Errorf("scenario conversation media lease return %q has payload %T", envelope.ItemID, envelope.Payload)
	} else if envelope.SessionID != bridge.sessionID || len(envelope.CausalParents) != 1 ||
		envelope.CausalParents[0] != pending.requestID {
		resultErr = errors.New("scenario conversation media lease return drifted from its exact session or parent")
	} else if result.LeaseID != pending.leaseID || result.Handle != pending.handle {
		resultErr = errors.New("scenario conversation media lease return identity drifted")
	} else if result.Kind != mediaelements.ResultSucceeded {
		resultErr = fmt.Errorf("scenario conversation media lease return reached %s/%s: %s",
			result.Kind, result.Code, result.Message)
	}
	select {
	case pending.reply <- resultErr:
	case <-bridge.done:
		return bridge.closedError()
	}
	return resultErr
}

func (bridge *mediaResolverBridge) Close(cause error) {
	if bridge == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.closed {
		bridge.mu.Unlock()
		return
	}
	bridge.closed = true
	if cause == nil {
		cause = errors.New("scenario conversation media resolver closed")
	}
	bridge.closeCause = cause
	close(bridge.done)
	bridge.mu.Unlock()
}

func (bridge *mediaResolverBridge) abortResolution(pending *pendingMediaResolution) {
	if bridge == nil || pending == nil {
		return
	}
	bridge.mu.Lock()
	if bridge.pending[pending.requestID] == pending {
		delete(bridge.pending, pending.requestID)
		delete(bridge.byReply, pending.replyID)
		bridge.dropActiveLocked(pending.handle.Handle)
	}
	bridge.mu.Unlock()
}

func (bridge *mediaResolverBridge) removeLeaseReturn(pending *pendingLeaseReturn) {
	bridge.mu.Lock()
	if bridge.returns[pending.replyID] == pending {
		delete(bridge.returns, pending.replyID)
	}
	bridge.mu.Unlock()
}

func (bridge *mediaResolverBridge) makeHandleRoomLocked() error {
	for len(bridge.handles) >= bridge.limits.MaxItems {
		removed := false
		for index, handleID := range bridge.handleOrder {
			if bridge.activeHandles[handleID] != 0 {
				continue
			}
			record := bridge.handles[handleID]
			delete(bridge.handles, handleID)
			revisionKey := record.handle.Source + "\x00" + record.handle.AttachmentID
			if bridge.revisions[revisionKey] == record.handle.SourceRevision {
				delete(bridge.revisions, revisionKey)
			}
			bridge.handleOrder = slices.Delete(bridge.handleOrder, index, index+1)
			removed = true
			break
		}
		if !removed {
			return errors.New("scenario conversation media handle bound is pinned by active resolutions")
		}
	}
	return nil
}

func (bridge *mediaResolverBridge) dropActiveLocked(handleID string) {
	if bridge.activeHandles[handleID] <= 1 {
		delete(bridge.activeHandles, handleID)
	} else {
		bridge.activeHandles[handleID]--
	}
}

func (bridge *mediaResolverBridge) nextIDLocked(kind string) (string, uint64, error) {
	if bridge.sequence == ^uint64(0) {
		return "", 0, errors.New("scenario conversation media resolver sequence exhausted")
	}
	next := bridge.sequence + 1
	digest := sha256.Sum256([]byte(bridge.sessionID))
	prefix := base64.RawURLEncoding.EncodeToString(digest[:12])
	return fmt.Sprintf("%s_%s_%d", kind, prefix, next), next, nil
}

func (bridge *mediaResolverBridge) sequenceValue() uint64 {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.sequence
}

func (bridge *mediaResolverBridge) handleItemID(handleID string) string {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.handles[handleID].itemID
}

func (bridge *mediaResolverBridge) closedError() error {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	return bridge.closedErrorLocked()
}

func (bridge *mediaResolverBridge) closedErrorLocked() error {
	if bridge.closeCause != nil {
		return fmt.Errorf("scenario conversation media resolver closed: %w", bridge.closeCause)
	}
	return errors.New("scenario conversation media resolver is closed")
}

func validateAttachmentHandle(handle mediaelements.AttachmentHandle, maxBytes int) error {
	for label, value := range map[string]string{
		"handle": handle.Handle, "capability": handle.Capability,
		"attachment ID": handle.AttachmentID, "source": handle.Source, "scope": handle.Scope,
	} {
		if !canonicalIdentity(value) {
			return fmt.Errorf("%s is not canonical", label)
		}
	}
	switch handle.Kind {
	case mediaelements.AttachmentImage, mediaelements.AttachmentFile, mediaelements.AttachmentOther:
	default:
		return fmt.Errorf("unsupported attachment kind %q", handle.Kind)
	}
	if handle.SourceRevision == 0 || handle.Bytes < 1 || handle.Bytes > maxBytes {
		return errors.New("attachment revision or byte bound is invalid")
	}
	if handle.Kind == mediaelements.AttachmentImage && (handle.Width <= 0 || handle.Height <= 0) {
		return errors.New("image attachment requires positive dimensions")
	}
	if handle.Width < 0 || handle.Height < 0 || strings.ContainsAny(handle.Name, "\x00\r\n") {
		return errors.New("attachment dimensions or name are invalid")
	}
	parsed, parameters, err := mime.ParseMediaType(handle.MIMEType)
	if err != nil || !strings.Contains(parsed, "/") || mime.FormatMediaType(strings.ToLower(parsed), parameters) != handle.MIMEType {
		return errors.New("attachment MIME type is not canonical")
	}
	if !canonicalSHA256Digest(handle.SHA256) {
		return errors.New("attachment digest is not canonical")
	}
	return nil
}

func validateMediaResult(result mediaelements.ResolvedAttachment) error {
	switch result.Kind {
	case mediaelements.ResultSucceeded:
		if result.Lease.LeaseID == "" || result.Lease.Handle != result.Handle {
			return errors.New("successful media resolution has a missing or drifted lease")
		}
	case mediaelements.ResultCanceled, mediaelements.ResultRefused, mediaelements.ResultFailed,
		mediaelements.ResultExpired, mediaelements.ResultIgnored:
		if result.Lease.LeaseID != "" {
			return errors.New("non-successful media resolution carried a live lease")
		}
	default:
		return fmt.Errorf("media resolution has unsupported result kind %q", result.Kind)
	}
	return nil
}

func readExactLease(
	lease mediaelements.BlobLease, handle mediaelements.AttachmentHandle, maxBytes int,
) ([]byte, error) {
	size, err := lease.Size()
	if err != nil {
		return nil, fmt.Errorf("resolve scenario conversation media: inspect lease: %w", err)
	}
	if size != handle.Bytes || size < 1 || size > maxBytes {
		return nil, errors.New("resolve scenario conversation media: lease size drifted from retained identity")
	}
	reader, err := lease.Open()
	if err != nil {
		return nil, fmt.Errorf("resolve scenario conversation media: open lease: %w", err)
	}
	content, readErr := io.ReadAll(io.LimitReader(reader, int64(maxBytes)+1))
	closeErr := reader.Close()
	if readErr != nil || closeErr != nil {
		return nil, errors.Join(fmt.Errorf("resolve scenario conversation media: read lease: %w", readErr), closeErr)
	}
	if len(content) != handle.Bytes || mediaByteDigest(content) != handle.SHA256 {
		return nil, errors.New("resolve scenario conversation media: lease bytes drifted from retained identity")
	}
	return content, nil
}

func attachmentHandlePayload(payload any) (mediaelements.AttachmentHandle, bool) {
	switch value := payload.(type) {
	case mediaelements.AttachmentHandle:
		return value, true
	case *mediaelements.AttachmentHandle:
		if value != nil {
			return *value, true
		}
	}
	return mediaelements.AttachmentHandle{}, false
}

func resolvedAttachmentPayload(payload any) (mediaelements.ResolvedAttachment, bool) {
	switch value := payload.(type) {
	case mediaelements.ResolvedAttachment:
		return value, true
	case *mediaelements.ResolvedAttachment:
		if value != nil {
			return *value, true
		}
	}
	return mediaelements.ResolvedAttachment{}, false
}

func returnLeaseResultPayload(payload any) (mediaelements.ReturnLeaseResult, bool) {
	switch value := payload.(type) {
	case mediaelements.ReturnLeaseResult:
		return value, true
	case *mediaelements.ReturnLeaseResult:
		if value != nil {
			return *value, true
		}
	}
	return mediaelements.ReturnLeaseResult{}, false
}

func canonicalSHA256Digest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func mediaByteDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return "sha256:" + hex.EncodeToString(digest[:])
}
