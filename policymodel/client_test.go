package policymodel_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/admission"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/policymodel"
)

type stubServer struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []map[string]any
	answer   string
	logprob  float64
	delay    time.Duration
	status   int
	raw      string
}

func newStub(t *testing.T, answer string) *stubServer {
	stub := &stubServer{answer: answer, status: http.StatusOK}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		stub.mu.Lock()
		stub.requests = append(stub.requests, decoded)
		answer, delay, status, logprob, raw := stub.answer, stub.delay, stub.status, stub.logprob, stub.raw
		stub.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != http.StatusOK {
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(`{"error":{"message":"unavailable"}}`))
			return
		}
		if raw != "" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(raw))
			return
		}
		response := map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"content": answer},
				"logprobs": map[string]any{"content": []map[string]any{{
					"top_logprobs": []map[string]any{{"token": answer, "logprob": logprob}},
				}}},
			}},
		}
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(response)
	}))
	t.Cleanup(stub.server.Close)
	return stub
}

type dumpedPolicyExchange struct {
	StartedAt  string          `json:"started_at"`
	EndedAt    string          `json:"ended_at"`
	DurationMS float64         `json:"duration_ms"`
	Request    json.RawMessage `json:"request"`
	Status     int             `json:"status"`
	Response   string          `json:"response"`
	Error      string          `json:"error"`
}

func readPolicyDump(t *testing.T, path string) dumpedPolicyExchange {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(payload)), "\n")
	if len(lines) != 1 {
		t.Fatalf("policy dump has %d records, want 1: %s", len(lines), payload)
	}
	var record dumpedPolicyExchange
	if err := json.Unmarshal([]byte(lines[0]), &record); err != nil {
		t.Fatalf("decode policy dump: %v: %s", err, payload)
	}
	return record
}

func requirePolicyDumpTiming(t *testing.T, record dumpedPolicyExchange, minimum time.Duration) {
	t.Helper()
	startedAt, err := time.Parse(time.RFC3339Nano, record.StartedAt)
	if err != nil {
		t.Fatalf("parse started_at %q: %v", record.StartedAt, err)
	}
	endedAt, err := time.Parse(time.RFC3339Nano, record.EndedAt)
	if err != nil {
		t.Fatalf("parse ended_at %q: %v", record.EndedAt, err)
	}
	if endedAt.Before(startedAt) {
		t.Fatalf("policy attempt ended before it started: %s < %s", endedAt, startedAt)
	}
	if record.DurationMS < 0 {
		t.Fatalf("duration_ms = %f, want non-negative", record.DurationMS)
	}
	minimumMS := float64(minimum) / float64(time.Millisecond)
	if record.DurationMS < minimumMS {
		t.Fatalf("duration_ms = %f, want at least %f", record.DurationMS, minimumMS)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type failingReader struct{ err error }

func (reader failingReader) Read([]byte) (int, error) { return 0, reader.err }

func (stub *stubServer) prompts() []string {
	stub.mu.Lock()
	defer stub.mu.Unlock()
	var prompts []string
	for _, request := range stub.requests {
		messages, _ := request["messages"].([]any)
		for _, message := range messages {
			content, _ := message.(map[string]any)["content"].(string)
			prompts = append(prompts, content)
		}
	}
	return prompts
}

func newClient(t *testing.T, stub *stubServer, configure func(*policymodel.Config)) *policymodel.Client {
	t.Helper()
	config := policymodel.Config{
		BaseURL: stub.server.URL, Model: "policy-3b", Timeout: 2 * time.Second,
	}
	if configure != nil {
		configure(&config)
	}
	client, err := policymodel.New(config)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return client
}

func TestAnEnumeratedAnswerIsAccepted(t *testing.T) {
	stub := newStub(t, "finished")
	client := newClient(t, stub, nil)
	outcome, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if outcome.Option != "finished" || outcome.Index != 1 {
		t.Fatalf("unexpected outcome %+v", outcome)
	}
	if outcome.ElapsedNS == 0 {
		t.Fatal("a decision must record how long it took: latency is the point of a small model")
	}
}

func TestOptInPolicyDumpRetainsExactRequestAndResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	stub := newStub(t, "finished")
	stub.mu.Lock()
	stub.delay = 25 * time.Millisecond
	stub.mu.Unlock()
	client := newClient(t, stub, func(config *policymodel.Config) {
		config.APIKey = "must-not-appear-in-policy-dump"
	})
	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
		Evidence: "current utterance",
	}); err != nil {
		t.Fatal(err)
	}
	record := readPolicyDump(t, path)
	var request struct {
		Model    string           `json:"model"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(record.Request, &request); err != nil {
		t.Fatalf("decode dumped request: %v: %s", err, record.Request)
	}
	if request.Model != "policy-3b" || len(request.Messages) != 1 ||
		record.Status != http.StatusOK || !strings.Contains(record.Response, `"finished"`) {
		t.Fatalf("policy dump = %+v", record)
	}
	if strings.Contains(string(record.Request), "must-not-appear-in-policy-dump") {
		t.Fatal("policy dump must not retain the authorization header")
	}
	requirePolicyDumpTiming(t, record, 10*time.Millisecond)
}

func TestOptInPolicyDumpRetainsExactGenerationExchange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy-generation.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	stub := newStub(t, "pin conversation tell them when the build finishes")
	client := newClient(t, stub, nil)
	answer, err := client.Generate(
		context.Background(), "extract a standing policy", "Tell me when the build finishes.", 17,
	)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "pin conversation tell them when the build finishes" {
		t.Fatalf("generation answer = %q", answer)
	}
	record := readPolicyDump(t, path)
	var request struct {
		Model     string `json:"model"`
		MaxTokens int    `json:"max_tokens"`
		Messages  []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(record.Request, &request); err != nil {
		t.Fatalf("decode dumped generation request: %v: %s", err, record.Request)
	}
	if request.Model != "policy-3b" || request.MaxTokens != 17 ||
		len(request.Messages) != 2 ||
		request.Messages[0].Role != "system" ||
		request.Messages[0].Content != "extract a standing policy" ||
		request.Messages[1].Role != "user" ||
		request.Messages[1].Content != "Tell me when the build finishes." ||
		record.Status != http.StatusOK ||
		!strings.Contains(record.Response, `pin conversation tell them when the build finishes`) {
		t.Fatalf("policy generation dump = %+v", record)
	}
	requirePolicyDumpTiming(t, record, 0)
}

func TestPolicyDumpRetainsProviderFailureTimingAndEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "provider-error.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	stub := newStub(t, "finished")
	stub.mu.Lock()
	stub.status = http.StatusServiceUnavailable
	stub.mu.Unlock()
	client := newClient(t, stub, nil)
	_, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "p", Options: []string{"a", "b"},
	})
	if err == nil {
		t.Fatal("an unavailable policy model must be reported")
	}
	record := readPolicyDump(t, path)
	if record.Status != http.StatusServiceUnavailable ||
		record.Response != `{"error":{"message":"unavailable"}}` ||
		!strings.Contains(record.Error, "HTTP 503") {
		t.Fatalf("provider failure dump = %+v", record)
	}
	requirePolicyDumpTiming(t, record, 0)
}

func TestPolicyDumpRetainsMalformedGenerationResponse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "malformed-response.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	stub := newStub(t, "unused")
	stub.mu.Lock()
	stub.raw = `{"choices":`
	stub.mu.Unlock()
	client := newClient(t, stub, nil)
	if _, err := client.Generate(context.Background(), "extract policy", "evidence", 16); err == nil {
		t.Fatal("malformed provider JSON must be reported")
	}
	record := readPolicyDump(t, path)
	if record.Status != http.StatusOK || record.Response != `{"choices":` || record.Error == "" {
		t.Fatalf("malformed response dump = %+v", record)
	}
	requirePolicyDumpTiming(t, record, 0)
}

func TestPolicyDumpRetainsTransportFailureTimingWithoutHeaders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transport-error.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	client, err := policymodel.New(policymodel.Config{
		BaseURL: "http://policy.invalid/v1",
		Model:   "policy-3b",
		APIKey:  "must-not-appear-in-policy-dump",
		Timeout: time.Second,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return nil, errors.New("wire unavailable")
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Decide(context.Background(), interaction.Decision{
		Prompt: "p", Options: []string{"a", "b"},
	})
	if err == nil {
		t.Fatal("transport failure must be reported")
	}
	record := readPolicyDump(t, path)
	if record.Status != 0 || record.Response != "" || !strings.Contains(record.Error, "wire unavailable") {
		t.Fatalf("transport failure dump = %+v", record)
	}
	if strings.Contains(string(record.Request), "must-not-appear-in-policy-dump") {
		t.Fatal("policy dump must not retain the authorization header")
	}
	requirePolicyDumpTiming(t, record, 0)
}

func TestPolicyDumpRetainsPartialResponseWhenBodyReadFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "body-read-error.jsonl")
	t.Setenv("OPENREALTIME_DUMP_POLICY_REQUESTS", path)
	readErr := errors.New("response body broke")
	client, err := policymodel.New(policymodel.Config{
		BaseURL: "http://policy.invalid/v1",
		Model:   "policy-3b",
		Timeout: time.Second,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body: io.NopCloser(io.MultiReader(
					strings.NewReader("partial"), failingReader{err: readErr},
				)),
				Request: request,
			}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Generate(context.Background(), "extract policy", "evidence", 16); err == nil {
		t.Fatal("response body failure must be reported")
	}
	record := readPolicyDump(t, path)
	if record.Status != http.StatusOK || record.Response != "partial" ||
		!strings.Contains(record.Error, readErr.Error()) {
		t.Fatalf("body-read failure dump = %+v", record)
	}
	requirePolicyDumpTiming(t, record, 0)
}

// An answer that is not on the list is a refusal, not something to interpret.
// Coercing it to the nearest option is how an enumerated output quietly
// becomes free generation.
func TestAnAnswerOffTheListIsRefused(t *testing.T) {
	stub := newStub(t, "yes, I think they are done speaking")
	client := newClient(t, stub, nil)
	_, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
	})
	if err == nil {
		t.Fatal("an answer outside the enumeration must be refused")
	}
	if !strings.Contains(err.Error(), "not one of") {
		t.Fatalf("the refusal must say why: %v", err)
	}
	if client.Metrics().Refusals != 1 {
		t.Fatal("refusals must be counted: a model that keeps answering off the list is too small for the job")
	}
}

func TestPunctuationAroundAValidAnswerIsAccepted(t *testing.T) {
	for _, answer := range []string{"finished.", " Finished ", "\"finished\"", "finished!"} {
		stub := newStub(t, answer)
		client := newClient(t, stub, nil)
		outcome, err := client.Decide(context.Background(), interaction.Decision{
			Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
		})
		if err != nil {
			t.Fatalf("answer %q was refused: %v", answer, err)
		}
		if outcome.Option != "finished" {
			t.Fatalf("answer %q resolved to %q", answer, outcome.Option)
		}
	}
}

func TestTheOptionsAreSentToTheModel(t *testing.T) {
	stub := newStub(t, "none")
	client := newClient(t, stub, func(config *policymodel.Config) { config.GuidedChoice = true })
	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "should the agent interject", Options: []string{"none", "acknowledge"},
		Evidence: "so far: I would like to",
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	prompts := stub.prompts()
	if len(prompts) != 1 {
		t.Fatalf("expected one prompt, got %d", len(prompts))
	}
	if !strings.Contains(prompts[0], "exactly one of: none, acknowledge") {
		t.Fatalf("the enumeration must reach the model: %q", prompts[0])
	}
	stub.mu.Lock()
	structured, present := stub.requests[0]["structured_outputs"].(map[string]any)
	_, legacyPresent := stub.requests[0]["guided_choice"]
	maxTokens := stub.requests[0]["max_tokens"]
	stub.mu.Unlock()
	choice, choicePresent := structured["choice"].([]any)
	if !present || !choicePresent || len(choice) != 2 || choice[0] != "none" || choice[1] != "acknowledge" {
		t.Fatalf("structured choice must carry the exact ordered enumeration when enabled: %#v", structured)
	}
	if legacyPresent {
		t.Fatal("current vLLM ignores the obsolete top-level guided_choice field")
	}
	if maxTokens != float64(len("acknowledge")+1) {
		t.Fatalf("completion budget %v does not conservatively cover the longest exact option", maxTokens)
	}
}

func TestEveryEnumeratedAnswerFitsTheCompletionBudget(t *testing.T) {
	stub := newStub(t, "addressed-elsewhere")
	client := newClient(t, stub, nil)
	options := []string{"condition-met", "direct-request", "addressed-elsewhere", "wait"}
	outcome, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "who is the current utterance addressed to", Options: options,
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Option != "addressed-elsewhere" {
		t.Fatalf("option = %q", outcome.Option)
	}
	stub.mu.Lock()
	maxTokens := stub.requests[0]["max_tokens"]
	stub.mu.Unlock()
	if maxTokens != float64(len("addressed-elsewhere")+1) {
		t.Fatalf("completion budget = %v, want %d", maxTokens, len("addressed-elsewhere")+1)
	}
}

func TestStructuredChoiceIsAbsentUnlessExplicitlyEnabled(t *testing.T) {
	stub := newStub(t, "none")
	client := newClient(t, stub, nil)
	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "should the agent interject", Options: []string{"none", "acknowledge"},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if _, present := stub.requests[0]["structured_outputs"]; present {
		t.Fatal("structured decoding must not be assumed for an undeclared provider")
	}
	if _, present := stub.requests[0]["guided_choice"]; present {
		t.Fatal("the obsolete top-level guided_choice field must never be sent")
	}
}

func TestAMalformedQuestionIsRefusedBeforeAModelSeesIt(t *testing.T) {
	stub := newStub(t, "none")
	client := newClient(t, stub, nil)
	for _, decision := range []interaction.Decision{
		{Prompt: "", Options: []string{"a", "b"}},
		{Prompt: "p", Options: []string{"only"}},
		{Prompt: "p", Options: []string{"a", "a"}},
	} {
		if _, err := client.Decide(context.Background(), decision); err == nil {
			t.Fatalf("expected %+v to be refused", decision)
		}
	}
	if len(stub.prompts()) != 0 {
		t.Fatal("a malformed question must never reach the model")
	}
}

func TestAdmissionGatesPolicyDecisions(t *testing.T) {
	stub := newStub(t, "finished")
	governor, err := admission.NewGovernor(admission.Config{Capacity: 1})
	if err != nil {
		t.Fatalf("new governor: %v", err)
	}
	// Hold the only unit of capacity with higher-priority work.
	lease, err := governor.Acquire(context.Background(), admission.Request{
		Class: admission.ClassUrgent, Cost: 1, Label: "foreground",
	})
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	client := newClient(t, stub, func(config *policymodel.Config) {
		config.Governor = governor
		config.Timeout = 100 * time.Millisecond
	})
	_, err = client.Decide(context.Background(), interaction.Decision{
		Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
	})
	if err == nil {
		t.Fatal("a policy decision that cannot get compute must not run")
	}
	if !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("the failure must name the reason: %v", err)
	}
	if len(stub.prompts()) != 0 {
		t.Fatal("an unadmitted decision must not reach the model")
	}
	lease.Release()

	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "has the turn ended", Options: []string{"continuing", "finished"},
	}); err != nil {
		t.Fatalf("with capacity free the decision must run: %v", err)
	}
}

func TestAProviderFailureIsReportedRatherThanGuessed(t *testing.T) {
	stub := newStub(t, "finished")
	stub.mu.Lock()
	stub.status = http.StatusServiceUnavailable
	stub.mu.Unlock()
	client := newClient(t, stub, nil)
	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "p", Options: []string{"a", "b"},
	}); err == nil {
		t.Fatal("an unavailable policy model must be reported")
	}
}

func TestSlowDecisionsTimeOutRatherThanDelayTheConversation(t *testing.T) {
	stub := newStub(t, "finished")
	stub.mu.Lock()
	stub.delay = 300 * time.Millisecond
	stub.mu.Unlock()
	client := newClient(t, stub, func(config *policymodel.Config) {
		config.Timeout = 50 * time.Millisecond
	})
	started := time.Now()
	if _, err := client.Decide(context.Background(), interaction.Decision{
		Prompt: "p", Options: []string{"a", "b"},
	}); err == nil {
		t.Fatal("a decision that arrives late is worthless and must time out")
	}
	if elapsed := time.Since(started); elapsed > 200*time.Millisecond {
		t.Fatalf("the timeout must bound the wait, took %s", elapsed)
	}
}

func TestNewRequiresAModel(t *testing.T) {
	if _, err := policymodel.New(policymodel.Config{}); err == nil {
		t.Fatal("a policy model needs a model identity")
	}
}
