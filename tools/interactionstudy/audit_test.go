package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench/capability"
)

func TestAuditRejectsFutureObservationAndUndeclaredInput(t *testing.T) {
	for _, mutation := range []string{"none", "history-omitted", "empty-pending-v2", "future", "request", "future-heard", "affordance-v2", "affordance-undeclared", "affordance-v3-idle", "affordance-v3-wrong-state"} {
		t.Run(mutation, func(t *testing.T) {
			root := t.TempDir()
			pair, _ := capability.PairByID("st-01")
			cell, _ := capability.CellByID("A1")
			if err := writeJSON(filepath.Join(root, "fixtures.json"), []capability.Pair{pair}); err != nil {
				t.Fatal(err)
			}
			if err := writeJSON(filepath.Join(root, "cells.json"), []capability.Cell{cell}); err != nil {
				t.Fatal(err)
			}
			source := capability.NewSource(pair.Branch(pair.Variants[0]), cell.Channels, capability.DefaultDelays)
			listener := capability.NewListener(cell.Channels)
			at := 500 * time.Millisecond
			fresh := source.Advance(at)
			listener.Admit(fresh)
			observation := listener.Observe(at)
			self := capability.Self{}
			if mutation == "future-heard" {
				self.Heard = "not yet played"
			}
			content := cell.Render(observation, self) + "\nRecent decisions and actual execution outcomes (rejected actions produced no speech): null\nCurrent pending segment ID: "
			if mutation == "history-omitted" {
				content = cell.Render(observation, self) + "\nCurrent pending segment ID: "
			}
			if mutation == "future" {
				observation.Words = append(observation.Words, capability.Heard{Text: "future-secret", AdmittedAt: time.Hour})
			}
			if mutation == "empty-pending-v2" {
				content += "segment-opening"
			}
			if mutation == "request" {
				content = "future-secret\n" + content
			}
			if !strings.HasPrefix(mutation, "affordance") {
				content += "\n" + pendingAffordances["v1"].pending
			}
			if mutation == "affordance-v2" || mutation == "affordance-undeclared" {
				content += "\n" + pendingAffordances["v2"].pending
			}
			if mutation == "affordance-v3-idle" {
				content += "\n" + pendingAffordances["v3"].idle
			}
			if mutation == "affordance-v3-wrong-state" {
				// Nothing is pending, so the pending-state wording is undeclared.
				content += "\n" + pendingAffordances["v3"].pending
			}
			request, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "system", "content": "policy"}, {"role": "user", "content": content}}})
			row := map[string]any{"trace_schema_version": 2, "policy_history_omitted": mutation == "history-omitted", "session_id": "run/trial", "turn_id": "decision-1", "pair_id": pair.ID, "variant_id": pair.Variants[0].ID, "cell_id": cell.ID, "model_admission_ns": at, "fresh_evidence": fresh, "observation": observation, "self_at_admission": self, "decision": capability.Decision{Request: request}}
			if mutation == "empty-pending-v2" {
				row["playback_state_version"] = 2
			}
			if mutation == "affordance-v2" {
				row["pending_affordance_version"] = "v2"
			}
			if mutation == "affordance-v3-idle" || mutation == "affordance-v3-wrong-state" {
				row["pending_affordance_version"] = "v3"
			}
			dir := filepath.Join(root, "trial")
			if err := os.Mkdir(dir, 0755); err != nil {
				t.Fatal(err)
			}
			future := time.Hour
			if err := writeJSON(filepath.Join(dir, "result.json"), map[string]any{"ledger": capability.Playback{Segments: []capability.Segment{{ID: "future", Text: "not yet played", CompletedAt: &future}}}}); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(row)
			if err := os.WriteFile(filepath.Join(dir, "actions.jsonl"), append(raw, '\n'), 0644); err != nil {
				t.Fatal(err)
			}
			err := auditLive(root, filepath.Join(root, "audit.json"))
			if (err != nil) != (mutation != "none" && mutation != "history-omitted" && mutation != "affordance-v2" && mutation != "affordance-v3-idle") {
				t.Fatalf("mutation %s: %v", mutation, err)
			}
		})
	}
}

func TestHistoryAuditRejectsFutureIntentAndFalseOutcomes(t *testing.T) {
	history := []executedDecision{{Action: capability.Action{Act: "wait"}, Status: "no-output"},
		{Action: capability.Action{Act: "revise", Text: "Rejected content", ReplacesPending: "old"}, Status: "invalid-replacement"}}
	prefix := "\nRecent decisions and actual execution outcomes (rejected actions produced no speech): "
	suffix := "\nCurrent pending segment ID: current"
	serialize := func(h []executedDecision) string { raw, _ := json.Marshal(h); return prefix + string(raw) + suffix }
	if err := auditHistorySuffix(serialize(history), history, false); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []string{"future", "effect", "omitted", "truncated"} {
		changed := append([]executedDecision(nil), history...)
		switch mutation {
		case "future":
			changed = append(changed, executedDecision{Action: capability.Action{Act: "speak", Text: "Future answer"}, Status: "synthesis-started"})
		case "effect":
			changed[1].Status = "synthesis-started"
		case "omitted":
			changed = nil
		case "truncated":
			changed = changed[1:]
		}
		if auditHistorySuffix(serialize(changed), history, false) == nil {
			t.Fatalf("accepted %s history", mutation)
		}
	}
	if auditHistorySuffix(suffix, history, true) != nil {
		t.Fatal("rejected actual omission")
	}
	if auditHistorySuffix(suffix+serialize(history), history, true) == nil {
		t.Fatal("accepted history after declared omission")
	}
	for len(history) < 12 {
		history = append(history, executedDecision{Action: capability.Action{Act: "wait"}, Status: "no-output"})
	}
	if err := auditHistorySuffix(serialize(history[4:]), history, false); err != nil {
		t.Fatal(err)
	}
	if auditHistorySuffix(serialize(history), history, false) == nil {
		t.Fatal("accepted unbounded history")
	}
}

func TestAuditRejectsIdentityChangeAcrossRows(t *testing.T) {
	for _, field := range []string{"session_id", "pair_id", "variant_id", "cell_id", "policy_history_omitted", "pending_affordance_version"} {
		t.Run(field, func(t *testing.T) {
			root := t.TempDir()
			pair, _ := capability.PairByID("st-01")
			cell, _ := capability.CellByID("A2")
			writeJSON(filepath.Join(root, "fixtures.json"), []capability.Pair{pair})
			writeJSON(filepath.Join(root, "cells.json"), []capability.Cell{cell})
			dir := filepath.Join(root, "trial")
			os.Mkdir(dir, 0755)
			writeJSON(filepath.Join(dir, "result.json"), map[string]any{"ledger": capability.Playback{}})
			source := capability.NewSource(pair.Branch(pair.Variants[0]), cell.Channels, capability.DefaultDelays)
			listener := capability.NewListener(cell.Channels)
			var lines []byte
			for i := 0; i < 2; i++ {
				at := time.Duration(i+1) * 500 * time.Millisecond
				fresh := source.Advance(at)
				listener.Admit(fresh)
				obs := listener.Observe(at)
				self := capability.Self{}
				// Both requests omit history. A changed treatment flag is invalid even
				// if its request is also rewritten consistently with the changed flag.
				omit := true
				if field == "policy_history_omitted" && i == 1 {
					omit = false
				}
				suffix := "\nCurrent pending segment ID: \n" + pendingAffordances["v1"].pending
				if !omit {
					h, _ := json.Marshal([]executedDecision{{Action: capability.Action{Act: "wait"}, Status: "no-output"}})
					suffix = "\nRecent decisions and actual execution outcomes (rejected actions produced no speech): " + string(h) + suffix
				}
				req, _ := json.Marshal(map[string]any{"messages": []map[string]string{{"role": "system", "content": "policy"}, {"role": "user", "content": cell.Render(obs, self) + suffix}}})
				row := map[string]any{"trace_schema_version": 2, "session_id": "session", "pair_id": pair.ID, "variant_id": pair.Variants[0].ID, "cell_id": cell.ID,
					"turn_id": fmt.Sprintf("decision-%d", i+1), "model_admission_ns": at, "fresh_evidence": fresh, "observation": obs, "self_at_admission": self,
					"policy_history_omitted": omit, "execution_status": "no-output", "decision": capability.Decision{Request: req, Action: capability.Action{Act: "wait"}}}
				if i == 1 && field != "policy_history_omitted" {
					row[field] = "changed"
				}
				raw, _ := json.Marshal(row)
				lines = append(lines, raw...)
				lines = append(lines, '\n')
			}
			os.WriteFile(filepath.Join(dir, "actions.jsonl"), lines, 0644)
			out := filepath.Join(root, "audit.json")
			if auditLive(root, out) == nil {
				t.Fatal("accepted identity change")
			}
			raw, _ := os.ReadFile(out)
			if !strings.Contains(string(raw), "session or treatment identity changed within trial") {
				t.Fatalf("wrong failure: %s", raw)
			}
		})
	}
}
