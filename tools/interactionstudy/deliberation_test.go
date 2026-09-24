package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
)

func TestUserWordsCountsOnlyAdmittedUserWords(t *testing.T) {
	admitted := []capability.Admission{
		{Speaker: capability.SpeakerUser, Mark: capability.MarkWord},
		{Speaker: capability.SpeakerUser, Mark: capability.MarkRevision},
		{Speaker: capability.SpeakerUser, Mark: capability.MarkWord, Withheld: "provisional"},
		{Speaker: capability.SpeakerUser, Mark: capability.MarkSoundOnset},
		{Speaker: "other", Mark: capability.MarkWord},
	}
	if got := userWords(admitted); got != 2 {
		t.Fatalf("counted %d", got)
	}
}

func readStatuses(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "deliberations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var row struct {
			Status string `json:"status"`
		}
		json.Unmarshal([]byte(line), &row)
		out = append(out, row.Status)
	}
	return out
}

func TestProposalIsDiscardedWhenNewerUserWordsArrived(t *testing.T) {
	dir := t.TempDir()
	bg, err := newBackground(dir, capability.JointPolicy{}, "", time.Now(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var speech liveSpeech
	revise := capability.Decision{Action: capability.Action{Act: "speak", Text: "Now the connection."}}
	for _, tc := range []struct{ snapshotWords, nowWords int }{{3, 5}, {5, 5}} {
		d := &deliberation{id: tc.nowWords, userWords: tc.snapshotWords, decision: revise, done: make(chan struct{})}
		if err := bg.admit(context.Background(), d, time.Second, tc.nowWords, &speech, speechsocket.Config{}); err != nil {
			t.Fatal(err)
		}
		if tc.snapshotWords != tc.nowWords {
			if _, active := speech.state(); active != "" {
				t.Fatal("a stale proposal started speech")
			}
		}
	}
	speech.stop("test")
	speech.workers.Wait()
	bg.close(2*time.Second, 5)
	if got := readStatuses(t, dir); strings.Join(got, ",") != "stale,synthesis-started" {
		t.Fatalf("statuses %v", got)
	}
}

func TestDeliberationAuditRejectsFutureEvidenceAndStaleExecution(t *testing.T) {
	cell, _ := capability.CellByID("A2D")
	observation := capability.Observation{Now: 5 * time.Second,
		Words: []capability.Heard{{Speaker: capability.SpeakerUser, Text: "skip", AdmittedAt: 4 * time.Second}}}
	good := func() map[string]any {
		request, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "system", "content": "p"},
			{"role": "user", "content": cell.Render(observation, capability.Self{Heard: "earlier"}) + "\n" + deliberationHint}}})
		return map[string]any{"deliberation_id": 1, "snapshot_ns": 5 * time.Second, "user_words_at_snapshot": 1,
			"observation": observation, "self_at_snapshot": capability.Self{Heard: "earlier"}, "finished_ns": 8 * time.Second,
			"admission_check_ns": 8500 * time.Millisecond, "user_words_at_check": 1,
			"decision": capability.Decision{Request: request}, "status": "synthesis-started"}
	}
	for name, mutate := range map[string]func(map[string]any){
		"clean":           func(map[string]any) {},
		"future word":     func(r map[string]any) { r["snapshot_ns"] = 3 * time.Second; r["finished_ns"] = 8 * time.Second },
		"stale executed":  func(r map[string]any) { r["user_words_at_check"] = 2 },
		"early executed":  func(r map[string]any) { r["admission_check_ns"] = 7 * time.Second },
		"before speaking": func(r map[string]any) { r["self_at_snapshot"] = capability.Self{} },
		"wrong request": func(r map[string]any) {
			request, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"content": "p"}, {"content": "other"}}})
			r["decision"] = capability.Decision{Request: request}
		},
	} {
		row := good()
		mutate(row)
		raw, _ := json.Marshal(row)
		path := filepath.Join(t.TempDir(), "deliberations.jsonl")
		os.WriteFile(path, append(raw, '\n'), 0644)
		violations, err := auditDeliberations(path, cell)
		if err != nil {
			t.Fatal(err)
		}
		if (len(violations) == 0) != (name == "clean") {
			t.Errorf("%s: violations %v", name, violations)
		}
	}
}

func TestDeliberationWaitsUntilTheAssistantHasSpoken(t *testing.T) {
	if hasSpoken(capability.Self{}) {
		t.Fatal("deliberation may start before any assistant speech")
	}
	for _, self := range []capability.Self{{Heard: "x"}, {Partial: "x"}, {Pending: "x"}, {Speaking: true}} {
		if !hasSpoken(self) {
			t.Fatalf("%+v not counted as spoken", self)
		}
	}
}
