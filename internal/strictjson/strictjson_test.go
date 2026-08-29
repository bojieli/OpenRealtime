package strictjson_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

func TestValidation(t *testing.T) {
	for _, source := range []string{
		`{"a":[1,{"b":true}],"c":null}`,
		`"value"`,
	} {
		if err := strictjson.Validate([]byte(source)); err != nil {
			t.Fatalf("valid JSON %s: %v", source, err)
		}
	}
	for _, source := range []string{
		`{"a":1,"a":2}`,
		`{"a":{"b":1,"b":2}}`,
		`{} {}`,
		`{"a":`,
	} {
		if err := strictjson.Validate([]byte(source)); err == nil {
			t.Fatalf("invalid JSON accepted: %s", source)
		}
	}
}
