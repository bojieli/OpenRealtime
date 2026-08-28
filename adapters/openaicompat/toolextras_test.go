package openaicompat

import (
	"encoding/json"
	"strings"
	"testing"
)

// A tool call carries fields this adapter does not understand, and they have
// to survive being decoded and sent back. Gemini hangs a thought_signature
// here when it is thinking and refuses the next request without it - HTTP 400,
// "Function call is missing a thought_signature in functionCall parts" - which
// took the recorded-menu scenario from passing to erroring the moment the
// voice was given a reasoning budget.
func TestAToolCallKeepsFieldsThisAdapterDoesNotUnderstand(t *testing.T) {
	const wire = `{"id":"call_1","type":"function",` +
		`"extra_content":{"google":{"thought_signature":"EocDCoQD"}},` +
		`"function":{"name":"press_key","arguments":"{\"digit\":\"2\"}"}}`
	var call chatToolCall
	if err := json.Unmarshal([]byte(wire), &call); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if call.Function.Name != "press_key" {
		t.Fatalf("the part we do understand was lost: %+v", call)
	}
	again, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if !strings.Contains(string(again), "thought_signature") {
		t.Fatalf("the signature did not survive the round trip: %s", again)
	}
	if !strings.Contains(string(again), "EocDCoQD") {
		t.Fatalf("the signature came back changed: %s", again)
	}
}

// And a call without one does not grow an empty field, because a provider that
// has never heard of extra_content should see the request it expects.
func TestAToolCallWithoutExtrasStaysClean(t *testing.T) {
	var call chatToolCall
	if err := json.Unmarshal([]byte(`{"id":"call_2","type":"function",`+
		`"function":{"name":"lookup","arguments":"{}"}}`), &call); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	again, err := json.Marshal(call)
	if err != nil {
		t.Fatalf("encoding: %v", err)
	}
	if strings.Contains(string(again), "extra_content") {
		t.Fatalf("an empty extension was invented: %s", again)
	}
}
