package main

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReplacementUsesCurrentPlaybackState(t *testing.T) {
	for _, tc := range []struct {
		requested, current string
		valid              bool
	}{
		{"segment-1", "segment-1", true},
		{"segment-1", "", false},
		{"segment-1", "segment-2", false},
		{"", "segment-1", false},
		{"", "", false},
	} {
		if got := validReplacement(tc.requested, tc.current); got != tc.valid {
			t.Errorf("requested=%q current=%q got=%v", tc.requested, tc.current, got)
		}
	}
}

func TestCanceledTrialRetainsEvidence(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	pair, _ := capability.PairByID("st-01")
	pair.Variants[0].Expect.Silent = true // Empty failed playback must not pass silence.
	cell, _ := capability.CellByID("A2")
	dir := filepath.Join(t.TempDir(), "canceled-trial")
	err := runLive(ctx, dir, pair, pair.Variants[0], cell, capability.JointPolicy{}, speechsocket.Config{}, false, "v1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	for _, name := range []string{"actions.jsonl", "admitted-evidence.json", "playback.json", "result.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Status  string                    `json:"status"`
		Error   string                    `json:"trial_error"`
		Session string                    `json:"session_id"`
		Screen  *capability.ContentScreen `json:"lexical_playback_screen"`
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Status != "failed" || result.Error == "" || result.Session == "" || result.Screen != nil {
		t.Fatalf("missing terminal identity/failure: %s", raw)
	}
}

func TestExecutionHistoryKeepsRejectedIntentSeparate(t *testing.T) {
	history := executedDecision{
		Action: capability.Action{Act: "revise", Text: "Replacement words", ReplacesPending: "completed-1"},
		Status: "invalid-replacement",
	}
	raw, err := json.Marshal(history)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err = json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded["execution_status"]) != `"invalid-replacement"` || len(decoded["requested_action"]) == 0 {
		t.Fatalf("execution outcome lost: %s", raw)
	}
}

func TestCancellationRetainsItsCause(t *testing.T) {
	canceled := false
	speech := liveSpeech{active: "segment-1", cancel: func() { canceled = true }}
	speech.ledger.Add("segment-1", "Unfinished content.", 0, 24000)
	speech.stop("trial-horizon")
	if !canceled || len(speech.marks) != 1 || speech.marks[0]["reason"] != "trial-horizon" {
		t.Fatalf("lost cancellation cause: %+v", speech.marks)
	}
	if speech.active != "" || speech.cancel != nil {
		t.Fatal("cancel left active work")
	}
}

func TestHistoryTreatmentChangesActualRequestsAndRetainsDeclaration(t *testing.T) {
	for _, omit := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "omitted"}[omit], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			requests := make(chan string, 2)
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 2 {
					http.Error(w, "invalid request", 400)
					return
				}
				requests <- body.Messages[1].Content
				calls++
				if calls == 2 {
					cancel()
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": "stop", "message": map[string]string{"content": `{"act":"wait","text":"","replaces_pending":""}`},
				}}})
			}))
			defer server.Close()
			pair, _ := capability.PairByID("st-01")
			cell, _ := capability.CellByID("A2")
			cell.TickInterval = time.Millisecond
			dir := filepath.Join(t.TempDir(), "trial")
			err := runLive(ctx, dir, pair, pair.Variants[0], cell,
				capability.JointPolicy{URL: server.URL, Model: "fixture", MaxTokens: 128}, speechsocket.Config{}, omit, "v1")
			if err == nil {
				t.Fatal("expected canceled diagnostic")
			}
			for i := 0; i < 2; i++ {
				select {
				case content := <-requests:
					hasHistory := strings.Contains(content, "Recent decisions and actual execution outcomes")
					if hasHistory == omit || !strings.Contains(content, "The user has actually heard you say:") || !strings.Contains(content, "Current pending segment ID:") {
						t.Fatalf("wrong treatment request: %s", content)
					}
					if !omit && i == 1 && !strings.Contains(content, `"execution_status":"no-output"`) {
						t.Fatal("baseline lost prior execution")
					}
				default:
					t.Fatal("missing model request")
				}
			}
			raw, err := os.ReadFile(filepath.Join(dir, "result.json"))
			if err != nil {
				t.Fatal(err)
			}
			var result struct {
				Omit bool `json:"policy_history_omitted"`
			}
			if json.Unmarshal(raw, &result) != nil || result.Omit != omit {
				t.Fatalf("lost treatment identity: %s", raw)
			}
		})
	}
}

func TestSpeechRejectsKnownControlReferencesWithoutSubstringMatching(t *testing.T) {
	for _, tc := range []struct {
		text   string
		leaked bool
	}{
		{"plan-15", true}, {"Use [plan-15].", true}, {"SEGMENT-11!", true},
		{"plan-150", false}, {"Use fifteen units of electricity.", false}, {"", false},
	} {
		if got := speechContainsReference(tc.text, "plan-15", "segment-11", ""); got != tc.leaked {
			t.Fatalf("text=%q leaked=%v want=%v", tc.text, got, tc.leaked)
		}
	}
}

func TestOpeningSynthesisIsPendingBeforeAnyAudioExists(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		http.Error(w, "fixture unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)
	var speech liveSpeech
	speech.start(t.Context(), speechsocket.Config{URL: "ws" + strings.TrimPrefix(server.URL, "http")}, "segment-opening", "Unheard requested content.", time.Now(), 0)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("did not attempt synthesis")
	}
	self, id := speech.state()
	if id != "segment-opening" || self.Heard != "" || self.Speaking || !strings.Contains(self.Pending, "Unheard requested content.") || !strings.Contains(self.Pending, "no audio played") {
		t.Fatalf("opening speech misrepresented: id=%q state=%+v", id, self)
	}
	speech.stop("test-cancel")
	speech.workers.Wait()
	self, id = speech.state()
	if id != "" || self.Pending != "" || self.Heard != "" || len(speech.ledger.Segments) != 0 {
		t.Fatalf("cancelled startup left fictional speech: id=%q state=%+v ledger=%+v", id, self, speech.ledger)
	}
}

func TestPendingAffordanceReachesRequestAndTrace(t *testing.T) {
	pair, _ := capability.PairByID("st-01")
	cell, _ := capability.CellByID("A2")
	cell.TickInterval = time.Millisecond
	if err := runLive(t.Context(), filepath.Join(t.TempDir(), "trial"), pair, pair.Variants[0], cell, capability.JointPolicy{}, speechsocket.Config{}, true, "v9"); err == nil || !strings.Contains(err.Error(), "unknown pending affordance") {
		t.Fatalf("undeclared affordance not rejected as such: %v", err)
	}
	for version := range pendingAffordances {
		// The first request precedes any speech, so nothing is pending.
		line, _ := affordanceLine(version, "")
		t.Run(version, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			requests := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Messages []struct {
						Content string `json:"content"`
					} `json:"messages"`
				}
				if json.NewDecoder(r.Body).Decode(&body) != nil || len(body.Messages) != 2 {
					http.Error(w, "invalid request", 400)
					return
				}
				select {
				case requests <- body.Messages[1].Content:
					cancel()
				default:
				}
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
					"finish_reason": "stop", "message": map[string]string{"content": `{"act":"wait","text":"","replaces_pending":""}`},
				}}})
			}))
			defer server.Close()
			dir := filepath.Join(t.TempDir(), "trial")
			runLive(ctx, dir, pair, pair.Variants[0], cell,
				capability.JointPolicy{URL: server.URL, Model: "fixture", MaxTokens: 128}, speechsocket.Config{}, true, version)
			content := <-requests
			for _, other := range pendingAffordances {
				for _, otherLine := range []string{other.pending, other.idle} {
					if otherLine != "" && otherLine != line && strings.Contains(content, otherLine) {
						t.Fatalf("request for %s carries wrong affordance wording: %s", version, content)
					}
				}
			}
			if !strings.HasSuffix(content, "\n"+line) {
				t.Fatalf("affordance is not the final line: %s", content)
			}
			raw, err := os.ReadFile(filepath.Join(dir, "actions.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			var row struct {
				Version string `json:"pending_affordance_version"`
			}
			if json.Unmarshal([]byte(strings.SplitN(string(raw), "\n", 2)[0]), &row) != nil || row.Version != version {
				t.Fatalf("trace lost affordance identity: %s", raw)
			}
		})
	}
}

func TestAffordanceLineFollowsPendingState(t *testing.T) {
	for _, tc := range []struct{ version, active, want string }{
		{"v1", "", pendingAffordances["v1"].pending}, {"v1", "segment-3", pendingAffordances["v1"].pending},
		{"v2", "", pendingAffordances["v2"].pending},
		{"v3", "", pendingAffordances["v3"].idle}, {"v3", "segment-3", pendingAffordances["v3"].pending},
		{"v4", "", pendingAffordances["v1"].pending}, {"v4", "segment-3", pendingAffordances["v2"].pending},
	} {
		if got, ok := affordanceLine(tc.version, tc.active); !ok || got != tc.want {
			t.Fatalf("%s active=%q: got %q", tc.version, tc.active, got)
		}
	}
	if _, ok := affordanceLine("v9", ""); ok {
		t.Fatal("accepted unknown version")
	}
}
