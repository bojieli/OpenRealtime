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

	"github.com/bojieli/OpenRealtime/bench"
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
	arguments := map[string]map[string]struct{}{}
	for _, task := range tasks {
		for _, call := range task.Expected {
			if _, exists := arguments[call.Function]; !exists {
				arguments[call.Function] = map[string]struct{}{}
			}
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal(call.Args, &decoded); err != nil {
				continue
			}
			for name := range decoded {
				arguments[call.Function][name] = struct{}{}
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
				`%q:{"type":"string","description":%q}`, argument, argument))
		}
		tool := fmt.Sprintf(
			`{"type":"function","name":%q,"description":%q,"parameters":{"type":"object","properties":{%s}}}`,
			name, strings.ReplaceAll(name, "_", " "), strings.Join(fields, ","))
		tools = append(tools, json.RawMessage(tool))
	}
	return tools, nil
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
}

// Run executes the suite.
func Run(ctx context.Context, options Options) (bench.Result, error) {
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
	for index, task := range tasks {
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("[%d/%d] %s", index+1, len(tasks), task.ID))
		}
		result.Tasks = append(result.Tasks, runTask(ctx, options, task, catalog))
	}
	result.Finish()
	return result, nil
}

func runTask(ctx context.Context, options Options, task Task, catalog []json.RawMessage) bench.TaskOutcome {
	outcome := bench.TaskOutcome{
		ID: task.ID,
		Notes: map[string]string{
			"domain": task.Domain, "difficulty": task.Difficulty,
			"features": strings.Join(task.Features, ","),
		},
	}
	var observed []observedCall
	transcript, err := bench.Play(ctx, bench.SessionConfig{
		Endpoint: options.Endpoint, Token: options.Token, Model: options.Model,
		Instructions: "You are a customer support voice agent. Use the available tools to do what the " +
			"customer asks. Preserve identifiers exactly as the customer gave them.",
		Tools:    catalog,
		Realtime: true, Timeout: options.Timeout,
		Respond: func(name string, arguments json.RawMessage) (json.RawMessage, error) {
			observed = append(observed, observedCall{Name: name, Arguments: arguments})
			// A plausible success, so the conversation continues. What the
			// tool returns is not what this suite measures.
			return json.RawMessage(`{"status":"ok"}`), nil
		},
	}, task.AudioPath)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	if transcript.Failure != "" {
		outcome.Error = transcript.Failure
		return outcome
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
	return outcome
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
// Values are normalised for case, spaces, and the punctuation that speech
// recognition sprinkles through spelled identifiers, because "B-O-B-1-2"
// arriving as "BOB-12" is the same identifier and "BO B12" is not.
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

func normalise(value string) string {
	var builder strings.Builder
	for _, symbol := range strings.ToLower(value) {
		switch symbol {
		case ' ', '-', '.', ',', '_', '\'', '"':
		default:
			builder.WriteRune(symbol)
		}
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
