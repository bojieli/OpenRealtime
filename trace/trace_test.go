package trace

import (
	"bytes"
	"errors"
	"testing"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
)

func TestTraceRoundTripAndCausality(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	writer := NewWriter(&output)
	records := []Record{
		{
			SchemaVersion: SchemaVersion,
			TraceID:       "trace_0", SessionID: "session", Sequence: 0, MonotonicNS: 10,
			Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime,
			CausalParentIDs: []string{}, Message: []byte(`{"type":"input_audio_buffer.clear"}`),
		},
		{
			SchemaVersion: SchemaVersion,
			TraceID:       "trace_1", SessionID: "session", Sequence: 1, MonotonicNS: 20,
			Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime,
			CausalParentIDs: []string{"trace_0"}, Message: []byte(`{"type":"input_audio_buffer.commit"}`),
		},
	}
	for _, record := range records {
		if err := writer.Write(record); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	count, err := Read(bytes.NewReader(output.Bytes()), nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("Read() count = %d, want 2", count)
	}
}

func TestTraceRejectsUnknownParent(t *testing.T) {
	t.Parallel()
	state := NewState()
	err := state.Accept(Record{
		SchemaVersion: SchemaVersion,
		TraceID:       "trace_0", SessionID: "session", Sequence: 0,
		Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime,
		CausalParentIDs: []string{"missing"}, Message: []byte(`{"type":"input_audio_buffer.clear"}`),
	})
	var invariant *InvariantError
	if !errors.As(err, &invariant) {
		t.Fatalf("Accept() error = %v, want InvariantError", err)
	}
}
