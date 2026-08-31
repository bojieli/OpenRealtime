package secret

import (
	"reflect"
	"strings"
	"testing"
)

// MarshalJSON, MarshalYAML, and Validate are the programmatic half of this
// package's API -- what a deployment tool uses to build a catalog rather than
// read one -- and all three had zero coverage. They share `normalize` with the
// parsers, so the property that matters is that a document built in Go and one
// parsed from a file cannot disagree: marshalling then parsing must return the
// same catalog, and Validate must refuse exactly what the parsers refuse.
func roundTripDocument() Document {
	return Document{
		APIVersion: APIVersion,
		Catalog:    "deployment-catalog",
		Secrets: map[string]Binding{
			"secret://gateway/token": {Provider: "env", Locator: "OPENREALTIME_TOKEN"},
			"secret://asr/key":       {Provider: "file", Locator: "/run/secrets/asr"},
		},
	}
}

func TestSecretCatalogRoundTripsThroughBothEncodings(t *testing.T) {
	t.Parallel()
	document := roundTripDocument()
	if err := Validate(document); err != nil {
		t.Fatalf("validate constructed document: %v", err)
	}

	encodedJSON, err := MarshalJSON(document)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	if !strings.HasSuffix(string(encodedJSON), "\n") {
		t.Error("marshalled JSON is not newline terminated")
	}
	parsedJSON, err := ParseJSON("secrets.json", encodedJSON)
	if err != nil {
		t.Fatalf("parse marshalled JSON: %v", err)
	}

	encodedYAML, err := MarshalYAML(document)
	if err != nil {
		t.Fatalf("marshal YAML: %v", err)
	}
	parsedYAML, err := ParseYAML("secrets.yaml", encodedYAML)
	if err != nil {
		t.Fatalf("parse marshalled YAML: %v", err)
	}

	// Both encodings must land on the same catalog, or a deployment could be
	// described two ways that are not the same deployment.
	if !reflect.DeepEqual(parsedJSON, parsedYAML) {
		t.Fatalf("JSON and YAML round trips disagree:\n%+v\n%+v", parsedJSON, parsedYAML)
	}
	if len(parsedJSON.Secrets) != len(document.Secrets) {
		t.Fatalf("round trip changed the secret population: %d, want %d",
			len(parsedJSON.Secrets), len(document.Secrets))
	}
	for name, want := range document.Secrets {
		if got := parsedJSON.Secrets[name]; got != want {
			t.Fatalf("round-tripped binding %q = %+v, want %+v", name, got, want)
		}
	}

	// Neither encoding may carry a secret value; only a provider and locator.
	for _, encoded := range [][]byte{encodedJSON, encodedYAML} {
		for _, forbidden := range []string{"value", "data", "inline"} {
			if strings.Contains(string(encoded), forbidden) {
				t.Errorf("encoded catalog contains %q: %s", forbidden, encoded)
			}
		}
	}
}

// Validate is documented as the same check the parsers perform, so a document
// it accepts must parse and one it refuses must not.
func TestProgrammaticValidateAgreesWithTheParsers(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		edit func(*Document)
		want string
	}{
		{
			name: "wrong apiVersion",
			edit: func(document *Document) { document.APIVersion = "openrealtime.ai/secrets/v2" },
			want: "apiVersion",
		},
		{
			name: "no catalog identity",
			edit: func(document *Document) { document.Catalog = "" },
			want: "secret catalog ID",
		},
		{
			name: "catalog identity is whitespace",
			edit: func(document *Document) { document.Catalog = "   " },
			want: "secret catalog ID",
		},
		{
			name: "binding names no provider",
			edit: func(document *Document) {
				document.Secrets["secret://gateway/token"] = Binding{Locator: "OPENREALTIME_TOKEN"}
			},
			want: "provider",
		},
		{
			name: "binding names no locator",
			edit: func(document *Document) {
				document.Secrets["secret://gateway/token"] = Binding{Provider: "env"}
			},
			want: "locator",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			document := roundTripDocument()
			test.edit(&document)
			err := Validate(document)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate = %v, want one containing %q", err, test.want)
			}
			if _, err := MarshalJSON(document); err == nil {
				t.Fatal("MarshalJSON accepted a document Validate refused")
			}
			if _, err := MarshalYAML(document); err == nil {
				t.Fatal("MarshalYAML accepted a document Validate refused")
			}
		})
	}
}
