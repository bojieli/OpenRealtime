package timeline

import (
	"bytes"
	"strings"
	"testing"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/trace"
)

func TestRenderProducesSelfContainedTimeline(t *testing.T) {
	t.Parallel()
	records := []trace.Record{
		{SchemaVersion: trace.SchemaVersion, TraceID: "trace_0", SessionID: "session", Sequence: 0, MonotonicNS: 0, Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime, CausalParentIDs: []string{}, Message: []byte(`{"type":"input_audio_buffer.commit"}`)},
		{SchemaVersion: trace.SchemaVersion, TraceID: "trace_1", SessionID: "session", Sequence: 1, MonotonicNS: 100_000_000, Direction: openaiwire.DirectionServer, Profile: openaiwire.ProfileRealtime, CausalParentIDs: []string{"trace_0"}, Message: []byte(`{"event_id":"event_1","type":"output_audio_buffer.started","response_id":"resp_1"}`)},
	}
	var output bytes.Buffer
	if err := Render(&output, "M1 trial", records); err != nil {
		t.Fatal(err)
	}
	html := output.String()
	for _, expected := range []string{"<!doctype html>", "M1 trial", "input_audio_buffer.commit", "output_audio_buffer.started", "100.000"} {
		if !strings.Contains(html, expected) {
			t.Fatalf("rendered HTML does not contain %q", expected)
		}
	}
}
