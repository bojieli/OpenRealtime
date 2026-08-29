package syntax

import (
	"fmt"
	"strings"
)

// Error is a source-mapped syntax diagnostic.
type Error struct {
	Path    string
	Span    Span
	Message string
}

func (failure *Error) Error() string {
	location := fmt.Sprintf("%d:%d", failure.Span.Start.Line, failure.Span.Start.Column)
	if failure.Path != "" {
		location = failure.Path + ":" + location
	}
	return location + ": " + failure.Message
}

type parser struct {
	lexer    *lexer
	current  token
	comments []string
}

// Parse parses one complete .ortg file.
func Parse(path string, source []byte) (File, error) {
	reader := &parser{lexer: newLexer(path, source)}
	if err := reader.advance(); err != nil {
		return File{}, err
	}
	return reader.parseFile(path)
}

func (reader *parser) parseFile(path string) (File, error) {
	file := File{Path: path}
	for reader.keyword("import") {
		comments := reader.takeComments()
		start := reader.current.span.Start
		if err := reader.advance(); err != nil {
			return File{}, err
		}
		pathToken, err := reader.expect(tokenString, "an import path string")
		if err != nil {
			return File{}, err
		}
		declaration := Import{Path: pathToken.text, Comments: comments}
		if reader.keyword("as") {
			if err := reader.advance(); err != nil {
				return File{}, err
			}
			alias, err := reader.expect(tokenIdentifier, "an import alias")
			if err != nil {
				return File{}, err
			}
			declaration.Alias = alias.text
		}
		end, err := reader.expect(tokenSemicolon, ";")
		if err != nil {
			return File{}, err
		}
		declaration.Span = Span{Start: start, End: end.span.End}
		file.Imports = append(file.Imports, declaration)
	}

	comments := reader.takeComments()
	if !reader.keyword("graph") {
		return File{}, reader.unexpected("graph")
	}
	start := reader.current.span.Start
	if err := reader.advance(); err != nil {
		return File{}, err
	}
	name, err := reader.expect(tokenIdentifier, "a graph name")
	if err != nil {
		return File{}, err
	}
	if _, err := reader.expect(tokenLeftBrace, "{"); err != nil {
		return File{}, err
	}
	graph := Graph{Name: name.text, Comments: comments}
	for reader.current.kind != tokenRightBrace && reader.current.kind != tokenEOF {
		statement, err := reader.parseStatement()
		if err != nil {
			return File{}, err
		}
		graph.Statements = append(graph.Statements, statement)
	}
	closing, err := reader.expect(tokenRightBrace, "}")
	if err != nil {
		return File{}, err
	}
	graph.Span = Span{Start: start, End: closing.span.End}
	graph.TrailingComments = reader.takeComments()
	if reader.current.kind != tokenEOF {
		return File{}, reader.unexpected("end of file")
	}
	file.Graph = graph
	return file, nil
}

func (reader *parser) parseStatement() (Statement, error) {
	comments := reader.takeComments()
	switch {
	case reader.keyword("input"):
		boundary, err := reader.parseBoundary(BoundaryInput, comments)
		return Statement{Boundary: boundary}, err
	case reader.keyword("output"):
		boundary, err := reader.parseBoundary(BoundaryOutput, comments)
		return Statement{Boundary: boundary}, err
	case reader.keyword("edge"):
		edge, err := reader.parseNamedEdge(comments)
		return Statement{Edge: edge}, err
	case reader.current.kind != tokenIdentifier:
		return Statement{}, reader.unexpected("an element, edge, input, or output declaration")
	}

	start := reader.current.span.Start
	qualified, spans, err := reader.parseQualified()
	if err != nil {
		return Statement{}, err
	}
	switch reader.current.kind {
	case tokenDeclare:
		if len(qualified) < 2 {
			return Statement{}, reader.errorAt(spans[0], "element references must be namespace-qualified")
		}
		if err := reader.advance(); err != nil {
			return Statement{}, err
		}
		name, err := reader.expect(tokenIdentifier, "an element instance name")
		if err != nil {
			return Statement{}, err
		}
		end, err := reader.expect(tokenSemicolon, ";")
		if err != nil {
			return Statement{}, err
		}
		return Statement{Node: &Node{
			Element: strings.Join(qualified, "."), Name: name.text, Comments: comments,
			Span: Span{Start: start, End: end.span.End},
		}}, nil
	case tokenLossless, tokenLossy:
		from, err := reader.endpointFromQualified(qualified, spans)
		if err != nil {
			return Statement{}, err
		}
		edge, err := reader.parseEdgeTail(from, "", comments, start)
		return Statement{Edge: edge}, err
	default:
		return Statement{}, reader.unexpected("::, ->, or =>")
	}
}

func (reader *parser) parseNamedEdge(comments []string) (*Edge, error) {
	start := reader.current.span.Start
	if err := reader.advance(); err != nil {
		return nil, err
	}
	name, err := reader.expect(tokenIdentifier, "an edge name")
	if err != nil {
		return nil, err
	}
	if _, err := reader.expect(tokenAssign, "="); err != nil {
		return nil, err
	}
	from, err := reader.parseEndpoint()
	if err != nil {
		return nil, err
	}
	return reader.parseEdgeTail(from, name.text, comments, start)
}

func (reader *parser) parseEdgeTail(from Endpoint, name string, comments []string, start Position) (*Edge, error) {
	var delivery Delivery
	switch reader.current.kind {
	case tokenLossless:
		delivery = Lossless
	case tokenLossy:
		delivery = Lossy
	default:
		return nil, reader.unexpected("-> or =>")
	}
	if err := reader.advance(); err != nil {
		return nil, err
	}
	to, err := reader.parseEndpoint()
	if err != nil {
		return nil, err
	}
	end, err := reader.expect(tokenSemicolon, ";")
	if err != nil {
		return nil, err
	}
	return &Edge{
		Name: name, From: from, To: to, Delivery: delivery, Comments: comments,
		Span: Span{Start: start, End: end.span.End},
	}, nil
}

func (reader *parser) parseBoundary(direction BoundaryDirection, comments []string) (*Boundary, error) {
	start := reader.current.span.Start
	if err := reader.advance(); err != nil {
		return nil, err
	}
	name, err := reader.expect(tokenIdentifier, "a boundary name")
	if err != nil {
		return nil, err
	}
	if _, err := reader.expect(tokenAssign, "="); err != nil {
		return nil, err
	}
	endpoint, err := reader.parseEndpoint()
	if err != nil {
		return nil, err
	}
	end, err := reader.expect(tokenSemicolon, ";")
	if err != nil {
		return nil, err
	}
	return &Boundary{
		Direction: direction, Name: name.text, Endpoint: endpoint, Comments: comments,
		Span: Span{Start: start, End: end.span.End},
	}, nil
}

func (reader *parser) parseEndpoint() (Endpoint, error) {
	qualified, spans, err := reader.parseQualified()
	if err != nil {
		return Endpoint{}, err
	}
	return reader.endpointFromQualified(qualified, spans)
}

func (reader *parser) parseQualified() ([]string, []Span, error) {
	first, err := reader.expect(tokenIdentifier, "an identifier")
	if err != nil {
		return nil, nil, err
	}
	values := []string{first.text}
	spans := []Span{first.span}
	for reader.current.kind == tokenDot {
		if err := reader.advance(); err != nil {
			return nil, nil, err
		}
		part, err := reader.expect(tokenIdentifier, "an identifier after .")
		if err != nil {
			return nil, nil, err
		}
		values = append(values, part.text)
		spans = append(spans, part.span)
	}
	return values, spans, nil
}

func (reader *parser) endpointFromQualified(values []string, spans []Span) (Endpoint, error) {
	if len(values) != 2 {
		span := spans[0]
		span.End = spans[len(spans)-1].End
		return Endpoint{}, reader.errorAt(span,
			fmt.Sprintf("endpoint must be instance.port, got %q", strings.Join(values, ".")))
	}
	return Endpoint{
		Node: values[0], Port: values[1],
		Span: Span{Start: spans[0].Start, End: spans[1].End},
	}, nil
}

func (reader *parser) keyword(value string) bool {
	return reader.current.kind == tokenIdentifier && reader.current.text == value
}

func (reader *parser) expect(kind tokenKind, description string) (token, error) {
	if reader.current.kind != kind {
		return token{}, reader.unexpected(description)
	}
	current := reader.current
	if err := reader.advance(); err != nil {
		return token{}, err
	}
	return current, nil
}

func (reader *parser) advance() error {
	for {
		next, err := reader.lexer.next()
		if err != nil {
			return err
		}
		if next.kind == tokenComment {
			reader.comments = append(reader.comments, strings.Split(next.text, "\n")...)
			continue
		}
		reader.current = next
		return nil
	}
}

func (reader *parser) takeComments() []string {
	comments := append([]string(nil), reader.comments...)
	reader.comments = nil
	return comments
}

func (reader *parser) unexpected(want string) error {
	found := reader.current.text
	if reader.current.kind == tokenEOF {
		found = "end of file"
	} else if found == "" {
		found = "token"
	} else {
		found = fmt.Sprintf("%q", found)
	}
	return reader.errorAt(reader.current.span, fmt.Sprintf("expected %s, found %s", want, found))
}

func (reader *parser) errorAt(span Span, message string) error {
	return &Error{Path: reader.lexer.path, Span: span, Message: message}
}
