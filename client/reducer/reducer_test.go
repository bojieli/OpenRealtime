package reducer_test

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/client/reducer"
	"github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

const corpusPath = "testdata/reducer_vectors.json"

func loadCorpus(t testing.TB) reducer.Corpus {
	t.Helper()
	file, err := os.Open(corpusPath)
	if err != nil {
		t.Fatalf("open corpus: %v", err)
	}
	defer file.Close()
	corpus, err := reducer.LoadCorpus(file)
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	return corpus
}

func TestGoReducerPassesCanonicalCorpus(t *testing.T) {
	corpus := loadCorpus(t)
	for _, vector := range corpus.Vectors {
		vector := vector
		t.Run(vector.Name, func(t *testing.T) {
			t.Parallel()
			result, err := reducer.RunVector(vector, corpus.Limits)
			if err != nil {
				t.Fatalf("run vector: %v", err)
			}
			if err := reducer.CompareResult(vector.Expect, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCanonicalOutboundCommandsConformToPinnedOpenAIRealtimeSchema(t *testing.T) {
	corpus := loadCorpus(t)
	validator := openai.NewValidator()
	for _, vector := range corpus.Vectors {
		result, err := reducer.RunVector(vector, corpus.Limits)
		if err != nil {
			t.Fatalf("%s: run vector: %v", vector.Name, err)
		}
		for index, raw := range result.Outbound {
			message, err := openai.Decode(raw)
			if err != nil {
				t.Fatalf("%s outbound %d decode: %v", vector.Name, index, err)
			}
			if err := validator.Validate(openai.ProfileRealtime, openai.DirectionClient, message); err != nil {
				t.Errorf("%s outbound %d (%s): %v\n%s", vector.Name, index, message.Type(), err, raw)
			}
		}
	}
}

func TestCanonicalInboundVocabularyUsesPinnedProtocolNames(t *testing.T) {
	corpus := loadCorpus(t)
	extensions := map[string]bool{
		openrealtime.EventObservationAdded: true,
		openrealtime.EventDebug:            true,
	}
	for _, vector := range corpus.Vectors {
		for index, step := range vector.Steps {
			if step.Operation.Kind != "inbound" {
				continue
			}
			message, err := openai.Decode(step.Operation.Event)
			if err != nil {
				t.Fatalf("%s step %d event envelope: %v", vector.Name, index, err)
			}
			eventType := string(message.Type())
			if extensions[eventType] || strings.HasPrefix(eventType, "future.") ||
				strings.HasPrefix(eventType, "vendor.") {
				continue
			}
			if _, ok := openai.Lookup(openai.ProfileRealtime, openai.DirectionServer, message.Type()); !ok {
				t.Errorf("%s step %d uses unregistered inbound event %q", vector.Name, index, eventType)
			}
		}
	}
}

func TestCorpusParserRejectsAmbiguousOrUnboundedJSON(t *testing.T) {
	valid, err := os.ReadFile(corpusPath)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		payload []byte
		want    string
	}{
		{
			name:    "duplicate root key",
			payload: []byte(`{"format_version":1,"format_version":1}`),
			want:    "duplicate JSON key",
		},
		{
			name: "duplicate nested event key",
			payload: bytes.Replace(valid,
				[]byte(`"event": {"type": "session.created"`),
				[]byte(`"event": {"type": "session.created", "type": "session.created"`), 1),
			want: "duplicate JSON key",
		},
		{
			name: "unknown typed field",
			payload: bytes.Replace(valid, []byte(`"format_version": 1,`),
				[]byte(`"format_version": 1, "surprise": true,`), 1),
			want: "unknown field",
		},
		{
			name: "irrelevant zero-value field",
			payload: bytes.Replace(valid, []byte(`{"kind": "connected"}`),
				[]byte(`{"kind": "connected", "speaking": false}`), 1),
			want: "does not allow field",
		},
		{
			name:    "trailing document",
			payload: append(bytes.Clone(valid), []byte(` {}`)...),
			want:    "trailing JSON token",
		},
		{
			name:    "oversized",
			payload: bytes.Repeat([]byte(" "), reducer.MaxCorpusBytes+1),
			want:    "exceeds",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := reducer.LoadCorpus(bytes.NewReader(test.payload))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want containing %q", err, test.want)
			}
		})
	}
}

func TestDirectInboundRejectsDuplicateKeysAndPreservesState(t *testing.T) {
	machine, err := reducer.New(reducer.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := machine.Apply(0, reducer.Operation{Kind: "connect", Transport: "websocket"}); err != nil {
		t.Fatal(err)
	}
	if err := machine.Apply(1, reducer.Operation{Kind: "connected"}); err != nil {
		t.Fatal(err)
	}
	before := machine.Snapshot()
	err = machine.Apply(2, reducer.Operation{
		Kind:  "inbound",
		Event: json.RawMessage(`{"type":"session.created","session":{"id":"first","id":"second"}}`),
	})
	if err == nil || !strings.Contains(err.Error(), "duplicate JSON key") {
		t.Fatalf("duplicate event error = %v", err)
	}
	if after := machine.Snapshot(); !reflect.DeepEqual(after, before) {
		t.Fatalf("failed operation mutated reducer\nbefore: %+v\nafter: %+v", before, after)
	}
}

func TestOperationBoundsAndAtomicCollectionLimits(t *testing.T) {
	limits := reducer.DefaultLimits()
	limits.MaxConversationItems = 1
	limits.MaxOutboundEvents = 1
	machine, err := reducer.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	applyOK(t, machine, 0, reducer.Operation{Kind: "connect", Transport: "websocket"})
	applyOK(t, machine, 1, reducer.Operation{Kind: "connected"})

	before := machine.Snapshot()
	err = machine.Apply(2, reducer.Operation{Kind: "typed_text", ItemID: "u1", Text: "hello"})
	if err == nil || !strings.Contains(err.Error(), "outbound event limit") {
		t.Fatalf("typed text bound error = %v", err)
	}
	if !reflect.DeepEqual(machine.Snapshot(), before) || len(machine.Outbound()) != 0 {
		t.Fatal("multi-command operation was not atomic")
	}

	applyOK(t, machine, 2, reducer.Operation{
		Kind: "inbound", Event: json.RawMessage(`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`),
	})
	applyOK(t, machine, 3, reducer.Operation{
		Kind: "inbound", Event: json.RawMessage(`{"type":"response.output_text.delta","item_id":"a1","delta":"one"}`),
	})
	before = machine.Snapshot()
	err = machine.Apply(4, reducer.Operation{
		Kind: "inbound", Event: json.RawMessage(`{"type":"response.output_text.delta","item_id":"a2","delta":"two"}`),
	})
	if err == nil || !strings.Contains(err.Error(), "conversation item limit") {
		t.Fatalf("conversation bound error = %v", err)
	}
	if !reflect.DeepEqual(machine.Snapshot(), before) {
		t.Fatal("conversation overflow mutated reducer")
	}
}

func TestEventAndStringBounds(t *testing.T) {
	limits := reducer.DefaultLimits()
	machine, err := reducer.New(limits)
	if err != nil {
		t.Fatal(err)
	}
	applyOK(t, machine, 0, reducer.Operation{Kind: "connect", Transport: "websocket"})
	applyOK(t, machine, 1, reducer.Operation{Kind: "connected"})

	tooLarge := json.RawMessage(`{"type":"future.event","payload":"` +
		strings.Repeat("x", limits.MaxEventBytes) + `"}`)
	if err := machine.Apply(2, reducer.Operation{Kind: "inbound", Event: tooLarge}); err == nil ||
		!strings.Contains(err.Error(), "operation JSON exceeds") {
		t.Fatalf("large event error = %v", err)
	}
	if err := machine.Apply(2, reducer.Operation{Kind: "typed_text", ItemID: "u", Text: strings.Repeat("x", limits.MaxStringBytes+1)}); err == nil ||
		!strings.Contains(err.Error(), "text exceeds") {
		t.Fatalf("large string error = %v", err)
	}
	if err := machine.Apply(limits.MaxVirtualTimeMS+1, reducer.Operation{Kind: "disconnect"}); err == nil ||
		!strings.Contains(err.Error(), "virtual time") {
		t.Fatalf("large time error = %v", err)
	}
}

func TestInvalidAudioAndEarlyRetryAreAtomic(t *testing.T) {
	machine, err := reducer.New(reducer.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	applyOK(t, machine, 0, reducer.Operation{Kind: "connect", Transport: "websocket"})
	applyOK(t, machine, 1, reducer.Operation{Kind: "connected"})
	applyOK(t, machine, 2, reducer.Operation{
		Kind: "inbound", Event: json.RawMessage(`{"type":"response.created","response":{"id":"r","status":"in_progress"}}`),
	})
	before := machine.Snapshot()
	if err := machine.Apply(3, reducer.Operation{
		Kind: "inbound", Event: json.RawMessage(`{"type":"response.output_audio.delta","item_id":"a","delta":"%%%"}`),
	}); err == nil || !strings.Contains(err.Error(), "not base64") {
		t.Fatalf("invalid audio error = %v", err)
	}
	if !reflect.DeepEqual(machine.Snapshot(), before) {
		t.Fatal("invalid audio mutated reducer")
	}
	applyOK(t, machine, 10, reducer.Operation{Kind: "transport_lost", Reason: "drop"})
	before = machine.Snapshot()
	if err := machine.Apply(259, reducer.Operation{Kind: "retry"}); err == nil || !strings.Contains(err.Error(), "precedes deadline") {
		t.Fatalf("early retry error = %v", err)
	}
	if !reflect.DeepEqual(machine.Snapshot(), before) {
		t.Fatal("early retry mutated reducer")
	}
}

func TestReturnedSnapshotsAndCommandsDoNotAliasReducer(t *testing.T) {
	corpus := loadCorpus(t)
	result, err := reducer.RunVector(corpus.Vectors[0], corpus.Limits)
	if err != nil {
		t.Fatal(err)
	}
	machine, err := reducer.New(corpus.Limits)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range corpus.Vectors[0].Steps {
		if err := machine.Apply(step.AtMS, step.Operation); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := machine.Snapshot()
	snapshot.Conversation[0].Text = "mutated"
	snapshot.ProtocolLog[0].Type = "mutated"
	snapshot.Session.OpenRealtime.Enabled[0] = "mutated"
	if reflect.DeepEqual(snapshot, machine.Snapshot()) {
		t.Fatal("snapshot aliases reducer state")
	}
	if len(result.Outbound) != 0 {
		t.Fatal("first vector unexpectedly emitted commands")
	}

	commandsMachine, err := reducer.New(corpus.Limits)
	if err != nil {
		t.Fatal(err)
	}
	applyOK(t, commandsMachine, 0, reducer.Operation{Kind: "connect", Transport: "websocket"})
	applyOK(t, commandsMachine, 1, reducer.Operation{Kind: "connected"})
	applyOK(t, commandsMachine, 2, reducer.Operation{Kind: "session_update", Session: json.RawMessage(`{"type":"realtime"}`)})
	commands := commandsMachine.Outbound()
	commands[0][0] = 'x'
	if bytes.Equal(commands[0], commandsMachine.Outbound()[0]) {
		t.Fatal("outbound command aliases reducer state")
	}
}

func TestCorpusExecutionIsDeterministic(t *testing.T) {
	corpus := loadCorpus(t)
	for _, vector := range corpus.Vectors {
		first, err := reducer.RunVector(vector, corpus.Limits)
		if err != nil {
			t.Fatal(err)
		}
		second, err := reducer.RunVector(vector, corpus.Limits)
		if err != nil {
			t.Fatal(err)
		}
		left, _ := json.Marshal(first)
		right, _ := json.Marshal(second)
		if !bytes.Equal(left, right) {
			t.Fatalf("vector %q was nondeterministic", vector.Name)
		}
	}
}

func TestCorpusFileRemainsInsidePublishedBound(t *testing.T) {
	info, err := os.Stat(filepath.Clean(corpusPath))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > reducer.MaxCorpusBytes {
		t.Fatalf("corpus size %d exceeds %d", info.Size(), reducer.MaxCorpusBytes)
	}
}

func TestJavaScriptConformanceRunner(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable; JavaScript conformance is a separate required release gate")
	}
	command := exec.Command(node, "javascript/conformance.mjs", corpusPath)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("JavaScript conformance: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "9 vectors passed") {
		t.Fatalf("unexpected JavaScript conformance output: %s", output)
	}

	command = exec.Command(node, "--test", "javascript/reducer.test.mjs")
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("JavaScript unit tests: %v\n%s", err, output)
	}
}

func TestSwiftConformanceRunner(t *testing.T) {
	swiftc, err := exec.LookPath("swiftc")
	if err != nil {
		t.Skip("swiftc is unavailable; actual Swift conformance remains a required release gate")
	}
	binary := filepath.Join(t.TempDir(), "client-reducer-conformance")
	command := exec.Command(
		swiftc,
		"swift/StrictJSON.swift", "swift/ClientReducer.swift", "swift/main.swift",
		"-o", binary,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("compile Swift conformance runner: %v\n%s", err, output)
	}
	command = exec.Command(binary, corpusPath)
	output, err = command.CombinedOutput()
	if err != nil {
		t.Fatalf("Swift conformance: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "9 vectors passed") {
		t.Fatalf("unexpected Swift conformance output: %s", output)
	}
}

func FuzzLoadCorpus(f *testing.F) {
	valid, err := os.ReadFile(corpusPath)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{"format_version":1,"format_version":2}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, payload []byte) {
		_ = reducer.ValidateCorpusJSON(payload)
	})
}

func BenchmarkCanonicalCorpus(b *testing.B) {
	corpus := loadCorpus(b)
	b.ReportAllocs()
	for range b.N {
		for _, vector := range corpus.Vectors {
			if _, err := reducer.RunVector(vector, corpus.Limits); err != nil {
				b.Fatal(err)
			}
		}
	}
}

func applyOK(t *testing.T, machine *reducer.Reducer, atMS int64, operation reducer.Operation) {
	t.Helper()
	if err := machine.Apply(atMS, operation); err != nil {
		t.Fatalf("apply %s at %d: %v", operation.Kind, atMS, err)
	}
}

// The JavaScript reducer is published as a package, which means its version is
// a fourth copy of a number that already exists in VERSION, in the constant the
// binary prints, and on the tag. Three of those are checked against each other;
// an unchecked fourth is how a published package ends up claiming a release it
// was not built from.
//
// The manifest is also what makes the shared client contract installable rather
// than something to vendor by hand, so the fields the install path depends on
// are asserted rather than assumed present.
func TestJavaScriptPackageDeclaresTheReleaseAndItsEntryPoints(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("javascript/package.json")
	if err != nil {
		t.Fatalf("read package manifest: %v", err)
	}
	var manifest struct {
		Name    string            `json:"name"`
		Version string            `json:"version"`
		Type    string            `json:"type"`
		License string            `json:"license"`
		Exports map[string]string `json:"exports"`
		Files   []string          `json:"files"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("decode package manifest: %v", err)
	}
	release, err := os.ReadFile(filepath.Join("..", "..", "VERSION"))
	if err != nil {
		t.Fatalf("read VERSION: %v", err)
	}
	if got, want := manifest.Version, strings.TrimSpace(string(release)); got != want {
		t.Fatalf("package.json says %q and VERSION says %q; they name the same release", got, want)
	}
	if manifest.Type != "module" {
		t.Fatalf("package type = %q, want module: the sources are ES modules", manifest.Type)
	}
	if manifest.License != "MIT" {
		t.Fatalf("package license = %q, want MIT", manifest.License)
	}
	if manifest.Exports["."] != "./reducer.mjs" {
		t.Fatalf("package entry point = %q, want ./reducer.mjs", manifest.Exports["."])
	}
	// A published package that omits a source file installs and then fails to
	// import, which is a failure nothing in this repository would see.
	for _, required := range []string{"reducer.mjs", "conformance.mjs"} {
		if !slices.Contains(manifest.Files, required) {
			t.Fatalf("package.json does not publish %s: %v", required, manifest.Files)
		}
		if _, err := os.Stat(filepath.Join("javascript", required)); err != nil {
			t.Fatalf("published file %s does not exist: %v", required, err)
		}
	}
}
