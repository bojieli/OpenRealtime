package candidate_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
)

func TestNewAttemptIsCandidateOnlyCanonicalAndOwnsInputs(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginProduction, bench.TransportWebSocket,
		"wss://user:secret@example.test/v1/realtime?api_key=do-not-retain#fragment",
	)
	if err != nil {
		t.Fatal(err)
	}
	cell := bench.Reference()
	cell.Name = "fdb-candidate"
	contextValue := map[string]any{"z": 2, "a": []string{"one", "two"}}
	attempt, err := candidate.NewAttempt(
		"fdb-v1.5", "user_interruption/1", 1, cell,
		bench.Provenance{Revision: "candidate-revision"}, origin, contextValue,
	)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(attempt.Context), `{"a":["one","two"],"z":2}`; got != want {
		t.Fatalf("context = %s, want %s", got, want)
	}
	encoded, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		[]byte("secret"), []byte("api_key"), []byte("do-not-retain"), []byte("historical"),
	} {
		if bytes.Contains(encoded, forbidden) {
			t.Fatalf("retained attempt contains forbidden bytes %q", forbidden)
		}
	}
	cell.Levels[bench.FactorBinding] = "mutated"
	contextValue["z"] = 99
	if attempt.Cell.Levels[bench.FactorBinding] == "mutated" ||
		string(attempt.Context) != `{"a":["one","two"],"z":2}` {
		t.Fatal("attempt aliases caller-owned inputs")
	}
}

func TestRunOriginSeparatesProductionAndHermeticWithoutEndpointSecrets(t *testing.T) {
	production, err := candidate.NewRunOrigin(
		candidate.OriginProduction, bench.TransportWebRTC,
		"https://example.test/realtime/calls?token=one",
	)
	if err != nil {
		t.Fatal(err)
	}
	samePublicEndpoint, err := candidate.NewRunOrigin(
		candidate.OriginProduction, bench.TransportWebRTC,
		"https://other:credentials@example.test/realtime/calls?token=two#private",
	)
	if err != nil {
		t.Fatal(err)
	}
	if production.EndpointSHA256 != samePublicEndpoint.EndpointSHA256 {
		t.Fatal("credential-only URL changes altered the public endpoint identity")
	}
	hermetic, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebRTC,
		"https://example.test/realtime/calls",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !production.Live || hermetic.Live {
		t.Fatal("run-origin live status does not follow its explicit kind")
	}
}

func TestAttemptRejectsNonObjectAndExecutionDrift(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	cell := bench.Reference()
	if _, err := candidate.NewAttempt(
		"suite", "case", 1, cell, bench.Provenance{}, origin, []string{"not", "an", "object"},
	); err == nil {
		t.Fatal("non-object context was accepted")
	}
	attempt, err := candidate.NewAttempt(
		"suite", "case", 1, cell, bench.Provenance{}, origin, struct {
			Criterion string `json:"criterion"`
		}{Criterion: "retain every new attempt"},
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt.ExecutionRequirement = bench.ExecutionRequirement{FormatVersion: 99}
	if err := attempt.Validate(); err == nil {
		t.Fatal("execution-requirement drift was accepted")
	}
}

func TestCloneCompletionOwnsOutcomeTranscriptAndContext(t *testing.T) {
	origin, err := candidate.NewRunOrigin(
		candidate.OriginHermetic, bench.TransportWebSocket, "ws://127.0.0.1:8080/v1/realtime",
	)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := candidate.NewAttempt(
		"suite", "case", 1, bench.Reference(), bench.Provenance{}, origin,
		map[string]any{"criterion": "exact"},
	)
	if err != nil {
		t.Fatal(err)
	}
	source := candidate.Completion{
		Attempt:    attempt,
		Outcome:    bench.TaskOutcome{ID: "case", Completed: true, Notes: map[string]string{"state": "ok"}},
		Transcript: bench.Transcript{Moments: []bench.Moment{{Kind: bench.MomentTranscript, Text: "hello"}}},
	}
	clone, err := candidate.CloneCompletion(source)
	if err != nil {
		t.Fatal(err)
	}
	source.Attempt.Context[0] = '['
	source.Outcome.Notes["state"] = "mutated"
	source.Transcript.Moments[0].Text = "mutated"
	if clone.Outcome.Notes["state"] != "ok" || clone.Transcript.Moments[0].Text != "hello" ||
		clone.Attempt.Context[0] != '{' {
		t.Fatal("cloned completion aliases source storage")
	}
}

func TestCloneResultAndStageErrorDoNotAliasOrHideCause(t *testing.T) {
	source := bench.Result{
		Suite: "suite", Cell: bench.Reference(),
		Tasks: []bench.TaskOutcome{{ID: "case", Notes: map[string]string{"state": "original"}}},
		Summary: bench.Summary{Distributions: map[string]bench.Distribution{
			"latency_ms": {Count: 1, Unit: "ms", P50: 10},
		}},
	}
	clone, err := candidate.CloneResult(source)
	if err != nil {
		t.Fatal(err)
	}
	source.Cell.Levels[bench.FactorBinding] = "mutated"
	source.Tasks[0].Notes["state"] = "mutated"
	source.Summary.Distributions["latency_ms"] = bench.Distribution{P50: 99}
	if clone.Cell.Levels[bench.FactorBinding] == "mutated" ||
		clone.Tasks[0].Notes["state"] != "original" ||
		clone.Summary.Distributions["latency_ms"].P50 != 10 {
		t.Fatal("cloned result aliases source storage")
	}

	cause := errors.New("retention unavailable")
	wrapped := candidate.StageError("case", "finish suite", cause)
	if !errors.Is(wrapped, cause) || candidate.StageError("", "", nil) != nil {
		t.Fatal("candidate stage error did not preserve its cause or nil semantics")
	}
}
