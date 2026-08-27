package gateway_test

import (
	"encoding/json"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestDebugEventsAreNegotiatedTimestampedAndRedactedByDefault(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version,
		"debug": map[string]any{
			"enabled": true, "categories": []string{"session"},
		},
	})
	updated := client.await("session.updated", 5*time.Second)
	extension := updated["session"].(map[string]any)["openrealtime"].(map[string]any)
	debug := extension["debug"].(map[string]any)
	if debug["enabled"] != true || debug["timestamp_resolution"] != "milliseconds" {
		t.Fatalf("unexpected negotiated debug configuration: %v", debug)
	}

	entry := client.await(openrealtime.EventDebug, 5*time.Second)
	if entry["category"] != "session" || entry["name"] != "session.updated" {
		t.Fatalf("unexpected debug entry: %v", entry)
	}
	if timestamp, ok := entry["timestamp_ms"].(float64); !ok || timestamp < 1_000_000_000_000 {
		t.Fatalf("debug entries require an absolute millisecond timestamp: %v", entry["timestamp_ms"])
	}
	attributes := entry["attributes"].(map[string]any)
	if attributes["payloads_redacted"] != true {
		t.Fatalf("the prompt must be redacted unless payloads were opted into: %v", attributes)
	}
	if entry["payload"] != nil {
		t.Fatalf("redacted debug event leaked a payload: %v", entry["payload"])
	}
}

func TestVideoDebugReportsGeometryAdmissionAndRuntimeLatency(t *testing.T) {
	server := startVideoServer(t, "a marked browser frame")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version, "supports": []string{"video.input", "observations"},
		"debug": map[string]any{"enabled": true, "categories": []string{"video"}},
	})
	client.await("session.updated", 5*time.Second)
	client.send(map[string]any{
		"type": openrealtime.EventVideoSourceUpdate, "source": "browser",
		"state": "active", "width": 320, "height": 240,
	})
	source := client.await(openrealtime.EventDebug, 5*time.Second)
	if source["name"] != "video.source_updated" || source["correlation_id"] != "browser" {
		t.Fatalf("unexpected source trace: %v", source)
	}
	client.send(map[string]any{
		"type": openrealtime.EventVideoFrameAppend, "source": "browser",
		"frame": screenFrame(t, 20, image.Rect(20, 20, 200, 160)), "timestamp_ms": 1_700_000_000_123,
	})
	accepted := client.await(openrealtime.EventDebug, 5*time.Second)
	if accepted["name"] != "video.frame_accepted" {
		t.Fatalf("unexpected admission trace: %v", accepted)
	}
	attributes := accepted["attributes"].(map[string]any)
	if attributes["source"] != "browser" || attributes["captured_ms"] != float64(1_700_000_000_123) {
		t.Fatalf("admission metadata does not identify the frame: %v", attributes)
	}
	dispatched := client.await(openrealtime.EventDebug, 5*time.Second)
	if dispatched["name"] != "video.runtime_dispatch" || dispatched["duration_ms"] == nil {
		t.Fatalf("unexpected runtime trace: %v", dispatched)
	}
}

func TestDebugStreamCoversTheAudioToolAndSpeechLifecycle(t *testing.T) {
	server := startServer(t,
		fast([]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Checking."}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "Forty dollars."}}),
		slow([]continuation.Event{{Kind: continuation.EventToolCall, ToolCall: &trajectory.ToolCall{
			CallID: "call_1", Name: "get_balance", Arguments: json.RawMessage(`{"account":"A1"}`),
		}}},
			[]continuation.Event{{Kind: continuation.EventAssistantDelta, Text: "The balance is $40.00."}}),
		"what is my balance")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.send(map[string]any{"type": "session.update", "session": map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
		"tools": []map[string]any{{
			"type": "function", "name": "get_balance", "description": "read a balance",
			"parameters": map[string]any{"type": "object", "properties": map[string]any{}},
		}},
		"openrealtime": map[string]any{
			"version": openrealtime.Version,
			"debug": map[string]any{
				"enabled": true,
				"categories": []string{
					"vad", "asr", "cognition", "policy", "tts", "tool", "session",
				},
			},
		},
	}})
	client.await("session.updated", 5*time.Second)
	client.speak()

	call := client.await("response.function_call_arguments.done", 10*time.Second)
	if call["name"] != "get_balance" || call["call_id"] != "call_1" {
		t.Fatalf("unexpected call: %v", call)
	}
	client.send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "function_call_output", "call_id": "call_1", "output": `{"balance":40}`,
	}})
	client.await("conversation.item.created", 5*time.Second)
	for {
		delta := client.await("response.output_audio_transcript.delta", 10*time.Second)
		if text, _ := delta["delta"].(string); strings.Contains(text, "Forty dollars") {
			break
		}
	}
	client.await("response.output_audio.done", 10*time.Second)
	client.await("response.done", 10*time.Second)

	entries := client.messages(openrealtime.EventDebug)
	if len(entries) == 0 {
		t.Fatal("the driven lifecycle produced no debug events")
	}
	byName := make(map[string][]map[string]any)
	for _, entry := range entries {
		name, _ := entry["name"].(string)
		category, _ := entry["category"].(string)
		phase, _ := entry["phase"].(string)
		if name == "" || category == "" || phase == "" {
			t.Fatalf("debug entry is missing its identity: %v", entry)
		}
		if timestamp, ok := entry["timestamp_ms"].(float64); !ok || timestamp < 1_000_000_000_000 {
			t.Fatalf("debug entry %q lacks an absolute millisecond timestamp: %v", name, entry)
		}
		if entry["payload"] != nil {
			t.Fatalf("redacted lifecycle trace %q leaked a payload: %v", name, entry["payload"])
		}
		byName[name] = append(byName[name], entry)
	}

	required := map[string]string{
		"session.updated":           "session",
		"vad.gate_opened":           "vad",
		"vad.speech_started":        "vad",
		"vad.endpoint_detected":     "vad",
		"vad.speech_stopped":        "vad",
		"asr.observe":               "asr",
		"asr.finalize":              "asr",
		"asr.transcript":            "asr",
		"policy.rollout":            "policy",
		"policy.tool_authorization": "policy",
		"cognition.fast":            "cognition",
		"cognition.slow":            "cognition",
		"tool.call.planned":         "tool",
		"tool.call.emitted":         "tool",
		"tool.result.received":      "tool",
		"tool.result.committed":     "tool",
		"tts.utterance_started":     "tts",
		"tts.first_audio":           "tts",
		"tts.utterance_completed":   "tts",
	}
	for name, category := range required {
		found := byName[name]
		if len(found) == 0 {
			t.Fatalf("missing %s debug evidence; saw %s", name, debugNames(entries))
		}
		if found[0]["category"] != category {
			t.Fatalf("%s used category %v, want %s", name, found[0]["category"], category)
		}
	}

	for _, name := range []string{"asr.observe", "asr.finalize", "cognition.fast", "cognition.slow"} {
		if !hasTimedPhase(byName[name], "end") {
			t.Fatalf("%s has no completed duration measurement: %v", name, byName[name])
		}
	}
	for name, phase := range map[string]string{
		"tts.first_audio":         "update",
		"tts.utterance_completed": "end",
		"tool.result.received":    "end",
	} {
		if !hasTimedPhase(byName[name], phase) {
			t.Fatalf("%s has no %s latency measurement: %v", name, phase, byName[name])
		}
	}
	for _, name := range []string{
		"tool.call.planned", "tool.call.emitted", "tool.result.received", "tool.result.committed",
	} {
		if !hasCorrelation(byName[name], "call_1") {
			t.Fatalf("%s is not correlated to call_1: %v", name, byName[name])
		}
	}
	if !hasCorrelatedTTSCycle(byName) {
		t.Fatalf("TTS start, first audio, and completion do not share an utterance id: %v", byName)
	}
}

func TestDebugStreamReportsSessionErrors(t *testing.T) {
	server := startServer(t, fast(), slow(), "hello")
	client := dial(t, server)
	client.await("session.created", 5*time.Second)
	client.configurePCM16(map[string]any{
		"version": openrealtime.Version,
		"debug": map[string]any{
			"enabled": true, "categories": []string{"error"},
		},
	})
	client.await("session.updated", 5*time.Second)

	// This is a valid base-protocol event with an invalid session state, so it
	// reaches the gateway's ordinary error path rather than failing wire decode.
	client.send(map[string]any{"type": "output_audio_buffer.clear"})
	failure := client.await("error", 5*time.Second)
	errorBody, _ := failure["error"].(map[string]any)
	message, _ := errorBody["message"].(string)
	if !strings.Contains(message, "no agent audio to clear") {
		t.Fatalf("unexpected client error: %v", failure)
	}
	entries := client.messages(openrealtime.EventDebug)
	if len(entries) != 1 {
		t.Fatalf("expected one negotiated error trace, got %v", entries)
	}
	entry := entries[0]
	if entry["category"] != "error" || entry["name"] != "session.error" || entry["phase"] != "error" {
		t.Fatalf("unexpected error trace: %v", entry)
	}
	if !strings.Contains(entry["message"].(string), "no agent audio to clear") {
		t.Fatalf("error trace lost the actionable cause: %v", entry)
	}
}

func debugNames(entries []map[string]any) string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, _ := entry["name"].(string); name != "" {
			names = append(names, name)
		}
	}
	return strings.Join(names, ", ")
}

func hasTimedPhase(entries []map[string]any, phase string) bool {
	for _, entry := range entries {
		if entry["phase"] != phase {
			continue
		}
		if duration, ok := entry["duration_ms"].(float64); ok && duration >= 0 {
			return true
		}
	}
	return false
}

func hasCorrelation(entries []map[string]any, correlationID string) bool {
	for _, entry := range entries {
		if entry["correlation_id"] == correlationID {
			return true
		}
	}
	return false
}

func hasCorrelatedTTSCycle(byName map[string][]map[string]any) bool {
	for _, started := range byName["tts.utterance_started"] {
		correlationID, _ := started["correlation_id"].(string)
		if correlationID != "" &&
			hasCorrelation(byName["tts.first_audio"], correlationID) &&
			hasCorrelation(byName["tts.utterance_completed"], correlationID) {
			return true
		}
	}
	return false
}

func TestDebugEventsNeverAppearWithoutNegotiation(t *testing.T) {
	response, err := openrealtime.Negotiate(
		openrealtime.Request{Version: openrealtime.Version}, openrealtime.Features())
	if err != nil {
		t.Fatal(err)
	}
	if response.Debug != nil {
		t.Fatal("ordinary extension negotiation enabled debugging")
	}
}
