package codec_test

import (
	"bytes"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/element/codec"
)

func TestJSONAndYAMLRoundTripCanonicalBundle(t *testing.T) {
	bundle := codec.New(descriptor())
	jsonSource, err := codec.MarshalJSON(bundle)
	if err != nil {
		t.Fatal(err)
	}
	yamlSource, err := codec.MarshalYAML(bundle)
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := codec.ParseJSON("elements.json", jsonSource)
	if err != nil {
		t.Fatal(err)
	}
	fromYAML, err := codec.ParseYAML("elements.yaml", yamlSource)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := codec.MarshalJSON(fromJSON)
	right, _ := codec.MarshalJSON(fromYAML)
	if !bytes.Equal(left, right) {
		t.Fatalf("formats differ:\n%s\n%s", left, right)
	}
}

func TestStrictBundleRejectsUnknownAndDuplicateFields(t *testing.T) {
	for name, source := range map[string]string{
		"unknown JSON":   `{"apiVersion":"openrealtime.ai/elements/v1alpha1","elements":[],"extra":true}`,
		"duplicate YAML": "apiVersion: openrealtime.ai/elements/v1alpha1\nelements: []\nelements: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			if name == "unknown JSON" {
				if _, err := codec.ParseJSON("bad.json", []byte(source)); err == nil {
					t.Fatal("expected failure")
				}
			} else if _, err := codec.ParseYAML("bad.yaml", []byte(source)); err == nil {
				t.Fatal("expected failure")
			}
		})
	}
}

func descriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Source", Revision: 1,
		Ports: []element.Port{{
			Name: "out", Direction: element.Output, Type: element.Event(element.Named("test.Value")),
			Cardinality: element.One, DefaultDepth: 4,
		}},
	}
}
