package main

import (
	"testing"
	"time"
)

func TestMakeAdapterDryRunOpenRealtimeUsesStandardProtocolBoundary(t *testing.T) {
	t.Parallel()
	adapter, descriptor, err := makeAdapter("openrealtime", "", time.Second, true)
	if err != nil {
		t.Fatal(err)
	}
	if adapter != nil {
		t.Fatal("dry-run unexpectedly created a live adapter")
	}
	if descriptor.Provider != "openrealtime" || descriptor.Model != "openrealtime-local" ||
		descriptor.Transport != "websocket-openai-realtime" ||
		descriptor.Profile != "fdb-v1.5-openai-realtime-adapter-i1-qg-v1" {
		t.Fatalf("unexpected descriptor: %+v", descriptor)
	}
}
