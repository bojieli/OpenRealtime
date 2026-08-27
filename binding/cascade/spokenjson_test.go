package cascade

import "testing"

// A model that writes a tool call as prose has not said anything a person
// should hear. Measured on a phone menu, [{"name":"press_key",...}] was spoken
// aloud - both useless and unmistakably a bug to whoever is listening.
func TestAToolCallWrittenAsProseIsNotSpeech(t *testing.T) {
	for _, text := range []string{
		`[{"name":"press_key","arguments":{"key":"2"}}]`,
		`{"name": "track_order", "arguments": {"id": "AB1"}}`,
		` {"function":"press_key","parameters":{"key":"9"}} `,
		`<tool_call> {"name":"find_user","arguments":{"email":"a@example.com"}} </tool_call>`,
	} {
		if !looksLikeToolCall(text) {
			t.Fatalf("a call written as prose was treated as speech: %s", text)
		}
	}
}

// A guard that swallows real speech to catch a rare embarrassment trades it
// for a common silence.
func TestOrdinarySpeechIsNotMistakenForAToolCall(t *testing.T) {
	for _, text := range []string{
		"I'll press two for you.",
		"Your order arrives on the third.",
		`He said "name your price" and hung up.`,
		"{ the brace was in the transcript }",
		"One.",
		"",
	} {
		if looksLikeToolCall(text) {
			t.Fatalf("ordinary speech was refused: %q", text)
		}
	}
}
