package openai

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestValidatorAcceptsOfficialEventShapes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		profile   Profile
		direction Direction
		payload   string
	}{
		{
			"realtime client audio",
			ProfileRealtime,
			DirectionClient,
			`{"event_id":"event_456","type":"input_audio_buffer.append","audio":"Base64EncodedAudioData"}`,
		},
		{
			"realtime server audio delta",
			ProfileRealtime,
			DirectionServer,
			`{"event_id":"event_4950","type":"response.output_audio.delta","response_id":"resp_001","item_id":"msg_008","output_index":0,"content_index":0,"delta":"Base64EncodedAudioDelta"}`,
		},
		{
			"translation client audio",
			ProfileTranslation,
			DirectionClient,
			`{"event_id":"event_456","type":"session.input_audio_buffer.append","audio":"Base64EncodedAudioData"}`,
		},
		{
			"transcription client audio",
			ProfileTranscription,
			DirectionClient,
			`{"event_id":"event_456","type":"input_audio_buffer.append","audio":"Base64EncodedAudioData"}`,
		},
		{
			"beta client clear",
			ProfileBeta,
			DirectionClient,
			`{"event_id":"event_456","type":"input_audio_buffer.clear"}`,
		},
	}
	validator := NewValidator()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			message, err := Decode([]byte(test.payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := validator.Validate(test.profile, test.direction, message); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestValidatorRejectsMissingRequiredField(t *testing.T) {
	t.Parallel()
	message, err := Decode([]byte(`{"type":"input_audio_buffer.append"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewValidator().Validate(ProfileRealtime, DirectionClient, message); err == nil {
		t.Fatal("Validate() error = nil, want missing audio error")
	}
}

func TestValidatorRejectsWrongDirection(t *testing.T) {
	t.Parallel()
	message, err := Decode([]byte(`{"type":"input_audio_buffer.append","audio":"AA=="}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := NewValidator().Validate(ProfileRealtime, DirectionServer, message); err == nil {
		t.Fatal("Validate() error = nil, want direction error")
	}
}

func BenchmarkValidateInputAudio(b *testing.B) {
	message, err := Decode([]byte(`{"event_id":"event_456","type":"input_audio_buffer.append","audio":"QmFzZTY0RW5jb2RlZEF1ZGlvRGF0YQ=="}`))
	if err != nil {
		b.Fatal(err)
	}
	validator := NewValidator()
	if err := validator.Validate(ProfileRealtime, DirectionClient, message); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := validator.Validate(ProfileRealtime, DirectionClient, message); err != nil {
			b.Fatal(err)
		}
	}
}

// TestThePinnedSchemaBundleCompilesOncePerProcess is a cost test, and it is
// here because no correctness test can be.
//
// Every session held its own validator, and a validator compiled the whole
// pinned bundle the first time it was asked to check an event. That is correct
// - the schemas are identical, so every session validated exactly as it should
// - and it is invisible to every test in this repository, because compiling a
// schema twice produces the same schema. What it costs is paid per connection:
// roughly 34 ms of CPU before the first event can be checked, and about 1.6 MiB
// held for as long as the session lasts. At a thousand concurrent sessions that
// is a third of a CPU-second of connect latency and 1.6 GiB of duplicated
// immutable data.
//
// So the property worth asserting is not what a validator answers but how many
// times the bundle is built. One, for the life of the process, however many
// validators exist.
func TestThePinnedSchemaBundleCompilesOncePerProcess(t *testing.T) {
	message, err := Decode([]byte(
		`{"event_id":"event_456","type":"input_audio_buffer.append","audio":"AA=="}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	// Enough validators that a per-instance compile is unmistakable, and few
	// enough that the pre-fix run finishes: each one used to cost 34 ms.
	const validators = 32
	for range validators {
		if err := NewValidator().Validate(ProfileRealtime, DirectionClient, message); err != nil {
			t.Fatal(err)
		}
	}
	// Not "at most validators" and not a ratio: the count is exactly one for
	// the whole process no matter which test in this package ran first, which
	// is the only threshold that stays meaningful as tests are added.
	if compilations := bundleCompilations.Load(); compilations != 1 {
		t.Fatalf("the pinned schema bundle was compiled %d times after %d validators, want 1",
			compilations, validators)
	}
}

// audioDeltaEvent is one 20 ms frame of 24 kHz mono PCM16 as the gateway
// renders it: the shape that crosses the wire fifty times a second for the
// life of every speaking session, and therefore the shape whose validation
// cost is worth knowing.
func audioDeltaEvent(tb testing.TB) []byte {
	tb.Helper()
	encoded, err := json.Marshal(map[string]any{
		"type": "response.output_audio.delta", "event_id": "event_000000000001",
		"response_id": "resp_000000000001", "item_id": "item_000000000001",
		"output_index": 0, "content_index": 0,
		"delta": base64.StdEncoding.EncodeToString(make([]byte, 960)),
	})
	if err != nil {
		tb.Fatal(err)
	}
	return encoded
}

// TestValidateEncodedAgreesWithValidate is the whole safety argument for the
// single-pass form.
//
// It reads the event type out of the document it decoded rather than out of a
// separate envelope pass, so the only thing worth proving is that the two
// forms reach the same verdict - including on the malformed inputs, where a
// second parser is exactly where an accidental difference would hide.
func TestValidateEncodedAgreesWithValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		payload string
	}{
		{"valid client audio", `{"event_id":"e","type":"input_audio_buffer.append","audio":"AA=="}`},
		{"missing required field", `{"type":"input_audio_buffer.append"}`},
		{"undefined event", `{"type":"nonsense.event"}`},
		{"wrong field type", `{"event_id":"e","type":"input_audio_buffer.append","audio":7}`},
		{"unexpected extra shape", `{"event_id":"e","type":"input_audio_buffer.clear","audio":[1,2]}`},
	}
	validator := NewValidator()
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			encodedErr := validator.ValidateEncoded(
				ProfileRealtime, DirectionClient, []byte(test.payload),
			)
			message, decodeErr := Decode([]byte(test.payload))
			if decodeErr != nil {
				t.Fatal(decodeErr)
			}
			messageErr := validator.Validate(ProfileRealtime, DirectionClient, message)
			if (encodedErr == nil) != (messageErr == nil) {
				t.Fatalf("ValidateEncoded() = %v but Validate() = %v", encodedErr, messageErr)
			}
		})
	}
}

// TestValidateEncodedRefusesDocumentsThatAreNotEvents covers the inputs Decode
// used to reject before a validator ever saw them. Nothing else would: a
// caller with bytes in hand has not been through Decode.
func TestValidateEncodedRefusesDocumentsThatAreNotEvents(t *testing.T) {
	t.Parallel()
	for _, payload := range []string{`[]`, `"text"`, `7`, `{}`, `{"type":""}`, `{"type":7}`, `not json`} {
		if err := NewValidator().ValidateEncoded(
			ProfileRealtime, DirectionServer, []byte(payload),
		); err == nil {
			t.Fatalf("ValidateEncoded(%s) error = nil, want refusal", payload)
		}
	}
}

// BenchmarkValidateAudioDeltaEncoded is the outbound cost per audio frame, and
// BenchmarkValidateAudioDeltaDecoded is what it was: the sender held the bytes
// and had to parse the envelope and copy the payload before the validator
// parsed the whole document again.
func BenchmarkValidateAudioDeltaEncoded(b *testing.B) {
	raw := audioDeltaEvent(b)
	validator := NewValidator()
	if err := validator.ValidateEncoded(ProfileRealtime, DirectionServer, raw); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := validator.ValidateEncoded(ProfileRealtime, DirectionServer, raw); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidateAudioDeltaDecoded(b *testing.B) {
	raw := audioDeltaEvent(b)
	validator := NewValidator()
	b.ReportAllocs()
	for b.Loop() {
		message, err := Decode(raw)
		if err != nil {
			b.Fatal(err)
		}
		if err := validator.Validate(ProfileRealtime, DirectionServer, message); err != nil {
			b.Fatal(err)
		}
	}
}
