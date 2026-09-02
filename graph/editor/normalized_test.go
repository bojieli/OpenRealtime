package editor

import (
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestNormalizedDocumentMutationsAreCanonicalAtomicAndStaleSafe(t *testing.T) {
	parsed, err := syntax.Parse("fixture.ortg", []byte(`graph normalized {
    test.Source :: source;
    test.Sink :: sink;
    edge optional = source.out -> sink.in;
    input start = source.start;
    output done = sink.done;
}
`))
	if err != nil {
		t.Fatal(err)
	}
	base := manifest.FromSyntax(parsed)
	encodings := []struct {
		name, path string
		marshal    func(manifest.Document) ([]byte, error)
	}{
		{name: "yaml", path: "fixture.yaml", marshal: manifest.MarshalYAML},
		{name: "yml", path: "fixture.yml", marshal: manifest.MarshalYAML},
		{name: "json", path: "fixture.json", marshal: manifest.MarshalJSON},
	}
	for _, encoding := range encodings {
		t.Run(encoding.name, func(t *testing.T) {
			source, err := encoding.marshal(base)
			if err != nil {
				t.Fatal(err)
			}
			document, err := AnalyzeNormalized(encoding.path, source, Limits{})
			if err != nil || document.Path() != encoding.path ||
				document.SourceDigest() == "" || string(document.Source()) != string(source) {
				t.Fatalf("normalized analysis = %+v, %v", document, err)
			}

			rename, err := document.RenameNodeID("source", "camera")
			if err != nil || rename.Path != encoding.path || rename.SourceDigest != document.SourceDigest() ||
				len(rename.Edits) != 1 || rename.Edits[0].OldText != string(source) {
				t.Fatalf("normalized rename = %+v, %v", rename, err)
			}
			renamed, err := ApplyEdits(source, rename)
			wantRename := cloneManifest(base)
			wantRename.Graph.Nodes[0].ID = "camera"
			wantRename.Graph.Edges[0].From = "camera.out"
			wantRename.Graph.Boundaries[0].Endpoint = "camera.start"
			wantRenamed, marshalErr := encoding.marshal(wantRename)
			if err != nil || marshalErr != nil || string(renamed) != string(wantRenamed) {
				t.Fatalf("normalized renamed source:\n%s\nwant:\n%s\nerrors: %v, %v",
					renamed, wantRenamed, err, marshalErr)
			}
			if _, err := AnalyzeNormalized(encoding.path, renamed, Limits{}); err != nil {
				t.Fatalf("renamed normalized source is not canonical: %v", err)
			}
			if _, err := ApplyEdits(append(source, ' '), rename); !errors.Is(err, ErrStalePosition) {
				t.Fatalf("stale normalized rename = %v", err)
			}
			noOp, err := document.RenameNodeID("source", "source")
			if err != nil || len(noOp.Edits) != 0 || noOp.SourceDigest != document.SourceDigest() {
				t.Fatalf("normalized no-op rename = %+v, %v", noOp, err)
			}

			remove, err := document.RemoveEdgeID("optional")
			if err != nil || len(remove.Edits) != 1 {
				t.Fatalf("normalized edge removal = %+v, %v", remove, err)
			}
			removed, err := ApplyEdits(source, remove)
			wantRemovedDocument := cloneManifest(base)
			wantRemovedDocument.Graph.Edges = nil
			wantRemoved, marshalErr := encoding.marshal(wantRemovedDocument)
			if err != nil || marshalErr != nil || string(removed) != string(wantRemoved) {
				t.Fatalf("normalized edge-removed source:\n%s\nwant:\n%s\nerrors: %v, %v",
					removed, wantRemoved, err, marshalErr)
			}

			withoutEdge, err := AnalyzeNormalized(encoding.path, removed, Limits{})
			if err != nil {
				t.Fatal(err)
			}
			create, err := withoutEdge.CreateEdgeID("restored",
				syntax.Endpoint{Node: "camera", Port: "out"},
				syntax.Endpoint{Node: "sink", Port: "in"}, syntax.Lossless)
			if !errors.Is(err, ErrInvalidEdgeMutation) || len(create.Edits) != 0 {
				t.Fatalf("creation with missing renamed node = %+v, %v", create, err)
			}
			create, err = withoutEdge.CreateEdgeID("restored",
				syntax.Endpoint{Node: "source", Port: "out"},
				syntax.Endpoint{Node: "sink", Port: "in"}, syntax.Lossy)
			if err != nil || len(create.Edits) != 1 {
				t.Fatalf("normalized edge creation = %+v, %v", create, err)
			}
			created, err := ApplyEdits(removed, create)
			wantCreatedDocument := cloneManifest(wantRemovedDocument)
			wantCreatedDocument.Graph.Edges = []manifest.Edge{{
				ID: "restored", From: "source.out", To: "sink.in", Delivery: "lossy",
			}}
			wantCreated, marshalErr := encoding.marshal(wantCreatedDocument)
			if err != nil || marshalErr != nil || string(created) != string(wantCreated) {
				t.Fatalf("normalized edge-created source:\n%s\nwant:\n%s\nerrors: %v, %v",
					created, wantCreated, err, marshalErr)
			}
			if _, err := AnalyzeNormalized(encoding.path, created, Limits{}); err != nil {
				t.Fatalf("edge-created normalized source is not canonical: %v", err)
			}
		})
	}
}

func TestNormalizedDocumentMutationsRejectNoncanonicalAmbiguousAndInvalidInputs(t *testing.T) {
	parsed, err := syntax.Parse("fixture.ortg", []byte(`graph normalized {
    test.Source :: source;
    test.Sink :: sink;
    edge optional = source.out -> sink.in;
}
`))
	if err != nil {
		t.Fatal(err)
	}
	source, err := manifest.MarshalJSON(manifest.FromSyntax(parsed))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := AnalyzeNormalized("fixture.json", append(source, '\n'), Limits{}); !errors.Is(err, ErrSyntaxUnavailable) {
		t.Fatalf("noncanonical normalized source = %v", err)
	}
	document, err := AnalyzeNormalized("fixture.json", source, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	for _, rename := range [][2]string{{"missing", "camera"}, {"source", "sink"}, {"source", "bad.name"}} {
		if _, err := document.RenameNodeID(rename[0], rename[1]); err == nil {
			t.Fatalf("invalid normalized rename %q -> %q succeeded", rename[0], rename[1])
		}
	}
	if _, err := document.RemoveEdgeID("missing"); !errors.Is(err, ErrInvalidEdgeMutation) {
		t.Fatalf("missing normalized edge removal = %v", err)
	}
	for _, invalid := range []struct {
		edge     string
		from, to syntax.Endpoint
		delivery syntax.Delivery
	}{
		{edge: "optional", from: syntax.Endpoint{Node: "source", Port: "out"},
			to: syntax.Endpoint{Node: "sink", Port: "in"}, delivery: syntax.Lossless},
		{edge: "bad.name", from: syntax.Endpoint{Node: "source", Port: "out"},
			to: syntax.Endpoint{Node: "sink", Port: "in"}, delivery: syntax.Lossless},
		{edge: "new", from: syntax.Endpoint{Node: "missing", Port: "out"},
			to: syntax.Endpoint{Node: "sink", Port: "in"}, delivery: syntax.Lossless},
		{edge: "new", from: syntax.Endpoint{Node: "source", Port: "out"},
			to: syntax.Endpoint{Node: "sink", Port: "in"}, delivery: "unknown"},
	} {
		if _, err := document.CreateEdgeID(invalid.edge, invalid.from, invalid.to, invalid.delivery); !errors.Is(err, ErrInvalidEdgeMutation) {
			t.Fatalf("invalid normalized edge creation %+v = %v", invalid, err)
		}
	}

	duplicate := manifest.FromSyntax(parsed)
	duplicate.Graph.Nodes = append(duplicate.Graph.Nodes, manifest.Node{ID: "source", Element: "test.Source"})
	duplicateSource, err := manifest.MarshalJSON(duplicate)
	if err != nil {
		t.Fatal(err)
	}
	ambiguous, err := AnalyzeNormalized("duplicate.json", duplicateSource, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ambiguous.RenameNodeID("source", "camera"); !errors.Is(err, ErrInvalidRename) {
		t.Fatalf("ambiguous normalized rename = %v", err)
	}
	if !strings.Contains(string(source), `"apiVersion": "openrealtime.ai/graph/v1alpha1"`) {
		t.Fatal("normalized JSON fixture is not the canonical manifest form")
	}
}
