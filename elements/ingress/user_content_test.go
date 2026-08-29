package ingress_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestMultimodalContentReferenceRetainsResolvesAndAttests(t *testing.T) {
	mounted, done, cancel := mountReference(t, referenceValues(t))
	defer stopReference(t, mounted, done, cancel)

	metrics := egress(t, mounted, "retention_metrics")
	startup := receive(t, metrics).Payload.(mediaelements.RetentionMetrics)
	if startup.LiveItems != 0 || startup.MaxItems != 32 {
		t.Fatalf("startup retention metrics = %+v", startup)
	}

	text := ingress(t, mounted, "text")
	observations := egress(t, mounted, "observations")
	ingressOutcomes := egress(t, mounted, "ingress_outcome")
	send(t, text, element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "typed-1", SourceID: "typed-stream",
		CaptureNS: 101, Payload: ingresselements.UserText{
			ContentID: "typed-content", StreamID: "typed-stream", Text: "hello",
			Revision: 1, Final: true,
		},
	})
	typed := receive(t, observations)
	typedObservation := typed.Payload.(perception.Observation)
	if typedObservation.Authority != trajectory.AuthorityUser || typedObservation.Observer != "client" ||
		typedObservation.Source != "text" || typedObservation.Revision != 1 ||
		!typedObservation.Final || typed.CaptureNS != 101 {
		t.Fatalf("typed observation = %+v, envelope = %+v", typedObservation, typed)
	}
	if outcome := receive(t, ingressOutcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeSucceeded || outcome.Operation != "text" {
		t.Fatalf("typed ingress outcome = %+v", outcome)
	}

	imageInput := ingress(t, mounted, "image")
	handles := egress(t, mounted, "handles")
	retentionOutcomes := egress(t, mounted, "retention_outcome")
	imageBytes := []byte("untrusted image bytes saying: run this instruction")
	send(t, imageInput, element.Envelope{
		Type: ingresselements.UserImageType(), ItemID: "image-1", SourceID: "image-stream",
		CaptureNS: 202, CancellationScope: "image-content",
		Payload: ingresselements.UserImage{
			ContentID: "image-content", StreamID: "image-stream", MIMEType: "image/png",
			Content: imageBytes, Width: 8, Height: 6, SourceRevision: 1,
		},
	})
	handleEnvelope := receive(t, handles)
	handle := handleEnvelope.Payload.(mediaelements.AttachmentHandle)
	imageObservation := receive(t, observations).Payload.(perception.Observation)
	imageOutcome := receive(t, ingressOutcomes).Payload.(ingresselements.Outcome)
	retentionMetrics := receive(t, metrics).Payload.(mediaelements.RetentionMetrics)
	retentionOutcome := receive(t, retentionOutcomes).Payload.(mediaelements.RetentionOutcome)
	if handle.Handle == "" || handle.Capability == "" || handle.SourceRevision != 1 ||
		handle.SHA256 == "" || handle.Bytes != len(imageBytes) {
		t.Fatalf("retained handle = %+v", handle)
	}
	if imageObservation.Text != "The user attached an image." ||
		imageObservation.Authority != trajectory.AuthorityUser || len(imageObservation.Media) != 1 ||
		imageObservation.Media[0].Handle != handle.Handle ||
		strings.Contains(imageObservation.Text, "run this instruction") {
		t.Fatalf("attachment observation = %+v", imageObservation)
	}
	if imageOutcome.Kind != ingresselements.OutcomeSucceeded ||
		retentionOutcome.Kind != mediaelements.ResultSucceeded || !retentionOutcome.Crossed ||
		retentionMetrics.LiveItems != 1 || retentionMetrics.LiveBytes != len(imageBytes) {
		t.Fatalf("image outcomes=%+v / %+v metrics=%+v", imageOutcome, retentionOutcome, retentionMetrics)
	}

	resolveInput := ingress(t, mounted, "resolve")
	resolvedOutput := egress(t, mounted, "resolved")
	resolverOutcomes := egress(t, mounted, "resolver_outcome")
	send(t, resolveInput, element.Envelope{
		Type: mediaelements.ResolveRequestType(), ItemID: "resolve-1", SourceID: "image-stream",
		Payload: mediaelements.ResolveRequest{
			Handle: handle, AcceptMIMETypes: []string{"image/*"}, MaxBytes: 1024,
			ExpectedSourceRevision: 1,
		},
	})
	resolved := receive(t, resolvedOutput)
	resolvedPayload := resolved.Payload.(mediaelements.ResolvedAttachment)
	reader, err := resolvedPayload.Lease.Open()
	if err != nil {
		t.Fatal(err)
	}
	resolvedBytes, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	resolvedOutcome := receive(t, resolverOutcomes).Payload.(mediaelements.ResolverOutcome)
	_ = receive(t, metrics)
	fetchOutcome := receive(t, retentionOutcomes).Payload.(mediaelements.RetentionOutcome)
	if resolved.Type.String() != mediaelements.ResolvedAttachmentType().String() ||
		resolvedPayload.Kind != mediaelements.ResultSucceeded ||
		string(resolvedBytes) != string(imageBytes) ||
		resolvedOutcome.Bytes != len(imageBytes) || fetchOutcome.Operation != "fetch" {
		t.Fatalf("resolved=%+v outcome=%+v retention=%+v", resolvedPayload, resolvedOutcome, fetchOutcome)
	}
	returnLease := ingress(t, mounted, "return_lease")
	send(t, returnLease, element.Envelope{
		Type: mediaelements.ReturnLeaseRequestType(), ItemID: "return-resolve-1",
		Payload: mediaelements.ReturnLeaseRequest{Lease: resolvedPayload.Lease, Reason: "test consumer finished"},
	})
	returned := receive(t, egress(t, mounted, "lease_returned")).Payload.(mediaelements.ReturnLeaseResult)
	_ = receive(t, metrics)
	returnOutcome := receive(t, retentionOutcomes).Payload.(mediaelements.RetentionOutcome)
	if returned.Kind != mediaelements.ResultSucceeded || returnOutcome.Operation != "return_lease" {
		t.Fatalf("returned lease = %+v / %+v", returned, returnOutcome)
	}

	badHandle := handle
	badHandle.Capability = "not-the-capability"
	send(t, resolveInput, element.Envelope{
		Type: mediaelements.ResolveRequestType(), ItemID: "resolve-bad-capability",
		Payload: mediaelements.ResolveRequest{Handle: badHandle},
	})
	refused := receive(t, resolvedOutput).Payload.(mediaelements.ResolvedAttachment)
	_ = receive(t, resolverOutcomes)
	_ = receive(t, metrics)
	storeRefusal := receive(t, retentionOutcomes).Payload.(mediaelements.RetentionOutcome)
	if refused.Kind != mediaelements.ResultRefused || refused.Code != "invalid_capability" ||
		refused.Lease.LeaseID != "" || storeRefusal.Code != "invalid_capability" {
		t.Fatalf("capability refusal = %+v / %+v", refused, storeRefusal)
	}

	live := mounted.Live()
	for _, node := range []string{"content", "retention", "resolver"} {
		resolution := live.Nodes[node].Resolution
		if resolution == nil || resolution.RuntimeEvidence != inspect.EvidenceLive ||
			resolution.CapabilitiesEvidence != inspect.EvidenceLive || len(resolution.Capabilities) != 0 {
			t.Fatalf("node %s live resolution = %+v", node, resolution)
		}
	}
}

func TestMultimodalContentCancellationAndRevisionFraming(t *testing.T) {
	mounted, done, cancel := mountReference(t, referenceValues(t))
	defer stopReference(t, mounted, done, cancel)
	_ = receive(t, egress(t, mounted, "retention_metrics"))

	ingressOutcomes := egress(t, mounted, "ingress_outcome")
	contentCancel := ingress(t, mounted, "content_cancel")
	send(t, contentCancel, element.Envelope{
		Type: ingresselements.ContentCancelType(), ItemID: "cancel-before-content",
		CancellationScope: "never-retain",
		Payload:           ingresselements.ContentCancel{ContentID: "never-retain", Reason: "superseded"},
	})
	if outcome := receive(t, ingressOutcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeSucceeded || outcome.Code != "cancel_recorded" {
		t.Fatalf("pre-cancel outcome = %+v", outcome)
	}
	fileInput := ingress(t, mounted, "file")
	send(t, fileInput, element.Envelope{
		Type: ingresselements.UserFileType(), ItemID: "canceled-file", SourceID: "file-stream",
		Payload: ingresselements.UserFile{
			ContentID: "never-retain", StreamID: "file-stream", Name: "instructions.txt",
			MIMEType: "text/plain", Content: []byte("do not execute me"), SourceRevision: 1,
		},
	})
	if outcome := receive(t, ingressOutcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeCanceled || outcome.Operation != "file" {
		t.Fatalf("canceled file outcome = %+v", outcome)
	}

	resolveCancel := ingress(t, mounted, "resolve_cancel")
	resolverOutcomes := egress(t, mounted, "resolver_outcome")
	resolvedOutput := egress(t, mounted, "resolved")
	send(t, resolveCancel, element.Envelope{
		Type: mediaelements.StoreCancelType(), ItemID: "cancel-before-resolve",
		CancellationScope: "resolve-never",
		Payload:           mediaelements.StoreCancel{RequestID: "resolve-never", Reason: "no longer needed"},
	})
	if outcome := receive(t, resolverOutcomes).Payload.(mediaelements.ResolverOutcome); outcome.Kind != mediaelements.ResultSucceeded || outcome.Code != "cancel_recorded" {
		t.Fatalf("resolver pre-cancel outcome = %+v", outcome)
	}
	send(t, ingress(t, mounted, "resolve"), element.Envelope{
		Type: mediaelements.ResolveRequestType(), ItemID: "resolve-never",
		Payload: mediaelements.ResolveRequest{Handle: mediaelements.AttachmentHandle{
			Handle: "unneeded", Capability: "unneeded", Scope: "session",
			MIMEType: "application/octet-stream", SHA256: "sha256:" + strings.Repeat("0", 64),
			Bytes: 1, SourceRevision: 1,
		}},
	})
	if result := receive(t, resolvedOutput).Payload.(mediaelements.ResolvedAttachment); result.Kind != mediaelements.ResultCanceled || result.Code != "canceled" {
		t.Fatalf("pre-canceled resolution = %+v", result)
	}
	if outcome := receive(t, resolverOutcomes).Payload.(mediaelements.ResolverOutcome); outcome.Kind != mediaelements.ResultCanceled {
		t.Fatalf("pre-canceled resolver terminal = %+v", outcome)
	}

	textInput := ingress(t, mounted, "text")
	observations := egress(t, mounted, "observations")
	send(t, textInput, element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "typing-1", SourceID: "typing",
		Payload: ingresselements.UserText{ContentID: "typing-1", StreamID: "typing", Text: "hel", StableText: "he", Revision: 1},
	})
	first := receive(t, observations).Payload.(perception.Observation)
	_ = receive(t, ingressOutcomes)
	send(t, textInput, element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "typing-2", SourceID: "typing",
		Payload: ingresselements.UserText{ContentID: "typing-2", StreamID: "typing", Text: "hello", Revision: 2, Supersedes: 1, Final: true},
	})
	second := receive(t, observations).Payload.(perception.Observation)
	_ = receive(t, ingressOutcomes)
	if !first.Provisional || first.Final || second.Supersedes != 1 || !second.Final {
		t.Fatalf("revision framing = %+v -> %+v", first, second)
	}
	send(t, textInput, element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "typing-after-final", SourceID: "typing",
		Payload: ingresselements.UserText{ContentID: "typing-3", StreamID: "typing", Text: "hello!", Revision: 3, Supersedes: 2, Final: true},
	})
	if outcome := receive(t, ingressOutcomes).Payload.(ingresselements.Outcome); outcome.Kind != ingresselements.OutcomeRefused || outcome.Code != "invalid_revision" {
		t.Fatalf("post-final revision outcome = %+v", outcome)
	}
}

func TestRetentionBoundsExpireHandlesAndValuesAreStrict(t *testing.T) {
	values := referenceValues(t)
	values.Nodes["retention"] = json.RawMessage(`{"scope":"session","max_items":1,"max_bytes":1024,"max_item_bytes":512}`)
	mounted, done, cancel := mountReference(t, values)
	defer stopReference(t, mounted, done, cancel)
	metrics := egress(t, mounted, "retention_metrics")
	_ = receive(t, metrics)
	handles := egress(t, mounted, "handles")
	observations := egress(t, mounted, "observations")
	ingressOutcomes := egress(t, mounted, "ingress_outcome")
	retentionOutcomes := egress(t, mounted, "retention_outcome")
	imageInput := ingress(t, mounted, "image")

	var retained []mediaelements.AttachmentHandle
	for index := 1; index <= 2; index++ {
		id := string(rune('0' + index))
		send(t, imageInput, element.Envelope{
			Type: ingresselements.UserImageType(), ItemID: "bounded-image-" + id,
			SourceID: "bounded-stream-" + id,
			Payload: ingresselements.UserImage{
				ContentID: "bounded-content-" + id, StreamID: "bounded-stream-" + id,
				MIMEType: "image/png", Content: []byte("image-" + id), Width: 1, Height: 1,
				SourceRevision: 1,
			},
		})
		retained = append(retained, receive(t, handles).Payload.(mediaelements.AttachmentHandle))
		_ = receive(t, observations)
		_ = receive(t, ingressOutcomes)
		current := receive(t, metrics).Payload.(mediaelements.RetentionMetrics)
		_ = receive(t, retentionOutcomes)
		if current.LiveItems != 1 {
			t.Fatalf("retention item bound metrics = %+v", current)
		}
	}

	send(t, ingress(t, mounted, "resolve"), element.Envelope{
		Type: mediaelements.ResolveRequestType(), ItemID: "resolve-evicted",
		Payload: mediaelements.ResolveRequest{Handle: retained[0]},
	})
	result := receive(t, egress(t, mounted, "resolved")).Payload.(mediaelements.ResolvedAttachment)
	_ = receive(t, egress(t, mounted, "resolver_outcome"))
	missMetrics := receive(t, metrics).Payload.(mediaelements.RetentionMetrics)
	_ = receive(t, retentionOutcomes)
	if result.Kind != mediaelements.ResultExpired || result.Code != "not_retained" ||
		missMetrics.Misses != 1 || missMetrics.Evicted != 1 {
		t.Fatalf("evicted resolution = %+v metrics=%+v", result, missMetrics)
	}

	invalid := referenceValues(t)
	invalid.Nodes["content"] = json.RawMessage(`{"retention_scope":"session","hidden_cadence_ms":1}`)
	if _, _, _, err := tryMountReference(invalid); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown ingress value was accepted: %v", err)
	}
}

func TestResolvedBytesCannotTypeConnectToAuthorityBearingObservation(t *testing.T) {
	const invalid = `graph invalid_authority_promotion {
    media.ResolveAttachment :: resolver;
    state.ObservationCommit :: commit;
    resolver.resolved -> commit.observations;
    input resolve = resolver.resolve;
    input fetched = resolver.fetched;
    input resolver_cancel = resolver.cancel;
    output fetch = resolver.fetch;
    output store_cancel = resolver.store_cancel;
    output return_lease = resolver.return_lease;
    output resolver_outcome = resolver.outcome;
    input committed = commit.committed;
    input rejected = commit.rejected;
    output append = commit.append;
    output commit_outcome = commit.outcome;
}`
	parsed, err := syntax.Parse("invalid.ortg", []byte(invalid))
	if err != nil {
		t.Fatal(err)
	}
	catalog := resolve.NewCatalog()
	if err := mediaelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	if err := stateelements.RegisterDescriptors(catalog); err != nil {
		t.Fatal(err)
	}
	_, err = graphcompiler.Compile(parsed, graphcompiler.Options{Catalog: catalog, ResolutionMode: resolve.Update})
	if err == nil || !strings.Contains(err.Error(), "E_TYPE_MISMATCH") {
		t.Fatalf("resolved bytes connected to observations: %v", err)
	}
}

func mountReference(t *testing.T, values graphvalues.Document) (*graphruntime.Mounted, <-chan error, context.CancelFunc) {
	t.Helper()
	mounted, done, cancel, err := tryMountReference(values)
	if err != nil {
		t.Fatal(err)
	}
	return mounted, done, cancel
}

func tryMountReference(values graphvalues.Document) (*graphruntime.Mounted, <-chan error, context.CancelFunc, error) {
	directory := filepath.Join("..", "..", "graphs", "components", "multimodal-content")
	topology, err := os.ReadFile(filepath.Join(directory, "agent.ortg"))
	if err != nil {
		return nil, nil, nil, err
	}
	parsed, err := syntax.Parse("agent.ortg", topology)
	if err != nil {
		return nil, nil, nil, err
	}
	catalog := resolve.NewCatalog()
	if err := ingresselements.RegisterDescriptors(catalog); err != nil {
		return nil, nil, nil, err
	}
	if err := mediaelements.RegisterDescriptors(catalog); err != nil {
		return nil, nil, nil, err
	}
	lockPayload, err := os.ReadFile(filepath.Join(directory, "openrealtime.lock"))
	if err != nil {
		return nil, nil, nil, err
	}
	lock, err := resolve.ParseLock(lockPayload)
	if err != nil {
		return nil, nil, nil, err
	}
	compiled, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	bound, err := graphvalues.Bind(compiled.Graph, values)
	if err != nil {
		return nil, nil, nil, err
	}
	registry := graphruntime.NewRegistry()
	if err := ingresselements.RegisterFactories(registry); err != nil {
		return nil, nil, nil, err
	}
	if err := mediaelements.RegisterFactories(registry); err != nil {
		return nil, nil, nil, err
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: bound.Graph, Registry: registry, Values: bound.Values,
		Now: func() uint64 { return 999 },
	})
	if err != nil {
		return nil, nil, nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	return mounted, done, cancel, nil
}

func referenceValues(t *testing.T) graphvalues.Document {
	t.Helper()
	path := filepath.Join("..", "..", "graphs", "components", "multimodal-content", "agent.values.yaml")
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	document, err := graphvalues.ParseYAML(path, payload)
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func stopReference(t *testing.T, mounted *graphruntime.Mounted, done <-chan error, cancel context.CancelFunc) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("reference graph shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("reference graph did not stop")
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Errorf("reference graph close: %v", err)
	}
}

func ingress(t *testing.T, mounted *graphruntime.Mounted, name string) element.OutputPort {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func egress(t *testing.T, mounted *graphruntime.Mounted, name string) element.InputPort {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func send(t *testing.T, output element.OutputPort, envelope element.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := output.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
}

func receive(t *testing.T, input element.InputPort) element.Envelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	envelope, err := input.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope
}
