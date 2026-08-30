package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	graphschema "github.com/bojieli/OpenRealtime/graph/schema"
)

func TestMeetingConfigsRejectNonCanonicalOrOpenEndedInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		decode func(json.RawMessage) error
		value  string
	}{
		{
			name: "screen source whitespace",
			decode: func(source json.RawMessage) error {
				_, err := decodeScreenForkConfig(source)
				return err
			},
			value: `{"source":" screen","kind":"screen"}`,
		},
		{
			name: "screen unknown field",
			decode: func(source json.RawMessage) error {
				_, err := decodeScreenForkConfig(source)
				return err
			},
			value: `{"source":"screen","kind":"screen","fallback":true}`,
		},
		{
			name: "screen missing kind",
			decode: func(source json.RawMessage) error {
				_, err := decodeScreenForkConfig(source)
				return err
			},
			value: `{"source":"screen"}`,
		},
		{
			name: "screen explicit zero bound",
			decode: func(source json.RawMessage) error {
				_, err := decodeScreenForkConfig(source)
				return err
			},
			value: `{"source":"screen","kind":"screen","max_frame_bytes":0}`,
		},
		{
			name: "background authority role",
			decode: func(source json.RawMessage) error {
				_, err := decodeBackgroundInjectionConfig(source)
				return err
			},
			value: `{"role":"system","instruction":"Use it."}`,
		},
		{
			name: "background instruction whitespace",
			decode: func(source json.RawMessage) error {
				_, err := decodeBackgroundInjectionConfig(source)
				return err
			},
			value: `{"role":"background","instruction":" Use it."}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.decode(json.RawMessage(test.value)); err == nil {
				t.Fatal("non-canonical configuration was accepted")
			}
		})
	}
}

func TestMeetingSchemaResolverIsClosedAndReturnsOwnedDocuments(t *testing.T) {
	resolver, err := NewSchemaResolver(nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolver.ResolveConfigSchema(context.Background(), BackgroundConfigSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(resolved.Document), `"const":"background"`) {
		t.Fatalf("background schema = %s", resolved.Document)
	}
	resolved.Document[0] = '!'
	again, err := resolver.ResolveConfigSchema(context.Background(), BackgroundConfigSchema)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Document) == 0 || again.Document[0] == '!' {
		t.Fatal("schema resolver returned mutable shared storage")
	}
	if _, err := resolver.ResolveConfigSchema(context.Background(), "schema://uninstalled"); !errors.Is(err, graphschema.ErrSchemaNotFound) {
		t.Fatalf("unknown schema error = %v", err)
	}

	cancelled, cancel := context.WithCancelCause(context.Background())
	want := errors.New("profile cancelled")
	cancel(want)
	if _, err := resolver.ResolveConfigSchema(cancelled, ScreenForkConfigSchema); !errors.Is(err, want) {
		t.Fatalf("cancelled schema resolution = %v", err)
	}
}
