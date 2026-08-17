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
