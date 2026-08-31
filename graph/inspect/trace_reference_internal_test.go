package inspect

import (
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/ir"
)

// A live trace is only useful if every event can be tied back to the exact
// graph it came from, so the reference carried on each one has to be complete:
// the IR format the reader expects, a canonical ID, a positive revision, and a
// real fingerprint. None of those refusals was exercised, which means a trace
// naming no graph at all, or a graph at a format the reader cannot interpret,
// would have been accepted and then read as though it described this run.
func validGraphReference() GraphReference {
	return GraphReference{
		FormatVersion: ir.FormatVersion,
		ID:            "scenario-conversation",
		Revision:      1,
		Fingerprint:   "sha256:" + strings.Repeat("a", 64),
	}
}

func TestTraceGraphReferenceMustIdentifyExactlyOneGraph(t *testing.T) {
	t.Parallel()
	if err := validateGraphReference(validGraphReference()); err != nil {
		t.Fatalf("well-formed reference = %v, want accepted", err)
	}
	for _, test := range []struct {
		name string
		edit func(*GraphReference)
		want string
	}{
		{
			name: "IR format is not the one this reader understands",
			edit: func(reference *GraphReference) { reference.FormatVersion = ir.FormatVersion + 1 },
			want: "Graph IR format is",
		},
		{
			name: "no IR format at all",
			edit: func(reference *GraphReference) { reference.FormatVersion = 0 },
			want: "Graph IR format is",
		},
		{
			name: "no graph ID",
			edit: func(reference *GraphReference) { reference.ID = "" },
			want: "canonical ID",
		},
		{
			name: "graph ID carries surrounding space",
			edit: func(reference *GraphReference) { reference.ID = " scenario" },
			want: "canonical ID",
		},
		{
			name: "graph ID carries a newline",
			edit: func(reference *GraphReference) { reference.ID = "scenario\nother" },
			want: "canonical ID",
		},
		{
			name: "revision is zero",
			edit: func(reference *GraphReference) { reference.Revision = 0 },
			want: "positive revision",
		},
		{
			name: "fingerprint is not a digest",
			edit: func(reference *GraphReference) { reference.Fingerprint = "sha256:short" },
			want: "not a canonical SHA-256 digest",
		},
		{
			name: "fingerprint is absent",
			edit: func(reference *GraphReference) { reference.Fingerprint = "" },
			want: "not a canonical SHA-256 digest",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reference := validGraphReference()
			test.edit(&reference)
			err := validateGraphReference(reference)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("reference error = %v, want one containing %q", err, test.want)
			}
		})
	}
}
