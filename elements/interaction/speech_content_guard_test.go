package interaction

import (
	"strings"
	"testing"
)

func TestSpeechContentGuardAcrossEveryChunkBoundary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		input  string
		output string
	}{
		{
			name:  "tagged call",
			input: `<tool_call>{"name":"track_order","arguments":{"order_id":"A}\\\"B"}}</tool_call>`,
		},
		{
			name:   "case folded tag and leading whitespace",
			input:  " \n<TOOL_CALL> [ {\"name\":\"one\",\"parameters\":{}} ] </ToOl_CaLl>\t",
			output: " \n\t",
		},
		{
			name:   "safe suffix after tagged call",
			input:  `<tool_call>{"name":"lookup","arguments":{}}</tool_call>Done.`,
			output: "Done.",
		},
		{
			name: "closing tag inside quoted JSON is data",
			input: `<tool_call>{"name":"lookup","arguments":{"note":"literal </tool_call> and escaped \" quote"}}` +
				`</tool_call>Done.`,
			output: "Done.",
		},
		{
			name: "multiple tagged calls",
			input: `<tool_call>{"name":"one","arguments":{}}</tool_call>` +
				`<tool_call>{"name":"two","arguments":{}}</tool_call>`,
		},
		{
			name:   "direct bare call",
			input:  ` {"name":"track_order","arguments":{"order_id":"ABC123"}} `,
			output: "  ",
		},
		{
			name:  "nested provider call",
			input: `{"type":"function","function":{"name":"lookup","arguments":"{\"x\":1}"}}`,
		},
		{
			name:  "array of calls",
			input: `[{"name":"one","arguments":{}},{"function":{"name":"two","parameters":{}}}]`,
		},
		{
			name:   "tool call prefix",
			input:  `Tool call: {"name":"lookup","arguments":{"query":"x"}}Done.`,
			output: "Done.",
		},
		{
			name:   "fenced tool call",
			input:  "Before.\n```json\n{\"tool_calls\":[{\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}\n```\nAfter.",
			output: "Before.\n\nAfter.",
		},
		{
			name:   "combined prefix and fence",
			input:  "Tool call:\n```json\n{\"name\":\"lookup\",\"arguments\":{}}\n```Done.",
			output: "Done.",
		},
		{
			name: "multiple wrapper families",
			input: `<tool_call>{"name":"one","arguments":{}}</tool_call>` +
				`Tool call: {"name":"two","arguments":{}}` +
				"```json\n{\"name\":\"three\",\"parameters\":{}}\n```Done.",
			output: "Done.",
		},
		{
			name:   "standalone callable expression",
			input:  "track_order({\"order_id\":\"A\\\"B\",\"note\":\"braces }) are data\"})\nDone.",
			output: "\nDone.",
		},
		{
			name:   "function name tag",
			input:  `<function=track_order>{"order_id":"XYZ88","note":"</function> in data"}</function>Done.`,
			output: "Done.",
		},
		{
			name: "function call tag",
			input: `<function_call>{"name":"track_order","arguments":{"order_id":"XYZ88"}}` +
				`</function_call>Done.`,
			output: "Done.",
		},
		{
			name: "adjacent XML wrappers",
			input: `<function=one>{"x":1}</function>` +
				`<function_call>{"name":"two","arguments":{}}</function_call>Done.`,
			output: "Done.",
		},
		{
			name:   "ordinary prose",
			input:  "There are five cars. Say this now.",
			output: "There are five cars. Say this now.",
		},
		{
			name:   "tag discussion after prose",
			input:  "The <tool_call> tag wraps a JSON object.",
			output: "The <tool_call> tag wraps a JSON object.",
		},
		{
			name:   "opening tag discussion",
			input:  "<tool_call> is the literal tag.",
			output: "<tool_call> is the literal tag.",
		},
		{
			name:   "non tool JSON",
			input:  ` {"answer":"braces } and escaped quote \" remain data","count":2} follows`,
			output: ` {"answer":"braces } and escaped quote \" remain data","count":2} follows`,
		},
		{
			name:   "mixed array is not a call envelope",
			input:  `[{"name":"one","arguments":{}},{"answer":2}]`,
			output: `[{"name":"one","arguments":{}},{"answer":2}]`,
		},
		{
			name:   "ordinary fenced JSON",
			input:  "```json\n{\"answer\":\"show this object\"}\n```",
			output: "```json\n{\"answer\":\"show this object\"}\n```",
		},
		{
			name:   "prefix discussion",
			input:  "Tool call: is a label, not a request.",
			output: "Tool call: is a label, not a request.",
		},
		{
			name:   "callable expression embedded in prose",
			input:  `For documentation, track_order({"order_id":"XYZ88"}) is an example.`,
			output: `For documentation, track_order({"order_id":"XYZ88"}) is an example.`,
		},
		{
			name:   "standalone-looking expression followed by prose",
			input:  `track_order({"order_id":"XYZ88"}) is how the syntax looks.`,
			output: `track_order({"order_id":"XYZ88"}) is how the syntax looks.`,
		},
		{
			name:   "function tag discussion",
			input:  `<function=track_order> is an opening tag, not a call.`,
			output: `<function=track_order> is an opening tag, not a call.`,
		},
		{
			name:   "incomplete bare JSON has no strict shape",
			input:  `{"name":"lookup","arguments":`,
			output: `{"name":"lookup","arguments":`,
		},
		{
			name:   "ambiguous marker prefix",
			input:  "  <tool",
			output: "  <tool",
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			// Every possible two-chunk boundary must make the same decision.
			for split := 0; split <= len(test.input); split++ {
				guard := newControlSerializationFilter(4<<20, 1024)
				first, _ := guard.push(test.input[:split])
				second, _ := guard.push(test.input[split:])
				beforeFlush := guard
				got := first + second
				flushed, _ := guard.finish()
				got += flushed
				if got != test.output {
					t.Fatalf("split %d (%q | %q): output = %q (%q + %q), want %q; state before flush: %+v",
						split, test.input[:split], test.input[split:], got, first, second,
						test.output, beforeFlush)
				}
			}

			// One-byte provider deltas exercise every boundary in one run.
			guard := newControlSerializationFilter(4<<20, 1024)
			var got strings.Builder
			for index := range test.input {
				part, _ := guard.push(test.input[index : index+1])
				got.WriteString(part)
			}
			part, _ := guard.finish()
			got.WriteString(part)
			if got.String() != test.output {
				t.Fatalf("one-byte chunks: output = %q, want %q", got.String(), test.output)
			}
		})
	}
}

func TestSpeechContentGuardSuppressesTruncatedTaggedCallAndResets(t *testing.T) {
	t.Parallel()
	guard := newControlSerializationFilter(4<<20, 1024)
	if got, _ := guard.push(" \n<tool_call> {\"name\":\"lookup\","); got != " \n" {
		t.Fatalf("ordinary leading whitespace changed before terminal: %q", got)
	}
	if got, _ := guard.finish(); got != "" {
		t.Fatalf("truncated tagged control content escaped at terminal: %q", got)
	}
	first, _ := guard.push("Ordinary next run.")
	second, _ := guard.finish()
	if got := first + second; got != "Ordinary next run." {
		t.Fatalf("guard did not reset after terminal: %q", got)
	}
}

func TestControlSerializationFilterReportsDigestWithoutPayload(t *testing.T) {
	t.Parallel()
	const serialized = `<tool_call>{"name":"lookup","arguments":{"secret":"do-not-copy"}}</tool_call>`
	filter := newControlSerializationFilter(4096, 4)
	safe, spans := filter.push(serialized)
	tail, terminal := filter.finish()
	spans = append(spans, terminal...)
	if safe+tail != "" || len(spans) != 1 {
		t.Fatalf("filter output = %q, spans = %+v", safe+tail, spans)
	}
	want := spanFor(ControlSyntaxTagged, ControlSerializationComplete, []byte(serialized))
	if spans[0] != want || strings.Contains(spans[0].SHA256, "secret") {
		t.Fatalf("quarantine evidence = %+v, want %+v", spans[0], want)
	}
}

func TestControlSerializationFilterBoundsRecognizedCandidates(t *testing.T) {
	t.Parallel()
	filter := newControlSerializationFilter(32, 4)
	input := `<tool_call>{"name":"lookup","arguments":{"long":"` + strings.Repeat("x", 128)
	safe, spans := filter.push(input)
	tail, terminal := filter.finish()
	spans = append(spans, terminal...)
	if safe+tail != "" || len(spans) != 1 ||
		spans[0].Disposition != ControlSerializationTooLarge || spans[0].Bytes != len(input) {
		t.Fatalf("bounded filter output = %q, spans = %+v", safe+tail, spans)
	}

	const compact = `<tool_call>{"name":"x","arguments":{}}</tool_call>`
	filter = newControlSerializationFilter(len(compact), 4)
	suffix := strings.Repeat("ordinary ", 1024)
	safe, spans = filter.push(compact + suffix)
	tail, terminal = filter.finish()
	spans = append(spans, terminal...)
	if safe+tail != suffix || len(spans) != 1 ||
		spans[0].Disposition != ControlSerializationComplete || spans[0].Bytes != len(compact) {
		t.Fatalf("bounded candidate swallowed safe suffix: output bytes=%d spans=%+v", len(safe+tail), spans)
	}
}

func TestControlSerializationFilterHandlesMalformedAndTruncatedForms(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		input       string
		output      string
		syntax      ControlSerializationSyntax
		disposition ControlSerializationDisposition
		spans       int
	}{
		{
			name: "truncated tagged", input: `<tool_call>{"name":"lookup","arguments":`,
			syntax: ControlSyntaxTagged, disposition: ControlSerializationTruncated, spans: 1,
		},
		{
			name: "malformed tagged", input: `<tool_call>{"name":"lookup","arguments":[}] tail`,
			syntax: ControlSyntaxTagged, disposition: ControlSerializationMalformed, spans: 1,
		},
		{
			name: "truncated function tag", input: `<function=lookup>{"query":"x"}`,
			syntax: ControlSyntaxFunctionTag, disposition: ControlSerializationTruncated, spans: 1,
		},
		{
			name: "truncated function call tag", input: `<function_call>{"name":"lookup"`,
			syntax: ControlSyntaxFunctionCallTag, disposition: ControlSerializationTruncated, spans: 1,
		},
		{
			name: "truncated callable", input: `track_order({"order_id":"XYZ88"}`,
			syntax: ControlSyntaxCallableExpression, disposition: ControlSerializationTruncated, spans: 1,
		},
		{
			name: "malformed callable", input: `track_order({"order_id":[}) and untrusted tail`,
			syntax: ControlSyntaxCallableExpression, disposition: ControlSerializationMalformed, spans: 1,
		},
		{
			name: "incomplete bare JSON remains prose", input: `{"name":"lookup","arguments":`,
			output: `{"name":"lookup","arguments":`, spans: 0,
		},
		{
			name:  "malformed generic fence remains prose",
			input: "```json\n{\"name\": [}\n```", output: "```json\n{\"name\": [}\n```", spans: 0,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var filter = newControlSerializationFilter(4096, 16)
			var safe strings.Builder
			var spans []quarantinedControlSpan
			for index := range test.input {
				part, found := filter.push(test.input[index : index+1])
				safe.WriteString(part)
				spans = append(spans, found...)
			}
			part, found := filter.finish()
			safe.WriteString(part)
			spans = append(spans, found...)
			if safe.String() != test.output || len(spans) != test.spans {
				t.Fatalf("output = %q, spans = %+v; want %q and %d", safe.String(), spans, test.output, test.spans)
			}
			if test.spans == 1 &&
				(spans[0].Syntax != test.syntax || spans[0].Disposition != test.disposition ||
					spans[0].Bytes != len(test.input)) {
				t.Fatalf("span = %+v, want %q/%q/%d", spans[0], test.syntax, test.disposition, len(test.input))
			}
		})
	}
}

func TestControlSerializationFilterBoundsUnrecognizedFunctionIdentifiers(t *testing.T) {
	t.Parallel()
	for _, input := range []string{
		strings.Repeat("a", maximumSerializedFunctionNameBytes+1) + `({"x":1})`,
		functionTagOpen + strings.Repeat("a", maximumSerializedFunctionNameBytes+1) +
			`>{"x":1}</function>`,
	} {
		filter := newControlSerializationFilter(4096, 16)
		var safe strings.Builder
		var spans []quarantinedControlSpan
		releasedBeforeTerminal := false
		maximumRetained := 0
		for index := range input {
			part, found := filter.push(input[index : index+1])
			safe.WriteString(part)
			spans = append(spans, found...)
			if part != "" && index+1 < len(input) {
				releasedBeforeTerminal = true
			}
			retained := len(filter.pending)
			if filter.active != nil {
				retained += len(filter.active.raw)
			}
			maximumRetained = max(maximumRetained, retained)
		}
		part, found := filter.finish()
		safe.WriteString(part)
		spans = append(spans, found...)
		if safe.String() != input || len(spans) != 0 || !releasedBeforeTerminal {
			t.Fatalf("long identifier output bytes=%d spans=%+v early=%t",
				len(safe.String()), spans, releasedBeforeTerminal)
		}
		if maximumRetained > len(functionTagOpen)+maximumSerializedFunctionNameBytes {
			t.Fatalf("long identifier retained %d bytes", maximumRetained)
		}
	}
}

func TestControlSerializationFilterBoundsFramingAndSuffixAcrossEveryPartition(t *testing.T) {
	t.Parallel()
	callableProse := "lookup" + strings.Repeat(" ", 40) +
		`({"name":"lookup","arguments":{}})`
	explicitPrefix := toolCallOpen + strings.Repeat(" ", 40) +
		`{"name":"lookup","arguments":{}}</tool_call>ordinary tail`
	wrappedJSON := toolCallOpen + `{"name":"lookup","arguments":{}}`
	wrappedSuffix := wrappedJSON + strings.Repeat(" ", 9) + toolCallClose + "ordinary tail"
	genericLongFraming := toolCallJSONFenceOpen + strings.Repeat(" ", 80) +
		`{"answer":"ordinary"}` + toolCallJSONFenceEnd
	genericLongSuffix := toolCallJSONFenceOpen + `{"answer":"ordinary"}` +
		strings.Repeat(" ", 256) + toolCallJSONFenceEnd
	oversizedGenericTool := toolCallJSONFenceOpen +
		`{"name":"lookup","arguments":{"long":"` + strings.Repeat("x", 128)

	tests := []struct {
		name      string
		input     string
		maxBytes  int
		output    string
		syntax    ControlSerializationSyntax
		dispose   ControlSerializationDisposition
		spanCount int
	}{
		{
			name: "ambiguous callable whitespace is prose", input: callableProse,
			maxBytes: 16, output: callableProse,
		},
		{
			name: "explicit marker whitespace fails closed", input: explicitPrefix,
			maxBytes: 32, syntax: ControlSyntaxTagged,
			dispose: ControlSerializationTooLarge, spanCount: 1,
		},
		{
			name: "recognized wrapper suffix fails closed", input: wrappedSuffix,
			maxBytes: len(wrappedJSON) + 8, syntax: ControlSyntaxTagged,
			dispose: ControlSerializationTooLarge, spanCount: 1,
		},
		{
			name: "generic fence framing remains prose", input: genericLongFraming,
			maxBytes: 32, output: genericLongFraming,
		},
		{
			name: "generic non-tool suffix remains prose", input: genericLongSuffix,
			maxBytes: 64, output: genericLongSuffix,
		},
		{
			name: "generic tool-shaped body fails closed", input: oversizedGenericTool,
			maxBytes: 64, syntax: ControlSyntaxJSONFence,
			dispose: ControlSerializationTooLarge, spanCount: 1,
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			for split := 0; split <= len(test.input); split++ {
				filter := newControlSerializationFilter(test.maxBytes, 16)
				var safe strings.Builder
				var spans []quarantinedControlSpan
				part, found := filter.push(test.input[:split])
				safe.WriteString(part)
				spans = append(spans, found...)
				part, found = filter.push(test.input[split:])
				safe.WriteString(part)
				spans = append(spans, found...)
				part, found = filter.finish()
				safe.WriteString(part)
				spans = append(spans, found...)
				assertBoundedFilterDecision(t, split, test.input, safe.String(), spans,
					test.output, test.syntax, test.dispose, test.spanCount)
			}

			filter := newControlSerializationFilter(test.maxBytes, 16)
			var safe strings.Builder
			var spans []quarantinedControlSpan
			maximumRetained := 0
			for index := range test.input {
				part, found := filter.push(test.input[index : index+1])
				safe.WriteString(part)
				spans = append(spans, found...)
				retained := len(filter.pending)
				if filter.active != nil {
					retained += len(filter.active.raw)
				}
				maximumRetained = max(maximumRetained, retained)
			}
			part, found := filter.finish()
			safe.WriteString(part)
			spans = append(spans, found...)
			assertBoundedFilterDecision(t, -1, test.input, safe.String(), spans,
				test.output, test.syntax, test.dispose, test.spanCount)
			fixedGrammarAllowance := len(functionTagOpen) + maximumSerializedFunctionNameBytes + 1
			if maximumRetained > max(test.maxBytes+1, fixedGrammarAllowance) {
				t.Fatalf("one-byte input retained %d bytes with bound %d",
					maximumRetained, test.maxBytes)
			}
		})
	}
}

func assertBoundedFilterDecision(
	t *testing.T, split int, input, output string, spans []quarantinedControlSpan,
	wantOutput string, syntax ControlSerializationSyntax,
	disposition ControlSerializationDisposition, spanCount int,
) {
	t.Helper()
	if output != wantOutput || len(spans) != spanCount {
		t.Fatalf("split %d output = %q spans = %+v, want %q and %d spans",
			split, output, spans, wantOutput, spanCount)
	}
	if spanCount == 1 && (spans[0].Syntax != syntax ||
		spans[0].Disposition != disposition || spans[0].Bytes != len(input)) {
		t.Fatalf("split %d span = %+v, want %q/%q/%d bytes",
			split, spans[0], syntax, disposition, len(input))
	}
}
