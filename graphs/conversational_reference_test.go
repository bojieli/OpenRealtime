package graphs_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	v1 "github.com/bojieli/OpenRealtime/api/v1"
	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/ir"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type conversationalReference struct {
	name   string
	graph  ir.Graph
	values graphvalues.Document
	bound  graphvalues.Bound
}

func TestConversationalReferenceFamilyIsLockedTypedAndComposable(t *testing.T) {
	names := []string{
		"conversational-fast-only",
		"conversational-slow-only",
		"conversational-both",
	}
	references := make(map[string]conversationalReference, len(names))
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			reference := loadConversationalReference(t, name)
			assertConversationalControlBoundaries(t, reference)
			assertNoModelToModelEdges(t, reference.graph)
			exerciseConversationalProviderBoundary(t, reference)
			references[name] = reference
		})
	}
	if len(references) != len(names) {
		t.Fatalf("loaded %d conversational references, want %d", len(references), len(names))
	}

	assertSharedConversationalBackbone(t, references)
	assertConversationalRoutingVariants(t, references)
}

func loadConversationalReference(t *testing.T, name string) conversationalReference {
	t.Helper()
	directory := filepath.Join("components", name)
	topology := conversationalRead(t, filepath.Join(directory, "agent.ortg"))
	parsed, err := syntax.Parse("agent.ortg", topology)
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
	lock, err := resolve.ParseLock(conversationalRead(t, filepath.Join(directory, "openrealtime.lock")))
	if err != nil {
		t.Fatal(err)
	}
	if !updated.Lock.Equal(lock) {
		got, _ := updated.Lock.Marshal()
		want, _ := lock.Marshal()
		t.Fatalf("committed lock is stale\ngenerated:\n%scommitted:\n%s", got, want)
	}
	locked, err := graphcompiler.Compile(parsed, graphcompiler.Options{
		Catalog: catalog, Lock: lock, ResolutionMode: resolve.Locked,
	})
	if err != nil {
		t.Fatal(err)
	}
	if locked.Graph.Fingerprint != updated.Graph.Fingerprint {
		t.Fatalf("locked fingerprint %s differs from generated %s",
			locked.Graph.Fingerprint, updated.Graph.Fingerprint)
	}
	if err := locked.Graph.Validate(); err != nil {
		t.Fatal(err)
	}

	values, err := graphvalues.ParseYAML("agent.values.yaml",
		conversationalRead(t, filepath.Join(directory, "agent.values.yaml")))
	if err != nil {
		t.Fatal(err)
	}
	bound, err := graphvalues.Bind(locked.Graph, values)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Graph.Fingerprint == locked.Graph.Fingerprint {
		t.Fatal("values binding did not change executable graph identity")
	}
	if err := bound.Graph.Validate(); err != nil {
		t.Fatal(err)
	}
	return conversationalReference{name: name, graph: locked.Graph, values: values, bound: bound}
}

func assertConversationalControlBoundaries(t *testing.T, reference conversationalReference) {
	t.Helper()
	want := map[string]string{
		"endpoint_tick":          "Trigger<timing.Tick>",
		"audio_cancel":           "Interrupt<audio.StreamID>",
		"fast_activation_cancel": "Interrupt<policy.GenerationAddress>",
		"slow_activation_cancel": "Interrupt<policy.GenerationAddress>",
		"fast_model_cancel":      "Interrupt<flow.RunID>",
		"slow_model_cancel":      "Interrupt<flow.RunID>",
		"segmentation_timeout":   "Event<interaction.Timeout>",
		"segmentation_cancel":    "Interrupt<flow.RunID>",
		"tts_cancel":             "Interrupt<speech.UtteranceID>",
		"playback_cancel":        "Interrupt<speech.UtteranceID>",
	}
	if reference.name == "conversational-both" {
		want["speech_selection"] = "Trigger<interaction.SpeechSelection>"
		want["arbitration_timeout"] = "Event<interaction.Timeout>"
		want["arbitration_cancel"] = "Interrupt<flow.RunID>"
	}
	boundaries := make(map[string]ir.Boundary, len(reference.graph.Boundaries))
	for _, boundary := range reference.graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	for name, typeName := range want {
		boundary, found := boundaries[name]
		if !found {
			t.Errorf("missing explicit control boundary %s", name)
			continue
		}
		if boundary.Direction != ir.InputBoundary || boundary.Type.String() != typeName {
			t.Errorf("boundary %s = %s %s, want input %s",
				name, boundary.Direction, boundary.Type.String(), typeName)
		}
	}
	for _, connector := range []struct {
		id      string
		element string
	}{
		{id: "append_mux", element: "flow.Mux"},
		{id: "snapshot_copy", element: "flow.Tee"},
		{id: "observation_outcome_copy", element: "flow.Tee"},
		{id: "speech_cancel_copy", element: "flow.Tee"},
	} {
		if got := conversationalNodeElement(reference.graph, connector.id); got != connector.element {
			t.Errorf("connector %s = %q, want %q", connector.id, got, connector.element)
		}
	}
}

func assertNoModelToModelEdges(t *testing.T, graph ir.Graph) {
	t.Helper()
	modelNodes := make(map[string]bool)
	for _, node := range graph.Nodes {
		if node.Element.Name == "cognition.TextModel" {
			modelNodes[node.ID] = true
		}
	}
	if len(modelNodes) != 2 || !modelNodes["fast_model"] || !modelNodes["slow_model"] {
		t.Fatalf("cognition model instances = %v, want independent fast_model and slow_model", modelNodes)
	}
	for _, edge := range graph.Edges {
		if modelNodes[edge.From.Node] && modelNodes[edge.To.Node] {
			t.Errorf("forbidden model-to-model edge %s", edge.ID)
		}
		if strings.HasPrefix(edge.From.Node, "fast_") && strings.HasPrefix(edge.To.Node, "slow_") ||
			strings.HasPrefix(edge.From.Node, "slow_") && strings.HasPrefix(edge.To.Node, "fast_") {
			t.Errorf("implicit cross-role handoff edge %s (%s -> %s)",
				edge.ID, edge.From.String(), edge.To.String())
		}
	}
	for _, edge := range []struct {
		fromNode string
		fromPort string
		toNode   string
		toPort   string
	}{
		{"fast_model", "result", "fast_result_copy", "in"},
		{"slow_model", "result", "slow_result_copy", "in"},
		{"fast_result_copy", "out", "fast_result_commit", "result"},
		{"slow_result_copy", "out", "slow_result_commit", "result"},
		{"append_mux", "out", "trajectory", "append"},
		{"snapshot_copy", "out", "fast_model", "context"},
		{"snapshot_copy", "out", "slow_model", "context"},
	} {
		if !conversationalHasEdge(graph, edge.fromNode, edge.fromPort, edge.toNode, edge.toPort) {
			t.Errorf("missing explicit state/context edge %s.%s -> %s.%s",
				edge.fromNode, edge.fromPort, edge.toNode, edge.toPort)
		}
	}
}

func assertSharedConversationalBackbone(
	t *testing.T, references map[string]conversationalReference,
) {
	t.Helper()
	fast := references["conversational-fast-only"]
	for _, name := range []string{"conversational-slow-only", "conversational-both"} {
		candidate := references[name]
		if got, want := conversationalCoreNodes(candidate.graph), conversationalCoreNodes(fast.graph); !equalStrings(got, want) {
			t.Errorf("%s core nodes drifted\ngot  %v\nwant %v", name, got, want)
		}
		if got, want := conversationalCoreEdges(candidate.graph), conversationalCoreEdges(fast.graph); !equalStrings(got, want) {
			t.Errorf("%s core edges drifted\ngot  %v\nwant %v", name, got, want)
		}
		for node, want := range fast.values.Nodes {
			if node == "arbiter" {
				continue
			}
			got, found := candidate.values.Nodes[node]
			if !found || !bytes.Equal(got, want) {
				t.Errorf("%s values for shared node %s differ: got %s want %s", name, node, got, want)
			}
		}
	}
}

func assertConversationalRoutingVariants(
	t *testing.T, references map[string]conversationalReference,
) {
	t.Helper()
	fast := references["conversational-fast-only"].graph
	if !conversationalHasEdge(fast, "fast_text_copy", "out", "segment", "text") ||
		!conversationalHasEdge(fast, "fast_outcome_copy", "out", "segment", "terminal") {
		t.Error("fast-only graph does not route fast text and terminal directly to segmentation")
	}
	if conversationalHasEdgeToPort(fast, "slow_text_copy", "segment", "text") ||
		conversationalNodeElement(fast, "arbiter") != "" {
		t.Error("fast-only graph grants an unintended slow or arbitration speech path")
	}

	slow := references["conversational-slow-only"].graph
	if !conversationalHasEdge(slow, "slow_text_copy", "out", "segment", "text") ||
		!conversationalHasEdge(slow, "slow_outcome_copy", "out", "segment", "terminal") {
		t.Error("slow-only graph does not route deliberative text and terminal directly to segmentation")
	}
	if conversationalHasEdgeToPort(slow, "fast_text_copy", "segment", "text") ||
		conversationalNodeElement(slow, "arbiter") != "" {
		t.Error("slow-only graph grants an unintended fast or arbitration speech path")
	}

	both := references["conversational-both"].graph
	if conversationalNodeElement(both, "arbiter") != "interaction.SpeechArbiter" {
		t.Fatal("both-speaking graph has no explicit stream-aware arbiter")
	}
	for _, source := range []string{"fast_text_copy", "slow_text_copy"} {
		if !conversationalHasEdge(both, source, "out", "arbiter", "text") {
			t.Errorf("both-speaking graph does not route %s through arbiter", source)
		}
		if conversationalHasEdgeToPort(both, source, "segment", "text") {
			t.Errorf("both-speaking graph bypasses arbiter from %s", source)
		}
	}
	if !conversationalHasEdge(both, "arbiter", "selected", "segment", "text") {
		t.Error("both-speaking graph does not route selected arbiter output to segmentation")
	}
	for _, graph := range []ir.Graph{fast, slow, both} {
		if !conversationalHasEdge(graph, "segment", "segments", "tts", "text") ||
			!conversationalHasEdge(graph, "tts", "audio", "playback", "audio") {
			t.Errorf("%s does not retain the typed segmentation -> TTS -> playback chain", graph.ID)
		}
	}
}

func conversationalCoreNodes(graph ir.Graph) []string {
	result := make([]string, 0, len(graph.Nodes))
	for _, node := range graph.Nodes {
		if node.ID == "arbiter" {
			continue
		}
		result = append(result, node.ID+"="+node.Element.Name)
	}
	sort.Strings(result)
	return result
}

func conversationalCoreEdges(graph ir.Graph) []string {
	result := make([]string, 0, len(graph.Edges))
	for _, edge := range graph.Edges {
		if edge.From.Node == "arbiter" || edge.To.Node == "arbiter" ||
			edge.To.Node == "segment" && (edge.To.Port == "text" || edge.To.Port == "terminal") {
			continue
		}
		result = append(result, strings.Join([]string{
			edge.From.Node, edge.From.Port, edge.To.Node, edge.To.Port,
			edge.Type.String(), string(edge.Delivery), strconv.Itoa(edge.Depth),
		}, "|"))
	}
	sort.Strings(result)
	return result
}

func conversationalHasEdge(
	graph ir.Graph, fromNode, fromPort, toNode, toPort string,
) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.From.Port == fromPort &&
			edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func conversationalHasEdgeToPort(graph ir.Graph, fromNode, toNode, toPort string) bool {
	for _, edge := range graph.Edges {
		if edge.From.Node == fromNode && edge.To.Node == toNode && edge.To.Port == toPort {
			return true
		}
	}
	return false
}

func conversationalNodeElement(graph ir.Graph, id string) string {
	for _, node := range graph.Nodes {
		if node.ID == id {
			return node.Element.Name
		}
	}
	return ""
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func exerciseConversationalProviderBoundary(t *testing.T, reference conversationalReference) {
	t.Helper()
	services := graphruntime.NewServiceSet()
	asrProviders := perceptionelements.NewASRProviderRegistry()
	if err := asrProviders.Register("deployment.asr", conversationalASRDescriptor,
		func() (v1.PerceptionProvider, error) { return &conversationalASR{}, nil }); err != nil {
		t.Fatal(err)
	}
	cognitionProviders := cognitionelements.NewProviderRegistry()
	for _, registration := range []struct {
		reference  string
		descriptor continuation.Descriptor
	}{
		{reference: "deployment.fast", descriptor: conversationalFastDescriptor},
		{reference: "deployment.slow", descriptor: conversationalSlowDescriptor},
	} {
		descriptor := registration.descriptor
		if err := cognitionProviders.Register(registration.reference, descriptor,
			func() (continuation.Provider, error) {
				return &conversationalContinuation{descriptor: descriptor}, nil
			}); err != nil {
			t.Fatal(err)
		}
	}
	ttsProviders := speechelements.NewTTSProviderRegistry()
	if err := ttsProviders.Register("deployment.tts", conversationalTTSDescriptor,
		func() (v1.SpeechProvider, error) { return &conversationalTTS{}, nil }); err != nil {
		t.Fatal(err)
	}
	playbackSinks := speechelements.NewPlaybackSinkRegistry()
	if err := playbackSinks.Register("deployment.playback", conversationalPlaybackDescriptor,
		func() (speechelements.PlaybackSink, error) { return &conversationalPlayback{}, nil }); err != nil {
		t.Fatal(err)
	}
	for name, service := range map[string]any{
		perceptionelements.ASRProviderRegistryService: asrProviders,
		cognitionelements.ProviderRegistryService:     cognitionProviders,
		speechelements.TTSProviderRegistryService:     ttsProviders,
		speechelements.PlaybackSinkRegistryService:    playbackSinks,
		speechelements.IrreversibilityLedgerService:   action.NewLedger(),
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{
		Graph: reference.bound.Graph, Registry: registry, Services: services,
		Values: reference.bound.Values, Now: func() uint64 { return 42 },
	})
	if err != nil {
		t.Fatal(err)
	}
	runContext, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(runContext) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("conversational graph stopped with %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("conversational graph did not stop")
		}
	}()

	if got := conversationalReceive(t, mounted, "asr_resolution").(perceptionelements.ProviderResolution).Reference; got != "deployment.asr" {
		t.Errorf("ASR resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "fast_model_resolution").(cognitionelements.ProviderResolution).Reference; got != "deployment.fast" {
		t.Errorf("fast model resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "slow_model_resolution").(cognitionelements.ProviderResolution).Reference; got != "deployment.slow" {
		t.Errorf("slow model resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "tts_resolution").(speechelements.ProviderResolution).Reference; got != "deployment.tts" {
		t.Errorf("TTS resolution = %q", got)
	}
	if got := conversationalReceive(t, mounted, "playback_resolution").(speechelements.SinkResolution).Reference; got != "deployment.playback" {
		t.Errorf("playback resolution = %q", got)
	}
	if snapshot := conversationalReceive(t, mounted, "trajectory_snapshot").(trajectory.Snapshot); snapshot.Version != 0 {
		t.Errorf("initial trajectory version = %d, want 0", snapshot.Version)
	}
}

func conversationalReceive(t *testing.T, mounted *graphruntime.Mounted, name string) any {
	t.Helper()
	port, err := mounted.Egress(name)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	envelope, err := port.Receive(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Payload
}

func conversationalRead(t *testing.T, path string) []byte {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

var (
	conversationalASRDescriptor = v1.Descriptor{
		Name: "conversational-test-asr", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityRevisions:      true,
			v1.CapabilityCancellation:   true,
		},
	}
	conversationalFastDescriptor = continuation.Descriptor{
		Provider: "test", Model: "fast", Phase: trajectory.PhaseFast,
		Effort: continuation.EffortMinimal, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthorityVoice,
	}
	conversationalSlowDescriptor = continuation.Descriptor{
		Provider: "test", Model: "deliberative", Phase: trajectory.PhaseSlow,
		Effort: continuation.EffortHigh, Streaming: true,
		ToolAuthority:   continuation.ToolAuthorityPropose,
		SpeechAuthority: continuation.SpeechAuthoritySilent,
	}
	conversationalTTSDescriptor = v1.Descriptor{
		Name: "conversational-test-tts", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityPCM16Output:  true,
			v1.CapabilityCancellation: true,
		},
	}
	conversationalPlaybackDescriptor = v1.Descriptor{
		Name: "conversational-test-playback", Version: "1",
		Capabilities: v1.Capabilities{
			v1.CapabilityStreamingInput: true,
			v1.CapabilityCancellation:   true,
		},
	}
)

type conversationalASR struct{}

func (*conversationalASR) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalASRDescriptor)
}
func (*conversationalASR) PushFrame(context.Context, v1.AudioFrame) ([]v1.PerceptionRevision, error) {
	return nil, nil
}
func (*conversationalASR) Finalize(context.Context, uint64) (v1.PerceptionRevision, error) {
	return v1.PerceptionRevision{RevisionID: 1}, nil
}

type conversationalContinuation struct {
	descriptor continuation.Descriptor
}

func (provider *conversationalContinuation) Descriptor() continuation.Descriptor {
	return provider.descriptor
}
func (*conversationalContinuation) Continue(
	context.Context, continuation.Request, continuation.Emit,
) (continuation.Completion, error) {
	return continuation.Completion{StopReason: "stop"}, nil
}

type conversationalTTS struct{}

func (*conversationalTTS) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalTTSDescriptor)
}
func (*conversationalTTS) Synthesize(context.Context, v1.SpeechPlan) ([]v1.SpeechChunk, error) {
	return nil, nil
}

type conversationalPlayback struct{}

func (*conversationalPlayback) Descriptor() v1.Descriptor {
	return conversationalCloneDescriptor(conversationalPlaybackDescriptor)
}
func (*conversationalPlayback) Begin(context.Context, action.Utterance) error { return nil }
func (*conversationalPlayback) Audio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (*conversationalPlayback) End(context.Context, action.Utterance, action.Outcome) error {
	return nil
}

func conversationalCloneDescriptor(source v1.Descriptor) v1.Descriptor {
	result := source
	result.Capabilities = make(v1.Capabilities, len(source.Capabilities))
	for capability, enabled := range source.Capabilities {
		result.Capabilities[capability] = enabled
	}
	return result
}
