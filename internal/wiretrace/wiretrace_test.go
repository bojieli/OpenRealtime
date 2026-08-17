package wiretrace

import (
	"testing"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

func TestMaterializeSortsAndValidates(t *testing.T) {
	t.Parallel()
	commit, err := Marshal(map[string]any{"type": "input_audio_buffer.commit"})
	if err != nil {
		t.Fatal(err)
	}
	clear, err := Marshal(map[string]any{"type": "input_audio_buffer.clear"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := Materialize([]Event{
		{Key: "second", Parents: []string{"first"}, AtNS: 2, Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime, Message: commit},
		{Key: "first", Parents: []string{}, AtNS: 1, Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime, Message: clear},
	}, "session", openaiwire.NewValidator())
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].TraceID != "first" || records[1].Sequence != 1 {
		t.Fatalf("unexpected records: %+v", records)
	}
}
