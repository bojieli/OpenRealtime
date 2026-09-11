package graphs_test

import (
	"context"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	"github.com/bojieli/OpenRealtime/graphs"
	"github.com/bojieli/OpenRealtime/perception/noisefilter"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestFilteredConversationHasNoAudioBypassAndCompiles(t *testing.T) {
	config := newScenarioProfileFixture(t).pluginConfig()
	baseline, err := graphs.ScenarioConversationArtifacts(config)
	if err != nil {
		t.Fatal(err)
	}
	config.NoiseFilter = &noisefilter.Config{URL: "http://127.0.0.1:8125", TimeoutMS: 30}
	artifacts, err := graphs.ScenarioConversationArtifacts(config)
	if err != nil {
		t.Fatal(err)
	}
	topology := string(artifacts.Topology.Data)
	if !strings.Contains(topology, "input audio = noise_filter.audio;") || !strings.Contains(topology, "noise_filter.filtered -> admission.audio;") || strings.Contains(topology, "input audio = admission.audio;") {
		t.Fatal("raw ingress can bypass filtering")
	}
	if strings.Contains(string(baseline.Topology.Data), "NoiseFilter") {
		t.Fatal("existing profile changed")
	}
	if !strings.Contains(string(artifacts.Values.Data), `"unclassified":"keep_speaking"`) {
		t.Fatal("unclassified audio can still cancel output")
	}
	launch, err := graphs.ScenarioConversationLaunchConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graphlaunch.New(context.Background(), launch); err != nil {
		t.Fatal(err)
	}
}

// Exercise the public Realtime gateway, per-frame acknowledgements, ASR,
// cognition, tool results, synthesis, and playback through the new topology.
func TestFilteredConversationRealtimeRoundTrip(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		body, _ := io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
		w.Header().Set("X-Filter-Model", "rnnoise")
		w.Header().Set("X-Audio-Delay-MS", "20")
		w.Write(body)
	}))
	defer server.Close()
	selected := scenarioEndpointToolCases()[0]
	selected.noiseFilterURL = server.URL
	testScenarioConversationGraphRoundTrip(t, selected)
	if calls.Load() < 2 {
		t.Fatal("audio did not traverse filter")
	}
}

func TestTargetConversationRealtimeRoundTrip(t *testing.T) {
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			return
		}
		body, _ := io.ReadAll(r.Body)
		calls.Add(1)
		w.Header().Set("X-Sequence", r.Header.Get("X-Sequence"))
		w.Header().Set("X-Filter-Model", "real-tse")
		w.Header().Set("X-Audio-Delay-MS", "65")
		w.Header().Set("X-Target-Voice-State", "extracting")
		w.Write(body)
	}))
	defer server.Close()
	selected := scenarioEndpointToolCases()[0]
	selected.noiseFilterURL = server.URL
	selected.noiseFilterModel = "real-tse"
	testScenarioConversationGraphRoundTrip(t, selected)
	if calls.Load() < 2 {
		t.Fatal("audio bypassed target extractor")
	}
}
