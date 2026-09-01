package editor

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

const (
	lspPlaintext            = "plaintext"
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
	start, err := document.lspPositionAtOffset(hover.Range.Start.Offset)
	if err != nil {
		return LSPHover{}, err
	}
	end, err := document.lspPositionAtOffset(hover.Range.End.Offset)
	if err != nil {
		return LSPHover{}, err
	}
	return LSPHover{
		Contents: LSPMarkupContent{Kind: lspPlaintext, Value: value},
		Range:    LSPRange{Start: start, End: end},
	}, nil
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
