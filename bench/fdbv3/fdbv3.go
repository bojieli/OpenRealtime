// Package fdbv3 runs the Full-Duplex Bench tool-use suite.
//
// Each recording is a person asking for something that requires a function
// call, spoken with the disfluencies people actually produce - fillers,
// pauses, restarts, and spelled-out identifiers. The annotation says which
// call should happen and with what arguments, so scoring is a comparison
// rather than a judgement.
//
// The arguments are where this suite earns its place. "Track order
// B-O-B-1-2" has to become track_order(order_id="BOB12"), and reassembling a
// spelled identifier from speech is a failure mode of its own - distinct from
// not knowing which tool to call, and invisible to a suite that only checks
// the function name. Both are reported.
package fdbv3

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

// ExpectedCall is one call the recording says should happen.
type ExpectedCall struct {
	Function string          `json:"function"`
	Args     json.RawMessage `json:"args"`
}

// Task is one recording.
type Task struct {
	ID         string         `json:"id"`
	Domain     string         `json:"domain"`
	Title      string         `json:"title"`
	Difficulty string         `json:"difficulty"`
	Expected   []ExpectedCall `json:"expected_tool_calls"`
	Features   []string       `json:"disfluency_features"`
	AudioPath  string         `json:"-"`
	Directory  string         `json:"-"`
}

// Load reads the dataset.
func Load(root string, limit int) ([]Task, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", root, err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), "__") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	var tasks []Task
	for _, name := range names {
		directory := filepath.Join(root, name)
		payload, err := os.ReadFile(filepath.Join(directory, "metadata.json"))
		if err != nil {
			continue
		}
		var task Task
		if err := json.Unmarshal(payload, &task); err != nil {
			return nil, fmt.Errorf("decode %s: %w", name, err)
		}
		task.Directory = directory
		task.AudioPath = filepath.Join(directory, "input.wav")
		if _, err := os.Stat(task.AudioPath); err != nil {
			continue
		}
		// The directory name, not the metadata id: two recordings share the
		// id "ecommerce_01", and a report where two rows have the same name is
		// a report where one of them cannot be found again.
		task.ID = name
		tasks = append(tasks, task)
		if limit > 0 && len(tasks) >= limit {
			return tasks, nil
		}
	}
	return tasks, nil
}

// Catalog builds the tool surface offered to the agent.
//
// It is the union of every function and argument name in the whole dataset,
// not the ones this task expects. Handing an agent exactly the tool it is
// supposed to call would measure whether it can call the only tool available,
// which is not the question.
func Catalog(tasks []Task) ([]json.RawMessage, error) {
	arguments := map[string]map[string]string{}
	for _, task := range tasks {
		for _, call := range task.Expected {
			if _, exists := arguments[call.Function]; !exists {
				arguments[call.Function] = map[string]string{}
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(call.Args, &decoded); err != nil {
				continue
			}
			for name, value := range decoded {
				// The declared type comes from the value the suite expects.
				// Declaring everything a string and then scoring against a
				// number asks the model to guess the harness's mistake: it
				// obeys the schema, emits "200", and is marked wrong for it.
				arguments[call.Function][name] = jsonTypeOf(value)
			}
		}
	}
	names := make([]string, 0, len(arguments))
	for name := range arguments {
		names = append(names, name)
	}
	sort.Strings(names)

	tools := make([]json.RawMessage, 0, len(names))
	for _, name := range names {
		properties := make([]string, 0, len(arguments[name]))
		for argument := range arguments[name] {
			properties = append(properties, argument)
		}
		sort.Strings(properties)
		fields := make([]string, 0, len(properties))
		for _, argument := range properties {
			fields = append(fields, fmt.Sprintf(
				`%q:{"type":%q,"description":%q}`, argument, arguments[name][argument], argument))
		}
		tool := fmt.Sprintf(
			`{"type":"function","name":%q,"description":%q,"parameters":{"type":"object","properties":{%s}}}`,
			name, strings.ReplaceAll(name, "_", " "), strings.Join(fields, ","))
		tools = append(tools, json.RawMessage(tool))
	}
	return tools, nil
}

// jsonTypeOf reports the JSON Schema type of a value the suite expects.
//
// A declared type that contradicts the expected value is not a small
// inaccuracy: it is the harness testing whether the model will disobey the
// schema it was given, and scoring it as if it had got the answer wrong.
func jsonTypeOf(value json.RawMessage) string {
	trimmed := strings.TrimSpace(string(value))
	if trimmed == "" {
		return "string"
	}
	switch trimmed[0] {
	case '"':
		return "string"
	case '[':
		return "array"
	case '{':
		return "object"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "string"
	default:
		return "number"
	}
}

// Options configures a run.
type Options struct {
	Root     string
	Endpoint string
	Token    string
	Model    string
	Cell     bench.Cell
	Limit    int
	Timeout  time.Duration
	Progress func(string)
	// RuntimeAttestor captures exact graph execution evidence per task.
	RuntimeAttestor bench.RuntimeAttestor
	// Evidence receives only newly executed candidate attempts and exact audio
	// from the same shared Realtime session used by deterministic scoring.
	Evidence       candidate.Plugin
	EvidenceOrigin candidate.RunOrigin
}

// Run executes the suite.
func Run(ctx context.Context, options Options) (bench.Result, error) {
	if ctx == nil {
		return bench.Result{}, errors.New("FDB v3 evaluation requires a context")
	}
	if options.Evidence != nil {
		if err := options.EvidenceOrigin.Validate(); err != nil {
			return bench.Result{}, fmt.Errorf("FDB v3 candidate evidence origin: %w", err)
		}
	}
	if options.Timeout <= 0 {
		options.Timeout = 3 * time.Minute
	}
	all, err := Load(options.Root, 0)
	if err != nil {
		return bench.Result{}, err
	}
	if len(all) == 0 {
		return bench.Result{}, errors.New("the dataset contains no recordings")
	}
	catalog, err := Catalog(all)
	if err != nil {
		return bench.Result{}, err
	}
	tasks := all
	if options.Limit > 0 && options.Limit < len(all) {
		tasks = all[:options.Limit]
	}

	result := bench.Result{
		Suite: "fdb-v3", Cell: options.Cell, Provenance: bench.Capture(), Expected: len(all),
	}
	var evidenceLifecycle *candidate.Lifecycle
	if options.Evidence != nil {
		evidenceLifecycle, err = candidate.NewLifecycle(candidate.LifecycleConfig{
			Context: ctx, Plugin: options.Evidence, Suite: result.Suite,
			Cell: result.Cell, Provenance: result.Provenance, Origin: options.EvidenceOrigin,
		})
		if err != nil {
			return bench.Result{}, fmt.Errorf("create FDB v3 candidate evidence lifecycle: %w", err)
		}
		result.Provenance = evidenceLifecycle.Provenance()
	}
	finish := func(runErr error) (bench.Result, error) {
		result.Finish()
		if evidenceLifecycle == nil {
			return result, runErr
		}
		return result, errors.Join(runErr, evidenceLifecycle.Finish(result))
	}
	var runErr error
	for index, task := range tasks {
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("[%d/%d] %s", index+1, len(tasks), task.ID))
		}
		outcome, evidenceErr := runTask(ctx, options, task, catalog, evidenceLifecycle)
		result.Tasks = append(result.Tasks, outcome)
		runErr = errors.Join(runErr, evidenceErr)
	}
	return finish(runErr)
}

func runTask(
	ctx context.Context, options Options, task Task, catalog []json.RawMessage,
	evidenceLifecycle *candidate.Lifecycle,
) (outcome bench.TaskOutcome, evidenceErr error) {
	outcome = bench.TaskOutcome{
		ID: task.ID,
		Notes: map[string]string{
			"domain": task.Domain, "difficulty": task.Difficulty,
			"features": strings.Join(task.Features, ","),
		},
	}
	var transcript bench.Transcript
	var attempt *candidate.ActiveAttempt
	if evidenceLifecycle != nil {
		var err error
		attempt, err = evidenceLifecycle.Begin(task.ID, 1, struct {
			Task        Task              `json:"task"`
			ToolCatalog []json.RawMessage `json:"tool_catalog"`
			Criterion   string            `json:"criterion"`
		}{
			Task: task, ToolCatalog: catalog,
			Criterion: "every expected call must use the expected function and annotated argument values",
		})
		if err != nil {
			outcome.Error = err.Error()
			return outcome, err
		}
		recovered, found, err := attempt.Recovered()
		if err != nil {
			outcome.Error = err.Error()
			return outcome, err
		}
		if found {
			return recovered.Outcome, nil
		}
		defer func() {
			evidenceErr = errors.Join(evidenceErr, attempt.Complete(outcome, transcript))
		}()
	}
	var observed []observedCall
	config := bench.SessionConfig{
		Endpoint: options.Endpoint, Token: options.Token, Model: options.Model,
		Instructions: "You are a customer support voice agent. Use the available tools to do what the " +
			"customer asks. Preserve identifiers exactly as the customer gave them.",
		Tools: catalog, Realtime: true, Timeout: options.Timeout,
		RuntimeAttestor: options.RuntimeAttestor, AttestationScope: task.ID,
		Respond: func(name string, arguments json.RawMessage) (json.RawMessage, error) {
			observed = append(observed, observedCall{Name: name, Arguments: arguments})
			// A plausible success, so the conversation continues. What the
			// tool returns is not what this suite measures.
			return json.RawMessage(`{"status":"ok"}`), nil
		},
	}
	if attempt != nil {
		config.CaptureAudio = attempt.CaptureAudio
	}
	var err error
	transcript, err = bench.Play(ctx, config, task.AudioPath)
	outcome.AttachExecution(transcript)
	if err != nil {
		outcome.Error = err.Error()
		return outcome, evidenceErr
	}
	if transcript.Failure != "" {
		outcome.Error = transcript.Failure
		return outcome, evidenceErr
	}
	outcome.Completed = true

	matchedName, matchedArguments := score(task.Expected, observed)
	outcome.Metrics = map[string]float64{
		"expected_calls":   float64(len(task.Expected)),
		"observed_calls":   float64(len(observed)),
		"name_matches":     float64(matchedName),
		"argument_matches": float64(matchedArguments),
	}
	if len(observed) > 0 {
		outcome.Notes["observed"] = describe(observed)
	}
	// The task passes when every expected call happened with its expected
	// arguments. Getting the function right and the identifier wrong is a
	// failure - it is a call to the wrong record.
	outcome.Passed = matchedArguments == len(task.Expected)
	return outcome, evidenceErr
}

type observedCall struct {
	Name      string
	Arguments json.RawMessage
}

// score matches expected calls against observed ones, counting name matches
// and full argument matches separately.
func score(expected []ExpectedCall, observed []observedCall) (names, arguments int) {
	used := make([]bool, len(observed))
	for _, want := range expected {
		nameIndex := -1
		for index, got := range observed {
			if used[index] || got.Name != want.Function {
				continue
			}
			if nameIndex < 0 {
				nameIndex = index
			}
			if argumentsMatch(want.Args, got.Arguments) {
				used[index] = true
				names++
				arguments++
				nameIndex = -1
				break
			}
		}
		if nameIndex >= 0 {
			used[nameIndex] = true
			names++
		}
	}
	return names, arguments
}

// argumentsMatch compares expected arguments against what was sent.
//
// Comparison is on the values the annotation names, not on the whole object: a
// model that supplies an extra optional argument has not got the call wrong.
// Values are normalised for case and for the punctuation speech recognition
// sprinkles through spelled identifiers, because "B-O-B-1-2" arriving as
// "BOB-12" is the same identifier.
//
// Whitespace is *not* normalised away, and that is the load-bearing part.
// Reassembling a spelled identifier from speech is the distinct failure this
// suite exists to separate from not knowing which tool to call: an agent that
// hears "B O B 1 2" and sends order_id="B O B 1 2" has not got the identifier,
// and a real API would reject it. A scorer that stripped the spaces would mark
// that call correct and report the reassembly as working - which is exactly
// the failure mode this project identified, scored as its own absence.
func argumentsMatch(expected, observed json.RawMessage) bool {
	var want, got map[string]any
	if json.Unmarshal(expected, &want) != nil {
		return false
	}
	if json.Unmarshal(observed, &got) != nil {
		return false
	}
	for name, value := range want {
		other, present := got[name]
		if !present {
			return false
		}
		if normalise(fmt.Sprint(value)) != normalise(fmt.Sprint(other)) {
			return false
		}
	}
	return true
}

// normalise lowercases, drops the punctuation a recogniser adds, and collapses
// runs of whitespace - but keeps a word boundary a boundary. "Morgan  Smith"
// and "Morgan Smith" are the same passenger; "BOB12" and "B O B 1 2" are not
// the same order.
func normalise(value string) string {
	var builder strings.Builder
	space := false
	for _, symbol := range strings.ToLower(value) {
		switch {
		case symbol == '-' || symbol == '.' || symbol == ',' || symbol == '_' ||
			symbol == '\'' || symbol == '"':
			continue
		case unicode.IsSpace(symbol):
			space = builder.Len() > 0
			continue
		}
		if space {
			builder.WriteRune(' ')
			space = false
		}
		builder.WriteRune(symbol)
	}
	return builder.String()
}

func describe(calls []observedCall) string {
	parts := make([]string, 0, len(calls))
	for _, call := range calls {
		parts = append(parts, call.Name+"("+string(call.Arguments)+")")
	}
	return strings.Join(parts, "; ")
}

// Breakdown reports the two failure modes separately, because they have
// different causes and different fixes.
type Breakdown struct {
	Tasks int `json:"tasks"`
	// CalledRightTool is how often the right function was invoked at all.
	CalledRightTool int `json:"called_right_tool"`
	// CalledWithRightArguments is how often it also carried the right values.
	CalledWithRightArguments int `json:"called_with_right_arguments"`
	// SpellingFailures is the gap between them: the tool was right and the
	// identifier was not, which is a speech reassembly failure rather than a
	// reasoning one.
	SpellingFailures int `json:"identifier_failures"`
	NoCall           int `json:"no_call_at_all"`
}

// Summarise reads a result into the two failure modes.
func Summarise(result bench.Result) Breakdown {
	breakdown := Breakdown{}
	for _, task := range result.Tasks {
		if !task.Completed {
			continue
		}
		breakdown.Tasks++
		expected := task.Metrics["expected_calls"]
		names := task.Metrics["name_matches"]
		arguments := task.Metrics["argument_matches"]
		switch {
		case names >= expected && arguments >= expected:
			breakdown.CalledRightTool++
			breakdown.CalledWithRightArguments++
		case names >= expected:
			breakdown.CalledRightTool++
			breakdown.SpellingFailures++
		default:
			breakdown.NoCall++
		}
	}
	return breakdown
}
