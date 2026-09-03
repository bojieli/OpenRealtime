package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
)

type backgroundInjectionFactory struct{}

func (backgroundInjectionFactory) Descriptor() element.Descriptor {
	return BackgroundInjectionDescriptor()
}
func (backgroundInjectionFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeBackgroundInjectionConfig(source)
	return err
}

func (backgroundInjectionFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeBackgroundInjectionConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("meeting.BackgroundInjection %s config: %w", mount.InstanceID, err)
	}
	text, err := mount.Ports.Input("text")
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]element.OutputPort, 3)
	for _, name := range []string{"injection", "trigger", "outcome"} {
		output, outputErr := mount.Ports.Output(name)
		if outputErr != nil {
			return nil, outputErr
		}
		outputs[name] = output
	}
	return &backgroundInjectionRunner{
		config: config, input: text, outputs: outputs, resolution: mount.Resolution,
		active:   make(map[string]*backgroundText, config.MaxRuns),
		terminal: newBoundedIdentifiers(config.TerminalMemory),
	}, nil
}

type backgroundText struct {
	begin element.Envelope
	next  uint64
	text  strings.Builder
}

type backgroundInjectionRunner struct {
	config     BackgroundInjectionConfig
	input      element.InputPort
	outputs    map[string]element.OutputPort
	resolution element.ResolutionReporter
	active     map[string]*backgroundText
	terminal   *boundedIdentifiers
}

func (runner *backgroundInjectionRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run meeting background injection: nil context")
	}
	if err := reportRuntime(runner.resolution, backgroundRuntimeID); err != nil {
		return err
	}
	for {
		envelope, err := runner.input.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive meeting background text: %w", err)
		}
		if err := runner.accept(ctx, envelope); err != nil {
			return err
		}
	}
}

func (runner *backgroundInjectionRunner) accept(
	ctx context.Context, envelope element.Envelope,
) error {
	if !canonicalText(envelope.ItemID) || !canonicalText(envelope.SessionID) {
		return errors.New("meeting background text requires canonical item and session identities")
	}
	delta, ok := preparedTextPayload(envelope.Payload)
	runID := envelope.RunID
	if !canonicalText(runID) {
		return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
			Kind: BackgroundRejected, Code: "invalid_run_id",
			Message: "prepared text requires a bounded canonical run ID",
		})
	}
	if !ok {
		return runner.reject(ctx, envelope, runID, "invalid_payload",
			fmt.Sprintf("prepared text payload has type %T", envelope.Payload))
	}
	if runner.terminal.contains(runID) {
		return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
			Kind: BackgroundIgnored, RunID: runID, Code: "terminal_replay",
			Message: "background run is already terminal",
		})
	}

	switch delta.Boundary {
	case cognitionelements.TextBegin:
		if delta.Index != 0 || delta.Text != "" || delta.Interrupted {
			return runner.reject(ctx, envelope, runID, "invalid_begin", "begin must be empty index zero")
		}
		if _, duplicate := runner.active[runID]; duplicate {
			return runner.reject(ctx, envelope, runID, "duplicate_begin", "background run is already active")
		}
		if len(runner.active) >= runner.config.MaxRuns {
			return runner.reject(ctx, envelope, runID, "capacity", "background run capacity is exhausted")
		}
		runner.active[runID] = &backgroundText{begin: envelope.Clone(), next: 1}
		return nil

	case cognitionelements.TextChunk:
		active := runner.active[runID]
		if active == nil || delta.Index != active.next || delta.Interrupted {
			return runner.reject(ctx, envelope, runID, "invalid_delta", "delta is missing its exact active predecessor")
		}
		active.next++
		if delta.Text == "" {
			return nil
		}
		if len(delta.Text) > runner.config.MaxTextBytes-active.text.Len() {
			delete(runner.active, runID)
			runner.terminal.add(runID)
			return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
				Kind: BackgroundRejected, RunID: runID, Bytes: active.text.Len(),
				Code: "text_too_large", Message: "background text exceeded its configured bound",
			})
		}
		if !utf8.ValidString(delta.Text) || strings.ContainsRune(delta.Text, '\x00') {
			return runner.reject(ctx, envelope, runID, "invalid_delta", "delta contains malformed text")
		}
		active.text.WriteString(delta.Text)
		return nil

	case cognitionelements.TextEnd:
		active := runner.active[runID]
		if active == nil || delta.Index != active.next {
			return runner.reject(ctx, envelope, runID, "invalid_end", "end is missing its exact active predecessor")
		}
		if delta.Text != "" {
			if len(delta.Text) > runner.config.MaxTextBytes-active.text.Len() {
				delete(runner.active, runID)
				runner.terminal.add(runID)
				return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
					Kind: BackgroundRejected, RunID: runID, Bytes: active.text.Len(),
					Code: "text_too_large", Message: "background text exceeded its configured bound",
				})
			}
			if !utf8.ValidString(delta.Text) || strings.ContainsRune(delta.Text, '\x00') {
				return runner.reject(ctx, envelope, runID, "invalid_end", "end contains malformed text")
			}
			active.text.WriteString(delta.Text)
		}
		delete(runner.active, runID)
		runner.terminal.add(runID)
		if delta.Interrupted {
			return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
				Kind: BackgroundInterrupted, RunID: runID, Bytes: active.text.Len(),
				Code: "interrupted", Message: "interrupted background text was not injected",
			})
		}
		if active.text.Len() == 0 {
			return runner.publishOutcome(ctx, envelope, BackgroundInjectionOutcome{
				Kind: BackgroundIgnored, RunID: runID, Code: "empty",
				Message: "empty background text was not injected",
			})
		}
		return runner.inject(ctx, envelope, active, runID)

	default:
		return runner.reject(ctx, envelope, runID, "unknown_boundary", "prepared text boundary is unknown")
	}
}

func (runner *backgroundInjectionRunner) reject(
	ctx context.Context, cause element.Envelope, runID, code, message string,
) error {
	delete(runner.active, runID)
	if canonicalText(runID) {
		runner.terminal.add(runID)
	}
	return runner.publishOutcome(ctx, cause, BackgroundInjectionOutcome{
		Kind: BackgroundRejected, RunID: runID, Code: code, Message: message,
	})
}

func (runner *backgroundInjectionRunner) inject(
	ctx context.Context, cause element.Envelope, active *backgroundText, runID string,
) error {
	injection := derivedMeetingEnvelope(cause, "background-injection", modelelements.TextInputType())
	injection.RunID = runID
	injection.CausalParents = appendUnique(injection.CausalParents, active.begin.ItemID)
	injection.Payload = ContextInjection{
		Role: runner.config.Role, Source: "meeting.background", RunID: runID,
		Text: active.text.String(),
	}
	if _, err := runner.outputs["injection"].Broadcast(ctx, injection); err != nil {
		return fmt.Errorf("publish meeting background injection: %w", err)
	}

	trigger := derivedMeetingEnvelope(cause, "background-trigger", modelelements.GenerateType())
	trigger.RunID = "meeting-background:" + runID
	trigger.CausalParents = appendUnique(trigger.CausalParents, injection.ItemID)
	trigger.Payload = cognitionelements.Generate{Invocation: continuation.Invocation{
		Instruction: runner.config.Instruction, MaxOutputTokens: runner.config.MaxOutputTokens,
	}}
	if _, err := runner.outputs["trigger"].Broadcast(ctx, trigger); err != nil {
		return fmt.Errorf("publish meeting background trigger: %w", err)
	}
	return runner.publishOutcome(ctx, cause, BackgroundInjectionOutcome{
		Kind: BackgroundInjected, RunID: runID, Bytes: active.text.Len(),
	})
}

func (runner *backgroundInjectionRunner) publishOutcome(
	ctx context.Context, cause element.Envelope, outcome BackgroundInjectionOutcome,
) error {
	envelope := derivedMeetingEnvelope(cause, "background-outcome", backgroundOutcomeType)
	envelope.RunID = outcome.RunID
	envelope.Payload = outcome
	_, err := runner.outputs["outcome"].Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("publish meeting background outcome: %w", err)
	}
	return nil
}

func preparedTextPayload(payload any) (cognitionelements.PreparedTextDelta, bool) {
	switch typed := payload.(type) {
	case cognitionelements.PreparedTextDelta:
		return typed, true
	case *cognitionelements.PreparedTextDelta:
		if typed != nil {
			return *typed, true
		}
	}
	return cognitionelements.PreparedTextDelta{}, false
}

type boundedIdentifiers struct {
	maximum int
	values  map[string]struct{}
	order   []string
}

func newBoundedIdentifiers(maximum int) *boundedIdentifiers {
	return &boundedIdentifiers{maximum: maximum, values: make(map[string]struct{}, maximum)}
}

func (set *boundedIdentifiers) contains(value string) bool {
	_, found := set.values[value]
	return found
}

func (set *boundedIdentifiers) add(value string) {
	if _, exists := set.values[value]; exists {
		return
	}
	set.values[value] = struct{}{}
	set.order = append(set.order, value)
	if len(set.order) <= set.maximum {
		return
	}
	oldest := set.order[0]
	set.order = set.order[1:]
	delete(set.values, oldest)
}
