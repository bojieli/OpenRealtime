package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/adapters/speechsocket"
	"github.com/bojieli/OpenRealtime/bench/capability"
)

// deliberationHint is appended to the snapshot a background deliberation
// sees. It is part of the declared A2D treatment.
const deliberationHint = "\nThis is a background deliberation. Take the time you need: the result is applied at a later update, and discarded if the user has said anything new by then. Choose the best next spoken content for the user's latest words: revise to replace pending speech (use the pending ID shown), speak if nothing is pending, or wait if no change is needed."

// Thinking requests need room for reasoning before the action.
const thinkingTokens = 4000

// deliberation is one background thinking request started from the snapshot
// at snapshotAt. It may finish several ticks later.
type deliberation struct {
	id          int
	snapshotAt  time.Duration
	userWords   int
	observation capability.Observation
	self        capability.Self
	finishedAt  time.Duration
	decision    capability.Decision
	err         error
	done        chan struct{}
	cancel      context.CancelFunc
}

// background runs at most one deliberation at a time for the A2D cell.
type background struct {
	policy       capability.JointPolicy
	instructions string
	origin       time.Time
	current      *deliberation
	started      int
	lastWords    int
	trace        *json.Encoder
	file         *os.File
	identity     map[string]any
}

func newBackground(dir string, policy capability.JointPolicy, instructions string, origin time.Time, identity map[string]any) (*background, error) {
	file, err := os.Create(filepath.Join(dir, "deliberations.jsonl"))
	if err != nil {
		return nil, err
	}
	policy.Thinking, policy.MaxTokens = true, thinkingTokens
	return &background{policy: policy, instructions: instructions, origin: origin,
		trace: json.NewEncoder(file), file: file, identity: identity}, nil
}

// userWords counts admitted user words and revisions: the evidence whose
// arrival makes a pending deliberation stale.
func userWords(admitted []capability.Admission) int {
	n := 0
	for _, a := range admitted {
		if a.Admitted() && a.Speaker == capability.SpeakerUser &&
			(a.Mark == capability.MarkWord || a.Mark == capability.MarkRevision) {
			n++
		}
	}
	return n
}

// hasSpoken reports whether the assistant has produced any speech the user
// could have heard or that is queued: the precondition for deliberating.
func hasSpoken(self capability.Self) bool {
	return self.Speaking || self.Heard != "" || self.Partial != "" || self.Pending != ""
}

// start launches a deliberation from this tick's snapshot.
func (b *background) start(ctx context.Context, now time.Duration, words int, observation capability.Observation, self capability.Self, rendered string) {
	b.started++
	child, cancel := context.WithCancel(ctx)
	d := &deliberation{id: b.started, snapshotAt: now, userWords: words, observation: observation,
		self: self, done: make(chan struct{}), cancel: cancel}
	b.current, b.lastWords = d, words
	go func() {
		defer close(d.done)
		d.decision, d.err = b.policy.Decide(child, b.instructions, rendered+deliberationHint)
		d.finishedAt = time.Since(b.origin)
	}()
}

// finished returns the current deliberation if it has completed.
func (b *background) finished() *deliberation {
	if b.current == nil {
		return nil
	}
	select {
	case <-b.current.done:
		return b.current
	default:
		return nil
	}
}

// supersede cancels an in-flight deliberation because newer user words arrived.
func (b *background) supersede(now time.Duration, words int) error {
	d := b.current
	d.cancel()
	<-d.done
	b.current = nil
	return b.record(d, now, words, "superseded")
}

func (b *background) record(d *deliberation, now time.Duration, words int, status string) error {
	row := map[string]any{"deliberation_id": d.id, "snapshot_ns": d.snapshotAt, "user_words_at_snapshot": d.userWords,
		"observation": d.observation, "self_at_snapshot": d.self, "finished_ns": d.finishedAt,
		"admission_check_ns": now, "user_words_at_check": words, "decision": d.decision, "status": status}
	if d.err != nil {
		row["error"] = d.err.Error()
	}
	for k, v := range b.identity {
		row[k] = v
	}
	return b.trace.Encode(row)
}

// close records an unfinished deliberation at the end of the trial.
func (b *background) close(now time.Duration, words int) error {
	defer b.file.Close()
	if b.current == nil {
		return nil
	}
	d := b.current
	d.cancel()
	<-d.done
	b.current = nil
	return b.record(d, now, words, "trial-end")
}

// admit applies a finished deliberation if no user words arrived since its
// snapshot. It replaces any pending speech, cancelling only unplayed audio.
func (b *background) admit(ctx context.Context, d *deliberation, now time.Duration, words int, speech *liveSpeech, config speechsocket.Config) error {
	b.current = nil
	status := ""
	action := d.decision.Action
	_, active := speech.state()
	switch {
	case d.err != nil:
		status = "error"
	case words != d.userWords:
		status = "stale"
	case action.Act == "wait":
		status = "proposal-wait"
	case action.Act == "yield":
		speech.stop("deliberation-yield")
		status = "cancel"
	case speechContainsReference(action.Text, append(speech.knownIDs(), active, action.ReplacesPending)...):
		status = "control-reference-in-speech"
	default:
		if active != "" {
			speech.stop("deliberation-revise")
			speech.workers.Wait()
		}
		speech.start(ctx, config, fmt.Sprintf("deliberation-%d", d.id), strings.TrimSpace(action.Text), b.origin, now)
		status = "synthesis-started"
	}
	return b.record(d, now, words, status)
}
