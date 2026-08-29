package elementconfig_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

func TestDecodeAcceptsOnlyStrictKnownObjectFields(t *testing.T) {
	type config struct {
		Name string `json:"name"`
	}
	var decoded config
	if err := elementconfig.Decode(json.RawMessage(`{"name":"model"}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Name != "model" {
		t.Fatalf("decoded = %+v", decoded)
	}
	for _, source := range []string{
		`null`, `[]`, `{"unknown":true}`, `{"name":"a","name":"b"}`, `{} {}`,
	} {
		if err := elementconfig.Decode(json.RawMessage(source), &config{}); err == nil {
			t.Fatalf("invalid configuration %s was accepted", source)
		}
	}
	if err := elementconfig.Decode(nil, &config{}); err != nil {
		t.Fatalf("omitted configuration: %v", err)
	}
	if err := elementconfig.Decode(json.RawMessage(`{}`), config{}); err == nil ||
		!strings.Contains(err.Error(), "pointer") {
		t.Fatalf("non-pointer destination error = %v", err)
	}
}
