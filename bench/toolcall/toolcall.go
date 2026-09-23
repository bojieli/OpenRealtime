// Package toolcall is a small tool-correctness suite for voice agents.
//
// Ten short spoken requests each need one tool call; two of them correct
// themselves mid-sentence. The client really executes every call it receives,
// computing the answer from that call's own arguments, and returns it after a
// delay while the agent may still be speaking. A task passes when exactly one
// call executed, its arguments are the ones asked for (after any correction),
// and the agent then speaks the result. Nothing here judges wording beyond
// "the result was said".
package toolcall

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

//go:embed testdata/fixtures.json testdata/audio/*.wav
var assets embed.FS

// Task is one spoken request and what a correct agent does with it.
type Task struct {
	ID             string         `json:"id"`
	Text           string         `json:"text"`
	Tool           string         `json:"tool"`
	Arguments      map[string]any `json:"arguments"`
	StaleArguments map[string]any `json:"stale_arguments,omitempty"`
	Spoken         []string       `json:"spoken"`
	Audio          string         `json:"audio"`
}

// Tasks returns the embedded fixture set.
func Tasks() ([]Task, error) {
	raw, err := assets.ReadFile("testdata/fixtures.json")
	if err != nil {
		return nil, err
	}
	var file struct {
		Tasks []Task `json:"tasks"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		return nil, fmt.Errorf("decode tool-call fixtures: %w", err)
	}
	return file.Tasks, nil
}

// Tools are the declarations sent with every task.
var Tools = []json.RawMessage{
	json.RawMessage(`{"type":"function","name":"get_weather","description":"Current weather for a city.","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}`),
	json.RawMessage(`{"type":"function","name":"add_numbers","description":"Add two numbers.","parameters":{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"number"}},"required":["a","b"]}}`),
	json.RawMessage(`{"type":"function","name":"set_timer","description":"Start a countdown timer.","parameters":{"type":"object","properties":{"minutes":{"type":"number"}},"required":["minutes"]}}`),
	json.RawMessage(`{"type":"function","name":"convert_currency","description":"Convert an amount between currencies (ISO codes such as USD, EUR).","parameters":{"type":"object","properties":{"amount":{"type":"number"},"from":{"type":"string"},"to":{"type":"string"}},"required":["amount","from","to"]}}`),
}

// Options configures one run.
type Options struct {
	Endpoint, Token, Model string
	Cell                   bench.Cell
	Limit                  int
	Timeout                time.Duration
	// ResultDelay is how long each tool takes. Zero selects two seconds.
	ResultDelay    time.Duration
	WaitConfigured bool
	Player         bool
	Progress       func(string)
}

// Run plays every task and scores it.
func Run(ctx context.Context, options Options) (bench.Result, error) {
	tasks, err := Tasks()
	if err != nil {
		return bench.Result{}, err
	}
	if options.Limit > 0 && options.Limit < len(tasks) {
		tasks = tasks[:options.Limit]
	}
	if options.Timeout == 0 {
		options.Timeout = 60 * time.Second
	}
	if options.ResultDelay == 0 {
		options.ResultDelay = 2 * time.Second
	}
	all, _ := Tasks()
	result := bench.Result{Suite: "toolcall", Cell: options.Cell, Provenance: bench.Capture(), Expected: len(all)}
	for index, task := range tasks {
		if options.Progress != nil {
			options.Progress(fmt.Sprintf("[%d/%d] %s", index+1, len(tasks), task.ID))
		}
		result.Tasks = append(result.Tasks, runTask(ctx, options, task))
	}
	result.Finish()
	return result, nil
}

func runTask(ctx context.Context, options Options, task Task) bench.TaskOutcome {
	outcome := bench.TaskOutcome{ID: task.ID, Notes: map[string]string{"request": task.Text, "tool": task.Tool}}
	raw, err := assets.ReadFile("testdata/audio/" + task.Audio)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	samples, err := bench.DecodePCM24k(raw)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	transcript, err := bench.PlaySamples(ctx, bench.SessionConfig{
		Endpoint: options.Endpoint, Token: options.Token, Model: options.Model,
		Instructions: "You are a helpful voice assistant. Use the tools to answer, then tell the user the result.",
		Tools:        Tools, Realtime: true, Timeout: options.Timeout,
		WaitConfigured: options.WaitConfigured, Player: options.Player,
		ConcurrentTools: true, PostPlaybackQuiet: 5 * time.Second,
		HandleTool: func(toolContext context.Context, request bench.ToolRequest) (json.RawMessage, error) {
			select {
			case <-time.After(options.ResultDelay):
			case <-toolContext.Done():
				return nil, toolContext.Err()
			}
			return Execute(request.Name, request.Arguments)
		},
	}, samples)
	outcome.AttachExecution(transcript)
	if err != nil {
		outcome.Error = err.Error()
		return outcome
	}
	if transcript.Failure != "" {
		outcome.Error = transcript.Failure
		return outcome
	}
	outcome.Completed = true
	Score(&outcome, task, transcript)
	return outcome
}

// Execute runs a tool for real on its arguments. Unknown tools and unusable
// arguments return an error object, as a real backend would.
func Execute(name string, arguments json.RawMessage) (json.RawMessage, error) {
	var args map[string]any
	if json.Unmarshal(arguments, &args) != nil {
		args = map[string]any{}
	}
	var value any
	switch name {
	case "get_weather":
		weather := map[string]struct {
			temperature int
			condition   string
		}{"paris": {17, "light rain"}, "tokyo": {24, "sunny"}, "london": {12, "heavy rain"},
			"madrid": {29, "clear"}, "rome": {21, "cloudy"}}
		city, _ := args["city"].(string)
		entry, ok := weather[cityKey(city)]
		if !ok {
			value = map[string]any{"error": "unknown city: " + city}
			break
		}
		value = map[string]any{"city": city, "temperature_c": entry.temperature, "condition": entry.condition}
	case "add_numbers":
		a, okA := number(args["a"])
		b, okB := number(args["b"])
		if !okA || !okB {
			value = map[string]any{"error": "a and b must be numbers"}
			break
		}
		value = map[string]any{"sum": a + b}
	case "set_timer":
		minutes, ok := number(args["minutes"])
		if !ok {
			value = map[string]any{"error": "minutes must be a number"}
			break
		}
		// A fixed clock keeps the spoken end time checkable: 3:30 PM + minutes.
		end := 15*60 + 30 + int(math.Round(minutes))
		value = map[string]any{"status": "set", "minutes": minutes,
			"ends_at": fmt.Sprintf("%d:%02d PM", end/60-12, end%60)}
	case "convert_currency":
		amount, ok := number(args["amount"])
		from, to := currency(args["from"]), currency(args["to"])
		rates := map[string]float64{"USD>EUR": 0.922, "EUR>USD": 1.085}
		rate, known := rates[from+">"+to]
		if !ok || !known {
			value = map[string]any{"error": "unsupported conversion"}
			break
		}
		value = map[string]any{"amount": math.Round(amount*rate*10) / 10, "currency": to}
	default:
		value = map[string]any{"error": "unknown tool: " + name}
	}
	return json.Marshal(value)
}

// Score reads one transcript against its task.
func Score(outcome *bench.TaskOutcome, task Task, transcript bench.Transcript) {
	var calls []bench.Moment
	resultAt := -1.0
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentToolCall:
			calls = append(calls, moment)
		case bench.MomentToolResult:
			if resultAt < 0 {
				resultAt = moment.AtMS
			}
		}
	}
	correct, stale := 0, 0
	for _, call := range calls {
		var args map[string]any
		_ = json.Unmarshal([]byte(call.Arguments), &args)
		if call.Name == task.Tool && Matches(task.Arguments, args) {
			correct++
		} else if task.StaleArguments != nil && call.Name == task.Tool && Matches(task.StaleArguments, args) {
			stale++
		}
	}
	var spoken strings.Builder
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentAgentText && resultAt >= 0 && moment.AtMS >= resultAt {
			spoken.WriteString(moment.Text)
		}
	}
	said := normalise(spoken.String())
	heard := false
	for _, token := range task.Spoken {
		if strings.Contains(said, normalise(token)) {
			heard = true
		}
	}
	outcome.Metrics = map[string]float64{
		"tool_calls": float64(len(calls)), "correct_calls": float64(correct), "stale_calls": float64(stale),
		"result_spoken": boolMetric(heard),
	}
	outcome.Notes["spoken_after_result"] = strings.TrimSpace(spoken.String())
	switch {
	case len(calls) == 0:
		outcome.Notes["failure"] = "no tool call"
	case len(calls) > 1:
		outcome.Notes["failure"] = fmt.Sprintf("%d calls executed", len(calls))
	case correct != 1:
		outcome.Notes["failure"] = "wrong tool or arguments: " + calls[0].Name + " " + calls[0].Arguments
	case !heard:
		outcome.Notes["failure"] = "result not spoken"
	}
	outcome.Passed = outcome.Notes["failure"] == ""
}

// Matches reports whether every expected argument is present with an
// equivalent value: numbers numerically, cities and currencies normalised.
func Matches(expected, actual map[string]any) bool {
	for key, want := range expected {
		got, ok := actual[key]
		if !ok {
			return false
		}
		switch want := want.(type) {
		case float64:
			value, ok := number(got)
			if !ok || math.Abs(value-want) > 1e-9 {
				return false
			}
		case string:
			text, _ := got.(string)
			if key == "from" || key == "to" {
				if currency(text) != currency(want) {
					return false
				}
			} else if cityKey(text) != cityKey(want) {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func number(value any) (float64, bool) {
	switch value := value.(type) {
	case float64:
		return value, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err == nil
	}
	return 0, false
}

func cityKey(city string) string {
	city = strings.ToLower(strings.TrimSpace(city))
	if before, _, found := strings.Cut(city, ","); found {
		city = before
	}
	return strings.TrimSpace(city)
}

func currency(value any) string {
	text, _ := value.(string)
	text = strings.ToUpper(strings.TrimSpace(text))
	switch {
	case strings.Contains(text, "DOLLAR") || text == "USD" || text == "$":
		return "USD"
	case strings.Contains(text, "EURO") || text == "EUR" || text == "€":
		return "EUR"
	}
	return text
}

func normalise(text string) string {
	text = strings.ToLower(text)
	var out strings.Builder
	for _, r := range text {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ':', r == '-':
			out.WriteRune(r)
		default:
			out.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(out.String()), " ")
}

func boolMetric(value bool) float64 {
	if value {
		return 1
	}
	return 0
}
