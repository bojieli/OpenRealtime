package openai

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestGeneratedInventoryCoversEveryProfile(t *testing.T) {
	t.Parallel()
	wantCounts := map[string]int{
		"beta/client":          12,
		"beta/server":          40,
		"realtime/client":      11,
		"realtime/server":      46,
		"transcription/client": 4,
		"transcription/server": 10,
		"translation/client":   3,
		"translation/server":   7,
	}
	gotCounts := make(map[string]int)
	for _, definition := range Definitions() {
		key := string(definition.Profile) + "/" + string(definition.Direction)
		gotCounts[key]++
		if found, ok := Lookup(definition.Profile, definition.Direction, definition.Type); !ok || found != definition {
			t.Fatalf("Lookup(%+v) = %+v, %v", definition, found, ok)
		}
	}
	if len(Definitions()) != 133 {
		t.Fatalf("Definitions() count = %d, want 133", len(Definitions()))
	}
	if !mapsEqual(gotCounts, wantCounts) {
		t.Fatalf("inventory counts = %v, want %v", gotCounts, wantCounts)
	}
}

func TestGeneratedDefinitionsMatchSchemaConstants(t *testing.T) {
	t.Parallel()
	var bundle struct {
		Source struct {
			Revision string `json:"revision"`
			SHA256   string `json:"sha256"`
		} `json:"x-openrealtime-source"`
		Defs map[string]struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(SchemaBundle, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.Source.Revision != "2186421dca0cca7c1e67caa7739005e8b1ccc4dd" ||
		bundle.Source.SHA256 != "542299d304cdeb78deff4172b3790d52c7e7e75fb2b517e9c2787c52f1424acc" {
		t.Fatalf("unexpected pinned source: %+v", bundle.Source)
	}
	for _, definition := range Definitions() {
		name := definition.SchemaRef[len("#/$defs/"):]
		schema, ok := bundle.Defs[name]
		if !ok {
			t.Fatalf("generated definition references missing schema %q", name)
		}
		values := schema.Properties["type"].Enum
		if len(values) != 1 || EventType(values[0]) != definition.Type {
			t.Fatalf("schema %s type enum = %v, want %q", name, values, definition.Type)
		}
	}
}

func TestDecodePreservesFullUnknownPayload(t *testing.T) {
	t.Parallel()
	raw := []byte(`{"event_id":"event_1","type":"future.event","future":{"nested":true}}`)
	message, err := Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	if message.Type() != "future.event" {
		t.Fatalf("Type() = %q", message.Type())
	}
	if _, ok := Lookup(ProfileRealtime, DirectionServer, message.Type()); ok {
		t.Fatal("future event unexpectedly classified as known")
	}
	copyOfRaw := message.Raw()
	copyOfRaw[0] = 'x'
	if bytes.Equal(copyOfRaw, message.Raw()) {
		t.Fatal("Raw() exposed mutable internal storage")
	}
}

func TestDecodeRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	if _, err := Decode([]byte(`{"type":"response.cancel"}{"type":"response.cancel"}`)); err == nil {
		t.Fatal("Decode() error = nil, want trailing-document error")
	}
}

func mapsEqual(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func BenchmarkDecodeInputAudio(b *testing.B) {
	payload := []byte(`{"event_id":"event_456","type":"input_audio_buffer.append","audio":"QmFzZTY0RW5jb2RlZEF1ZGlvRGF0YQ=="}`)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Decode(payload); err != nil {
			b.Fatal(err)
		}
	}
}
