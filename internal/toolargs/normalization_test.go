package toolargs

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCoordinatePairXYV1DerivesSeparateCoordinatesAndPreservesOtherBytes(t *testing.T) {
	source := json.RawMessage(
		`{ "source" : "screen", "x" : [255, 566], "note" : {"z":2,"a":1} }`,
	)
	got, changed, err := Apply(source, []Rule{{Argument: "x", Normalizer: CoordinatePairXYV1}})
	if err != nil {
		t.Fatal(err)
	}
	want := `{ "source" : "screen", "x" : 255,"y":566, "note" : {"z":2,"a":1} }`
	if string(got) != want {
		t.Fatalf("coordinate normalization:\n got: %s\nwant: %s", got, want)
	}
	if len(changed) != 1 || changed[0].Argument != "x" || changed[0].Normalizer != CoordinatePairXYV1 {
		t.Fatalf("changed rules = %+v", changed)
	}
	if string(source) != `{ "source" : "screen", "x" : [255, 566], "note" : {"z":2,"a":1} }` {
		t.Fatalf("source bytes were mutated: %s", source)
	}
}

func TestCoordinatePairXYV1LeavesCanonicalScalarCoordinatesByteExact(t *testing.T) {
	source := json.RawMessage(`{ "source":"screen", "x":255, "y":566 }`)
	got, changed, err := Apply(source, []Rule{{Argument: "x", Normalizer: CoordinatePairXYV1}})
	if err != nil || len(changed) != 0 || string(got) != string(source) {
		t.Fatalf("canonical coordinates = %s, changed=%+v, err=%v", got, changed, err)
	}
}

func TestCoordinatePairXYV1RefusesEveryAmbiguousShape(t *testing.T) {
	for _, testCase := range []struct {
		name string
		json string
		want string
	}{
		{name: "existing y", json: `{"x":[255,566],"y":9}`, want: "y is already present"},
		{name: "one value", json: `{"x":[255]}`, want: "exactly a two-element"},
		{name: "three values", json: `{"x":[255,566,9]}`, want: "exactly a two-element"},
		{name: "fractional", json: `{"x":[255,566.5]}`, want: "exactly two integers"},
		{name: "string", json: `{"x":[255,"566"]}`, want: "exactly two integers"},
		{name: "object", json: `{"x":{"x":255,"y":566}}`, want: "require y unless"},
		{name: "scalar without y", json: `{"x":255}`, want: "require y unless"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := Apply(json.RawMessage(testCase.json), []Rule{{
				Argument: "x", Normalizer: CoordinatePairXYV1,
			}})
			if err == nil || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("Apply error = %v, want %q", err, testCase.want)
			}
		})
	}

	_, _, err := Apply(json.RawMessage(`{"coordinates":[255,566]}`), []Rule{{
		Argument: "coordinates", Normalizer: CoordinatePairXYV1,
	}})
	if err == nil || !strings.Contains(err.Error(), "must be attached to x") {
		t.Fatalf("wrong-argument error = %v", err)
	}
}
