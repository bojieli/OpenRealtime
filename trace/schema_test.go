package trace

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaPath = "../schemas/trace-record-v0.2.schema.json"

func compileTraceSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	data, err := os.ReadFile(filepath.Clean(schemaPath))
	if err != nil {
		t.Fatalf("read published trace schema: %v", err)
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode published trace schema: %v", err)
	}
	const resourceURL = "https://openrealtime.dev/schemas/trace-record-v0.2.schema.json"
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resourceURL, document); err != nil {
		t.Fatalf("add published trace schema: %v", err)
	}
	schema, err := compiler.Compile(resourceURL)
	if err != nil {
		t.Fatalf("compile published trace schema: %v", err)
	}
	return schema
}

// The schema in schemas/ is a released artifact: it is what a third party
// validates our traces with. The writer emits records from a Go struct that
// nothing checks against it, so the two can drift silently.
func TestWrittenRecordsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()
	schema := compileTraceSchema(t)
	var output bytes.Buffer
	writer := NewWriter(&output)
	records := []Record{
		{
			// A root record, written the way a caller naturally writes one:
			// with no causal parents named at all.
			SchemaVersion: SchemaVersion,
			TraceID:       "trace_0", SessionID: "session", Sequence: 0, MonotonicNS: 10,
			Direction: openaiwire.DirectionClient, Profile: openaiwire.ProfileRealtime,
			Message: []byte(`{"type":"input_audio_buffer.clear"}`),
		},
		{
			SchemaVersion: SchemaVersion,
			TraceID:       "trace_1", SessionID: "session", Sequence: 1, MonotonicNS: 20,
			Direction: openaiwire.DirectionServer, Profile: openaiwire.ProfileRealtime,
			CausalParentIDs: []string{"trace_0"},
			Message:         []byte(`{"type":"input_audio_buffer.committed","event_id":"event_0","previous_item_id":null,"item_id":"item_0"}`),
		},
	}
	for _, record := range records {
		if err := writer.Write(record); err != nil {
			t.Fatalf("Write(%s): %v", record.TraceID, err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	for index, line := range bytes.Split(bytes.TrimSpace(output.Bytes()), []byte("\n")) {
		var value any
		decoder := json.NewDecoder(bytes.NewReader(line))
		decoder.UseNumber()
		if err := decoder.Decode(&value); err != nil {
			t.Fatalf("record %d is not JSON: %v", index, err)
		}
		if err := schema.Validate(value); err != nil {
			t.Errorf("record %d does not match the published schema: %v\n  %s", index, err, line)
		}
	}
}

// The schema restates the writer's enumerations by value. If either side gains
// a case the other lacks, traces stop validating in the field.
func TestPublishedSchemaEnumerationsMatchTheWriter(t *testing.T) {
	t.Parallel()
	schema := compileTraceSchema(t)
	for _, direction := range []openaiwire.Direction{
		openaiwire.DirectionClient,
		openaiwire.DirectionServer,
	} {
		for _, profile := range []openaiwire.Profile{
			openaiwire.ProfileRealtime,
			openaiwire.ProfileTranscription,
			openaiwire.ProfileTranslation,
			openaiwire.ProfileBeta,
		} {
			record := map[string]any{
				"schema_version":    SchemaVersion,
				"trace_id":          "trace_0",
				"session_id":        "session",
				"sequence":          json.Number("0"),
				"monotonic_ns":      json.Number("0"),
				"direction":         string(direction),
				"profile":           string(profile),
				"causal_parent_ids": []any{},
				"message":           map[string]any{"type": "input_audio_buffer.clear"},
			}
			if err := schema.Validate(record); err != nil {
				t.Errorf("published schema rejects %s/%s, which the writer emits: %v",
					direction, profile, err)
			}
		}
	}
}

// Five reference traces shipped in benchmarks release v0.1.0 before the writer
// normalised a root record's absent causal parents, so each opens with
// "causal_parent_ids": null. Their bytes are hash-pinned by that release's
// manifest and are not rewritten here; reissuing a dated release is a release
// decision, not a test fixture change. The exception is bounded below: these
// files must fail in exactly that one way and be otherwise conformant.
var releasedNullRootParents = map[string]bool{
	"benchmarks/m5/reference/game-endpointed-trial-0000.jsonl":                    true,
	"benchmarks/m5/reference/game-microturn_50ms-trial-0000.jsonl":                true,
	"benchmarks/m5/reference/translation-aggressive_incremental-trial-0000.jsonl": true,
	"benchmarks/m5/reference/translation-endpointed-trial-0000.jsonl":             true,
	"benchmarks/m5/reference/translation-stable_incremental-trial-0000.jsonl":     true,
}

func discoverTraceArtifacts(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, root := range []string{"../benchmarks", "../tests"} {
		err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".jsonl" {
				return nil
			}
			data, readErr := os.ReadFile(filepath.Clean(path))
			if readErr != nil {
				return readErr
			}
			first, _, _ := bytes.Cut(bytes.TrimSpace(data), []byte("\n"))
			var probe map[string]json.RawMessage
			if json.Unmarshal(first, &probe) != nil {
				return nil
			}
			// A trace artifact is one whose records carry trace identity.
			if _, ok := probe["trace_id"]; ok {
				found = append(found, path)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(found) == 0 {
		t.Fatal("no trace artifacts were discovered; the search is vacuous")
	}
	return found
}

// Every trace artifact this repository ships is validated against the schema it
// ships alongside them. Without this the two drift, and the drift is invisible
// until a third party runs the published validator over published evidence.
func TestTrackedTraceArtifactsMatchThePublishedSchema(t *testing.T) {
	t.Parallel()
	schema := compileTraceSchema(t)
	exercised := make(map[string]bool)
	validated := 0
	for _, path := range discoverTraceArtifacts(t) {
		relative := filepath.ToSlash(strings.TrimPrefix(filepath.Clean(path), "../"))
		data, err := os.ReadFile(filepath.Clean(path))
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		for index, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var value any
			decoder := json.NewDecoder(bytes.NewReader(line))
			decoder.UseNumber()
			if err := decoder.Decode(&value); err != nil {
				t.Errorf("%s record %d is not JSON: %v", relative, index, err)
				continue
			}
			if schema.Validate(value) == nil {
				if !releasedNullRootParents[relative] {
					validated++
				}
				continue
			}
			if !releasedNullRootParents[relative] {
				t.Errorf("%s record %d does not match the published schema: %v",
					relative, index, schema.Validate(value))
				continue
			}
			// Bound the exception: the only tolerated defect is a null
			// causal_parent_ids, and the record must be conformant once that
			// one field is repaired.
			record, ok := value.(map[string]any)
			if !ok || record["causal_parent_ids"] != nil {
				t.Errorf("%s record %d fails for a reason the release exception does not cover: %v",
					relative, index, schema.Validate(value))
				continue
			}
			record["causal_parent_ids"] = []any{}
			if err := schema.Validate(record); err != nil {
				t.Errorf("%s record %d is non-conformant beyond its null causal parents: %v",
					relative, index, err)
				continue
			}
			exercised[relative] = true
		}
	}
	if validated == 0 {
		t.Error("no unexcepted trace record was validated; the sweep proves nothing")
	}
	// A stale exception is a silent licence to regress: every excepted file
	// must still exhibit the defect it is excepted for.
	for relative := range releasedNullRootParents {
		if !exercised[relative] {
			t.Errorf("%s no longer needs its release exception; remove it", relative)
		}
	}
}
