package policymodel_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
}

func newStub(t *testing.T, answer string) *stubServer {
	stub := &stubServer{answer: answer, status: http.StatusOK}
	stub.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		stub.mu.Lock()
		stub.requests = append(stub.requests, decoded)
		answer, delay, status, logprob := stub.answer, stub.delay, stub.status, stub.logprob
		stub.mu.Unlock()
		if delay > 0 {
			time.Sleep(delay)
		}
		if status != http.StatusOK {
			writer.WriteHeader(status)
			_, _ = writer.Write([]byte(`{"error":{"message":"unavailable"}}`))
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
	if maxTokens.(float64) > 8 {
		t.Fatal("a policy model is given no room to generate freely")
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
