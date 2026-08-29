package syntax_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const example = `// Reference graph.
import "policies/voice.ortg" as voice;

graph fast_and_deliberative_voice {
    interaction.Opportunity::opportunity;
    flow.Tee :: trigger_fork;
    cognition.TextModel :: fast;
    cognition.TextModel :: deliberative;

    // One branch per consumer; no count or index.
    opportunity.generate->trigger_fork.in;
    trigger_fork.out -> fast.trigger;
    trigger_fork.out -> deliberative.trigger;
    edge telemetry = fast.events => observer.events;

    input audio = opportunity.audio;
    output text = fast.text;
}
`

func TestParseAndFormatCompactGraph(t *testing.T) {
	file, err := syntax.Parse("example.ortg", []byte(example))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(file.Graph.Nodes()), 4; got != want {
		t.Fatalf("nodes = %d, want %d", got, want)
	}
	if got, want := len(file.Graph.Edges()), 4; got != want {
		t.Fatalf("edges = %d, want %d", got, want)
	}
	if got, want := len(file.Graph.Boundaries()), 2; got != want {
		t.Fatalf("boundaries = %d, want %d", got, want)
	}
	formatted := syntax.Format(file)
	for _, required := range []string{
		"interaction.Opportunity :: opportunity;",
		"opportunity.generate -> trigger_fork.in;",
		"edge telemetry = fast.events => observer.events;",
		"// One branch per consumer; no count or index.",
	} {
		if !strings.Contains(formatted, required) {
			t.Fatalf("formatted graph is missing %q:\n%s", required, formatted)
		}
	}
	second, err := syntax.Parse("formatted.ortg", []byte(formatted))
	if err != nil {
		t.Fatalf("formatted source did not parse: %v\n%s", err, formatted)
	}
	if got := syntax.Format(second); got != formatted {
		t.Fatalf("formatter is not idempotent:\nfirst:\n%s\nsecond:\n%s", formatted, got)
	}
}

func TestSyntaxErrorCarriesPathAndLocation(t *testing.T) {
	_, err := syntax.Parse("broken.ortg", []byte("graph bad {\n  flow.Tee :: fork\n}\n"))
	if err == nil {
		t.Fatal("expected missing semicolon to fail")
	}
	var diagnostic *syntax.Error
	if !errors.As(err, &diagnostic) {
		t.Fatalf("error type = %T, want *syntax.Error", err)
	}
	if diagnostic.Path != "broken.ortg" || diagnostic.Span.Start.Line != 3 {
		t.Fatalf("unexpected diagnostic: %+v", diagnostic)
	}
}

func TestEndpointRequiresInstanceAndPort(t *testing.T) {
	_, err := syntax.Parse("broken.ortg", []byte(`graph bad {
    flow.Tee :: fork;
    fork -> fork.in;
}`))
	if err == nil || !strings.Contains(err.Error(), "instance.port") {
		t.Fatalf("error = %v, want endpoint diagnostic", err)
	}
}

func TestBlockAndHashCommentsAreAccepted(t *testing.T) {
	source := `/* graph docs */
graph comments {
    # node docs
    flow.Tee :: fork;
}`
	file, err := syntax.Parse("comments.ortg", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	formatted := syntax.Format(file)
	if !strings.Contains(formatted, "// graph docs") || !strings.Contains(formatted, "// node docs") {
		t.Fatalf("comments were not retained:\n%s", formatted)
	}
}
