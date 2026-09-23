package toolcall

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/bojieli/OpenRealtime/bench"
)

func TestFixturesAreCompleteAndUnchanged(t *testing.T) {
	tasks, err := Tasks()
	if err != nil || len(tasks) != 10 {
		t.Fatalf("tasks = %d, err = %v", len(tasks), err)
	}
	raw, _ := assets.ReadFile("testdata/fixtures.json")
	var hashes struct {
		Tasks []struct {
			Audio  string `json:"audio"`
			SHA256 string `json:"sha256"`
		} `json:"tasks"`
	}
	_ = json.Unmarshal(raw, &hashes)
	corrections := 0
	for index, task := range tasks {
		audio, err := assets.ReadFile("testdata/audio/" + task.Audio)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(audio)
		if hex.EncodeToString(sum[:]) != hashes.Tasks[index].SHA256 {
			t.Fatalf("%s: audio does not match its recorded hash", task.ID)
		}
		if _, err := bench.DecodePCM24k(audio); err != nil {
			t.Fatalf("%s: %v", task.ID, err)
		}
		// A fixture's expected answer must be what the tool actually returns.
		arguments, _ := json.Marshal(task.Arguments)
		output, _ := Execute(task.Tool, arguments)
		found := false
		for _, token := range task.Spoken {
			found = found || strings.Contains(normalise(string(output)), normalise(token))
		}
		if !found {
			t.Fatalf("%s: tool output %s contains none of %v", task.ID, output, task.Spoken)
		}
		if task.StaleArguments != nil {
			corrections++
		}
	}
	if corrections != 2 {
		t.Fatalf("corrections = %d, want 2", corrections)
	}
}

func TestExecuteComputesFromItsOwnArguments(t *testing.T) {
	for _, check := range []struct{ name, arguments, want string }{
		{"get_weather", `{"city":"Rome"}`, `"temperature_c":21`},
		{"get_weather", `{"city":"Atlantis"}`, `unknown city`},
		{"add_numbers", `{"a":8,"b":9}`, `"sum":17`},
		{"add_numbers", `{"a":"112","b":30}`, `"sum":142`},
		{"set_timer", `{"minutes":10}`, `3:40 PM`},
		{"convert_currency", `{"amount":50,"from":"dollars","to":"euros"}`, `"amount":46.1`},
		{"launch_rocket", `{}`, `unknown tool`},
	} {
		output, err := Execute(check.name, json.RawMessage(check.arguments))
		if err != nil || !strings.Contains(string(output), check.want) {
			t.Fatalf("%s %s = %s (%v), want %s", check.name, check.arguments, output, err, check.want)
		}
	}
}

func TestMatchesNormalisesWhatModelsSay(t *testing.T) {
	yes := [][2]map[string]any{
		{{"city": "Paris"}, {"city": "paris, France"}},
		{{"a": 17.0, "b": 25.0}, {"a": "17", "b": 25.0, "note": "extra keys are fine"}},
		{{"from": "USD", "to": "EUR"}, {"from": "US dollars", "to": "euro"}},
	}
	no := [][2]map[string]any{
		{{"city": "Madrid"}, {"city": "Rome"}},
		{{"a": 8.0, "b": 19.0}, {"a": 8.0, "b": 9.0}},
		{{"a": 8.0}, {}},
	}
	for _, pair := range yes {
		if !Matches(pair[0], pair[1]) {
			t.Fatalf("%v should match %v", pair[1], pair[0])
		}
	}
	for _, pair := range no {
		if Matches(pair[0], pair[1]) {
			t.Fatalf("%v should not match %v", pair[1], pair[0])
		}
	}
}

func transcriptWith(calls []string, spoken string) bench.Transcript {
	var moments []bench.Moment
	for index, arguments := range calls {
		moments = append(moments, bench.Moment{AtMS: float64(1000 + index), Kind: bench.MomentToolCall,
			Name: "get_weather", Arguments: arguments})
	}
	moments = append(moments, bench.Moment{AtMS: 3000, Kind: bench.MomentToolResult},
		bench.Moment{AtMS: 3500, Kind: bench.MomentAgentText, Text: spoken})
	return bench.Transcript{Moments: moments}
}

func TestScoreFailsEachWayAToolCallGoesWrong(t *testing.T) {
	task := Task{ID: "correction-weather", Tool: "get_weather", Arguments: map[string]any{"city": "Madrid"},
		StaleArguments: map[string]any{"city": "Rome"}, Spoken: []string{"29", "twenty-nine"}}
	for _, check := range []struct {
		name    string
		calls   []string
		spoken  string
		pass    bool
		failure string
	}{
		{"correct", []string{`{"city":"Madrid"}`}, "It's twenty-nine degrees and clear in Madrid.", true, ""},
		{"stale then corrected", []string{`{"city":"Rome"}`, `{"city":"Madrid"}`}, "29 degrees", false, "2 calls"},
		{"stale only", []string{`{"city":"Rome"}`}, "21 degrees in Rome", false, "wrong tool or arguments"},
		{"result not said", []string{`{"city":"Madrid"}`}, "Let me check that for you.", false, "result not spoken"},
		{"no call", nil, "It is sunny.", false, "no tool call"},
	} {
		outcome := bench.TaskOutcome{Notes: map[string]string{}}
		Score(&outcome, task, transcriptWith(check.calls, check.spoken))
		if outcome.Passed != check.pass || !strings.Contains(outcome.Notes["failure"], check.failure) {
			t.Fatalf("%s: passed=%v failure=%q", check.name, outcome.Passed, outcome.Notes["failure"])
		}
	}
}

// A fake agent that calls the tool, waits for its output, then speaks it.
func TestRunDrivesAToolCallEndToEnd(t *testing.T) {
	outputs := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		ctx := request.Context()
		write := func(value map[string]any) {
			encoded, _ := json.Marshal(value)
			_ = connection.Write(ctx, websocket.MessageText, encoded)
		}
		called := false
		for {
			_, raw, err := connection.Read(ctx)
			if err != nil {
				return
			}
			var event map[string]any
			_ = json.Unmarshal(raw, &event)
			switch event["type"] {
			case "session.update":
				tools, _ := event["session"].(map[string]any)["tools"].([]any)
				if len(tools) != len(Tools) {
					write(map[string]any{"type": "error", "error": map[string]any{"message": "tools missing"}})
				}
				write(map[string]any{"type": "session.updated", "session": map[string]any{}})
			case "input_audio_buffer.append":
				if !called {
					called = true
					write(map[string]any{"type": "response.function_call_arguments.done", "call_id": "call_1",
						"name": "get_weather", "arguments": `{"city":"Paris"}`})
				}
			case "conversation.item.create":
				item, _ := event["item"].(map[string]any)
				output, _ := item["output"].(string)
				outputs <- output
			case "response.create":
				write(map[string]any{"type": "response.output_audio_transcript.delta", "response_id": "resp_2",
					"delta": "It is 17 degrees with light rain in Paris."})
				write(map[string]any{"type": "response.output_audio.delta", "response_id": "resp_2",
					"delta": base64.StdEncoding.EncodeToString(make([]byte, 480))})
			}
		}
	}))
	defer server.Close()
	result, err := Run(t.Context(), Options{
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Limit: 1, Timeout: 20 * time.Second,
		ResultDelay: 10 * time.Millisecond, WaitConfigured: true,
	})
	if err != nil || len(result.Tasks) != 1 {
		t.Fatalf("run: %v, tasks %d", err, len(result.Tasks))
	}
	task := result.Tasks[0]
	if !task.Passed || task.Metrics["tool_calls"] != 1 || task.Metrics["result_spoken"] != 1 {
		t.Fatalf("task = %+v", task)
	}
	if output := <-outputs; !strings.Contains(output, `"temperature_c":17`) {
		t.Fatalf("function_call_output = %s", output)
	}
}
