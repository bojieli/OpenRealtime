package acoustic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	graphcompiler "github.com/bojieli/OpenRealtime/graph"
	"github.com/bojieli/OpenRealtime/graph/resolve"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/perception/noisefilter"
)

// The egress here stands for EnergyAdmission/ASR. It must not observe even one
// sample until the filter response is complete, and must never see raw PCM.
func TestNoiseFilterGraphOrdersAndPreservesAudioEnvelope(t *testing.T) {
	testFilterGraph(t, false)
}
func TestTargetFilterGraphSurvivesLateFrame(t *testing.T) {
	testFilterGraph(t, true)
}
func testFilterGraph(t *testing.T, late bool) {
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("X-Sequence") == "0" {
			close(entered)
			<-release
		}
		w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
		w.Header().Set("X-Filter-Model", "rnnoise")
		w.Header().Set("X-Audio-Delay-MS", "20")
		if late {
			w.Header().Set("X-Filter-Model", "real-tse")
			w.Header().Set("X-Audio-Delay-MS", "65")
			w.Header().Set("X-Target-Voice-State", "extracting")
		}
		w.Write(make([]byte, len(body)))
	}))
	defer server.Close()
	file, err := syntax.Parse("filter.ortg", []byte(`graph filtered { acoustic.NoiseFilter :: filter; input audio = filter.audio; output filtered = filter.filtered; }`))
	if err != nil {
		t.Fatal(err)
	}
	identity, err := NoiseFilterDescriptor().Identity()
	if err != nil {
		t.Fatal(err)
	}
	lock := resolve.NewLock()
	lock.Entries = []resolve.Entry{{Reference: identity.Name, Identity: identity}}
	compiled, err := graphcompiler.Compile(file, graphcompiler.Options{Catalog: acousticCatalog(t), Lock: lock, ResolutionMode: resolve.Locked})
	if err != nil {
		t.Fatal(err)
	}
	filterConfig := noisefilter.Config{URL: server.URL, TimeoutMS: 50}
	if late {
		filterConfig.Model = "real-tse"
		filterConfig.TimeoutMS = 20
	}
	config, _ := json.Marshal(filterConfig)
	bound, err := graphvalues.Bind(compiled.Graph, graphvalues.Document{APIVersion: graphvalues.APIVersion, Graph: "filtered", Nodes: map[string]json.RawMessage{"filter": config}})
	if err != nil {
		t.Fatal(err)
	}
	registry := graphruntime.NewRegistry()
	if err := RegisterFactories(registry); err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(context.Background(), graphruntime.Config{Graph: bound.Graph, Registry: registry, Values: bound.Values})
	if err != nil {
		t.Fatal(err)
	}
	input, err := mounted.Ingress("audio")
	if err != nil {
		t.Fatal(err)
	}
	output, err := mounted.Egress("filtered")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	original := bytes.Repeat([]byte{33, 22}, 480)
	frame := InputFrame{StreamID: "stream", Frame: perception.Frame{Kind: perception.FrameAudio, Source: "microphone", SampleRateHz: 24000, PCM16LE: original, SampleOffset: 123, CapturedNS: 456}}
	envelope := element.Envelope{Type: input.Type(), ItemID: "frame-1", SessionID: "session", Sequence: 7, SourceID: "stream", Payload: frame}
	if _, err := input.Broadcast(ctx, envelope); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("filter not called")
	}
	wait, stop := context.WithTimeout(ctx, 5*time.Millisecond)
	if _, err := output.Receive(wait); err == nil {
		t.Fatal("downstream received unfiltered audio")
	}
	stop()
	if !late {
		close(release)
	}
	wait, stop = context.WithTimeout(ctx, time.Second)
	defer stop()
	got, err := output.Receive(wait)
	if err != nil {
		t.Fatal(err)
	}
	filtered := got.Payload.(InputFrame)
	if !bytes.Equal(filtered.Frame.PCM16LE, make([]byte, len(original))) || original[0] != 33 {
		t.Fatal("raw audio leaked or mutated")
	}
	if got.ItemID != envelope.ItemID+":filtered" || !slices.Contains(got.CausalParents, envelope.ItemID) || got.Sequence != 7 || filtered.Frame.SampleOffset != 123 || filtered.Frame.CapturedNS != 456 {
		t.Fatal("audio timeline or acknowledgement identity changed")
	}

	if late {
		close(release)
		envelope.ItemID = "frame-2"
		envelope.Sequence++
		if _, err := input.Broadcast(ctx, envelope); err != nil {
			t.Fatal(err)
		}
		next, err := output.Receive(wait)
		if err != nil || next.ItemID != "frame-2:filtered" {
			t.Fatal("graph stopped or replayed a stale envelope", err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("graph failed to stop")
	}
}
