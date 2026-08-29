package syntax

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

type tokenKind uint8

const (
	tokenInvalid tokenKind = iota
	tokenEOF
	tokenIdentifier
	tokenString
	tokenComment
	tokenLeftBrace
	tokenRightBrace
	tokenSemicolon
	tokenDot
	tokenAssign
	tokenDeclare
	tokenLossless
	tokenLossy
)

type token struct {
	kind tokenKind
	text string
	span Span
}

type lexer struct {
	path   string
	source []byte
	offset int
	line   int
	column int
}

func newLexer(path string, source []byte) *lexer {
	return &lexer{path: path, source: source, line: 1, column: 1}
}

func (scanner *lexer) next() (token, error) {
	scanner.skipWhitespace()
	start := scanner.position()
	if scanner.offset >= len(scanner.source) {
		return token{kind: tokenEOF, span: Span{Start: start, End: start}}, nil
	}

	if scanner.has("//") {
		return scanner.lineComment(2), nil
	}
	if scanner.has("#") {
		return scanner.lineComment(1), nil
	}
	if scanner.has("/*") {
		return scanner.blockComment()
	}
	// Keep multi-byte punctuation deterministic and ahead of its one-byte
	// prefixes. In particular, '=' starts both assignment and the lossy arrow.
	for _, candidate := range []struct {
		spelling string
		kind     tokenKind
	}{
		{spelling: "::", kind: tokenDeclare},
		{spelling: "->", kind: tokenLossless},
		{spelling: "=>", kind: tokenLossy},
	} {
		if scanner.has(candidate.spelling) {
			scanner.advanceBytes(len(candidate.spelling))
			return token{
				kind: candidate.kind, text: candidate.spelling,
				span: Span{Start: start, End: scanner.position()},
			}, nil
		}
	}

	character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
	if character == utf8.RuneError && size == 1 {
		scanner.advanceRune(character, size)
		return token{}, scanner.errorAt(
			Span{Start: start, End: scanner.position()}, "invalid UTF-8 encoding",
		)
	}
	switch character {
	case '{':
		return scanner.single(tokenLeftBrace), nil
	case '}':
		return scanner.single(tokenRightBrace), nil
	case ';':
		return scanner.single(tokenSemicolon), nil
	case '.':
		return scanner.single(tokenDot), nil
	case '=':
		return scanner.single(tokenAssign), nil
	case '"':
		return scanner.quoted()
	}
	if identifierStart(character) {
		for scanner.offset < len(scanner.source) {
			current, size := utf8.DecodeRune(scanner.source[scanner.offset:])
			if current == utf8.RuneError && size == 1 {
				return token{}, scanner.errorAt(
					Span{Start: scanner.position(), End: scanner.position()},
					"invalid UTF-8 encoding",
				)
			}
			// '-' is legal inside an identifier, but must not consume the first
			// half of a tightly-spaced lossless arrow (port->port).
			if current == '-' && scanner.has("->") {
				break
			}
			if !identifierContinue(current) {
				break
			}
			scanner.advanceRune(current, size)
		}
		return token{
			kind: tokenIdentifier,
			text: string(scanner.source[start.Offset:scanner.offset]),
			span: Span{Start: start, End: scanner.position()},
		}, nil
	}

	scanner.advanceRune(character, utf8.RuneLen(character))
	return token{}, scanner.errorAt(Span{Start: start, End: scanner.position()},
		fmt.Sprintf("unexpected character %q", character))
}

func (scanner *lexer) skipWhitespace() {
	for scanner.offset < len(scanner.source) {
		character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
		switch character {
		case ' ', '\t', '\r', '\n':
			scanner.advanceRune(character, size)
		default:
			return
		}
	}
}

func (scanner *lexer) lineComment(prefix int) token {
	start := scanner.position()
	scanner.advanceBytes(prefix)
	contentStart := scanner.offset
	for scanner.offset < len(scanner.source) {
		character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
		if character == '\n' || character == '\r' {
			break
		}
		scanner.advanceRune(character, size)
	}
	return token{
		kind: tokenComment,
		text: strings.TrimSpace(string(scanner.source[contentStart:scanner.offset])),
		span: Span{Start: start, End: scanner.position()},
	}
}

func (scanner *lexer) blockComment() (token, error) {
	start := scanner.position()
	scanner.advanceBytes(2)
	contentStart := scanner.offset
	for scanner.offset < len(scanner.source) && !scanner.has("*/") {
		character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
		scanner.advanceRune(character, size)
	}
	if scanner.offset >= len(scanner.source) {
		return token{}, scanner.errorAt(Span{Start: start, End: scanner.position()}, "unterminated block comment")
	}
	text := string(scanner.source[contentStart:scanner.offset])
	scanner.advanceBytes(2)
	lines := strings.Split(text, "\n")
	for index := range lines {
		lines[index] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[index]), "*"))
	}
	return token{
		kind: tokenComment,
		text: strings.TrimSpace(strings.Join(lines, "\n")),
		span: Span{Start: start, End: scanner.position()},
	}, nil
}

func (scanner *lexer) quoted() (token, error) {
	start := scanner.position()
	startOffset := scanner.offset
	scanner.advanceBytes(1)
	escaped := false
	for scanner.offset < len(scanner.source) {
		character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
		scanner.advanceRune(character, size)
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' {
			escaped = true
			continue
		}
		if character == '"' {
			raw := string(scanner.source[startOffset:scanner.offset])
			value, err := strconv.Unquote(raw)
			if err != nil {
				return token{}, scanner.errorAt(Span{Start: start, End: scanner.position()},
					fmt.Sprintf("invalid string: %v", err))
			}
			return token{kind: tokenString, text: value, span: Span{Start: start, End: scanner.position()}}, nil
		}
		if character == '\n' || character == '\r' {
			return token{}, scanner.errorAt(Span{Start: start, End: scanner.position()}, "newline in string")
		}
	}
	return token{}, scanner.errorAt(Span{Start: start, End: scanner.position()}, "unterminated string")
}

func (scanner *lexer) single(kind tokenKind) token {
	start := scanner.position()
	character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
	scanner.advanceRune(character, size)
	return token{kind: kind, text: string(character), span: Span{Start: start, End: scanner.position()}}
}

func (scanner *lexer) position() Position {
	return Position{Offset: scanner.offset, Line: scanner.line, Column: scanner.column}
}

func (scanner *lexer) has(value string) bool {
	return len(scanner.source)-scanner.offset >= len(value) &&
		string(scanner.source[scanner.offset:scanner.offset+len(value)]) == value
}

func (scanner *lexer) advanceBytes(count int) {
	remaining := count
	for remaining > 0 {
		character, size := utf8.DecodeRune(scanner.source[scanner.offset:])
		scanner.advanceRune(character, size)
		remaining -= size
	}
}

func (scanner *lexer) advanceRune(character rune, size int) {
	scanner.offset += size
	if character == '\n' {
		scanner.line++
		scanner.column = 1
	} else {
		scanner.column++
	}
}

func (scanner *lexer) errorAt(span Span, message string) error {
	return &Error{Path: scanner.path, Span: span, Message: message}
}

func identifierStart(character rune) bool {
	return character == '_' || character >= 'A' && character <= 'Z' ||
		character >= 'a' && character <= 'z'
}

func identifierContinue(character rune) bool {
	return identifierStart(character) || character >= '0' && character <= '9' || character == '-'
}
