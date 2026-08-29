package compatbinding

import (
	"reflect"
	"testing"
)

func TestCloneDebugMapOwnsNestedValuesAndPreservesCycles(t *testing.T) {
	nested := map[string]any{"value": "before"}
	input := map[string]any{"items": []any{nested}}
	input["self"] = input

	cloned := cloneDebugMap(input)
	if reflect.ValueOf(cloned).Pointer() == reflect.ValueOf(input).Pointer() {
		t.Fatal("clone retained the source map")
	}
	self, ok := cloned["self"].(map[string]any)
	if !ok || reflect.ValueOf(self).Pointer() != reflect.ValueOf(cloned).Pointer() {
		t.Fatalf("clone did not preserve the source cycle: %#v", cloned["self"])
	}
	nested["value"] = "after"
	items, ok := cloned["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("cloned items = %#v", cloned["items"])
	}
	entry, ok := items[0].(map[string]any)
	if !ok || entry["value"] != "before" {
		t.Fatalf("nested clone changed with its source: %#v", cloned)
	}
}
