package scenarioconversation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestMediaResolverBridgeRejectsIdentityRevisionAndDeliveryDrift(t *testing.T) {
	bridge := newMediaBridgeForTest(t, "session_1", MediaLimits{MaxItems: 2, MaxPending: 2})
	first := mediaHandleForTest("handle_1", "image_1", 1, []byte("first"))
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_other", "handle_item_1", first)); err == nil ||
		!strings.Contains(err.Error(), "session") {
		t.Fatalf("cross-session handle error = %v", err)
	}
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_1", first)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_2", first)); err == nil ||
		!strings.Contains(err.Error(), "twice") {
		t.Fatalf("duplicate handle error = %v", err)
	}
	drifted := mediaHandleForTest("handle_2", "image_1", 1, []byte("second"))
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_2", drifted)); err == nil ||
		!strings.Contains(err.Error(), "did not advance") {
		t.Fatalf("revision drift error = %v", err)
	}

	dropping := mediaTestOutput{typeOf: mediaelements.ResolveRequestType(), broadcast: func(
		context.Context, element.Envelope,
	) (element.SendResult, error) {
		return element.SendResult{Dropped: 1}, nil
	}}
	returned := mediaTestOutput{typeOf: mediaelements.ReturnLeaseRequestType()}
	if err := bridge.Bind(dropping, returned); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	if _, err := bridge.Resolve(first.Handle); err == nil || !strings.Contains(err.Error(), "dropped 1") {
		t.Fatalf("dropped resolve error = %v", err)
	}
	if len(bridge.pending) != 0 || len(bridge.byReply) != 0 || len(bridge.activeHandles) != 0 {
		t.Fatalf("failed delivery retained rendezvous state: pending=%d replies=%d active=%d",
			len(bridge.pending), len(bridge.byReply), len(bridge.activeHandles))
	}
}

func TestMediaResolverBridgeFailsClosedOnResolvedCausalDrift(t *testing.T) {
	requests := make(chan element.Envelope, 1)
	resolvePort := mediaTestOutput{typeOf: mediaelements.ResolveRequestType(), broadcast: func(
		_ context.Context, envelope element.Envelope,
	) (element.SendResult, error) {
		requests <- envelope
		return element.SendResult{Delivered: 1}, nil
	}}
	bridge := newMediaBridgeForTest(t, "session_1", MediaLimits{})
	handle := mediaHandleForTest("handle_1", "image_1", 1, []byte("image"))
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_1", handle)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Bind(resolvePort, mediaTestOutput{typeOf: mediaelements.ReturnLeaseRequestType()}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	resolvedErr := make(chan error, 1)
	go func() {
		_, err := bridge.Resolve(handle.Handle)
		resolvedErr <- err
	}()
	request := receiveMediaEnvelope(t, requests)
	acceptErr := make(chan error, 1)
	go func() {
		acceptErr <- bridge.AcceptResolved(element.Envelope{
			Type: mediaelements.ResolvedAttachmentType(), ItemID: request.ItemID + ":resolved",
			SessionID: "session_other", CausalParents: []string{request.ItemID},
			Payload: mediaelements.ResolvedAttachment{
				Kind: mediaelements.ResultRefused, Handle: handle, Code: "refused",
			},
		})
	}()
	if err := receiveMediaError(t, resolvedErr); err == nil || !strings.Contains(err.Error(), "session boundary") {
		t.Fatalf("Resolve causal drift error = %v", err)
	}
	if err := receiveMediaError(t, acceptErr); err == nil || !strings.Contains(err.Error(), "session boundary") {
		t.Fatalf("AcceptResolved causal drift error = %v", err)
	}
}

func TestMediaResolverBridgeCloseUnblocksConcurrentBoundedRendezvous(t *testing.T) {
	const parallel = 12
	requests := make(chan element.Envelope, parallel)
	resolvePort := mediaTestOutput{typeOf: mediaelements.ResolveRequestType(), broadcast: func(
		_ context.Context, envelope element.Envelope,
	) (element.SendResult, error) {
		requests <- envelope
		return element.SendResult{Delivered: 1}, nil
	}}
	bridge := newMediaBridgeForTest(t, "session_1", MediaLimits{MaxPending: parallel})
	handle := mediaHandleForTest("handle_1", "image_1", 1, []byte("image"))
	if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_1", handle)); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Bind(resolvePort, mediaTestOutput{typeOf: mediaelements.ReturnLeaseRequestType()}); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := bridge.Start(runCtx); err != nil {
		t.Fatal(err)
	}
	errorsOut := make(chan error, parallel)
	for index := 0; index < parallel; index++ {
		go func() {
			_, err := bridge.Resolve(handle.Handle)
			errorsOut <- err
		}()
	}
	for index := 0; index < parallel; index++ {
		_ = receiveMediaEnvelope(t, requests)
	}
	bridge.Close(errors.New("test shutdown"))
	for index := 0; index < parallel; index++ {
		if err := receiveMediaError(t, errorsOut); err == nil || !strings.Contains(err.Error(), "test shutdown") {
			t.Fatalf("closed resolve %d error = %v", index, err)
		}
	}
	if len(bridge.pending) > parallel || len(bridge.byReply) > parallel || len(bridge.handles) > bridge.limits.MaxItems {
		t.Fatalf("bridge state escaped configured bounds")
	}
}

func TestMediaResolverBridgeBoundsHandleAndRevisionMemory(t *testing.T) {
	bridge := newMediaBridgeForTest(t, "session_1", MediaLimits{MaxItems: 2})
	for index := 1; index <= 20; index++ {
		handle := mediaHandleForTest(fmt.Sprintf("handle_%d", index), fmt.Sprintf("image_%d", index), 1,
			[]byte(fmt.Sprintf("image-%d", index)))
		if err := bridge.RegisterHandle(mediaHandleEnvelope("session_1", fmt.Sprintf("handle_item_%d", index), handle)); err != nil {
			t.Fatal(err)
		}
	}
	if len(bridge.handles) != 2 || len(bridge.handleOrder) != 2 || len(bridge.revisions) != 2 {
		t.Fatalf("bounded handle state = handles %d order %d revisions %d",
			len(bridge.handles), len(bridge.handleOrder), len(bridge.revisions))
	}
}

func TestMountedMediaResolverBridgePreservesBytesAndReturnsEveryLease(t *testing.T) {
	harness := mountMediaBridgeGraph(t)
	defer harness.stop(t)
	content := []byte{0x89, 'P', 'N', 'G', 0x00, 0xff}
	retained := harness.retain(t, content)
	if err := harness.bridge.RegisterHandle(mediaHandleEnvelope("session_1", "handle_item_1", retained)); err != nil {
		t.Fatal(err)
	}

	const parallel = 8
	var wait sync.WaitGroup
	for index := 0; index < parallel; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			resolved, err := harness.bridge.Resolve(retained.Handle)
			if err != nil {
				t.Errorf("resolve retained image: %v", err)
				return
			}
			if resolved.MIMEType != "image/png" || !bytes.Equal(resolved.Bytes, content) {
				t.Errorf("resolved media = %#v", resolved)
			}
			resolved.Bytes[0] = 0
		}()
	}
	wait.Wait()
	if err := harness.nextError(); err != nil {
		t.Fatal(err)
	}

	// Fetch/lease resolution does not release the retained visual input. A
	// later pinned multimodal reviewer can resolve the same exact bytes again.
	again, err := harness.bridge.Resolve(retained.Handle)
	if err != nil || !bytes.Equal(again.Bytes, content) {
		t.Fatalf("second review resolution = %x, %v", again.Bytes, err)
	}
	if err := harness.nextError(); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case metrics := <-harness.metrics:
			if metrics.Returned >= parallel+1 && metrics.ActiveLeases == 0 {
				return
			}
		case <-deadline:
			t.Fatal("mounted retained-media runner did not report every lease returned")
		}
	}
}

type mediaTestOutput struct {
	typeOf    element.Type
	broadcast func(context.Context, element.Envelope) (element.SendResult, error)
}

func (output mediaTestOutput) Name() string       { return "test" }
func (output mediaTestOutput) Type() element.Type { return output.typeOf.Clone() }
func (mediaTestOutput) Lanes() []element.Sender   { return nil }
func (output mediaTestOutput) Broadcast(ctx context.Context, envelope element.Envelope) (element.SendResult, error) {
	if output.broadcast != nil {
		return output.broadcast(ctx, envelope)
	}
	return element.SendResult{Delivered: 1}, nil
}

func newMediaBridgeForTest(t *testing.T, sessionID string, limits MediaLimits) *mediaResolverBridge {
	t.Helper()
	bridge, err := newMediaResolverBridge(sessionID, limits)
	if err != nil {
		t.Fatal(err)
	}
	return bridge
}

func mediaHandleForTest(handleID, attachmentID string, revision uint64, content []byte) mediaelements.AttachmentHandle {
	return mediaelements.AttachmentHandle{
		Handle: handleID, Capability: "capability_" + handleID,
		AttachmentID: attachmentID, Kind: mediaelements.AttachmentImage,
		MIMEType: "image/png", SHA256: mediaByteDigest(content), Bytes: len(content),
		Source: SourceMessage, Scope: "session", SourceRevision: revision,
		CapturedNS: revision, Width: 1, Height: 1,
	}
}

func mediaHandleEnvelope(sessionID, itemID string, handle mediaelements.AttachmentHandle) element.Envelope {
	return element.Envelope{
		Type: mediaelements.ResolvedAttachmentType(), ItemID: itemID, SessionID: sessionID,
		CausalParents: []string{itemID + "_input", itemID + "_retained"}, Payload: handle,
	}
}

func receiveMediaEnvelope(t *testing.T, input <-chan element.Envelope) element.Envelope {
	t.Helper()
	select {
	case envelope := <-input:
		return envelope
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for media envelope")
		return element.Envelope{}
	}
}

func receiveMediaError(t *testing.T, input <-chan error) error {
	t.Helper()
	select {
	case err := <-input:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for media result")
		return nil
	}
}

const mediaBridgeTestGraph = `graph scenario_media_bridge_test {
    media.RetainedMedia :: store;
    media.ResolveAttachment :: resolver;
    resolver.fetch -> store.fetch;
    store.fetched -> resolver.fetched;
    resolver.store_cancel -> store.cancel;
    resolver.return_lease -> store.return_lease;
    input retain = store.retain;
    input release = store.release;
    input store_cancel = store.cancel;
    input resolve = resolver.resolve;
    input resolve_cancel = resolver.cancel;
    input return_lease = store.return_lease;
    output retained = store.retained;
    output released = store.released;
    output lease_returned = store.lease_returned;
    output store_outcome = store.outcome;
    output metrics = store.metrics;
    output resolved = resolver.resolved;
    output resolver_outcome = resolver.outcome;
}`

type mountedMediaBridge struct {
	mounted     *graphruntime.Mounted
	bridge      *mediaResolverBridge
	ctx         context.Context
	cancel      context.CancelFunc
	done        chan error
	errors      chan error
	metrics     chan mediaelements.RetentionMetrics
	retainInput element.OutputPort
	retained    element.InputPort
}

func mountMediaBridgeGraph(t *testing.T) *mountedMediaBridge {
	t.Helper()
	catalog := resolve.NewCatalog()
	if err := mediaelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	parsed, err := syntax.Parse("scenario-media-bridge-test.ortg", []byte(mediaBridgeTestGraph))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := mediaelements.RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	retainedConfig, err := json.Marshal(mediaelements.RetainedMediaConfig{
		Scope: "session", MaxItems: 32, MaxBytes: 1 << 20, MaxItemBytes: 1 << 20,
		MaxMetadataBytes: 64 << 10, MaxActiveLeases: 32,
		TerminalMemory: 64, CancelMemory: 32,
	})
	if err != nil {
		t.Fatal(err)
	}
	resolverConfig, err := json.Marshal(mediaelements.ResolveAttachmentConfig{
		MaxPending: 32, MaxBytes: 1 << 20, MaxMetadataBytes: 64 << 10,
		AllowedMIMETypes: []string{"image/png"}, TerminalMemory: 64,
	})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: compiled.Graph, Registry: registry,
		Values: map[string]json.RawMessage{"store": retainedConfig, "resolver": resolverConfig},
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge := newMediaBridgeForTest(t, "session_1", MediaLimits{
		MaxItems: 32, MaxBytes: 1 << 20, MaxItemBytes: 1 << 20,
		MaxPending: 32, MaxActiveLeases: 32,
	})
	resolvePort, err := mounted.Ingress("resolve")
	if err != nil {
		t.Fatal(err)
	}
	returnPort, err := mounted.Ingress("return_lease")
	if err != nil {
		t.Fatal(err)
	}
	if err := bridge.Bind(resolvePort, returnPort); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := bridge.Start(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	harness := &mountedMediaBridge{
		mounted: mounted, bridge: bridge, ctx: ctx, cancel: cancel,
		done: make(chan error, 1), errors: make(chan error, 32),
		metrics: make(chan mediaelements.RetentionMetrics, 64),
	}
	harness.retainInput, err = mounted.Ingress("retain")
	if err != nil {
		t.Fatal(err)
	}
	harness.retained, err = mounted.Egress("retained")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := mounted.Egress("resolved")
	if err != nil {
		t.Fatal(err)
	}
	leaseReturned, err := mounted.Egress("lease_returned")
	if err != nil {
		t.Fatal(err)
	}
	metrics, err := mounted.Egress("metrics")
	if err != nil {
		t.Fatal(err)
	}
	go harness.drainResolved(resolved)
	go harness.drainLeaseReturned(leaseReturned)
	go harness.drainMetrics(metrics)
	go func() { harness.done <- mounted.Run(ctx) }()
	return harness
}

func (harness *mountedMediaBridge) retain(t *testing.T, content []byte) mediaelements.AttachmentHandle {
	t.Helper()
	request := mediaelements.RetainRequest{
		AttachmentID: "image_1", Kind: mediaelements.AttachmentImage,
		MIMEType: "image/png", Content: append([]byte(nil), content...),
		SHA256: mediaByteDigest(content), Source: SourceMessage, Scope: "session",
		SourceRevision: 1, CapturedNS: 1, Width: 1, Height: 1,
	}
	delivery, err := harness.retainInput.Broadcast(harness.ctx, element.Envelope{
		Type: harness.retainInput.Type(), ItemID: "retain_1", SessionID: "session_1",
		SourceID: "message_1", TraceID: "retain_1", Payload: request,
	})
	if err != nil || delivery.Delivered != 1 || delivery.Dropped != 0 {
		t.Fatalf("retain delivery = %+v, %v", delivery, err)
	}
	envelopeCh := make(chan element.Envelope, 1)
	go func() {
		envelope, receiveErr := harness.retained.Receive(harness.ctx)
		if receiveErr != nil {
			harness.errors <- receiveErr
			return
		}
		envelopeCh <- envelope
	}()
	envelope := receiveMediaEnvelope(t, envelopeCh)
	result, ok := envelope.Payload.(mediaelements.RetainResult)
	if !ok || result.Kind != mediaelements.ResultSucceeded {
		t.Fatalf("retain result = %#v", envelope.Payload)
	}
	return result.Handle
}

func (harness *mountedMediaBridge) drainResolved(port element.InputPort) {
	for {
		envelope, err := port.Receive(harness.ctx)
		if err != nil {
			if harness.ctx.Err() == nil {
				harness.errors <- err
			}
			return
		}
		if err := harness.bridge.AcceptResolved(envelope); err != nil {
			harness.errors <- err
			return
		}
	}
}

func (harness *mountedMediaBridge) drainLeaseReturned(port element.InputPort) {
	for {
		envelope, err := port.Receive(harness.ctx)
		if err != nil {
			if harness.ctx.Err() == nil {
				harness.errors <- err
			}
			return
		}
		if err := harness.bridge.AcceptLeaseReturned(envelope); err != nil {
			harness.errors <- err
			return
		}
	}
}

func (harness *mountedMediaBridge) drainMetrics(port element.InputPort) {
	for {
		envelope, err := port.Receive(harness.ctx)
		if err != nil {
			return
		}
		metrics, ok := envelope.Payload.(mediaelements.RetentionMetrics)
		if !ok {
			harness.errors <- fmt.Errorf("retention metrics payload has type %T", envelope.Payload)
			return
		}
		select {
		case harness.metrics <- metrics:
		default:
		}
	}
}

func (harness *mountedMediaBridge) nextError() error {
	select {
	case err := <-harness.errors:
		return err
	default:
		return nil
	}
}

func (harness *mountedMediaBridge) stop(t *testing.T) {
	t.Helper()
	harness.bridge.Close(errors.New("test complete"))
	harness.cancel()
	closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := harness.mounted.Close(closeCtx); err != nil {
		t.Error(err)
	}
	select {
	case err := <-harness.done:
		if err != nil && !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "closed") {
			t.Error(err)
		}
	case <-time.After(2 * time.Second):
		t.Error("mounted media graph did not stop")
	}
}
