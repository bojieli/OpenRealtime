package gateway

import (
	"encoding/json"
	"testing"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

func TestEncodeToolResultNormalizesExplicitClientFailure(t *testing.T) {
	result := encodeToolResult("call_1", "lookup", "  Error: User not found  ")
	if result.Error != "User not found" || len(result.Output) != 0 {
		t.Fatalf("explicit client failure was not normalized: %+v", result)
	}
}

func TestEncodeToolResultLeavesOrdinaryOutputUntouched(t *testing.T) {
	for _, output := range []string{
		`{"error":"a historical error recorded as data","ok":true}`,
		`The log contains Error: timeout, but the lookup completed.`,
		`Error:`,
	} {
		result := encodeToolResult("call_1", "lookup", output)
		if result.Error != "" {
			t.Errorf("ordinary output %q was inferred to be a failure: %+v", output, result)
			continue
		}
		if !json.Valid(result.Output) {
			t.Errorf("ordinary output %q was not preserved as JSON: %q", output, result.Output)
		}
	}
}

func TestWireToolPreservesBackgroundExecutionPolicy(t *testing.T) {
	spec, err := (wireTool{
		Type: "function", Name: "analyze", Description: "analyze while listening",
		Parameters:   json.RawMessage(`{"type":"object"}`),
		OpenRealtime: &openrealtime.ToolExtension{Background: true},
	}).spec()
	if err != nil {
		t.Fatalf("parse tool: %v", err)
	}
	if !spec.Background {
		t.Fatal("wire parsing dropped background execution policy")
	}
}
