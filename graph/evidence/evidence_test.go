package evidence_test

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/element"
	graphevidence "github.com/bojieli/OpenRealtime/graph/evidence"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

func TestEmptyEvidenceManifestMakesNoClaimsAndRoundTrips(t *testing.T) {
	source := []byte(`apiVersion: openrealtime.ai/evidence/v1alpha1
graph: shipped_graph
profiles: []
`)
	document, err := graphevidence.ParseYAML("agent.evidence.yaml", source)
	if err != nil {
		t.Fatal(err)
	}
	if document.Profiles == nil || len(document.Profiles) != 0 {
		t.Fatalf("empty profiles = %#v", document.Profiles)
	}
	first, err := graphevidence.Fingerprint(document)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := graphevidence.MarshalYAML(document)
	if err != nil {
		t.Fatal(err)
	}
	again, err := graphevidence.ParseYAML("roundtrip.yaml", payload)
	if err != nil {
		t.Fatal(err)
	}
	second, err := graphevidence.Fingerprint(again)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !strings.Contains(string(payload), "profiles: []") {
		t.Fatalf("evidence round trip = %q, %s -> %s", payload, first, second)
	}
}

func TestEvidenceProfilesRequireEveryExactApplicabilityAxis(t *testing.T) {
	value := element.Event(element.Named("test.Value"))
	descriptor := element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "test.Measured", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "out", Direction: element.Output, Type: value, Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
	}
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	profile := graphevidence.Profile{
		Name: "warm-p50", Artifact: evidenceArtifact("evidence/result", "1"),
		PlanFingerprint: evidenceDigest("plan"), NodeID: "worker", Element: identity,
		Implementation: evidenceArtifact("runtime/worker", "1"),
		Hardware:       evidenceArtifact("hardware/test", "1"),
		Load:           evidenceArtifact("load/realtime", "1"),
	}
	document := graphevidence.Document{
		APIVersion: graphevidence.APIVersion, Graph: "measured", Profiles: []graphevidence.Profile{profile},
	}
	if _, err := graphevidence.Fingerprint(document); err != nil {
		t.Fatal(err)
	}

	tests := map[string]struct {
		mutate func(*graphevidence.Profile)
		want   string
	}{
		"plan":           {mutate: func(value *graphevidence.Profile) { value.PlanFingerprint = "" }, want: "plan fingerprint"},
		"element":        {mutate: func(value *graphevidence.Profile) { value.Element = element.Identity{} }, want: "element"},
		"implementation": {mutate: func(value *graphevidence.Profile) { value.Implementation = inspect.ArtifactIdentity{} }, want: "implementation"},
		"hardware":       {mutate: func(value *graphevidence.Profile) { value.Hardware = inspect.ArtifactIdentity{} }, want: "hardware"},
		"load":           {mutate: func(value *graphevidence.Profile) { value.Load = inspect.ArtifactIdentity{} }, want: "load"},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := profile
			test.mutate(&candidate)
			document.Profiles = []graphevidence.Profile{candidate}
			if _, err := graphevidence.Fingerprint(document); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestEvidenceParsingIsStrictAndCanonical(t *testing.T) {
	if _, err := graphevidence.ParseYAML("unknown.yaml", []byte(`apiVersion: openrealtime.ai/evidence/v1alpha1
graph: measured
profiles: []
credential: forbidden
`)); err == nil {
		t.Fatal("unknown evidence field was accepted")
	}
	if _, err := graphevidence.ParseJSON("trailing.json", []byte(`{"apiVersion":"openrealtime.ai/evidence/v1alpha1","graph":"measured","profiles":[]} {}`)); err == nil {
		t.Fatal("trailing JSON evidence was accepted")
	}
}

func FuzzEvidenceParsersNeverPanicAndSuccessfulDocumentsRefingerprint(f *testing.F) {
	f.Add([]byte(`apiVersion: openrealtime.ai/evidence/v1alpha1
graph: fuzz
profiles: []
`), false)
	f.Add([]byte(`{"apiVersion":"openrealtime.ai/evidence/v1alpha1","graph":"fuzz","profiles":[]}`), true)
	f.Add([]byte("profiles:\n  - credential: secret\n"), false)
	f.Fuzz(func(t *testing.T, source []byte, jsonEncoding bool) {
		var (
			document graphevidence.Document
			err      error
		)
		if jsonEncoding {
			document, err = graphevidence.ParseJSON("fuzz.json", source)
		} else {
			document, err = graphevidence.ParseYAML("fuzz.yaml", source)
		}
		if err != nil {
			return
		}
		if _, err := graphevidence.Fingerprint(document); err != nil {
			t.Fatalf("successfully parsed evidence no longer validates: %v", err)
		}
	})
}

func evidenceArtifact(id, revision string) inspect.ArtifactIdentity {
	return inspect.ArtifactIdentity{ID: id, Revision: revision, Digest: evidenceDigest(id + "\x00" + revision)}
}

func evidenceDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}
