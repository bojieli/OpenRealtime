package openaicompat

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The whole point of a profile is that a provider's own spelling reaches the
// wire. A knob that parses and then is not sent is the failure mode these
// tests exist for.

func sentRequest(t *testing.T, config Config) (map[string]json.RawMessage, http.Header) {
	t.Helper()
	var body map[string]json.RawMessage
	var header http.Header
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header = request.Header.Clone()
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	config.BaseURL = server.URL + "/v1"
	if config.Model == "" {
		config.Model = "test-model"
	}
	if config.Phase == "" {
		config.Phase = trajectory.PhaseSlow
	}
	if config.Effort == "" {
		config.Effort = continuation.EffortHigh
	}
	adapter, err := New(config)
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if _, err := adapter.Continue(context.Background(), continuation.Request{
		InvocationID: "inv-1", Descriptor: adapter.Descriptor(),
		Trajectory: trajectory.Snapshot{Version: 1, Items: []trajectory.Item{{
			ID: "obs", Kind: trajectory.KindObservation,
			Producer: trajectory.Producer{Phase: trajectory.PhaseUser}, Content: "hello",
		}}},
		Invocation: continuation.Invocation{Instruction: "answer", MaxOutputTokens: 48},
	}, func(continuation.Event) error { return nil }); err != nil {
		t.Fatalf("continue: %v", err)
	}
	return body, header
}

func TestExplicitZeroTemperatureReachesTheWireAndDescriptor(t *testing.T) {
	t.Parallel()
	zero := 0.0
	body, _ := sentRequest(t, Config{Temperature: &zero})
	if string(body["temperature"]) != "0" {
		t.Fatalf("temperature = %s", body["temperature"])
	}
	adapter, err := New(Config{
		Model: "test", BaseURL: "https://example.invalid/v1",
		Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		Temperature: &zero,
	})
	if err != nil {
		t.Fatal(err)
	}
	if adapter.Descriptor().SamplingTemperature != "0" {
		t.Fatalf("descriptor temperature = %q", adapter.Descriptor().SamplingTemperature)
	}
}

func TestEveryReasoningControlReachesTheWireInItsOwnSpelling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		config  Config
		field   string
		enabled string
		absent  []string
	}{
		{
			name: "effort", field: "reasoning_effort", enabled: `"high"`,
			config: Config{ReasoningControl: ReasoningControlEffort, ThinkingMode: ThinkingEnabled},
		},
		{
			name: "thinking object", field: "thinking", enabled: `{"type":"enabled"}`,
			config: Config{ReasoningControl: ReasoningControlThinkingObject, ThinkingMode: ThinkingEnabled},
		},
		{
			name: "enable thinking", field: "enable_thinking", enabled: `true`,
			config: Config{ReasoningControl: ReasoningControlEnableThinking, ThinkingMode: ThinkingEnabled},
		},
		{
			name: "template kwargs", field: "chat_template_kwargs", enabled: `{"enable_thinking":true}`,
			config: Config{ReasoningControl: ReasoningControlTemplateKwargs, ThinkingMode: ThinkingEnabled},
		},
		{
			name:   "none",
			config: Config{ReasoningControl: ReasoningControlNone, ThinkingMode: ThinkingEnabled},
			absent: []string{"reasoning_effort", "thinking", "enable_thinking", "chat_template_kwargs"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			body, _ := sentRequest(t, test.config)
			if test.field != "" && string(body[test.field]) != test.enabled {
				t.Fatalf("%s = %s, want %s", test.field, body[test.field], test.enabled)
			}
			for _, name := range test.absent {
				if _, present := body[name]; present {
					t.Fatalf("a profile that says nothing must send nothing, got %s", name)
				}
			}
		})
	}
}

// The voice runs with reasoning off, and each dialect has its own off switch.
func TestDisabledReasoningUsesTheProfilesOffSwitch(t *testing.T) {
	t.Parallel()
	body, _ := sentRequest(t, Config{
		ReasoningControl: ReasoningControlEffort, ThinkingMode: ThinkingDisabled,
		DisabledEffort: "none", Phase: trajectory.PhaseFast, Effort: continuation.EffortMinimal,
		ToolAuthority: continuation.ToolAuthorityPropose,
	})
	if string(body["reasoning_effort"]) != `"none"` {
		t.Fatalf("reasoning_effort = %s, want \"none\"", body["reasoning_effort"])
	}

	body, _ = sentRequest(t, Config{
		ReasoningControl: ReasoningControlThinkingObject, ThinkingMode: ThinkingDisabled,
	})
	if string(body["thinking"]) != `{"type":"disabled"}` {
		t.Fatalf("thinking = %s", body["thinking"])
	}
}

// OpenAI's reasoning models reject max_tokens outright, so the field name is
// part of the profile rather than a constant.
func TestTheOutputLimitUsesTheProfilesFieldName(t *testing.T) {
	t.Parallel()
	body, _ := sentRequest(t, Config{MaxTokensField: MaxTokensCompletion})
	if string(body["max_completion_tokens"]) != "48" {
		t.Fatalf("max_completion_tokens = %s", body["max_completion_tokens"])
	}
	if _, present := body["max_tokens"]; present {
		t.Fatal("both limit fields were sent; an endpoint given both has to guess")
	}

	body, _ = sentRequest(t, Config{})
	if string(body["max_tokens"]) != "48" {
		t.Fatalf("max_tokens = %s", body["max_tokens"])
	}
}

func TestProfileHeadersAndExtensionsReachTheRequest(t *testing.T) {
	t.Parallel()
	body, header := sentRequest(t, Config{
		Headers:   map[string]string{"HTTP-Referer": "https://example.test"},
		ExtraBody: map[string]json.RawMessage{"provider_flag": json.RawMessage(`{"sort":"latency"}`)},
	})
	if header.Get("HTTP-Referer") != "https://example.test" {
		t.Fatalf("profile header did not reach the request: %v", header)
	}
	if string(body["provider_flag"]) != `{"sort":"latency"}` {
		t.Fatalf("profile extension did not reach the request: %s", body["provider_flag"])
	}
}

// An effort the endpoint has no word for is refused, rather than answered at a
// neighbouring level that every later report would then misdescribe.
func TestAnUnnameableEffortIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	_, err := New(Config{
		Model: "test", BaseURL: "https://example.invalid/v1",
		Phase: trajectory.PhaseSlow, Effort: continuation.EffortMinimal,
		ReasoningControl: ReasoningControlEffort, ThinkingMode: ThinkingEnabled,
		EffortNames: map[continuation.Effort]string{continuation.EffortHigh: "high"},
	})
	if err == nil {
		t.Fatal("an effort the endpoint cannot express must be refused")
	}
}

// A profile must not be able to turn streaming off, because the adapter only
// knows how to read a stream.
func TestAProfileCannotOverrideAReservedField(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"stream", "model", "messages", "max_tokens"} {
		_, err := New(Config{
			Model: "test", BaseURL: "https://example.invalid/v1", Phase: trajectory.PhaseSlow,
			Effort:    continuation.EffortHigh,
			ExtraBody: map[string]json.RawMessage{field: json.RawMessage(`false`)},
		})
		if err == nil {
			t.Errorf("a profile must not be able to set %q", field)
		}
	}
}
