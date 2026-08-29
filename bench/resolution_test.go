package bench_test

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestExpectedResolutionArtifactIsStrictCanonicalAndIndependent(t *testing.T) {
	_, _, resolution := attestationFixture(t)
	first, err := bench.MarshalExpectedResolution(resolution)
	if err != nil {
		t.Fatalf("marshal expected resolution: %v", err)
	}
	second, err := bench.MarshalExpectedResolution(resolution)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("expected resolution serialization is not deterministic: %v", err)
	}
	decoded, err := bench.ParseExpectedResolution(first)
	if err != nil {
		t.Fatalf("parse expected resolution: %v", err)
	}
	remarshaled, err := bench.MarshalExpectedResolution(decoded)
	if err != nil || !bytes.Equal(remarshaled, first) {
		t.Fatalf("expected resolution changed across canonical round trip: %v", err)
	}
	if decoded.Elements[0].Node != "model" || decoded.Elements[1].Node != "sink" ||
		decoded.Elements[2].Node != "source" {
		t.Fatalf("expected elements were not canonicalized: %+v", decoded.Elements)
	}

	resolution.Elements[1].Runtime.ID = "mutated-runtime"
	resolution.Elements[1].Capabilities[0].Provider.ID = "mutated-provider"
	resolution.Elements[1].Capabilities[0].Adapter.ID = "mutated-adapter"
	resolution.Paths[0].Edges[0] = "mutated-edge"
	if decoded.Elements[0].Runtime.ID != "worker://model-4" ||
		decoded.Elements[0].Capabilities[0].Provider.ID != "deepseek/deepseek-v3.2" ||
		decoded.Elements[0].Capabilities[0].Adapter.ID != "adapter://deepseek-chat" ||
		decoded.Paths[0].Edges[0] != "source-to-model" {
		t.Fatal("parsed expected resolution retained aliases into its source")
	}

	unknown := bytes.Replace(first, []byte(`"elements":`), []byte(`"unknown":true,"elements":`), 1)
	if _, err := bench.ParseExpectedResolution(unknown); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("expected resolution accepted an unknown field: %v", err)
	}
	duplicate := bytes.Replace(first, []byte(`"format_version": 1`),
		[]byte(`"format_version": 1, "format_version": 1`), 1)
	if _, err := bench.ParseExpectedResolution(duplicate); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("expected resolution accepted a duplicate field: %v", err)
	}
	if _, err := bench.ParseExpectedResolution(append(first, []byte("{}")...)); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Fatalf("expected resolution accepted trailing JSON: %v", err)
	}
	wrongVersion := bytes.Replace(first, []byte(`"format_version": 1`), []byte(`"format_version": 2`), 1)
	if _, err := bench.ParseExpectedResolution(wrongVersion); err == nil ||
		!strings.Contains(err.Error(), "format must be") {
		t.Fatalf("expected resolution accepted another format: %v", err)
	}

	path := filepath.Join(t.TempDir(), "reviewed", "expected-resolution.json")
	if err := bench.WriteExpectedResolution(path, decoded); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(written, first) {
		t.Fatalf("written expected resolution is not canonical\nwant: %s\n got: %s", first, written)
	}
	read, err := bench.ReadExpectedResolution(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, decoded) {
		t.Fatalf("file round trip changed expected resolution: %+v", read)
	}
}

func TestExpectedResolutionRefusesIncompleteAndPlaceholderIdentities(t *testing.T) {
	_, _, valid := attestationFixture(t)
	tests := []struct {
		name   string
		mutate func(*bench.LiveResolution)
		want   string
	}{
		{name: "no elements", mutate: func(value *bench.LiveResolution) { value.Elements = nil }, want: "no elements"},
		{name: "runtime unresolved", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Runtime.Revision = ""
			value.Elements[0].Runtime.Digest = ""
		}, want: "immutable revision or digest"},
		{name: "runtime latest", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Runtime.Revision = "latest"
		}, want: "live placeholder"},
		{name: "runtime symbolic branch", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Runtime.Revision = "git:main"
		}, want: "live placeholder"},
		{name: "runtime ID template", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Runtime.ID = "${RUNTIME_ARTIFACT}"
		}, want: "live placeholder"},
		{name: "implementation unresolved", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Implementation = "unresolved"
		}, want: "live placeholder"},
		{name: "provider unknown", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Capabilities[0].Provider.Revision = "unknown"
		}, want: "live placeholder"},
		{name: "templated adapter", mutate: func(value *bench.LiveResolution) {
			value.Elements[0].Capabilities[0].Adapter.Revision = "${ADAPTER_REVISION}"
		}, want: "live placeholder"},
		{name: "empty path", mutate: func(value *bench.LiveResolution) {
			value.Paths[0].Edges = nil
		}, want: "has no edges"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			copy, err := bench.ParseExpectedResolution(mustExpectedResolution(t, valid))
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&copy)
			if _, err := bench.MarshalExpectedResolution(copy); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid expected resolution was accepted: %v", err)
			}
		})
	}
}

func mustExpectedResolution(t *testing.T, resolution bench.LiveResolution) []byte {
	t.Helper()
	payload, err := bench.MarshalExpectedResolution(resolution)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
