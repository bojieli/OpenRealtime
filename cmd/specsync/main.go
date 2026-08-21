// Command specsync extracts a reproducible Realtime-only JSON Schema bundle
// and Go event registry from OpenAI's official OpenAPI specification.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

const componentPrefix = "#/components/schemas/"

type sourceSpec struct {
	Components struct {
		Schemas map[string]any `json:"schemas"`
	} `json:"components"`
}

type definition struct {
	Profile   string
	Direction string
	Type      string
	Component string
}

type sourceMetadata struct {
	Revision    string `json:"revision"`
	SHA256      string `json:"sha256"`
	RetrievedAt string `json:"retrieved_at"`
	URL         string `json:"url"`
}

type schemaBundle struct {
	Schema   string         `json:"$schema"`
	ID       string         `json:"$id"`
	Title    string         `json:"title"`
	Source   sourceMetadata `json:"x-openrealtime-source"`
	Defs     map[string]any `json:"$defs"`
	Profiles map[string]any `json:"profiles"`
}

type options struct {
	Source       string
	SourceURL    string
	Revision     string
	ExpectedHash string
	RetrievedAt  string
	SchemaOut    string
	GoOut        string
}

func main() {
	var opts options
	flag.StringVar(&opts.Source, "source", "", "path to the official OpenAI OpenAPI JSON")
	flag.StringVar(&opts.SourceURL, "source-url", "", "canonical source URL recorded in metadata")
	flag.StringVar(&opts.Revision, "revision", "", "source Git revision")
	flag.StringVar(&opts.ExpectedHash, "sha256", "", "required lowercase SHA-256 of source")
	flag.StringVar(&opts.RetrievedAt, "retrieved-at", "", "ISO retrieval date")
	flag.StringVar(&opts.SchemaOut, "schema-out", "", "generated JSON Schema bundle path")
	flag.StringVar(&opts.GoOut, "go-out", "", "generated Go registry path")
	flag.Parse()

	if err := run(opts); err != nil {
		fmt.Fprintln(os.Stderr, "specsync:", err)
		os.Exit(1)
	}
}

func run(opts options) error {
	if opts.Source == "" || opts.SourceURL == "" || opts.Revision == "" ||
		opts.ExpectedHash == "" || opts.RetrievedAt == "" ||
		opts.SchemaOut == "" || opts.GoOut == "" {
		return errors.New("all flags are required")
	}
	source, err := os.ReadFile(opts.Source)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	digest := sha256.Sum256(source)
	actualHash := hex.EncodeToString(digest[:])
	if actualHash != strings.ToLower(opts.ExpectedHash) {
		return fmt.Errorf("source SHA-256 mismatch: got %s", actualHash)
	}

	var spec sourceSpec
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.UseNumber()
	if err := decoder.Decode(&spec); err != nil {
		return fmt.Errorf("decode source: %w", err)
	}
	if len(spec.Components.Schemas) == 0 {
		return errors.New("source contains no component schemas")
	}

	definitions, profiles, err := discoverDefinitions(spec.Components.Schemas)
	if err != nil {
		return err
	}
	roots := make([]string, 0, len(definitions))
	for _, item := range definitions {
		roots = append(roots, item.Component)
	}
	defs, err := collectClosure(spec.Components.Schemas, roots)
	if err != nil {
		return err
	}

	bundle := schemaBundle{
		Schema: "https://json-schema.org/draft/2020-12/schema",
		ID:     "https://openrealtime.dev/schemas/openai-realtime-events.json",
		Title:  "OpenAI Realtime protocol event schemas",
		Source: sourceMetadata{
			Revision:    opts.Revision,
			SHA256:      actualHash,
			RetrievedAt: opts.RetrievedAt,
			URL:         opts.SourceURL,
		},
		Defs:     defs,
		Profiles: profiles,
	}
	schemaData, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return fmt.Errorf("encode schema bundle: %w", err)
	}
	schemaData = append(schemaData, '\n')
	goData, err := renderGo(definitions, bundle.Source)
	if err != nil {
		return err
	}
	if err := writeAtomic(opts.SchemaOut, schemaData, 0o644); err != nil {
		return err
	}
	return writeAtomic(opts.GoOut, goData, 0o644)
}

func discoverDefinitions(schemas map[string]any) ([]definition, map[string]any, error) {
	groups := []struct {
		Profile   string
		Direction string
		Root      string
	}{
		{"realtime", "client", "RealtimeClientEvent"},
		{"realtime", "server", "RealtimeServerEvent"},
		{"translation", "client", "RealtimeTranslationClientEvent"},
		{"translation", "server", "RealtimeTranslationServerEvent"},
	}
	var definitions []definition
	profiles := make(map[string]any)
	seen := make(map[string]string)
	add := func(profile, direction, component string) error {
		schema, ok := schemas[component]
		if !ok {
			return fmt.Errorf("missing component %s", component)
		}
		eventType, err := schemaEventType(schema)
		if err != nil {
			return fmt.Errorf("component %s: %w", component, err)
		}
		key := profile + "\x00" + direction + "\x00" + eventType
		if previous, exists := seen[key]; exists {
			if previous == component {
				return nil
			}
			return fmt.Errorf("duplicate %s/%s event %q in %s and %s", profile, direction, eventType, previous, component)
		}
		seen[key] = component
		definitions = append(definitions, definition{profile, direction, eventType, component})
		return nil
	}

	for _, group := range groups {
		components, err := unionComponents(schemas, group.Root)
		if err != nil {
			return nil, nil, err
		}
		refs := make([]any, 0, len(components))
		for _, component := range components {
			if err := add(group.Profile, group.Direction, component); err != nil {
				return nil, nil, err
			}
			refs = append(refs, map[string]any{"$ref": "#/$defs/" + component})
		}
		profiles[group.Profile+"_"+group.Direction] = map[string]any{
			"discriminator": map[string]any{"propertyName": "type"},
			"anyOf":         refs,
		}
	}

	transcription := map[string][]string{
		"client": {
			"RealtimeClientEventInputAudioBufferAppend",
			"RealtimeClientEventInputAudioBufferClear",
			"RealtimeClientEventInputAudioBufferCommit",
			"RealtimeClientEventTranscriptionSessionUpdate",
		},
		"server": {
			"RealtimeServerEventError",
			"RealtimeServerEventInputAudioBufferCleared",
			"RealtimeServerEventInputAudioBufferCommitted",
			"RealtimeServerEventInputAudioBufferSpeechStarted",
			"RealtimeServerEventInputAudioBufferSpeechStopped",
			"RealtimeServerEventConversationItemInputAudioTranscriptionDelta",
			"RealtimeServerEventConversationItemInputAudioTranscriptionCompleted",
			"RealtimeServerEventConversationItemInputAudioTranscriptionFailed",
			"RealtimeServerEventConversationItemInputAudioTranscriptionSegment",
			"RealtimeServerEventTranscriptionSessionUpdated",
		},
	}
	for direction, components := range transcription {
		refs := make([]any, 0, len(components))
		for _, component := range components {
			if err := add("transcription", direction, component); err != nil {
				return nil, nil, err
			}
			refs = append(refs, map[string]any{"$ref": "#/$defs/" + component})
		}
		profiles["transcription_"+direction] = map[string]any{
			"discriminator": map[string]any{"propertyName": "type"},
			"anyOf":         refs,
		}
	}

	for _, direction := range []string{"client", "server"} {
		prefix := "RealtimeBeta" + strings.ToUpper(direction[:1]) + direction[1:] + "Event"
		var components []string
		for name := range schemas {
			if strings.HasPrefix(name, prefix) {
				if _, err := schemaEventType(schemas[name]); err == nil {
					components = append(components, name)
				}
			}
		}
		sort.Strings(components)
		refs := make([]any, 0, len(components))
		for _, component := range components {
			if err := add("beta", direction, component); err != nil {
				return nil, nil, err
			}
			refs = append(refs, map[string]any{"$ref": "#/$defs/" + component})
		}
		profiles["beta_"+direction] = map[string]any{
			"discriminator": map[string]any{"propertyName": "type"},
			"anyOf":         refs,
		}
	}

	sort.Slice(definitions, func(i, j int) bool {
		a, b := definitions[i], definitions[j]
		if a.Profile != b.Profile {
			return a.Profile < b.Profile
		}
		if a.Direction != b.Direction {
			return a.Direction < b.Direction
		}
		return a.Type < b.Type
	})
	return definitions, profiles, nil
}

func unionComponents(schemas map[string]any, root string) ([]string, error) {
	schema, ok := schemas[root].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("missing union root %s", root)
	}
	variants, ok := schema["anyOf"].([]any)
	if !ok {
		return nil, fmt.Errorf("union root %s has no anyOf", root)
	}
	components := make([]string, 0, len(variants))
	for _, variant := range variants {
		object, ok := variant.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("union root %s has non-object variant", root)
		}
		ref, ok := object["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, componentPrefix) {
			return nil, fmt.Errorf("union root %s has unsupported variant", root)
		}
		components = append(components, strings.TrimPrefix(ref, componentPrefix))
	}
	return components, nil
}

func schemaEventType(schema any) (string, error) {
	object, ok := schema.(map[string]any)
	if !ok {
		return "", errors.New("schema is not an object")
	}
	properties, ok := object["properties"].(map[string]any)
	if !ok {
		return "", errors.New("schema has no properties")
	}
	typeSchema, ok := properties["type"].(map[string]any)
	if !ok {
		return "", errors.New("schema has no type property")
	}
	if value, ok := typeSchema["const"].(string); ok && value != "" {
		return value, nil
	}
	if values, ok := typeSchema["enum"].([]any); ok && len(values) == 1 {
		if value, ok := values[0].(string); ok && value != "" {
			return value, nil
		}
	}
	return "", errors.New("type property is not a string constant")
}

func collectClosure(schemas map[string]any, roots []string) (map[string]any, error) {
	result := make(map[string]any)
	queue := append([]string(nil), roots...)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if _, exists := result[name]; exists {
			continue
		}
		schema, ok := schemas[name]
		if !ok {
			return nil, fmt.Errorf("referenced component %s does not exist", name)
		}
		copyValue, err := deepCopy(schema)
		if err != nil {
			return nil, err
		}
		refs := make(map[string]struct{})
		if err := rewriteRefs(copyValue, refs); err != nil {
			return nil, fmt.Errorf("component %s: %w", name, err)
		}
		admitDeclaredNulls(copyValue)
		result[name] = copyValue
		for ref := range refs {
			queue = append(queue, ref)
		}
	}
	return result, nil
}

// admitDeclaredNulls lets a schema accept the null its own default declares.
//
// The source specification contradicts itself in a handful of places. A field
// is given "type": "object" and, in the same object, "default": null and a
// description that says in words it can be set to null to turn the feature
// off. The API behaves as the description says: it accepts null, it returns
// null, and OpenAI's own published examples of session.created show null in
// exactly these fields - as does the session.update their own SDK sends on
// every connection.
//
// Converted to JSON Schema without that reconciliation, the type wins and the
// default becomes unrepresentable, which makes a validator built from this
// bundle stricter than the API it is a description of. A server using it then
// rejects OpenAI's own client while claiming to be compatible with it, which
// is a worse failure than not validating at all: the check that exists to stop
// compatibility decaying is itself the thing that breaks it.
//
// The rule is deliberately narrow and mechanical: a schema that declares null
// as its default accepts null. It reads the spec's own statement rather than
// adding a judgement of ours, it applies wherever the source says it, and the
// result is visible in the pinned artifact rather than hidden in a validator.
func admitDeclaredNulls(value any) {
	switch current := value.(type) {
	case map[string]any:
		defaultValue, declared := current["default"]
		if declared && defaultValue == nil {
			switch existing := current["type"].(type) {
			case string:
				if existing != "null" {
					current["type"] = []any{existing, "null"}
				}
			case []any:
				if !slices.ContainsFunc(existing, func(entry any) bool { return entry == "null" }) {
					current["type"] = append(append([]any{}, existing...), "null")
				}
			}
		}
		for _, child := range current {
			admitDeclaredNulls(child)
		}
	case []any:
		for _, child := range current {
			admitDeclaredNulls(child)
		}
	}
}

func rewriteRefs(value any, refs map[string]struct{}) error {
	switch current := value.(type) {
	case map[string]any:
		for key, child := range current {
			if key == "$ref" {
				ref, ok := child.(string)
				if !ok || !strings.HasPrefix(ref, componentPrefix) {
					return fmt.Errorf("unsupported reference %v", child)
				}
				name := strings.TrimPrefix(ref, componentPrefix)
				current[key] = "#/$defs/" + name
				refs[name] = struct{}{}
				continue
			}
			if err := rewriteRefs(child, refs); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range current {
			if err := rewriteRefs(child, refs); err != nil {
				return err
			}
		}
	}
	return nil
}

func deepCopy(value any) (any, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var result any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func renderGo(definitions []definition, source sourceMetadata) ([]byte, error) {
	var output bytes.Buffer
	fmt.Fprintln(&output, "// Code generated by cmd/specsync; DO NOT EDIT.")
	fmt.Fprintf(&output, "// Source: %s at %s (sha256:%s), retrieved %s.\n\n", source.URL, source.Revision, source.SHA256, source.RetrievedAt)
	fmt.Fprintln(&output, "package openai")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "const (")
	uniqueTypes := make(map[string]struct{})
	for _, item := range definitions {
		uniqueTypes[item.Type] = struct{}{}
	}
	types := make([]string, 0, len(uniqueTypes))
	for eventType := range uniqueTypes {
		types = append(types, eventType)
	}
	sort.Strings(types)
	for _, eventType := range types {
		fmt.Fprintf(&output, "\tEvent%s EventType = %q\n", goIdentifier(eventType), eventType)
	}
	fmt.Fprintln(&output, ")")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "var generatedDefinitions = []Definition{")
	for _, item := range definitions {
		fmt.Fprintf(&output, "\t{Profile: Profile(%q), Direction: Direction(%q), Type: EventType(%q), SchemaRef: %q},\n", item.Profile, item.Direction, item.Type, "#/$defs/"+item.Component)
	}
	fmt.Fprintln(&output, "}")
	formatted, err := format.Source(output.Bytes())
	if err != nil {
		return nil, fmt.Errorf("format generated Go: %w\n%s", err, output.String())
	}
	return formatted, nil
}

func goIdentifier(eventType string) string {
	var output strings.Builder
	upper := true
	for _, char := range eventType {
		if char == '.' || char == '_' || char == '-' {
			upper = true
			continue
		}
		if upper {
			output.WriteString(strings.ToUpper(string(char)))
			upper = false
		} else {
			output.WriteRune(char)
		}
	}
	return output.String()
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".specsync-*")
	if err != nil {
		return fmt.Errorf("create temporary output: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if _, err := io.Copy(temporary, bytes.NewReader(data)); err != nil {
		temporary.Close()
		return fmt.Errorf("write temporary output: %w", err)
	}
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return fmt.Errorf("set output mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("sync output: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close output: %w", err)
	}
	if err := os.Rename(temporaryName, path); err != nil {
		return fmt.Errorf("replace output: %w", err)
	}
	return nil
}
