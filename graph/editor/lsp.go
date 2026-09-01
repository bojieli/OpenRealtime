package editor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

const (
	lspPlaintext            = "plaintext"
	lspFullReport           = "full"
	lspSource               = "openrealtime"
	lspPlainTextFormat      = 1
	maxLSPPresentationBytes = 64 << 20
)

// LSPConfigurationHover converts one standards-defined LSP position to the
// exact immutable source snapshot and renders the node's complete values
// contract. It performs no I/O and returns no inline topology values.
func (document *Document) LSPConfigurationHover(position LSPPosition) (LSPHover, error) {
	if document == nil {
		return LSPHover{}, ErrSyntaxUnavailable
	}
	offset, err := document.offsetAtLSPPosition(position)
	if err != nil {
		return LSPHover{}, err
	}
	cursor, err := document.Cursor(offset)
	if err != nil {
		return LSPHover{}, err
	}
	hover, err := document.Hover(cursor)
	if err != nil {
		return LSPHover{}, err
	}
	if hover.Config == nil {
		return LSPHover{}, ErrNoSymbol
	}
	value, err := renderLSPConfiguration(hover)
	if err != nil {
		return LSPHover{}, err
	}
	rangeValue, err := document.lspRange(hover.Range)
	if err != nil {
		return LSPHover{}, err
	}
	result := LSPHover{
		Contents: LSPMarkupContent{Kind: lspPlaintext, Value: value},
		Range:    rangeValue,
	}
	if err := boundedLSPProjection(result); err != nil {
		return LSPHover{}, err
	}
	return result, nil
}

// LSPDiagnostics projects the complete immutable diagnostic snapshot into the
// standards-defined full document report. This single-document boundary fails
// closed if a result names another path, carries a stale span, has an unknown
// severity, was truncated by the editor limit, or exceeds the independent wire
// presentation bound.
func (document *Document) LSPDiagnostics() (LSPFullDocumentDiagnosticReport, error) {
	if document == nil {
		return LSPFullDocumentDiagnosticReport{}, ErrSyntaxUnavailable
	}
	report := document.Diagnostics()
	if report.Incomplete || report.Total != len(report.Items) {
		return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
			"%w: LSP diagnostics cannot label %d of %d items as a full report",
			ErrPresentationLimit, len(report.Items), report.Total,
		)
	}
	items := make([]LSPDiagnostic, len(report.Items))
	for index, diagnostic := range report.Items {
		if diagnostic.Path != "" && diagnostic.Path != document.path {
			return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
				"%w: diagnostic %d names another document %q",
				ErrInvalidPosition, index, diagnostic.Path,
			)
		}
		if diagnostic.Code == "" || len(diagnostic.Code) > 256 ||
			diagnostic.Message == "" || strings.ContainsRune(diagnostic.Message, '\x00') {
			return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
				"%w: diagnostic %d has invalid text", ErrPresentationLimit, index,
			)
		}
		severity := LSPDiagnosticSeverity(0)
		switch diagnostic.Severity {
		case SeverityError:
			severity = LSPDiagnosticError
		case SeverityWarning:
			severity = LSPDiagnosticWarning
		default:
			return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
				"%w: diagnostic %d has unsupported severity %q",
				ErrPresentationLimit, index, diagnostic.Severity,
			)
		}
		rangeValue, err := document.lspRange(diagnostic.Span)
		if err != nil {
			return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
				"project diagnostic %d range: %w", index, err,
			)
		}
		notes := append([]string(nil), diagnostic.Notes...)
		for noteIndex, note := range notes {
			if note == "" || strings.ContainsRune(note, '\x00') {
				return LSPFullDocumentDiagnosticReport{}, fmt.Errorf(
					"%w: diagnostic %d note %d has invalid text",
					ErrPresentationLimit, index, noteIndex,
				)
			}
		}
		items[index] = LSPDiagnostic{
			Range: rangeValue, Severity: severity, Code: diagnostic.Code,
			Source: lspSource, Message: diagnostic.Message,
			Data: LSPDiagnosticData{
				SourceDigest: document.digest, Path: diagnostic.Path, Notes: notes,
			},
		}
	}
	result := LSPFullDocumentDiagnosticReport{Kind: lspFullReport, Items: items}
	if err := boundedLSPProjection(result); err != nil {
		return LSPFullDocumentDiagnosticReport{}, err
	}
	return result, nil
}

// LSPCompletions converts one standards-defined position and the exact symbol
// completion result into a deterministic CompletionList. Replacement ranges
// are verified against the immutable source and expressed in UTF-16 units;
// opaque data retains the source digest so an adapter can reject stale resolve
// or apply operations.
func (document *Document) LSPCompletions(position LSPPosition) (LSPCompletionList, error) {
	if document == nil {
		return LSPCompletionList{}, ErrSyntaxUnavailable
	}
	offset, err := document.offsetAtLSPPosition(position)
	if err != nil {
		return LSPCompletionList{}, err
	}
	cursor, err := document.Cursor(offset)
	if err != nil {
		return LSPCompletionList{}, err
	}
	completions, err := document.Complete(cursor)
	if err != nil {
		return LSPCompletionList{}, err
	}
	if completions.Total < len(completions.Items) ||
		completions.Incomplete != (len(completions.Items) < completions.Total) {
		return LSPCompletionList{}, fmt.Errorf(
			"%w: completion bounds are inconsistent", ErrPresentationLimit,
		)
	}
	items := make([]LSPCompletionItem, len(completions.Items))
	previous := ""
	for index, completion := range completions.Items {
		if completion.Label == "" || completion.InsertText == "" ||
			strings.ContainsAny(completion.Label+completion.InsertText, "\x00\r\n") ||
			(index > 0 && completion.Label <= previous) {
			return LSPCompletionList{}, fmt.Errorf(
				"%w: completion item %d is not canonical", ErrPresentationLimit, index,
			)
		}
		previous = completion.Label
		kind := LSPCompletionItemKind(0)
		switch completion.Kind {
		case CompletionElement:
			kind = LSPCompletionClass
		case CompletionNode:
			kind = LSPCompletionVariable
		case CompletionPort:
			kind = LSPCompletionField
		default:
			return LSPCompletionList{}, fmt.Errorf(
				"%w: completion item %d has unknown kind %q",
				ErrPresentationLimit, index, completion.Kind,
			)
		}
		rangeValue, err := document.lspRange(completion.Replacement)
		if err != nil {
			return LSPCompletionList{}, fmt.Errorf(
				"project completion item %d range: %w", index, err,
			)
		}
		entry, found := document.catalog[completion.Element]
		if completion.Element == "" || !found {
			return LSPCompletionList{}, fmt.Errorf(
				"%w: completion item %d lacks an exact descriptor identity",
				ErrPresentationLimit, index,
			)
		}
		identity := entry.metadata.Identity
		items[index] = LSPCompletionItem{
			Label: completion.Label, Kind: kind, Detail: completion.Detail,
			SortText: completion.Label, FilterText: completion.Label,
			InsertTextFormat: lspPlainTextFormat,
			TextEdit:         LSPTextEdit{Range: rangeValue, NewText: completion.InsertText},
			Data: LSPCompletionData{
				SourceDigest: document.digest, Kind: completion.Kind,
				Element: completion.Element, ElementRevision: identity.Revision,
				ElementDigest: identity.Digest, Node: completion.Node,
				Port: completion.Port, Type: completion.Type,
			},
		}
	}
	result := LSPCompletionList{IsIncomplete: completions.Incomplete, Items: items}
	if err := boundedLSPProjection(result); err != nil {
		return LSPCompletionList{}, err
	}
	return result, nil
}

func (document *Document) offsetAtLSPPosition(position LSPPosition) (int, error) {
	if position.Line < 0 || position.Character < 0 || position.Line >= len(document.positions.lineStarts) {
		return 0, fmt.Errorf("%w: LSP position %+v is outside the source", ErrInvalidPosition, position)
	}
	start := document.positions.lineStarts[position.Line]
	end := len(document.source)
	if position.Line+1 < len(document.positions.lineStarts) {
		end = document.positions.lineStarts[position.Line+1] - 1
	}
	units := 0
	for offset := start; offset < end; {
		character, size := utf8.DecodeRune(document.source[offset:end])
		if character == utf8.RuneError && size == 1 {
			return 0, fmt.Errorf("%w: source contains invalid UTF-8", ErrInvalidPosition)
		}
		if units == position.Character {
			return offset, nil
		}
		width := 1
		if utf16.RuneLen(character) == 2 {
			width = 2
		}
		if position.Character > units && position.Character < units+width {
			return 0, fmt.Errorf("%w: LSP character %d splits a UTF-16 surrogate pair",
				ErrInvalidPosition, position.Character)
		}
		units += width
		offset += size
	}
	if units == position.Character {
		return end, nil
	}
	return 0, fmt.Errorf("%w: LSP character %d exceeds line width %d",
		ErrInvalidPosition, position.Character, units)
}

func (document *Document) lspPositionAtOffset(offset int) (LSPPosition, error) {
	position, err := document.positions.at(offset)
	if err != nil {
		return LSPPosition{}, err
	}
	line := position.Line - 1
	units := 0
	for cursor := document.positions.lineStarts[line]; cursor < offset; {
		character, size := utf8.DecodeRune(document.source[cursor:offset])
		if character == utf8.RuneError && size == 1 {
			return LSPPosition{}, fmt.Errorf("%w: source contains invalid UTF-8", ErrInvalidPosition)
		}
		width := utf16.RuneLen(character)
		if width < 1 {
			width = 1
		}
		units += width
		cursor += size
	}
	return LSPPosition{Line: line, Character: units}, nil
}

func (document *Document) lspRange(span syntax.Span) (LSPRange, error) {
	start, err := document.positions.at(span.Start.Offset)
	if err != nil || start != span.Start {
		return LSPRange{}, fmt.Errorf("%w: LSP range has a stale start position", ErrInvalidPosition)
	}
	end, err := document.positions.at(span.End.Offset)
	if err != nil || end != span.End || span.End.Offset < span.Start.Offset {
		return LSPRange{}, fmt.Errorf("%w: LSP range has a stale end position", ErrInvalidPosition)
	}
	left, err := document.lspPositionAtOffset(span.Start.Offset)
	if err != nil {
		return LSPRange{}, err
	}
	right, err := document.lspPositionAtOffset(span.End.Offset)
	if err != nil {
		return LSPRange{}, err
	}
	return LSPRange{Start: left, End: right}, nil
}

func boundedLSPProjection(value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal LSP projection: %w", err)
	}
	if len(encoded) > maxLSPPresentationBytes {
		return fmt.Errorf("%w: LSP projection has %d bytes; maximum is %d",
			ErrPresentationLimit, len(encoded), maxLSPPresentationBytes)
	}
	return nil
}

func renderLSPConfiguration(hover Hover) (string, error) {
	return renderLSPConfigurationBounded(hover, maxLSPPresentationBytes)
}

func renderLSPConfigurationBounded(hover Hover, maximum int) (string, error) {
	contract := hover.Config
	if contract == nil {
		return "", ErrNoSymbol
	}
	if maximum < 1 || maximum > maxLSPPresentationBytes {
		return "", ErrPresentationLimit
	}
	var output bytes.Buffer
	line := func(label, value string) error {
		// Schema resolution already bounds retained bytes. This independent
		// projection limit also covers labels and JSON rendering amplification.
		if len(label)+len(value)+3 > maximum-output.Len() {
			return ErrPresentationLimit
		}
		output.WriteString(label)
		output.WriteString(": ")
		output.WriteString(value)
		output.WriteByte('\n')
		return nil
	}
	quoted := func(value string) string { return strconv.Quote(value) }
	optional := func(value string) string {
		if value == "" {
			return "absent"
		}
		return quoted(value)
	}
	additional, err := compactJSONOrAbsent(contract.AdditionalProperties)
	if err != nil {
		return "", fmt.Errorf("render LSP additional-properties metadata: %w", err)
	}
	if err := line("configuration contract", quoted(hover.Node)); err != nil {
		return "", err
	}
	for _, field := range []struct{ label, value string }{
		{"element", quoted(hover.ElementReference)},
		{"values path", quoted(contract.ValuesPath)},
		{"artifact", quoted(contract.Artifact)},
		{"descriptor resolved", strconv.FormatBool(contract.Resolved)},
		{"inline topology values", strconv.FormatBool(contract.InlineTopologyValues)},
		{"empty object only", strconv.FormatBool(contract.EmptyObjectOnly)},
		{"schema status", quoted(string(contract.SchemaStatus))},
		{"schema reference", optional(contract.SchemaReference)},
		{"schema identity", optional(contract.SchemaID)},
		{"schema digest", optional(contract.SchemaDigest)},
		{"properties complete", strconv.FormatBool(contract.PropertiesComplete)},
		{"additional properties", additional},
	} {
		if err := line(field.label, field.value); err != nil {
			return "", err
		}
	}
	if err := line("property count", strconv.Itoa(len(contract.Properties))); err != nil {
		return "", err
	}
	for index, property := range contract.Properties {
		prefix := "property[" + strconv.Itoa(index) + "]."
		types := "absent"
		if len(property.Types) != 0 {
			encoded, err := json.Marshal(property.Types)
			if err != nil {
				return "", fmt.Errorf("render LSP property types: %w", err)
			}
			types = string(encoded)
		}
		defaultValue, err := compactJSONOrAbsent(property.Default)
		if err != nil {
			return "", fmt.Errorf("render LSP property %q default: %w", property.Name, err)
		}
		schemaValue, err := compactJSONOrAbsent(property.Schema)
		if err != nil {
			return "", fmt.Errorf("render LSP property %q schema: %w", property.Name, err)
		}
		enumeration := "absent"
		if len(property.Enum) != 0 {
			parts := make([]string, len(property.Enum))
			for enumIndex := range property.Enum {
				parts[enumIndex], err = compactJSONOrAbsent(property.Enum[enumIndex])
				if err != nil {
					return "", fmt.Errorf("render LSP property %q enum %d: %w",
						property.Name, enumIndex, err)
				}
			}
			enumeration = "[" + strings.Join(parts, ",") + "]"
		}
		fields := []struct{ label, value string }{
			{"name", quoted(property.Name)},
			{"pointer", quoted(property.Pointer)},
			{"required", strconv.FormatBool(property.Required)},
			{"types", types},
			{"title", optional(property.Title)},
			{"description", optional(property.Description)},
			{"format", optional(property.Format)},
			{"default", defaultValue},
			{"enum", enumeration},
			{"schema", schemaValue},
		}
		for _, field := range fields {
			if err := line(prefix+field.label, field.value); err != nil {
				return "", err
			}
		}
	}
	return strings.TrimSuffix(output.String(), "\n"), nil
}

func compactJSONOrAbsent(value json.RawMessage) (string, error) {
	if len(value) == 0 {
		return "absent", nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, value); err != nil {
		return "", err
	}
	return compact.String(), nil
}
