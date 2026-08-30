package syntax_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func TestRecoveryRetainsOnlyCompleteSynchronizedStatements(t *testing.T) {
	source := []byte(`graph partial {
    test.Source :: source;
    source.out -> ;
    test.Sink :: sink;
    output done = sink.done;
`)
	result, err := syntax.Recover("partial.ortg", source, syntax.RecoveryLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || result.File.Graph.Name != "partial" ||
		len(result.File.Graph.Nodes()) != 2 || len(result.File.Graph.Boundaries()) != 1 ||
		len(result.File.Graph.Edges()) != 0 || len(result.Diagnostics) != 2 {
		t.Fatalf("recovery snapshot = %+v", result)
	}
	if result.File.Graph.Nodes()[0].Name != "source" || result.File.Graph.Nodes()[1].Name != "sink" {
		t.Fatalf("recovered nodes = %+v", result.File.Graph.Nodes())
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for iteration := 0; iteration < 50; iteration++ {
		again, err := syntax.Recover("partial.ortg", source, syntax.RecoveryLimits{})
		if err != nil {
			t.Fatal(err)
		}
		other, _ := json.Marshal(again)
		if string(other) != string(encoded) {
			t.Fatalf("recovery changed:\n%s\n%s", encoded, other)
		}
	}
}

func TestRecoveryStrictSuccessAndBounds(t *testing.T) {
	valid := []byte("graph exact {\n}\n")
	result, err := syntax.Recover("exact.ortg", valid, syntax.RecoveryLimits{})
	if err != nil || !result.Complete || len(result.Diagnostics) != 0 ||
		syntax.Format(result.File) != string(valid) {
		t.Fatalf("strict recovery = %+v, %v", result, err)
	}
	if _, err := syntax.Recover("bounded.ortg", []byte("graph x { a.b -> c.d; }"), syntax.RecoveryLimits{
		MaxTokens: 1, MaxStatements: 1, MaxDiagnostics: 1,
	}); err == nil {
		t.Fatal("recovery token bound was not enforced")
	}
	if _, err := syntax.Recover("bounded.ortg", []byte(strings.Repeat("x", 9)), syntax.RecoveryLimits{
		MaxSourceBytes: 8,
	}); err == nil {
		t.Fatal("recovery source bound was not enforced")
	}
	trailing, err := syntax.Recover("trailing.ortg", []byte("graph x {} trailing"), syntax.RecoveryLimits{})
	if err != nil || trailing.Complete || len(trailing.Diagnostics) == 0 {
		t.Fatalf("strict parse failure was not retained: %+v, %v", trailing, err)
	}
}

func FuzzRecoveryIsBoundedAndNeverPromotesToStrictParse(f *testing.F) {
	for _, seed := range [][]byte{
		[]byte("graph x {"),
		[]byte("graph x { a.b -> ; c.D :: n;"),
		[]byte{0xff, '{', ';', '}'},
		[]byte("graph exact {\n}\n"),
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		if len(source) > 4096 {
			t.Skip()
		}
		result, err := syntax.Recover("fuzz.ortg", source, syntax.RecoveryLimits{
			MaxTokens: 4096, MaxStatements: 256, MaxDiagnostics: 64,
		})
		if err != nil {
			return
		}
		if result.Complete {
			if _, err := syntax.Parse("fuzz.ortg", source); err != nil {
				t.Fatalf("recovery promoted invalid source: %v", err)
			}
		}
		for _, statement := range result.File.Graph.Statements {
			span := statementSpan(statement)
			if span.Start.Offset < 0 || span.End.Offset > len(source) || span.End.Offset <= span.Start.Offset ||
				source[span.End.Offset-1] != ';' {
				t.Fatalf("recovery admitted an unterminated/forged statement: %+v", statement)
			}
		}
	})
}

func statementSpan(statement syntax.Statement) syntax.Span {
	switch {
	case statement.Node != nil:
		return statement.Node.Span
	case statement.Edge != nil:
		return statement.Edge.Span
	case statement.Boundary != nil:
		return statement.Boundary.Span
	default:
		return syntax.Span{}
	}
}

func BenchmarkRecoverIncomplete(b *testing.B) {
	source := []byte(`graph partial {
    test.Source :: source;
    source.out -> ;
    test.Sink :: sink;
    output done = sink.done;
`)
	b.ReportAllocs()
	for range b.N {
		result, err := syntax.Recover("partial.ortg", source, syntax.RecoveryLimits{})
		if err != nil || len(result.File.Graph.Nodes()) != 2 {
			b.Fatalf("recovery = %+v, %v", result, err)
		}
	}
}
