package graphs_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalidate "github.com/bojieli/OpenRealtime/graph/validate"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type textFileCognitionReference struct {
	graph ir.Graph
	bound graphvalues.Bound
}

func TestTextFileCognitionReferenceIsLockedCompleteAndAudioFree(t *testing.T) {
	reference := loadTextFileCognitionReference(t)
	if findings := graphvalidate.Check(reference.bound.Graph, graphvalidate.RealtimeAgent); len(findings) != 0 {
		t.Fatalf("text/file cognition warnings-as-errors findings = %+v", findings)
	}

	wantNodes := map[string]string{
		"content":                   "ingress.UserContent",
		"retention":                 "media.RetainedMedia",
		"resolver":                  "media.ResolveAttachment",
		"observation_commit":        "state.ObservationCommit",
		"trajectory":                "state.TrajectoryStore",
		"activation":                "policy.GenerateOnObservation",
		"model":                     "cognition.TextModel",
		"model_result_commit":       "interaction.ModelResultCommit",
		"trajectory_append_mux":     "flow.Mux",
		"content_observation_copy":  "flow.Tee",
		"trajectory_snapshot_copy":  "flow.Tee",
		"trajectory_committed_copy": "flow.Tee",
		"trajectory_rejected_copy":  "flow.Tee",
		"observation_outcome_copy":  "flow.Tee",
		"model_result_copy":         "flow.Tee",
	}
	if len(reference.graph.Nodes) != len(wantNodes) {
		t.Fatalf("text/file cognition nodes = %d, want %d", len(reference.graph.Nodes), len(wantNodes))
	}
	for id, want := range wantNodes {
		if got := textFileNodeElement(reference.graph, id); got != want {
			t.Errorf("node %s = %q, want %q", id, got, want)
		}
	}

	for _, edge := range []struct{ fromNode, fromPort, toNode, toPort string }{
		{"content", "retain", "retention", "retain"},
		{"retention", "retained", "content", "retained"},
		{"resolver", "fetch", "retention", "fetch"},
		{"retention", "fetched", "resolver", "fetched"},
		{"resolver", "return_lease", "retention", "return_lease"},
		{"content_observation_copy", "out", "observation_commit", "observations"},
		{"observation_commit", "append", "trajectory_append_mux", "in"},
		{"model_result_commit", "append", "trajectory_append_mux", "in"},
		{"trajectory_append_mux", "out", "trajectory", "append"},
		{"trajectory_snapshot_copy", "out", "model", "context"},
		{"trajectory_committed_copy", "out", "observation_commit", "committed"},
		{"trajectory_committed_copy", "out", "model_result_commit", "committed"},
		{"trajectory_rejected_copy", "out", "observation_commit", "rejected"},
		{"trajectory_rejected_copy", "out", "model_result_commit", "rejected"},
		{"observation_outcome_copy", "out", "activation", "committed"},
		{"activation", "trigger", "model", "trigger"},
		{"model_result_copy", "out", "model_result_commit", "result"},
	} {
		if !textFileHasEdge(reference.graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
			t.Errorf("missing text/file cognition edge %s.%s -> %s.%s",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
		}
	}

	wantBoundaries := map[string]ir.BoundaryDirection{
		"text": ir.InputBoundary, "image": ir.InputBoundary,
		"file": ir.InputBoundary, "attachment": ir.InputBoundary,
		"content_cancel": ir.InputBoundary, "media_resolve": ir.InputBoundary,
		"media_resolve_cancel": ir.InputBoundary, "media_return_lease": ir.InputBoundary,
		"activation_cancel": ir.InputBoundary, "model_cancel": ir.InputBoundary,
		"observations": ir.OutputBoundary, "handles": ir.OutputBoundary,
		"ingress_outcome": ir.OutputBoundary, "resolved": ir.OutputBoundary,
		"resolver_outcome": ir.OutputBoundary, "released": ir.OutputBoundary,
		"lease_returned": ir.OutputBoundary, "retention_outcome": ir.OutputBoundary,
		"retention_metrics": ir.OutputBoundary, "trajectory_snapshot": ir.OutputBoundary,
		"trajectory_commit": ir.OutputBoundary, "trajectory_rejection": ir.OutputBoundary,
		"observation_outcome": ir.OutputBoundary, "activation_authority": ir.OutputBoundary,
		"activation_state": ir.OutputBoundary, "activation_outcome": ir.OutputBoundary,
		"prepared_text": ir.OutputBoundary, "model_result": ir.OutputBoundary,
		"model_proposals": ir.OutputBoundary, "model_outcome": ir.OutputBoundary,
		"model_resolution": ir.OutputBoundary, "model_commit_outcome": ir.OutputBoundary,
	}
	if len(reference.graph.Boundaries) != len(wantBoundaries) {
		t.Fatalf("text/file cognition boundaries = %d, want %d",
			len(reference.graph.Boundaries), len(wantBoundaries))
	}
	for _, boundary := range reference.graph.Boundaries {
		if want, found := wantBoundaries[boundary.Name]; !found || boundary.Direction != want {
			t.Errorf("unexpected boundary %+v", boundary)
		}
		if strings.Contains(strings.ToLower(boundary.Name), "audio") ||
			strings.Contains(strings.ToLower(boundary.Type.String()), "audio") {
			t.Errorf("audio-free text/file graph exports audio boundary %+v", boundary)
		}
	}
	for _, node := range reference.graph.Nodes {
		for _, effect := range node.Effects {
			if effect.External {
				t.Errorf("audio-free cognition unexpectedly grants external effect at %s: %+v", node.ID, effect)
			}
		}
	}
}

func TestTextFileCognitionReferenceExecutesTextAndRetainedFileTurns(t *testing.T) {
	reference := loadTextFileCognitionReference(t)
	fileBody := []byte("launch window: 04:30 UTC\nowner: Ada\n")
	descriptor := textFileReferenceDescriptor()
	provider := &textFileReferenceProvider{descriptor: descriptor, expectedFile: fileBody}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("deployment.text-file", descriptor,
		func() (continuation.Provider, error) { return provider, nil }); err != nil {
		t.Fatal(err)
	}

	store := trajectory.NewStore()
	bridge := &textFileReferenceMediaBridge{}
	services := graphruntime.NewServiceSet()
	for name, service := range map[string]any{
		cognitionelements.ProviderRegistryService: providers,
		cognitionelements.MediaResolverService:    continuation.MediaResolver(bridge.Resolve),
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{
			Store: store,
		},
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	var now atomic.Uint64
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: reference.bound.Graph, Registry: registry, Services: services,
		Values: reference.bound.Values, Now: func() uint64 { return now.Add(1) },
		ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.mounted = mounted
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	defer func() {
		cancel()
		select {
		case runErr := <-done:
			if runErr != nil && !errors.Is(runErr, context.Canceled) {
				t.Errorf("text/file cognition graph stopped with %v", runErr)
			}
		case <-time.After(2 * time.Second):
			t.Error("text/file cognition graph did not stop")
		}
	}()

	textFileWaitReady(t, mounted, len(reference.bound.Graph.Nodes))
	resolutionEnvelope := textFileReceive(t, mounted, "model_resolution")
	resolution, ok := resolutionEnvelope.Payload.(cognitionelements.ProviderResolution)
	if !ok || resolution.Reference != "deployment.text-file" ||
		resolution.Descriptor != descriptor ||
		resolution.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		t.Fatalf("text/file model resolution = %#v", resolutionEnvelope.Payload)
	}
	if seed := textFileReceive(t, mounted, "trajectory_snapshot").Payload.(trajectory.Snapshot); seed.Version != 0 {
		t.Fatalf("text/file trajectory seed = %+v", seed)
	}
	textFileDrain(t, runContext, mounted, "activation_state", "trajectory_snapshot", "retention_metrics")

	textResponse := "Text received; send the retained report."
	textFileSend(t, mounted, "text", element.Envelope{
		Type: ingresselements.UserTextType(), ItemID: "text-input-1",
		SessionID: "text-file-session", SourceID: "participant",
		Payload: ingresselements.UserText{
			ContentID: "message-1", StreamID: "messages",
			Text: "Read the report I send next.", StableText: "Read the report I send next.",
			Revision: 1, Final: true,
		},
	})
	textFileAwaitTurn(t, mounted, "text", textResponse)

	fileResponse := "The retained report says Ada owns the 04:30 UTC launch window."
	textFileSend(t, mounted, "file", element.Envelope{
		Type: ingresselements.UserFileType(), ItemID: "file-input-1",
		SessionID: "text-file-session", SourceID: "participant",
		Payload: ingresselements.UserFile{
			ContentID: "report-1", StreamID: "documents", Name: "launch.txt",
			Caption: "launch report", MIMEType: "text/plain", Content: fileBody,
			SourceRevision: 1, CapturedNS: 100,
		},
	})
	textFileAwaitTurn(t, mounted, "file", fileResponse)

	if calls := provider.calls.Load(); calls != 2 {
		t.Fatalf("text/file provider calls = %d, want 2", calls)
	}
	if resolutions := bridge.resolutions.Load(); resolutions != 1 {
		t.Fatalf("retained-file resolutions = %d, want 1", resolutions)
	}

	snapshot := store.Snapshot()
	var observations, assistants int
	var retainedReference bool
	for _, item := range snapshot.Items {
		switch item.Kind {
		case trajectory.KindObservation:
			observations++
			if item.Observation != nil && len(item.Observation.Media) == 1 {
				reference := item.Observation.Media[0]
				retainedReference = reference.Handle != "" && reference.MIMEType == "text/plain" &&
					reference.Bytes == len(fileBody)
			}
		case trajectory.KindAssistant:
			assistants++
			if item.Producer.SpeechAuthority != string(continuation.SpeechAuthoritySilent) {
				t.Errorf("assistant item %s speech authority = %q, want silent",
					item.ID, item.Producer.SpeechAuthority)
			}
		case trajectory.KindToolProposal, trajectory.KindToolCall, trajectory.KindToolResult:
			t.Errorf("text/file cognition produced action item %+v", item)
		}
	}
	if observations != 2 || assistants != 2 || !retainedReference {
		t.Fatalf("canonical text/file trajectory observations=%d assistants=%d retained_reference=%v: %+v",
			observations, assistants, retainedReference, snapshot.Items)
	}
}

func loadTextFileCognitionReference(t *testing.T) textFileCognitionReference {
	t.Helper()
	directory := filepath.Join("components", "text-file-cognition")
	parsed, err := syntax.Parse("agent.ortg", readFile(t, filepath.Join(directory, "agent.ortg")))
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := elements.Catalog()
	if err != nil {
		t.Fatal(err)
	}
	updated, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, ResolutionMode: resolve.Update,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := resolve.ParseLock(readFile(t, filepath.Join(directory, "openrealtime.lock")))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Lock.Equal(lock) {
		t.Fatal("text/file cognition resolution lock is stale")
	}
	locked, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	values, err := graphvalues.ParseYAML("agent.values.yaml",
		readFile(t, filepath.Join(directory, "agent.values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(locked.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	return textFileCognitionReference{graph: locked.Graph, bound: bound}
}

func textFileWaitReady(t *testing.T, mounted *graphruntime.Mounted, nodes int) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for {
		live := mounted.Live()
		ready := len(live.Nodes) == nodes
		for _, node := range live.Nodes {
			ready = ready && node.Resolution != nil &&
				string(node.Resolution.RuntimeEvidence) == "live" &&
				string(node.Resolution.CapabilitiesEvidence) == "live"
		}
		if ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("text/file cognition graph did not become ready: %+v", live.Nodes)
		}
		time.Sleep(time.Millisecond)
	}
}

func textFileSend(t *testing.T, mounted *graphruntime.Mounted, name string, envelope element.Envelope) {
	t.Helper()
	port, err := mounted.Ingress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	delivery, err := port.Broadcast(ctx, envelope)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		t.Fatalf("send %s delivered %d and dropped %d lanes", name, delivery.Delivered, delivery.Dropped)
	}
}

func textFileDrain(t *testing.T, ctx context.Context, mounted *graphruntime.Mounted, names ...string) {
	t.Helper()
	for _, name := range names {
		port, err := mounted.Egress(name)
		if err != nil {
			t.Fatal(err)
		}
		go func(input element.InputPort) {
			for {
				if _, receiveErr := input.Receive(ctx); receiveErr != nil {
					return
				}
			}
		}(port)
	}
}

func textFileAwaitTurn(t *testing.T, mounted *graphruntime.Mounted, operation, response string) {
	t.Helper()
	ingressOutcome := textFileReceive(t, mounted, "ingress_outcome").Payload.(ingresselements.Outcome)
	if ingressOutcome.Kind != ingresselements.OutcomeSucceeded || ingressOutcome.Operation != operation {
		t.Fatalf("%s ingress outcome = %+v", operation, ingressOutcome)
	}
	observation := textFileReceive(t, mounted, "observation_outcome").Payload.(stateelements.ObservationCommitOutcome)
	if observation.Kind != stateelements.ObservationCommitted {
		t.Fatalf("%s observation commit = %+v", operation, observation)
	}
	emitted := false
	for range 4 {
		outcome := textFileReceive(t, mounted, "activation_outcome").Payload.(policyelements.GenerationOutcome)
		if outcome.Kind == policyelements.GenerationEmitted {
			emitted = true
			break
		}
		if outcome.Kind != policyelements.GenerationRefused || outcome.Code != "missing_session" {
			t.Fatalf("%s activation outcome = %+v", operation, outcome)
		}
	}
	if !emitted {
		t.Fatalf("%s activation did not emit", operation)
	}

	wantBoundaries := []cognitionelements.TextBoundary{
		cognitionelements.TextBegin, cognitionelements.TextChunk, cognitionelements.TextEnd,
	}
	for index, want := range wantBoundaries {
		delta := textFileReceive(t, mounted, "prepared_text").Payload.(cognitionelements.PreparedTextDelta)
		if delta.Boundary != want || (index == 1 && delta.Text != response) ||
			(index != 1 && delta.Text != "") {
			t.Fatalf("%s prepared text %d = %+v, want boundary %s", operation, index, delta, want)
		}
	}
	result := textFileReceive(t, mounted, "model_result").Payload.(cognitionelements.Result)
	if result.AssistantText != response || result.Interrupted || len(result.ToolProposals) != 0 ||
		result.Descriptor.EffectiveSpeechAuthority() != continuation.SpeechAuthoritySilent {
		t.Fatalf("%s prepared model result = %+v", operation, result)
	}
	modelOutcome := textFileReceive(t, mounted, "model_outcome").Payload.(cognitionelements.Outcome)
	if modelOutcome.Kind != cognitionelements.OutcomeSucceeded {
		t.Fatalf("%s model outcome = %+v", operation, modelOutcome)
	}
	for range 4 {
		commit := textFileReceive(t, mounted, "model_commit_outcome").Payload.(interactionelements.ModelCommitOutcome)
		if commit.Kind == interactionelements.ModelCommitted && commit.StoreVersion != 0 {
			return
		}
		if commit.Kind != interactionelements.ModelIgnored || commit.Code != "unknown_commit_reply" {
			t.Fatalf("%s model commit = %+v", operation, commit)
		}
	}
	t.Fatalf("%s model result did not commit", operation)
}

func textFileReceive(t *testing.T, mounted *graphruntime.Mounted, name string) element.Envelope {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatalf("receive %s: %v (live=%+v)", name, err, mounted.Live().Nodes)
	}
	return envelope
}

type textFileReferenceProvider struct {
	descriptor   continuation.Descriptor
	expectedFile []byte
	calls        atomic.Int32
}

func (provider *textFileReferenceProvider) Descriptor() continuation.Descriptor {
	return provider.descriptor
}

func (provider *textFileReferenceProvider) Continue(
	ctx context.Context, request continuation.Request, emit continuation.Emit,
) (continuation.Completion, error) {
	if request.Descriptor != provider.descriptor ||
		request.Invocation.Instruction != "Answer from committed participant text and resolve retained files when relevant." {
		return continuation.Completion{}, errors.New("text/file provider received a drifted descriptor or invocation")
	}
	select {
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	default:
	}

	call := provider.calls.Add(1)
	response := "Text received; send the retained report."
	handles := continuation.LatestMediaHandles(request.Trajectory.Items)
	switch call {
	case 1:
		if len(handles) != 0 {
			return continuation.Completion{}, fmt.Errorf("text turn exposed media handles %v", handles)
		}
	case 2:
		if len(handles) != 1 || request.Media == nil {
			return continuation.Completion{}, fmt.Errorf("file turn media handles=%v resolver_nil=%t",
				handles, request.Media == nil)
		}
		var handle string
		for candidate := range handles {
			handle = candidate
		}
		media, err := request.Media(handle)
		if err != nil {
			return continuation.Completion{}, fmt.Errorf("resolve retained reference %q: %w", handle, err)
		}
		if media.MIMEType != "text/plain" || !bytes.Equal(media.Bytes, provider.expectedFile) {
			return continuation.Completion{}, fmt.Errorf("resolved retained file MIME=%q bytes=%q",
				media.MIMEType, media.Bytes)
		}
		response = "The retained report says Ada owns the 04:30 UTC launch window."
	default:
		return continuation.Completion{}, fmt.Errorf("unexpected text/file provider call %d", call)
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: response}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}

func textFileReferenceDescriptor() continuation.Descriptor {
	return continuation.Descriptor{
		Provider: "test", Model: "text-file-reference", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityNone,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
}

type textFileReferenceMediaBridge struct {
	mounted     *graphruntime.Mounted
	sequence    atomic.Uint64
	resolutions atomic.Int32
}

func (bridge *textFileReferenceMediaBridge) Resolve(handleID string) (continuation.Media, error) {
	if bridge == nil || bridge.mounted == nil || strings.TrimSpace(handleID) == "" {
		return continuation.Media{}, errors.New("text/file media bridge is not mounted or received an empty handle")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	handles, err := bridge.mounted.Egress("handles")
	if err != nil {
		return continuation.Media{}, err
	}
	handleEnvelope, err := handles.Receive(ctx)
	if err != nil {
		return continuation.Media{}, fmt.Errorf("receive retained handle: %w", err)
	}
	handle, ok := handleEnvelope.Payload.(mediaelements.AttachmentHandle)
	if !ok || handle.Handle != handleID || handle.Capability == "" || handle.Bytes < 1 {
		return continuation.Media{}, fmt.Errorf("retained handle for %q = %#v", handleID, handleEnvelope.Payload)
	}

	resolvePort, err := bridge.mounted.Ingress("media_resolve")
	if err != nil {
		return continuation.Media{}, err
	}
	resolveID := fmt.Sprintf("text-file-resolve-%d", bridge.sequence.Add(1))
	delivery, err := resolvePort.Broadcast(ctx, element.Envelope{
		Type: resolvePort.Type(), ItemID: resolveID, SessionID: handleEnvelope.SessionID,
		SourceID: handle.Source, Sequence: bridge.sequence.Load(), TraceID: resolveID,
		CausalParents: []string{handleEnvelope.ItemID},
		Payload: mediaelements.ResolveRequest{
			Handle: handle, AcceptMIMETypes: []string{handle.MIMEType},
			MaxBytes: handle.Bytes, ExpectedSourceRevision: handle.SourceRevision,
		},
	})
	if err != nil {
		return continuation.Media{}, fmt.Errorf("send retained-file resolution: %w", err)
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return continuation.Media{}, fmt.Errorf("retained-file resolution delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	resolvedPort, err := bridge.mounted.Egress("resolved")
	if err != nil {
		return continuation.Media{}, err
	}
	resolvedEnvelope, err := resolvedPort.Receive(ctx)
	if err != nil {
		return continuation.Media{}, fmt.Errorf("receive retained-file resolution: %w", err)
	}
	resolved, ok := resolvedEnvelope.Payload.(mediaelements.ResolvedAttachment)
	if !ok || resolved.Kind != mediaelements.ResultSucceeded || resolved.Handle != handle ||
		resolved.Lease.LeaseID == "" || resolvedEnvelope.SessionID != handleEnvelope.SessionID ||
		len(resolvedEnvelope.CausalParents) != 1 || resolvedEnvelope.CausalParents[0] != resolveID {
		return continuation.Media{}, fmt.Errorf("retained-file resolution = %#v envelope %+v",
			resolvedEnvelope.Payload, resolvedEnvelope)
	}
	reader, err := resolved.Lease.Open()
	if err != nil {
		return continuation.Media{}, fmt.Errorf("open retained-file lease: %w", err)
	}
	body, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return continuation.Media{}, fmt.Errorf("read retained-file lease: %w", err)
	}

	returnPort, err := bridge.mounted.Ingress("media_return_lease")
	if err != nil {
		return continuation.Media{}, err
	}
	returnID := fmt.Sprintf("text-file-return-%d", bridge.sequence.Add(1))
	delivery, err = returnPort.Broadcast(ctx, element.Envelope{
		Type: returnPort.Type(), ItemID: returnID, SessionID: handleEnvelope.SessionID,
		RunID: resolved.Lease.LeaseID, Sequence: bridge.sequence.Load(), TraceID: returnID,
		CausalParents: []string{resolvedEnvelope.ItemID},
		Payload: mediaelements.ReturnLeaseRequest{
			Lease: resolved.Lease, Reason: "text/file reference provider consumed retained bytes",
		},
	})
	if err != nil {
		return continuation.Media{}, fmt.Errorf("return retained-file lease: %w", err)
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return continuation.Media{}, fmt.Errorf("lease return delivered %d and dropped %d lanes",
			delivery.Delivered, delivery.Dropped)
	}
	returnedPort, err := bridge.mounted.Egress("lease_returned")
	if err != nil {
		return continuation.Media{}, err
	}
	returnedEnvelope, err := returnedPort.Receive(ctx)
	if err != nil {
		return continuation.Media{}, fmt.Errorf("receive retained-file lease return: %w", err)
	}
	returned, ok := returnedEnvelope.Payload.(mediaelements.ReturnLeaseResult)
	if !ok || returned.Kind != mediaelements.ResultSucceeded ||
		returned.LeaseID != resolved.Lease.LeaseID || returned.Handle != handle.Handle ||
		returnedEnvelope.SessionID != handleEnvelope.SessionID ||
		len(returnedEnvelope.CausalParents) != 1 || returnedEnvelope.CausalParents[0] != returnID {
		return continuation.Media{}, fmt.Errorf("retained-file lease return = %#v envelope %+v",
			returnedEnvelope.Payload, returnedEnvelope)
	}
	bridge.resolutions.Add(1)
	return continuation.Media{MIMEType: handle.MIMEType, Bytes: body}, nil
}

func textFileNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func textFileHasEdge(graph ir.Graph, fromNode, fromPort, toNode, toPort string) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}
