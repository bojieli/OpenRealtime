package livekit_test

import (
	"encoding/json"
	"testing"

	livekit "github.com/bojieli/OpenRealtime/integrations/livekit"
)

func TestNewRequiresEverythingItNeedsToConnect(t *testing.T) {
	base := livekit.Config{
		URL: "ws://localhost:7880", APIKey: "key", APISecret: "secret",
		Room: "demo", Endpoint: "ws://localhost:8765/v1/realtime",
	}
	if _, err := livekit.New(base); err != nil {
		t.Fatalf("a complete configuration must be accepted: %v", err)
	}
	for _, missing := range []func(livekit.Config) livekit.Config{
		func(config livekit.Config) livekit.Config { config.URL = ""; return config },
		func(config livekit.Config) livekit.Config { config.APIKey = ""; return config },
		func(config livekit.Config) livekit.Config { config.APISecret = ""; return config },
		func(config livekit.Config) livekit.Config { config.Room = ""; return config },
		func(config livekit.Config) livekit.Config { config.Endpoint = ""; return config },
	} {
		if _, err := livekit.New(missing(base)); err == nil {
			t.Fatal("an incomplete configuration must be refused at construction")
		}
	}
}

// The agent terminates media, so it owns the protocol connection's audio
// format. A participant's opinion about it must not reach the endpoint, and
// everything else in the same event must survive.
func TestAudioFormatIsStrippedAndNothingElseIs(t *testing.T) {
	raw := []byte(`{"type":"session.update","session":{"type":"realtime","instructions":"be brief","audio":{"input":{"format":{"type":"audio/pcm","rate":24000},"turn_detection":{"type":"server_vad"}},"output":{"format":{"type":"audio/pcm"},"voice":"alloy"}}}}`)
	sanitised, err := livekit.StripAudioFormat(raw)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(sanitised, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	session := decoded["session"].(map[string]any)
	if session["instructions"] != "be brief" {
		t.Fatal("the participant's own configuration must survive")
	}
	audio := session["audio"].(map[string]any)
	input := audio["input"].(map[string]any)
	output := audio["output"].(map[string]any)
	if _, present := input["format"]; present {
		t.Fatal("the input format must be stripped")
	}
	if _, present := output["format"]; present {
		t.Fatal("the output format must be stripped")
	}
	if _, present := input["turn_detection"]; !present {
		t.Fatal("turn detection must survive")
	}
	if output["voice"] != "alloy" {
		t.Fatal("voice selection must survive")
	}
}

func TestNonSessionEventsCrossUntouched(t *testing.T) {
	raw := []byte(`{"type":"conversation.item.create","item":{"type":"function_call_output","call_id":"c1","output":"{}"}}`)
	sanitised, err := livekit.StripAudioFormat(raw)
	if err != nil {
		t.Fatalf("strip: %v", err)
	}
	if string(sanitised) != string(raw) {
		t.Fatalf("an ordinary event must cross byte for byte, got %s", sanitised)
	}
	if _, err := livekit.StripAudioFormat([]byte(`not json`)); err == nil {
		t.Fatal("a malformed event must be reported rather than forwarded")
	}
}

func TestMuLawMatchesTheStandard(t *testing.T) {
	// Reference values from G.711: silence encodes to 0xFF, full negative
	// scale to 0x00, and the encoding is sign-magnitude.
	cases := map[int]byte{0: 0xFF, -1: 0x7F, 32767: 0x80, -32768: 0x00}
	for sample, want := range cases {
		if got := livekit.LinearToMuLaw(sample); got != want {
			t.Fatalf("mu-law(%d) = %#x, want %#x", sample, got, want)
		}
	}
}
