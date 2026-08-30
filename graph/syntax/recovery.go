package syntax

import (
	"errors"
	"fmt"
	"strings"
)

// RecoveryLimits bounds the lexer and synchronization work used for an
// incomplete editor buffer. Recovery is never used by Parse or compilation.
type RecoveryLimits struct {
	MaxSourceBytes int
	MaxTokens      int
	MaxStatements  int
	MaxDiagnostics int
}

// DefaultRecoveryLimits returns interactive, independently bounded defaults.
func DefaultRecoveryLimits() RecoveryLimits {
	return RecoveryLimits{
		MaxSourceBytes: 1 << 20,
		MaxTokens:      131_072,
		MaxStatements:  16_384,
		MaxDiagnostics: 256,
	}
}

// Recovery is a best-effort immutable syntax snapshot for language tooling.
// Complete is true only when the strict parser accepted the entire source.
// A recovered File must never be passed to the graph compiler.
type Recovery struct {
	File        File
	Complete    bool
	Diagnostics []Error
}

// Recover lexes an incomplete .ortg buffer and retains only complete,
// semicolon-terminated statements. It synchronizes at semicolons and the graph
// closing brace, so one malformed statement cannot manufacture a neighboring
// declaration. Strict Parse remains the only compilation boundary.
func Recover(path string, source []byte, limits RecoveryLimits) (Recovery, error) {
	resolved, err := normalizeRecoveryLimits(limits)
	if err != nil {
		return Recovery{}, err
	}
	if len(source) > resolved.MaxSourceBytes {
		return Recovery{}, fmt.Errorf("recover .ortg: source bytes exceed %d", resolved.MaxSourceBytes)
	}
	tokens, lexical, err := recoveryTokens(path, source, resolved)
	if err != nil {
		return Recovery{}, err
	}
	var strictErr error
	if len(lexical) == 0 {
		if file, parseErr := Parse(path, source); parseErr == nil {
			if len(file.Graph.Statements) > resolved.MaxStatements {
				return Recovery{}, fmt.Errorf("recover .ortg: statement count exceeds %d", resolved.MaxStatements)
			}
			return Recovery{File: file, Complete: true, Diagnostics: []Error{}}, nil
		} else {
			strictErr = parseErr
		}
	}
	result := Recovery{
		File: File{Path: path}, Diagnostics: append([]Error(nil), lexical...),
	}
	appendDiagnostic := func(failure Error) error {
		if len(result.Diagnostics) >= resolved.MaxDiagnostics {
			return fmt.Errorf("recover .ortg: diagnostic count exceeds %d", resolved.MaxDiagnostics)
		}
		result.Diagnostics = append(result.Diagnostics, failure)
		return nil
	}

	graphIndex := -1
	for index, current := range tokens {
		if current.kind == tokenIdentifier && current.text == "graph" {
			graphIndex = index
			break
		}
	}
	if graphIndex < 0 {
		position := Position{Line: 1, Column: 1}
		if len(tokens) != 0 {
			position = tokens[0].span.Start
		}
		if err := appendDiagnostic(Error{Path: path, Span: Span{Start: position, End: position}, Message: "expected graph declaration"}); err != nil {
			return Recovery{}, err
		}
		return result, nil
	}
	graphToken := tokens[graphIndex]
	cursor := graphIndex + 1
	graph := Graph{Span: Span{Start: graphToken.span.Start, End: graphToken.span.End}}
	if cursor < len(tokens) && tokens[cursor].kind == tokenIdentifier {
		graph.Name = tokens[cursor].text
		graph.Span.End = tokens[cursor].span.End
		cursor++
	} else {
		span := graphToken.span
		if cursor < len(tokens) {
			span = tokens[cursor].span
		}
		if err := appendDiagnostic(Error{Path: path, Span: span, Message: "expected a graph name"}); err != nil {
			return Recovery{}, err
		}
	}
	for cursor < len(tokens) && tokens[cursor].kind != tokenLeftBrace && tokens[cursor].kind != tokenEOF {
		cursor++
	}
	if cursor >= len(tokens) || tokens[cursor].kind != tokenLeftBrace {
		if err := appendDiagnostic(Error{Path: path, Span: graph.Span, Message: "expected { after graph name"}); err != nil {
			return Recovery{}, err
		}
		result.File.Graph = graph
		return result, nil
	}
	graph.Span.End = tokens[cursor].span.End
	cursor++

	for cursor < len(tokens) {
		for cursor < len(tokens) && tokens[cursor].kind == tokenComment {
			cursor++
		}
		if cursor >= len(tokens) || tokens[cursor].kind == tokenEOF {
			break
		}
		if tokens[cursor].kind == tokenRightBrace {
			graph.Span.End = tokens[cursor].span.End
			cursor++
			break
		}
		start := cursor
		for cursor < len(tokens) && tokens[cursor].kind != tokenSemicolon &&
			tokens[cursor].kind != tokenRightBrace && tokens[cursor].kind != tokenEOF {
			cursor++
		}
		terminated := cursor < len(tokens) && tokens[cursor].kind == tokenSemicolon
		end := cursor
		if terminated {
			end++
		}
		chunk := tokens[start:end]
		if len(chunk) != 0 {
			if !terminated {
				span := Span{Start: chunk[0].span.Start, End: chunk[len(chunk)-1].span.End}
				if err := appendDiagnostic(Error{Path: path, Span: span, Message: "incomplete statement; expected ;"}); err != nil {
					return Recovery{}, err
				}
			} else if len(graph.Statements) >= resolved.MaxStatements {
				return Recovery{}, fmt.Errorf("recover .ortg: statement count exceeds %d", resolved.MaxStatements)
			} else if statement, parseErr := recoverStatement(path, chunk); parseErr != nil {
				var failure *Error
				if errors.As(parseErr, &failure) {
					if err := appendDiagnostic(*failure); err != nil {
						return Recovery{}, err
					}
				} else {
					return Recovery{}, parseErr
				}
			} else {
				graph.Statements = append(graph.Statements, statement)
				graph.Span.End = chunk[len(chunk)-1].span.End
			}
		}
		if terminated {
			cursor++
		}
	}
	if cursor == 0 || cursor > len(tokens) || tokens[cursor-1].kind != tokenRightBrace {
		end := graph.Span.End
		if len(tokens) != 0 {
			end = tokens[len(tokens)-1].span.End
		}
		if err := appendDiagnostic(Error{Path: path, Span: Span{Start: end, End: end}, Message: "incomplete graph; expected }"}); err != nil {
			return Recovery{}, err
		}
	}
	if len(result.Diagnostics) == 0 && strictErr != nil {
		var failure *Error
		if errors.As(strictErr, &failure) {
			if err := appendDiagnostic(*failure); err != nil {
				return Recovery{}, err
			}
		}
	}
	result.File.Graph = graph
	return result, nil
}

func normalizeRecoveryLimits(value RecoveryLimits) (RecoveryLimits, error) {
	defaults := DefaultRecoveryLimits()
	fields := []struct {
		value *int
		name  string
		def   int
		hard  int
	}{
		{&value.MaxSourceBytes, "source bytes", defaults.MaxSourceBytes, 16 << 20},
		{&value.MaxTokens, "tokens", defaults.MaxTokens, 1 << 20},
		{&value.MaxStatements, "statements", defaults.MaxStatements, 1 << 20},
		{&value.MaxDiagnostics, "diagnostics", defaults.MaxDiagnostics, 65_536},
	}
	for _, field := range fields {
		if *field.value == 0 {
			*field.value = field.def
		}
		if *field.value < 1 || *field.value > field.hard {
			return RecoveryLimits{}, fmt.Errorf("recover .ortg: max %s must be in 1..%d", field.name, field.hard)
		}
	}
	return value, nil
}

func recoveryTokens(path string, source []byte, limits RecoveryLimits) ([]token, []Error, error) {
	scanner := newLexer(path, source)
	result := make([]token, 0, min(len(source)/2+1, limits.MaxTokens))
	diagnostics := make([]Error, 0)
	for {
		before := scanner.offset
		current, err := scanner.next()
		if err != nil {
			var failure *Error
			if !errors.As(err, &failure) {
				return nil, nil, err
			}
			if len(diagnostics) >= limits.MaxDiagnostics {
				return nil, nil, fmt.Errorf("recover .ortg: diagnostic count exceeds %d", limits.MaxDiagnostics)
			}
			diagnostics = append(diagnostics, *failure)
			if scanner.offset == before && scanner.offset < len(source) {
				scanner.advanceBytes(1)
			}
			continue
		}
		if current.kind == tokenComment {
			continue
		}
		if len(result) >= limits.MaxTokens {
			return nil, nil, fmt.Errorf("recover .ortg: token count exceeds %d", limits.MaxTokens)
		}
		result = append(result, current)
		if current.kind == tokenEOF {
			return result, diagnostics, nil
		}
	}
}

func recoverStatement(path string, tokens []token) (Statement, error) {
	span := Span{Start: tokens[0].span.Start, End: tokens[len(tokens)-1].span.End}
	fail := func(message string) (Statement, error) {
		return Statement{}, &Error{Path: path, Span: span, Message: message}
	}
	if tokens[len(tokens)-1].kind != tokenSemicolon {
		return fail("incomplete statement; expected ;")
	}
	body := tokens[:len(tokens)-1]
	if len(body) == 0 {
		return fail("empty graph statement")
	}
	if body[0].kind == tokenIdentifier && (body[0].text == "input" || body[0].text == "output") {
		if len(body) != 6 || body[1].kind != tokenIdentifier || body[2].kind != tokenAssign {
			return fail("invalid boundary declaration")
		}
		endpoint, ok := recoverEndpoint(body[3:])
		if !ok {
			return fail("boundary endpoint must be instance.port")
		}
		direction := BoundaryInput
		if body[0].text == "output" {
			direction = BoundaryOutput
		}
		return Statement{Boundary: &Boundary{
			Direction: direction, Name: body[1].text, Endpoint: endpoint, Span: span,
		}}, nil
	}
	if body[0].kind == tokenIdentifier && body[0].text == "edge" {
		if len(body) != 10 || body[1].kind != tokenIdentifier || body[2].kind != tokenAssign {
			return fail("invalid named edge declaration")
		}
		return recoverEdge(path, span, body[3:], body[1].text)
	}
	declare := -1
	arrow := -1
	for index, current := range body {
		switch current.kind {
		case tokenDeclare:
			declare = index
		case tokenLossless, tokenLossy:
			arrow = index
		}
	}
	if declare >= 0 {
		parts, ok := recoverQualified(body[:declare])
		if !ok || len(parts) < 2 || declare+2 != len(body) || body[declare+1].kind != tokenIdentifier {
			return fail("invalid element declaration")
		}
		return Statement{Node: &Node{
			Element: strings.Join(parts, "."), Name: body[declare+1].text, Span: span,
		}}, nil
	}
	if arrow >= 0 {
		return recoverEdge(path, span, body, "")
	}
	return fail("expected an element, edge, input, or output declaration")
}

func recoverEdge(path string, span Span, body []token, name string) (Statement, error) {
	arrow := -1
	for index, current := range body {
		if current.kind == tokenLossless || current.kind == tokenLossy {
			if arrow >= 0 {
				return Statement{}, &Error{Path: path, Span: span, Message: "edge has more than one delivery arrow"}
			}
			arrow = index
		}
	}
	if arrow < 0 {
		return Statement{}, &Error{Path: path, Span: span, Message: "edge requires -> or =>"}
	}
	from, fromOK := recoverEndpoint(body[:arrow])
	to, toOK := recoverEndpoint(body[arrow+1:])
	if !fromOK || !toOK {
		return Statement{}, &Error{Path: path, Span: span, Message: "edge endpoints must be instance.port"}
	}
	delivery := Lossless
	if body[arrow].kind == tokenLossy {
		delivery = Lossy
	}
	return Statement{Edge: &Edge{Name: name, From: from, To: to, Delivery: delivery, Span: span}}, nil
}

func recoverEndpoint(tokens []token) (Endpoint, bool) {
	if len(tokens) != 3 || tokens[0].kind != tokenIdentifier ||
		tokens[1].kind != tokenDot || tokens[2].kind != tokenIdentifier {
		return Endpoint{}, false
	}
	return Endpoint{
		Node: tokens[0].text, Port: tokens[2].text,
		Span: Span{Start: tokens[0].span.Start, End: tokens[2].span.End},
	}, true
}

func recoverQualified(tokens []token) ([]string, bool) {
	if len(tokens) == 0 || len(tokens)%2 == 0 {
		return nil, false
	}
	result := make([]string, 0, (len(tokens)+1)/2)
	for index, current := range tokens {
		if index%2 == 0 {
			if current.kind != tokenIdentifier {
				return nil, false
			}
			result = append(result, current.text)
		} else if current.kind != tokenDot {
			return nil, false
		}
	}
	return result, true
}
