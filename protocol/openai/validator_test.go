package openai

import "testing"

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
