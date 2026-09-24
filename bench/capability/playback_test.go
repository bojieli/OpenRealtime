package capability_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench/capability"
)

const rate = 1000

// play generates and plays a whole segment, one mark per chunk.
func play(t *testing.T, p *capability.Playback, id, text string, at time.Duration, chunks ...int64) time.Duration {
	t.Helper()
	if err := p.Add(id, text, at, rate); err != nil {
		t.Fatal(err)
	}
	var played int64
	for _, n := range chunks {
		if err := p.Generated(id, n); err != nil {
			t.Fatal(err)
		}
		played += n
		at += time.Duration(n) * time.Second / rate
		if err := p.Mark(id, played, at); err != nil {
			t.Fatal(err)
		}
	}
	return at
}

func TestLedgerSeparatesHeardPartialAndPending(t *testing.T) {
	var p capability.Playback
	end := play(t, &p, "s1", "first sentence", 0, 500, 500)
	if err := p.End("s1"); err != nil {
		t.Fatal(err)
	}
	if err := p.FinishPlayback("s1", end); err != nil {
		t.Fatal(err)
	}
	play(t, &p, "s2", "cut sentence", 2*time.Second, 300)
	p.Cancel("s2")
	play(t, &p, "s3", "playing sentence", 3*time.Second, 200)
	if err := p.Add("s4", "unplayed", 4*time.Second, rate); err == nil {
		// A second active segment is the runner's error to prevent, but the
		// ledger must still describe it truthfully as unplayed.
		self := p.Self()
		if self.Heard != "first sentence" {
			t.Fatalf("heard %q", self.Heard)
		}
		for _, want := range []string{"[s2] CANCELED", "exact heard words unknown", "[s3] partially played"} {
			if !strings.Contains(self.Partial, want) {
				t.Fatalf("partial lacks %q: %s", want, self.Partial)
			}
		}
		if !strings.Contains(self.Pending, "[s4] no playback acknowledged yet") || strings.Contains(self.Pending, "s2") {
			t.Fatalf("pending: %s", self.Pending)
		}
		if !self.Speaking || self.Began != 3*time.Second {
			t.Fatalf("speaking=%t began=%v", self.Speaking, self.Began)
		}
	} else {
		t.Fatal(err)
	}
	p.Cancel("s1") // completed speech stays heard, and stays scoreable
	if p.Self().Heard != "first sentence" || p.Segments[0].Cut {
		t.Fatal("cancel erased heard speech")
	}
}

func TestLedgerRejectsImpossiblePlayback(t *testing.T) {
	var p capability.Playback
	if err := p.Add("s", "x", time.Second, rate); err != nil {
		t.Fatal(err)
	}
	if p.Add("s", "x", time.Second, rate) == nil {
		t.Fatal("accepted duplicate segment")
	}
	if p.Mark("s", 10, 2*time.Second) == nil {
		t.Fatal("accepted playback of ungenerated audio")
	}
	if err := p.Generated("s", 100); err != nil {
		t.Fatal(err)
	}
	if p.Mark("s", 50, time.Second/2) == nil {
		t.Fatal("accepted playback before generation")
	}
	if p.FinishPlayback("s", 3*time.Second) == nil {
		t.Fatal("completed a segment that never ended or played")
	}
	p.Cancel("s")
	if p.Mark("s", 50, 3*time.Second) == nil {
		t.Fatal("accepted playback after cut")
	}
}

func TestScreenCountsOnlyCompletedSpeechGeneratedInsideTheWindow(t *testing.T) {
	expect := capability.Expectation{Within: 10 * time.Second, MinWords: 3,
		RequireAnyOf: [][]string{{"olive oil"}}, Forbid: []string{"butter"}}
	complete := func(p *capability.Playback, id, text string, at time.Duration) {
		end := play(t, p, id, text, at, 1000)
		if err := p.End(id); err != nil {
			t.Fatal(err)
		}
		if err := p.FinishPlayback(id, end); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		build func(*capability.Playback)
		pass  bool
	}{
		"fulfilled":         {func(p *capability.Playback) { complete(p, "a", "heat the olive oil first", 12*time.Second) }, true},
		"silent":            {func(*capability.Playback) {}, false},
		"acknowledgement":   {func(p *capability.Playback) { complete(p, "a", "okay got it", 12*time.Second) }, false},
		"wrong content":     {func(p *capability.Playback) { complete(p, "a", "melt the butter now", 12*time.Second) }, false},
		"generated earlier": {func(p *capability.Playback) { complete(p, "a", "heat the olive oil first", 9*time.Second) }, false},
		"too late":          {func(p *capability.Playback) { complete(p, "a", "heat the olive oil first", 19800*time.Millisecond) }, false},
		"cut before heard": {func(p *capability.Playback) {
			play(t, p, "a", "heat the olive oil first", 12*time.Second, 400)
			p.Cancel("a")
		}, false},
		"generated but unheard": {func(p *capability.Playback) {
			if err := p.Add("a", "heat the olive oil first", 12*time.Second, rate); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		var p capability.Playback
		tc.build(&p)
		if got := capability.ScreenContent(expect, 10*time.Second, p); got.Passed != tc.pass {
			t.Errorf("%s: passed=%t want %t (%+v)", name, got.Passed, tc.pass, got)
		}
	}
	quiet := capability.Expectation{Within: 5 * time.Second, Silent: true}
	var talking capability.Playback
	play(t, &talking, "a", "hello", 11*time.Second, 500)
	if capability.ScreenContent(quiet, 10*time.Second, talking).Passed {
		t.Error("silent expectation passed while speaking in the window")
	}
	if !capability.ScreenContent(quiet, 10*time.Second, capability.Playback{}).Passed {
		t.Error("silent expectation failed with no speech")
	}
}

func TestActionValidation(t *testing.T) {
	for _, tc := range []struct {
		action capability.Action
		ok     bool
	}{
		{capability.Action{Act: "wait"}, true},
		{capability.Action{Act: "wait", Text: "hi"}, false},
		{capability.Action{Act: "speak", Text: "hi"}, true},
		{capability.Action{Act: "speak"}, false},
		{capability.Action{Act: "speak", Text: "hi", ReplacesPending: "s1"}, false},
		{capability.Action{Act: "revise", Text: "hi", ReplacesPending: "s1"}, true},
		{capability.Action{Act: "revise", Text: "hi"}, false},
		{capability.Action{Act: "shout", Text: "hi"}, false},
	} {
		if err := tc.action.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: err=%v", tc.action, err)
		}
	}
}

func TestPolicyRequestAndResponseHandling(t *testing.T) {
	var seen map[string]any
	reply := `{"act":"speak","text":"hello there","replaces_pending":""}`
	finish := "stop"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		json.NewDecoder(r.Body).Decode(&seen)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": finish, "message": map[string]string{"content": reply}}}})
	}))
	defer server.Close()
	policy := capability.JointPolicy{URL: server.URL + "/v1", Model: "m", MaxTokens: 128, Seed: 7}
	decision, err := policy.Decide(context.Background(), "Be brief.", "observation text")
	if err != nil || decision.Action.Text != "hello there" {
		t.Fatalf("decision %+v err %v", decision, err)
	}
	messages := seen["messages"].([]any)
	if !strings.HasSuffix(messages[0].(map[string]any)["content"].(string), "Session instructions:\nBe brief.") ||
		messages[1].(map[string]any)["content"] != "observation text" || seen["structured_outputs"] == nil {
		t.Fatalf("request %v", seen)
	}
	finish = "length"
	if _, err = policy.Decide(context.Background(), "", ""); err == nil {
		t.Fatal("accepted a truncated action")
	}
	finish, reply = "stop", `{"act":"wait","text":"leak","replaces_pending":""}`
	if _, err = policy.Decide(context.Background(), "", ""); err == nil {
		t.Fatal("accepted an invalid action")
	}
	if _, err = (capability.JointPolicy{URL: server.URL}).Decide(context.Background(), "", ""); err == nil {
		t.Fatal("accepted a zero token budget")
	}
}

func TestThinkingPolicyTakesTheFinalActionAfterReasoning(t *testing.T) {
	var seen map[string]any
	reply := `<think>maybe {"act":"wait","text":"","replaces_pending":""} is best</think>` +
		`{"act":"revise","text":"Now the connection.","replaces_pending":"segment-4"}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&seen)
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]string{"content": reply}}}})
	}))
	defer server.Close()
	policy := capability.JointPolicy{URL: server.URL, Model: "m", MaxTokens: 4000, Thinking: true}
	decision, err := policy.Decide(context.Background(), "", "")
	if err != nil || decision.Action.Act != "revise" || decision.Action.ReplacesPending != "segment-4" {
		t.Fatalf("decision %+v err %v", decision.Action, err)
	}
	if seen["structured_outputs"] != nil || seen["chat_template_kwargs"].(map[string]any)["enable_thinking"] != true {
		t.Fatalf("thinking request %v", seen)
	}
	reply = `<think>{"act":"speak","text":"inside reasoning only","replaces_pending":""}</think>no action here`
	if _, err = policy.Decide(context.Background(), "", ""); err == nil {
		t.Fatal("accepted an action that appeared only inside the reasoning")
	}
}
