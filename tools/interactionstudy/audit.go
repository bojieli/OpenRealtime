package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench/capability"
)

// auditLive replays fixture evidence independently of recorded observations,
// then checks the exact request's observation prefix. It does not certify the
// annotated audio alignment, remaining execution suffix, or semantic scorer.
func auditLive(root, out string) error {
	var pairs []capability.Pair
	raw, err := os.ReadFile(filepath.Join(root, "fixtures.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &pairs); err != nil {
		return err
	}
	var cells []capability.Cell
	raw, err = os.ReadFile(filepath.Join(root, "cells.json"))
	if err != nil {
		return err
	}
	if err = json.Unmarshal(raw, &cells); err != nil {
		return err
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "actions.jsonl"))
	if err != nil {
		return err
	}
	checked := 0
	violations := []string{}
	for _, path := range paths {
		var terminal struct {
			Ledger capability.Playback `json:"ledger"`
		}
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(path), "result.json"))
		if err != nil {
			return fmt.Errorf("audit requires terminal evidence: %w", err)
		}
		if err := json.Unmarshal(raw, &terminal); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		var source *capability.Source
		var listener *capability.Listener
		var cell capability.Cell
		last := time.Duration(-1)
		turns := map[string]bool{}
		var history []executedDecision
		var identity string
		for scanner.Scan() {
			var row struct {
				PlaybackStateVersion int                    `json:"playback_state_version"`
				PendingAffordance    string                 `json:"pending_affordance_version"`
				Status               string                 `json:"execution_status"`
				OmitHistory          bool                   `json:"policy_history_omitted"`
				Schema               int                    `json:"trace_schema_version"`
				Session              string                 `json:"session_id"`
				Turn                 string                 `json:"turn_id"`
				Pair                 string                 `json:"pair_id"`
				Variant              string                 `json:"variant_id"`
				Cell                 string                 `json:"cell_id"`
				At                   time.Duration          `json:"model_admission_ns"`
				Fresh                []capability.Admission `json:"fresh_evidence"`
				Observation          capability.Observation `json:"observation"`
				Self                 capability.Self        `json:"self_at_admission"`
				Decision             capability.Decision    `json:"decision"`
			}
			if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
				file.Close()
				return err
			}
			fail := func(message string) { violations = append(violations, path+":"+row.Turn+": "+message) }
			currentIdentity := fmt.Sprintf("%q/%q/%q/%q/%t", row.Session, row.Pair, row.Variant, row.Cell, row.OmitHistory) + fmt.Sprintf("/playback-v%d/affordance-%q", row.PlaybackStateVersion, row.PendingAffordance)
			if identity == "" {
				identity = currentIdentity
			} else if identity != currentIdentity {
				fail("session or treatment identity changed within trial")
			}
			if source == nil {
				for _, c := range cells {
					if c.ID == row.Cell {
						cell = c
					}
				}
				for _, pair := range pairs {
					if pair.ID != row.Pair {
						continue
					}
					for _, v := range pair.Variants {
						if strings.TrimSuffix(row.Variant, "-nofeedback") != v.ID {
							continue
						}
						if row.Variant != v.ID {
							v, err = pair.WithoutFeedback(v)
							if err != nil {
								file.Close()
								return err
							}
						}
						source = capability.NewSource(pair.Branch(v), cell.Channels, capability.DefaultDelays)
					}
				}
				if source == nil || cell.ID == "" {
					file.Close()
					return fmt.Errorf("unknown identity in %s", path)
				}
				listener = capability.NewListener(cell.Channels)
			}
			checked++
			if row.Schema != 2 || row.Session == "" || row.Turn == "" || turns[row.Turn] {
				fail("missing/duplicate v2 identity")
			}
			turns[row.Turn] = true
			if row.At <= last {
				fail("nonmonotonic admission")
			}
			last = row.At
			fresh := source.Advance(row.At)
			if !reflect.DeepEqual(fresh, row.Fresh) {
				fail("fresh evidence differs from causal fixture replay")
			}
			listener.Admit(fresh)
			observation := listener.Observe(row.At)
			if !reflect.DeepEqual(observation, row.Observation) {
				fail("observation differs from causal fixture replay")
			}
			var completed capability.Playback
			for _, segment := range terminal.Ledger.Segments {
				if segment.CompletedAt != nil && *segment.CompletedAt <= row.At {
					completed.Segments = append(completed.Segments, segment)
				}
			}
			if row.Self.Heard != completed.Self().Heard {
				fail("heard text differs from segments completed before admission")
			}
			var request struct {
				Messages []struct{ Role, Content string } `json:"messages"`
			}
			if err := json.Unmarshal(row.Decision.Request, &request); err != nil {
				fail("missing request")
				continue
			}
			expected := cell.Render(observation, row.Self)
			if len(request.Messages) != 2 || request.Messages[1].Role != "user" || !strings.HasPrefix(request.Messages[1].Content, expected) {
				fail("model observation differs from declared-channel rendering")
			} else if err := auditHistorySuffix(strings.TrimPrefix(request.Messages[1].Content, expected), history, row.OmitHistory); err != nil {
				fail(err.Error())
			}
			if len(request.Messages) == 2 {
				// Rows predating the field were all rendered with v1 wording.
				declared := row.PendingAffordance
				if declared == "" {
					declared = "v1"
				}
				content := request.Messages[1].Content
				if !strings.Contains(content, "One next-content plan is supported") {
					const label = "\nCurrent pending segment ID: "
					active := ""
					if at := strings.LastIndex(content, label); at >= 0 {
						active, _, _ = strings.Cut(content[at+len(label):], "\n")
					}
					expected, ok := affordanceLine(declared, active)
					if !ok || !strings.HasSuffix(content, "\n"+expected) {
						fail("pending-segment affordance differs from declared version " + declared)
					}
				}
			}
			if row.PlaybackStateVersion >= 2 && len(request.Messages) == 2 {
				for _, line := range strings.Split(request.Messages[1].Content, "\n") {
					const label = "Current pending segment ID: "
					if strings.HasPrefix(line, label) && strings.TrimSpace(strings.TrimPrefix(line, label)) != "" && row.Self.Pending == "" {
						fail("active segment lacks pending text in playback-state v2")
					}
				}
			}
			if row.Decision.Error == "" {
				history = append(history, executedDecision{Action: row.Decision.Action, Status: row.Status})
			}
		}
		readErr := scanner.Err()
		file.Close()
		if readErr != nil {
			return readErr
		}
	}
	if checked == 0 {
		violations = append(violations, "no decisions checked")
	}
	report := map[string]any{"run": root, "requests_checked": checked, "violations": violations,
		"scope":        "fixture release, observation accumulation, declared-channel request prefix, prior execution-history line or its omission, v2 identity, declared pending affordance wording, completed-heard text versus terminal ledger",
		"not_verified": []string{"audio annotation alignment", "remaining execution suffix", "heard word alignment", "partial/pending self state", "terminal ledger versus waveform", "semantic adaptation", "equal request times across cells"}}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, err = file.Write(append(data, '\n'))
	closeErr := file.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if len(violations) > 0 {
		return fmt.Errorf("%d audit violations; see %s", len(violations), out)
	}
	fmt.Printf("%d requests audited; see %s\n", checked, out)
	return nil
}

// auditHistorySuffix reconstructs the history from earlier rows only. It checks
// the full serialized history line, including rejected intent, without treating
// those rows as independent proof that the reported execution effects occurred.
func auditHistorySuffix(suffix string, history []executedDecision, omitted bool) error {
	const label = "\nRecent decisions and actual execution outcomes (rejected actions produced no speech): "
	const pending = "\nCurrent pending segment ID: "
	if omitted {
		if !strings.HasPrefix(suffix, pending) || strings.Contains(suffix, "Recent decisions and actual execution outcomes") {
			return fmt.Errorf("declared history omission differs from model request")
		}
		return nil
	}
	recent := history[max(0, len(history)-8):]
	raw, err := json.Marshal(recent)
	if err != nil {
		return err
	}
	expected := label + string(raw) + pending
	if !strings.HasPrefix(suffix, expected) {
		return fmt.Errorf("model execution history differs from preceding decisions")
	}
	return nil
}
