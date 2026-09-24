package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

type liveSpeech struct {
	mu          sync.Mutex
	ledger      capability.Playback
	cancel      context.CancelFunc
	active      string
	openingText string
	pcm         []byte
	rate        uint32
	errors      []string
	marks       []map[string]any
	workers     sync.WaitGroup
}

// The recorder preserves actual wall-clock output position, including gaps.
// One synthesis context per bounded segment is an explicit experimental
// confound until continuing-context revision has been validated.
func (s *liveSpeech) start(ctx context.Context, config speechsocket.Config, id, text string, origin time.Time, admittedAt time.Duration) {
	s.mu.Lock()
	if s.active != "" {
		s.mu.Unlock()
		return
	}
	child, cancel := context.WithCancel(ctx)
	s.cancel = cancel
	s.active = id
	s.openingText = text
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		speech, err := speechsocket.Open(child, config)
		if err != nil {
			s.fail(id, err)
			return
		}
		defer speech.Close()
		s.mu.Lock()
		if s.active != id {
			s.mu.Unlock()
			return
		}
		if s.rate != 0 && s.rate != speech.SampleRate() {
			s.mu.Unlock()
			s.fail(id, fmt.Errorf("sample rate changed"))
			return
		}
		s.rate = speech.SampleRate()
		err = s.ledger.Add(id, text, admittedAt, int(s.rate))
		s.openingText = ""
		s.mu.Unlock()
		if err != nil {
			s.fail(id, err)
			return
		}
		if err = speech.Append(child, text); err != nil {
			s.fail(id, err)
			return
		}
		if err = speech.End(child); err != nil {
			s.fail(id, err)
			return
		}
		var played int64
		for {
			select {
			case <-child.Done():
				s.fail(id, child.Err())
				return
			case chunk, ok := <-speech.Audio():
				if !ok {
					if err = speech.Err(); err != nil {
						s.fail(id, err)
						return
					}
					s.mu.Lock()
					err = s.ledger.End(id)
					if err == nil {
						err = s.ledger.FinishPlayback(id, time.Since(origin))
					}
					if s.active == id {
						s.active = ""
						s.openingText = ""
						s.cancel = nil
					}
					s.mu.Unlock()
					if err != nil {
						s.fail(id, err)
					}
					return
				}
				samples := int64(len(chunk.PCM16LE) / 2)
				s.mu.Lock()
				err = s.ledger.Generated(id, samples)
				s.mu.Unlock()
				if err != nil {
					s.fail(id, err)
					return
				}
				began := time.Since(origin)
				timer := time.NewTimer(time.Duration(samples) * time.Second / time.Duration(speech.SampleRate()))
				select {
				case <-child.Done():
					timer.Stop()
					s.fail(id, child.Err())
					return
				case <-timer.C:
				}
				s.mu.Lock()
				if s.active != id {
					s.mu.Unlock()
					return
				}
				offset := int(began.Seconds()*float64(s.rate)) * 2
				if len(s.pcm) < offset+len(chunk.PCM16LE) {
					s.pcm = append(s.pcm, make([]byte, offset+len(chunk.PCM16LE)-len(s.pcm))...)
				}
				copy(s.pcm[offset:], chunk.PCM16LE)
				played += samples
				at := time.Since(origin)
				err = s.ledger.Mark(id, played, at)
				s.marks = append(s.marks, map[string]any{"segment_id": id, "at_ns": at, "played_samples": played, "sink": "paced-recorder"})
				s.mu.Unlock()
				if err != nil {
					s.fail(id, err)
					return
				}
			}
		}
	}()
}

func (s *liveSpeech) fail(id string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errors = append(s.errors, fmt.Sprintf("%s: %v", id, err))
	s.ledger.Cancel(id)
	if s.active == id {
		s.active = ""
		s.openingText = ""
		s.cancel = nil
	}
}
func (s *liveSpeech) stop(reason string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.marks = append(s.marks, map[string]any{
			"segment_id": s.active, "mark": "cancel", "reason": reason,
			"wall_time": time.Now().UTC(),
		})
		s.cancel()
		s.ledger.Cancel(s.active)
	}
	s.active = ""
	s.openingText = ""
	s.cancel = nil
}
func (s *liveSpeech) state() (capability.Self, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	self := s.ledger.Self()
	if s.active != "" && s.openingText != "" {
		self.Pending += fmt.Sprintf("[%s] synthesis context opening; no audio played; requested text: %q\n", s.active, s.openingText)
	}
	return self, s.active
}

// pendingAffordances are the model-visible descriptions of what the executor
// does with a pending segment. v1 is the
// original wording, which lists only wait and revise; v2 states that waiting
// lets the segment play and that revision replaces its words. v1 and v2 show
// the same line whether or not a segment is pending; v3 shows the v2 line only
// while a segment is pending and otherwise states that nothing is pending; v4
// shows the v2 line while pending and the v1 line otherwise.
var pendingAffordances = map[string]struct{ pending, idle string }{
	"v1": {pending: "When a segment is pending, wait or revise it; new speech cannot be queued behind it."},
	"v2": {pending: "When a segment is pending, wait to let it play; revise only to change its words. New speech cannot be queued behind it."},
	"v3": {pending: "When a segment is pending, wait to let it play; revise only to change its words. New speech cannot be queued behind it.",
		idle: "Nothing is pending: speak or continue to start new speech now, or wait to stay silent."},
	// v4 changes only the pending-state line: v3's idle line caused verbatim
	// repetition of completed sentences outside st-02, while the v1 line in
	// idle states continued or waited appropriately.
	"v4": {pending: "When a segment is pending, wait to let it play; revise only to change its words. New speech cannot be queued behind it.",
		idle: "When a segment is pending, wait or revise it; new speech cannot be queued behind it."},
}

// affordanceLine returns the line a version renders for the current active
// segment ID, which is empty when nothing is pending.
func affordanceLine(version, active string) (string, bool) {
	a, ok := pendingAffordances[version]
	if !ok {
		return "", false
	}
	if active == "" && a.idle != "" {
		return a.idle, true
	}
	return a.pending, true
}

func runLive(ctx context.Context, dir string, pair capability.Pair, variant capability.Variant, cell capability.Cell, policy capability.JointPolicy, config speechsocket.Config, omitHistory bool, affordance string) (runErr error) {
	if _, ok := pendingAffordances[affordance]; !ok {
		return fmt.Errorf("unknown pending affordance %q", affordance)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		return err
	}
	trace, err := os.Create(filepath.Join(dir, "actions.jsonl"))
	if err != nil {
		return err
	}
	defer trace.Close()
	enc := json.NewEncoder(trace)
	source := capability.NewSource(pair.Branch(variant), cell.Channels, capability.DefaultDelays)
	listener := capability.NewListener(cell.Channels)
	var speech liveSpeech
	origin := time.Now()
	ticker := time.NewTicker(cell.TickInterval)
	defer ticker.Stop()
	// All branches, including feedback-withheld controls, have the same
	// observation horizon. Removing feedback must not shorten the trial.
	horizon := time.Duration(0)
	for _, original := range pair.Variants {
		originalSource := capability.NewSource(pair.Branch(original), cell.Channels, capability.DefaultDelays)
		end := originalSource.Ends() + original.Expect.Within + 5*time.Second
		if end > horizon {
			horizon = end
		}
	}
	var admitted []capability.Admission
	decisions := 0
	var history []executedDecision
	policyErrors := []string{}
	// Every started trial retains evidence, including timeout and write/request
	// failure paths. A missing result must never masquerade as an omitted case.
	sessionID := filepath.Base(filepath.Dir(dir)) + "/" + filepath.Base(dir)
	var bg *background
	switch cell.Deliberation {
	case "":
	case "synchronous":
		policy.Thinking, policy.MaxTokens = true, thinkingTokens
	case "background":
		bg, err = newBackground(dir, policy, pair.Instructions, origin, map[string]any{
			"pair_id": pair.ID, "variant_id": variant.ID, "cell_id": cell.ID, "session_id": sessionID})
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown deliberation %q", cell.Deliberation)
	}
	defer func() {
		reason := "trial-horizon"
		if runErr != nil {
			reason = "trial-failure"
		}
		if bg != nil {
			runErr = errors.Join(runErr, bg.close(time.Since(origin), userWords(admitted)))
		}
		speech.stop(reason)
		speech.workers.Wait()
		runErr = errors.Join(runErr, retainLiveEvidence(dir, pair, variant, &speech, admitted,
			omitHistory, policyErrors, decisions, sessionID, runErr))
	}()
	for time.Since(origin) < horizon {
		select {
		case <-ctx.Done():
			speech.stop("trial-timeout")
			speech.workers.Wait()
			return ctx.Err()
		case <-ticker.C:
		}
		now := time.Since(origin)
		fresh := source.Advance(now)
		admitted = append(admitted, fresh...)
		listener.Admit(fresh)
		words := userWords(admitted)
		if bg != nil {
			// A finished proposal is applied before this tick's fast decision,
			// so the fast policy observes its effect. Newer user words make an
			// unfinished one obsolete.
			if d := bg.finished(); d != nil {
				if err = bg.admit(ctx, d, now, words, &speech, config); err != nil {
					return err
				}
			} else if bg.current != nil && words > bg.current.userWords {
				if err = bg.supersede(now, words); err != nil {
					return err
				}
			}
		}
		self, active := speech.state()
		observation := listener.Observe(now)
		rendered := cell.Render(observation, self)
		historyStart := len(history) - 8
		if historyStart < 0 {
			historyStart = 0
		}
		if !omitHistory {
			historyJSON, _ := json.Marshal(history[historyStart:])
			rendered += "\nRecent decisions and actual execution outcomes (rejected actions produced no speech): " + string(historyJSON)
		}
		rendered += "\nCurrent pending segment ID: " + active
		line, _ := affordanceLine(affordance, active)
		rendered += "\n" + line
		// Deliberation targets feedback to the assistant's speech, so it starts
		// only on new user words once the assistant has spoken; the fast path
		// alone answers the opening request.
		if bg != nil {
			if !hasSpoken(self) {
				// Words before any assistant speech are the opening request,
				// never a trigger, even once speech has begun.
				bg.lastWords = words
			} else if bg.current == nil && words > bg.lastWords {
				bg.start(ctx, now, words, observation, self, rendered)
			}
		}
		decision, callErr := policy.Decide(ctx, pair.Instructions, rendered)
		decisions++
		action := decision.Action

		// Playback may finish during an immutable model request. Validate
		// the returned action against current state, not the stale snapshot.
		_, currentActive := speech.state()
		status := "no-output"
		if callErr == nil && speechContainsReference(action.Text, active, currentActive) {
			// Reject a leaked control reference before canceling
			// playback or sending any text to synthesis. Do not rewrite it.
			status = "control-reference-in-speech"
		} else if callErr == nil {
			switch action.Act {
			case "yield":
				speech.stop("policy-yield")
				status = "cancel"
			case "revise":
				if !validReplacement(action.ReplacesPending, currentActive) {
					status = "invalid-replacement"
				} else {
					speech.stop("policy-revise")
					speech.workers.Wait()
					speech.start(ctx, config, fmt.Sprintf("segment-%d", decisions), action.Text, origin, now)
					status = "synthesis-started"
				}
			case "speak", "continue", "backchannel":
				if currentActive != "" {
					status = "pending-conflict"
				} else {
					speech.start(ctx, config, fmt.Sprintf("segment-%d", decisions), action.Text, origin, now)
					status = "synthesis-started"
				}
			}
		}
		if callErr == nil {
			history = append(history, executedDecision{Action: action, Status: status})
		}
		if err = enc.Encode(map[string]any{"pair_id": pair.ID, "variant_id": variant.ID, "cell_id": cell.ID,
			"trace_schema_version": 2, "playback_state_version": 2, "pending_affordance_version": affordance, "policy_history_omitted": omitHistory, "session_id": sessionID, "turn_id": fmt.Sprintf("decision-%d", decisions),
			"fresh_evidence": fresh, "observation": observation, "self_at_admission": self,
			"model_admission_ns": now, "decision": decision, "execution_status": status,
			"deadline_missed": decision.Duration > cell.TickInterval}); err != nil {
			speech.stop("trace-write-failure")
			speech.workers.Wait()
			return err
		}
		if callErr != nil {
			policyErrors = append(policyErrors, callErr.Error())
			speech.stop("policy-request-failure")
			return callErr
		}
	}
	return nil
}

// retainLiveEvidence runs after all playback workers stop. Independent writes
// are attempted even when another artifact cannot be written.
func retainLiveEvidence(dir string, pair capability.Pair, variant capability.Variant, speech *liveSpeech, admitted []capability.Admission, omitHistory bool, policyErrors []string, decisions int, sessionID string, trialErr error) error {
	var failures []error
	failures = append(failures, writeJSON(filepath.Join(dir, "admitted-evidence.json"), admitted))
	failures = append(failures, writeJSON(filepath.Join(dir, "playback.json"), speech.marks))
	if speech.rate > 0 && len(speech.pcm) > 0 {
		wav, e := audio.EncodeWAVMono16(speech.pcm, speech.rate)
		if e != nil {
			failures = append(failures, e)
		} else {
			failures = append(failures, os.WriteFile(filepath.Join(dir, "output.wav"), wav, 0644))
		}
	}
	// Resolve the original scoring anchor even for withheld-feedback controls.
	var anchor time.Duration
	anchorFound := false
	for _, original := range pair.Variants {
		if original.ID != strings.TrimSuffix(variant.ID, "-nofeedback") {
			continue
		}
		for _, event := range pair.Branch(original) {
			if event.ID == variant.Expect.After {
				anchor = event.AvailableAt
				anchorFound = true
				break
			}
		}
		if anchorFound {
			break
		}
	}
	var screen any
	if anchorFound && trialErr == nil {
		screen = capability.ScreenContent(variant.Expect, anchor, speech.ledger)
	}
	status, trialError := "complete", ""
	if trialErr != nil {
		status, trialError = "failed", trialErr.Error()
	}
	failures = append(failures, writeJSON(filepath.Join(dir, "result.json"), map[string]any{
		"trace_schema_version": 2, "playback_state_version": 2, "policy_history_omitted": omitHistory, "session_id": sessionID, "status": status, "trial_error": trialError,
		"ledger": speech.ledger, "speech_errors": speech.errors, "policy_errors": policyErrors, "requests": decisions,
		"input_mode":              "annotated-estimated-word-times-no-input-audio",
		"output_mode":             "paced-recorder-one-context-per-segment",
		"lexical_playback_screen": screen, "audible_adaptation_score": nil, "physical_playback": false,
	}))
	return errors.Join(failures...)
}

// A reference to an already-completed segment cannot retract heard speech.
func validReplacement(requested, current string) bool {
	return current != "" && requested == current
}

// executedDecision distinguishes model intent from actual effects. In particular,
// rejected revisions must not become fictional assistant dialogue.
type executedDecision struct {
	Action capability.Action `json:"requested_action"`
	Status string            `json:"execution_status"`
}

// speechContainsReference detects known current control IDs used as spoken
// tokens. It does not claim to detect every possible control-language leak.
func speechContainsReference(text string, references ...string) bool {
	for _, token := range strings.Fields(text) {
		token = strings.Trim(token, "[](){}\"'.,:;!?")
		for _, ref := range references {
			if ref != "" && strings.EqualFold(token, ref) {
				return true
			}
		}
	}
	return false
}
