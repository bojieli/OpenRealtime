package strictyaml_test

import (
	"testing"

	"github.com/bojieli/OpenRealtime/internal/strictyaml"
)

func TestRejectsAmbiguousFeaturesAndAcceptsTypedScalars(t *testing.T) {
	type document struct {
		Name    string `yaml:"name"`
		Enabled bool   `yaml:"enabled"`
		Count   int    `yaml:"count"`
	}
	var valid document
	if _, err := strictyaml.Decode("valid.yaml", []byte("name: test\nenabled: true\ncount: 3\n"), &valid); err != nil {
		t.Fatal(err)
	}
	if !valid.Enabled || valid.Count != 3 {
		t.Fatalf("decoded = %+v", valid)
	}
	for name, source := range map[string]string{
		"duplicate": "name: one\nname: two\n",
		"anchor":    "name: &name test\nenabled: true\ncount: 1\n",
		"tag":       "!Thing {name: test, enabled: true, count: 1}\n",
		"unknown":   "name: test\nenabled: true\ncount: 1\nextra: no\n",
	} {
		t.Run(name, func(t *testing.T) {
			var decoded document
			if _, err := strictyaml.Decode("bad.yaml", []byte(source), &decoded); err == nil {
				t.Fatal("expected strict decoding to fail")
			}
		})
	}
}
