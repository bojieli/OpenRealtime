package management

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/syntax"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	maxAnalysisItems         = 65_536
	maxAnalysisPropertyBytes = 64 << 20
)

// ValidateAnalysisResult binds an untrusted authoring provider response to the
// exact source request and validates its bounded immutable editor metadata.
func ValidateAnalysisResult(input AuthoringDocument, result AnalysisResult) error {
	if err := validateDocument(input, true); err != nil {
		return err
	}
	digest := sha256.Sum256([]byte(input.Source))
	wantDigest := "sha256:" + hex.EncodeToString(digest[:])
	if result.SourceDigest != wantDigest || result.Parsed == result.Recovered ||
		result.Canonical && !result.Parsed {
		return fmt.Errorf("%w: authoring analysis has an invalid source/recovery identity", ErrConflict)
	}
	strictFile, strictErr := syntax.Parse(input.Path, []byte(input.Source))
	if result.Parsed != (strictErr == nil) {
		return fmt.Errorf("%w: authoring analysis disagrees with the strict parser", ErrConflict)
	}
	if result.Parsed {
		canonical := syntax.Format(strictFile)
		if result.Canonical != bytes.Equal([]byte(input.Source), []byte(canonical)) {
			return fmt.Errorf("%w: authoring analysis has an invalid canonicality claim", ErrConflict)
		}
	}
	if err := validateDiagnosticReport([]byte(input.Source), result.Diagnostics); err != nil {
		return fmt.Errorf("%w: authoring diagnostics: %v", ErrConflict, err)
	}
	if result.Recovered && !containsDiagnosticCode(result.Diagnostics, "E_SYNTAX") {
		return fmt.Errorf("%w: recovered analysis has no recovery diagnostic", ErrConflict)
	}
	if err := validateMetadataReport(result.Catalog); err != nil {
		return fmt.Errorf("%w: authoring metadata: %v", ErrConflict, err)
	}
	if result.Parsed {
		if result.Formatting == nil {
			return fmt.Errorf("%w: parsed analysis has no formatting edit set", ErrConflict)
		}
		formatted, err := editor.ApplyEdits([]byte(input.Source), *result.Formatting)
		if err != nil {
			return fmt.Errorf("%w: invalid formatting edits: %v", ErrConflict, err)
		}
		if !bytes.Equal(formatted, []byte(syntax.Format(strictFile))) {
			return fmt.Errorf("%w: formatting edits do not preserve the exact topology AST", ErrConflict)
		}
		if result.Formatting.Path != input.Path || result.Formatting.SourceDigest != wantDigest {
			return fmt.Errorf("%w: formatting edits target another source", ErrConflict)
		}
		if result.Canonical && (len(result.Formatting.Edits) != 0 || !bytes.Equal(formatted, []byte(input.Source))) {
			return fmt.Errorf("%w: canonical analysis proposes source changes", ErrConflict)
		}
	} else if result.Formatting != nil {
		return fmt.Errorf("%w: recovery analysis proposed destructive formatting", ErrConflict)
	}
	return nil
}

// ValidateCompileResult keeps recovery artifacts outside the mutation path and
// admits only independently valid immutable Graph IR plus a valid lock.
func ValidateCompileResult(input AuthoringDocument, result CompileResult) error {
	if err := validateDocument(input, false); err != nil {
		return err
	}
	file, err := parseAuthoringDocument(input.Path, []byte(input.Source))
	if err != nil {
		return fmt.Errorf("%w: compile result was returned for source rejected by the strict parser", ErrConflict)
	}
	if err := result.Graph.Validate(); err != nil {
		return fmt.Errorf("%w: compiled Graph IR: %v", ErrConflict, err)
	}
	if err := result.Lock.Validate(); err != nil {
		return fmt.Errorf("%w: compiled resolution lock: %v", ErrConflict, err)
	}
	wantRevision := input.Revision
	if wantRevision == 0 {
		wantRevision = 1
	}
	if result.Graph.Revision != wantRevision {
		return fmt.Errorf("%w: compiled graph revision is %d, want %d", ErrConflict, result.Graph.Revision, wantRevision)
	}
	if result.Graph.ID != file.Graph.Name {
		return fmt.Errorf("%w: compiled graph identity does not match the strict source", ErrConflict)
	}
	if input.Lock != nil && !result.Lock.Equal(*input.Lock) {
		return fmt.Errorf("%w: locked compile returned another resolution lock", ErrConflict)
	}
	return nil
}

func validateDiagnosticReport(source []byte, report editor.DiagnosticReport) error {
	if report.Total < len(report.Items) || report.Total > maxAnalysisItems ||
		report.Incomplete != (len(report.Items) < report.Total) {
		return fmt.Errorf("invalid report bounds")
	}
	previous := -1
	for _, diagnostic := range report.Items {
		if diagnostic.Code == "" || diagnostic.Code != strings.TrimSpace(diagnostic.Code) ||
			len(diagnostic.Code) > 256 || len(diagnostic.Message) == 0 || len(diagnostic.Message) > 64<<10 ||
			strings.ContainsAny(diagnostic.Path, "\x00\r\n") || len(diagnostic.Path) > 64<<10 ||
			(diagnostic.Severity != editor.SeverityError && diagnostic.Severity != editor.SeverityWarning) {
			return fmt.Errorf("invalid diagnostic fields")
		}
		if err := validateSourceSpan(source, diagnostic.Span); err != nil {
			return err
		}
		if diagnostic.Span.Start.Offset < previous {
			return fmt.Errorf("diagnostics are not in source order")
		}
		previous = diagnostic.Span.Start.Offset
		if len(diagnostic.Notes) > 256 {
			return fmt.Errorf("diagnostic notes exceed bound")
		}
		for _, note := range diagnostic.Notes {
			if len(note) > 64<<10 {
				return fmt.Errorf("diagnostic note exceeds bound")
			}
		}
	}
	return nil
}

func validateMetadataReport(report editor.MetadataReport) error {
	if report.Total < len(report.Elements) || report.Total > maxAnalysisItems ||
		report.Incomplete != (len(report.Elements) < report.Total) {
		return fmt.Errorf("invalid metadata report bounds")
	}
	previous := ""
	propertyBytes := 0
	for _, metadata := range report.Elements {
		if err := element.ValidateIdentity(metadata.Identity); err != nil {
			return err
		}
		if previous != "" && metadata.Identity.Name <= previous {
			return fmt.Errorf("element metadata is not in unique name order")
		}
		previous = metadata.Identity.Name
		generics := make(map[string]struct{}, len(metadata.Generics))
		for _, name := range metadata.Generics {
			generics[name] = struct{}{}
		}
		for _, port := range metadata.Ports {
			if port.Name == "" || port.Direction != element.Input && port.Direction != element.Output ||
				port.Type.ValidatePort(generics) != nil {
				return fmt.Errorf("invalid port metadata for %s", metadata.Identity.Name)
			}
		}
		if err := validateConfigMetadata(metadata.Config, &propertyBytes); err != nil {
			return fmt.Errorf("element %s: %w", metadata.Identity.Name, err)
		}
	}
	if propertyBytes > maxAnalysisPropertyBytes {
		return fmt.Errorf("property metadata exceeds %d bytes", maxAnalysisPropertyBytes)
	}
	return nil
}

func validateConfigMetadata(contract editor.ConfigContract, retained *int) error {
	if contract.Artifact == "" || contract.InlineTopologyValues || !contract.Resolved {
		return fmt.Errorf("invalid values contract boundary")
	}
	switch contract.SchemaStatus {
	case editor.ConfigSchemaEmpty:
		if !contract.EmptyObjectOnly || contract.SchemaReference != "" || !contract.PropertiesComplete ||
			len(contract.Properties) != 0 || contract.SchemaID != "" || contract.SchemaDigest != "" ||
			len(contract.AdditionalProperties) != 0 {
			return fmt.Errorf("invalid empty-object values contract")
		}
	case editor.ConfigSchemaUnresolved, editor.ConfigSchemaInvalid:
		if contract.SchemaReference == "" || contract.EmptyObjectOnly || contract.PropertiesComplete ||
			len(contract.Properties) != 0 || contract.SchemaID != "" || contract.SchemaDigest != "" ||
			len(contract.AdditionalProperties) != 0 {
			return fmt.Errorf("unresolved values contract invented schema metadata")
		}
	case editor.ConfigSchemaResolved:
		if contract.SchemaReference == "" || contract.SchemaID == "" || !CanonicalDigest(contract.SchemaDigest) ||
			contract.EmptyObjectOnly || len(contract.Properties) > maxAnalysisItems {
			return fmt.Errorf("resolved values contract is incomplete")
		}
		previous := ""
		for _, property := range contract.Properties {
			if property.Name == "" || property.Name <= previous || property.Pointer != "#/properties/"+escapePointer(property.Name) ||
				len(property.Schema) == 0 || strictjson.Validate(property.Schema) != nil {
				return fmt.Errorf("invalid property metadata")
			}
			previous = property.Name
			*retained += len(property.Schema) + len(property.Default)
			if len(property.Default) != 0 && strictjson.Validate(property.Default) != nil {
				return fmt.Errorf("invalid property default")
			}
			for _, value := range property.Enum {
				*retained += len(value)
				if strictjson.Validate(value) != nil {
					return fmt.Errorf("invalid property enum")
				}
			}
		}
		if len(contract.AdditionalProperties) != 0 {
			*retained += len(contract.AdditionalProperties)
			if strictjson.Validate(contract.AdditionalProperties) != nil {
				return fmt.Errorf("invalid additionalProperties metadata")
			}
		}
	default:
		return fmt.Errorf("unknown values schema status %q", contract.SchemaStatus)
	}
	return nil
}

func validateSourceSpan(source []byte, span syntax.Span) error {
	if span.Start.Offset < 0 || span.End.Offset < span.Start.Offset || span.End.Offset > len(source) ||
		positionAt(source, span.Start.Offset) != span.Start || positionAt(source, span.End.Offset) != span.End {
		return fmt.Errorf("invalid source span")
	}
	return nil
}

func positionAt(source []byte, offset int) syntax.Position {
	position := syntax.Position{Offset: offset, Line: 1, Column: 1}
	for cursor := 0; cursor < offset; {
		value, size := utf8.DecodeRune(source[cursor:])
		if size == 0 {
			break
		}
		cursor += size
		if value == '\n' {
			position.Line++
			position.Column = 1
		} else {
			position.Column++
		}
	}
	return position
}

func containsDiagnosticCode(report editor.DiagnosticReport, code string) bool {
	for _, diagnostic := range report.Items {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}

func escapePointer(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
