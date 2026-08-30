package review

import (
	"encoding/json"
	"testing"
)

func TestCanonicalContextSHA256MatchesPreparedRecordInput(t *testing.T) {
	request, _ := testRequest(t)
	request.Context = json.RawMessage("{\n  \"z\": 2,\n  \"a\": 1\n}\n")
	prepared, err := PrepareContext(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := CanonicalContextSHA256(t.Context(), request.Context)
	if err != nil {
		t.Fatal(err)
	}
	if identity != digest(prepared.Context) {
		t.Fatalf("canonical context identity = %q, want %q", identity, digest(prepared.Context))
	}
	if _, err := CanonicalContextSHA256(t.Context(), json.RawMessage(`[]`)); err == nil {
		t.Fatal("non-object review context unexpectedly received an identity")
	}
}
