package interaction

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
)

type controlSerializationQuarantineFactory struct{}

var (
	_ element.Factory                 = controlSerializationQuarantineFactory{}
	_ element.ConfigValidator         = controlSerializationQuarantineFactory{}
	_ element.InspectionCauseProvider = ControlSerializationQuarantine{}
)

func (controlSerializationQuarantineFactory) Descriptor() element.Descriptor {
	return ControlSerializationQuarantineDescriptor()
}

func (controlSerializationQuarantineFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeControlSerializationQuarantineConfig(source)
	return err
}

func (controlSerializationQuarantineFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeControlSerializationQuarantineConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf(
			"interaction.ControlSerializationQuarantine %s config: %w", mount.InstanceID, err,
		)
	}
	textInput, err := mount.Ports.Input("text")
	if err != nil {
		return nil, err
	}
	resultInput, err := mount.Ports.Input("result")
	if err != nil {
		return nil, err
	}
	safeTextOutput, err := mount.Ports.Output("safe_text")
	if err != nil {
		return nil, err
	}
	safeResultOutput, err := mount.Ports.Output("safe_result")
	if err != nil {
		return nil, err
	}
	quarantinedOutput, err := mount.Ports.Output("quarantined")
	if err != nil {
		return nil, err
	}
	return &controlSerializationQuarantineRunner{
		instance: mount.InstanceID, config: config,
		textInput: textInput, resultInput: resultInput,
		safeTextOutput: safeTextOutput, safeResultOutput: safeResultOutput,
		quarantinedOutput: quarantinedOutput,
		runs:              make(map[string]*quarantinedRun, config.MaxActiveStreams),
		resolution:        mount.Resolution,
	}, nil
}

type controlQuarantineInputKind uint8

const (
	controlQuarantineTextInput controlQuarantineInputKind = iota
	controlQuarantineResultInput
)

type controlQuarantineInput struct {
	kind     controlQuarantineInputKind
	envelope element.Envelope
}

type quarantinedTextStream struct {
	expected    uint64
	outputIndex uint64
	blockIndex  int
	begin       element.Envelope
	filter      controlSerializationFilter
}

// quarantinedRun is the bounded rendezvous between the independently drained
// text and result inputs. A completed-without-result entry is intentionally
// retained: without that tombstone a later result would be indistinguishable
// from one that overtook the stream's begin on the other input goroutine.
type quarantinedRun struct {
	text               *quarantinedTextStream
	textCompleted      bool
	safeTextTerminalID string
	pendingResult      *quarantinedResult
}

type quarantinedResult struct {
	envelope element.Envelope
	result   cognitionelements.Result
}

type controlSerializationQuarantineRunner struct {
	instance string
	config   ControlSerializationQuarantineConfig

	textInput   element.InputPort
	resultInput element.InputPort

	safeTextOutput    element.OutputPort
	safeResultOutput  element.OutputPort
	quarantinedOutput element.OutputPort

	runs       map[string]*quarantinedRun
	resolution element.ResolutionReporter
}

func (runner *controlSerializationQuarantineRunner) Run(parent context.Context) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	if err := reportInteractionResolution(
		runner.resolution, ControlSerializationQuarantineDescriptor(),
	); err != nil {
		return err
	}
	inputs := make(chan controlQuarantineInput)
	failures := make(chan error, 2)
	var receivers sync.WaitGroup
	for _, source := range []struct {
		kind controlQuarantineInputKind
		port element.InputPort
	}{
		{controlQuarantineTextInput, runner.textInput},
		{controlQuarantineResultInput, runner.resultInput},
	} {
		receivers.Add(1)
		go receiveControlQuarantineInput(ctx, source.kind, source.port, inputs, failures, &receivers)
	}
	defer func() {
		cancel(nil)
		receivers.Wait()
	}()
	for {
		select {
		case <-ctx.Done():
			return nil
		case err := <-failures:
			cancel(err)
			return err
		case input := <-inputs:
			var err error
			switch input.kind {
			case controlQuarantineTextInput:
				err = runner.acceptText(ctx, input.envelope)
			case controlQuarantineResultInput:
				err = runner.acceptResult(ctx, input.envelope)
			}
			if err != nil {
				cancel(err)
				return err
			}
		}
	}
}

func (runner *controlSerializationQuarantineRunner) acceptText(
	ctx context.Context, envelope element.Envelope,
) error {
	delta, valid := preparedTextPayload(envelope.Payload)
	runID := strings.TrimSpace(envelope.RunID)
	if !valid || runID == "" {
		return fmt.Errorf("control serialization quarantine received invalid prepared text %T for run %q",
			envelope.Payload, envelope.RunID)
	}
	state := runner.runs[runID]
	if state == nil || state.text == nil {
		if err := validatePreparedDelta(delta, cognitionelements.TextBegin, 0); err != nil {
			return fmt.Errorf("control serialization quarantine run %q: %w", runID, err)
		}
		if state != nil && state.textCompleted {
			return fmt.Errorf("control serialization quarantine run %q replayed text after its terminal", runID)
		}
		if state == nil {
			if len(runner.runs) >= runner.config.MaxActiveStreams {
				return fmt.Errorf("control serialization quarantine exceeds %d tracked runs",
					runner.config.MaxActiveStreams)
			}
			state = &quarantinedRun{}
			runner.runs[runID] = state
		}
		state.text = &quarantinedTextStream{
			expected: 1, begin: envelope.Clone(),
			filter: newControlSerializationFilter(
				runner.config.MaxCandidateBytes, runner.config.MaxBlocks,
			),
		}
		_, err := runner.publishSafeText(ctx, envelope, state.text, delta)
		return err
	}
	stream := state.text
	if delta.Index != stream.expected {
		return fmt.Errorf("control serialization quarantine run %q index %d, want %d",
			runID, delta.Index, stream.expected)
	}
	stream.expected++
	switch delta.Boundary {
	case cognitionelements.TextChunk:
		if delta.Text == "" || delta.Interrupted {
			return errors.New("control serialization quarantine received malformed text delta")
		}
		safe, spans := stream.filter.push(delta.Text)
		if err := runner.publishQuarantinedSpans(
			ctx, envelope, runID, ControlSerializationPreparedText, 0,
			&stream.blockIndex, spans,
		); err != nil {
			return err
		}
		delta.Text = safe
		_, err := runner.publishSafeText(ctx, envelope, stream, delta)
		return err
	case cognitionelements.TextEnd:
		if delta.Text != "" {
			return errors.New("control serialization quarantine received text on an end boundary")
		}
		safe, spans := stream.filter.finish()
		if err := runner.publishQuarantinedSpans(
			ctx, envelope, runID, ControlSerializationPreparedText, 0,
			&stream.blockIndex, spans,
		); err != nil {
			return err
		}
		delta.Text = safe
		terminal, err := runner.publishSafeText(ctx, envelope, stream, delta)
		if err != nil {
			return err
		}
		state.text = nil
		state.textCompleted = true
		state.safeTextTerminalID = terminal.ItemID
		if state.pendingResult == nil {
			return nil
		}
		pending := state.pendingResult
		if err := runner.publishSafeResult(
			ctx, pending.envelope, pending.result, state.safeTextTerminalID,
		); err != nil {
			return err
		}
		delete(runner.runs, runID)
		return nil
	case cognitionelements.TextBegin:
		return fmt.Errorf("control serialization quarantine run %q emitted duplicate begin", runID)
	default:
		return fmt.Errorf("control serialization quarantine run %q has boundary %q", runID, delta.Boundary)
	}
}

func (runner *controlSerializationQuarantineRunner) publishSafeText(
	ctx context.Context, cause element.Envelope, stream *quarantinedTextStream,
	delta cognitionelements.PreparedTextDelta,
) (element.Envelope, error) {
	delta.Index = stream.outputIndex
	stream.outputIndex++
	envelope := cause.Clone()
	envelope.Type = safePreparedTextType
	envelope.ItemID = fmt.Sprintf("%s:%s:safe-text:%d", runner.instance, cause.RunID, delta.Index)
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
	envelope.CausalParents = appendUniqueString(envelope.CausalParents, stream.begin.ItemID)
	envelope.Payload = delta
	return envelope, broadcastInteraction(ctx, runner.safeTextOutput, envelope)
}

func (runner *controlSerializationQuarantineRunner) acceptResult(
	ctx context.Context, envelope element.Envelope,
) error {
	result, valid := cognitionResultPayload(envelope.Payload)
	if !valid {
		return fmt.Errorf("control serialization quarantine received result payload %T", envelope.Payload)
	}
	if err := validateCognitionResult(result); err != nil {
		return fmt.Errorf("control serialization quarantine received invalid result: %w", err)
	}
	runID := strings.TrimSpace(result.RunID)
	if runID == "" || strings.TrimSpace(envelope.RunID) != runID {
		return errors.New("control serialization quarantine result and envelope name different runs")
	}
	state := runner.runs[runID]
	if state != nil && state.pendingResult != nil {
		return fmt.Errorf("control serialization quarantine run %q received duplicate results", runID)
	}
	if state == nil && result.AssistantText == "" {
		// A model that never emitted assistant text has no PreparedText stream to
		// close. Publish an explicit empty safe stream before the result so every
		// downstream speech lifecycle still observes a terminal for this run.
		// The result edge is independently drained, so consumers must not infer
		// this fact from result/outcome arrival ordering.
		begin := envelope.Clone()
		begin.Type = preparedTextType
		begin.ItemID = envelope.ItemID + ":empty-text:begin"
		begin.Sequence = 1
		begin.CausalParents = appendUniqueString(begin.CausalParents, envelope.ItemID)
		stream := &quarantinedTextStream{
			expected: 1, begin: begin,
			filter: newControlSerializationFilter(
				runner.config.MaxCandidateBytes, runner.config.MaxBlocks,
			),
		}
		if _, err := runner.publishSafeText(ctx, begin, stream, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextBegin,
		}); err != nil {
			return err
		}
		end := begin.Clone()
		end.ItemID = envelope.ItemID + ":empty-text:end"
		end.Sequence = 2
		terminal, err := runner.publishSafeText(ctx, end, stream, cognitionelements.PreparedTextDelta{
			Boundary: cognitionelements.TextEnd,
		})
		if err != nil {
			return err
		}
		if err := runner.publishSafeResult(ctx, envelope, result, terminal.ItemID); err != nil {
			return err
		}
		return nil
	}
	if state != nil && state.textCompleted {
		if err := runner.publishSafeResult(
			ctx, envelope, result, state.safeTextTerminalID,
		); err != nil {
			return err
		}
		delete(runner.runs, runID)
		return nil
	}
	if state != nil && state.text != nil || state == nil && result.AssistantText != "" {
		if state == nil {
			if len(runner.runs) >= runner.config.MaxActiveStreams {
				return fmt.Errorf("control serialization quarantine exceeds %d tracked runs",
					runner.config.MaxActiveStreams)
			}
			state = &quarantinedRun{}
			runner.runs[runID] = state
		}
		state.pendingResult = &quarantinedResult{envelope: envelope.Clone(), result: result}
		return nil
	}
	return errors.New("control serialization quarantine result has no exact safe text terminal")
}

func (runner *controlSerializationQuarantineRunner) publishSafeResult(
	ctx context.Context, envelope element.Envelope, result cognitionelements.Result,
	safeTextTerminalID string,
) error {
	if strings.TrimSpace(safeTextTerminalID) == "" {
		return errors.New("control serialization quarantine safe result requires a text terminal parent")
	}
	runID := strings.TrimSpace(result.RunID)
	outputs, records, _, err := sanitizeCompletedOutputs(
		result.Outputs, runner.config.MaxCandidateBytes, runner.config.MaxBlocks,
	)
	if err != nil {
		return fmt.Errorf("control serialization quarantine could not sanitize result: %w", err)
	}
	blockIndex := 0
	for _, record := range records {
		if err := runner.publishQuarantinedSpans(
			ctx, envelope, runID, record.source, record.outputIndex,
			&blockIndex, []quarantinedControlSpan{record.span},
		); err != nil {
			return err
		}
	}
	result.Outputs = outputs
	result.AssistantText, result.ReasoningText = aggregatePreparedText(outputs)
	// Opaque provider state is a second, uninspected representation of the
	// assistant turn. Until a provider-specific attestor proves that it is
	// semantically identical to Outputs, replay must use only the sanitized
	// portable projection.
	result.Completion.ProviderStateType = ""
	result.Completion.ProviderState = nil
	if err := validateCognitionResult(result); err != nil {
		return fmt.Errorf("control serialization quarantine produced invalid result: %w", err)
	}
	safeEnvelope := envelope.Clone()
	safeEnvelope.Type = safeModelResultType
	safeEnvelope.ItemID = envelope.ItemID + ":safe-result"
	safeEnvelope.CausalParents = appendUniqueString(safeEnvelope.CausalParents, envelope.ItemID)
	safeEnvelope.CausalParents = appendUniqueString(
		safeEnvelope.CausalParents, safeTextTerminalID,
	)
	safeEnvelope.Payload = result
	return broadcastInteraction(ctx, runner.safeResultOutput, safeEnvelope)
}

type completedTextRange struct {
	outputIndex int
	start       int
	end         int
}

type completedTextStream struct {
	source ControlSerializationSource
	text   strings.Builder
	ranges []completedTextRange
}

type completedQuarantineRecord struct {
	source      ControlSerializationSource
	outputIndex int
	span        quarantinedControlSpan
}

// sanitizeCompletedOutputs treats all assistant fragments as one logical text
// stream and all retained-reasoning fragments as another, matching the model's
// streaming text contract. PreparedOutput boundaries can be introduced by a
// reasoning or tool event and therefore are not parser reset points. Removed
// bytes are projected back onto their original records so sanitization cannot
// move ordinary text across an intervening tool proposal or reasoning item.
func sanitizeCompletedOutputs(
	source []cognitionelements.PreparedOutput, maxCandidateBytes, maxBlocks int,
) ([]cognitionelements.PreparedOutput, []completedQuarantineRecord, bool, error) {
	assistant := completedTextStream{source: ControlSerializationAssistant}
	reasoning := completedTextStream{source: ControlSerializationReasoning}
	for outputIndex, output := range source {
		var stream *completedTextStream
		switch output.Kind {
		case cognitionelements.PreparedAssistant:
			stream = &assistant
		case cognitionelements.PreparedReasoning:
			stream = &reasoning
		default:
			continue
		}
		start := stream.text.Len()
		stream.text.WriteString(output.Text)
		stream.ranges = append(stream.ranges, completedTextRange{
			outputIndex: outputIndex, start: start, end: stream.text.Len(),
		})
	}

	sanitized := make(map[int]string, len(assistant.ranges)+len(reasoning.ranges))
	var records []completedQuarantineRecord
	for _, stream := range []*completedTextStream{&assistant, &reasoning} {
		if len(stream.ranges) == 0 {
			continue
		}
		filter := newControlSerializationFilter(maxCandidateBytes, maxBlocks)
		safe, spans := filter.push(stream.text.String())
		tail, terminalSpans := filter.finish()
		safe += tail
		spans = append(spans, terminalSpans...)
		if len(spans) == 0 {
			continue
		}
		if err := validateCompletedSpanRanges(stream.text.Len(), spans); err != nil {
			return nil, nil, false, err
		}
		var rebuilt strings.Builder
		for _, textRange := range stream.ranges {
			value := retainCompletedTextRange(
				source[textRange.outputIndex].Text, textRange.start, textRange.end, spans,
			)
			sanitized[textRange.outputIndex] = value
			rebuilt.WriteString(value)
		}
		if rebuilt.String() != safe {
			return nil, nil, false, errors.New("sanitized output ranges disagree with parser output")
		}
		for _, span := range spans {
			outputIndex, found := completedSpanOutputIndex(stream.ranges, span.sourceStart)
			if !found {
				return nil, nil, false, errors.New("quarantined span does not begin in a prepared output")
			}
			records = append(records, completedQuarantineRecord{
				source: stream.source, outputIndex: outputIndex, span: span,
			})
		}
	}

	if len(records) == 0 {
		return append([]cognitionelements.PreparedOutput(nil), source...), nil, false, nil
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].outputIndex != records[j].outputIndex {
			return records[i].outputIndex < records[j].outputIndex
		}
		return records[i].span.sourceStart < records[j].span.sourceStart
	})
	result := make([]cognitionelements.PreparedOutput, 0, len(source))
	for outputIndex, output := range source {
		if value, found := sanitized[outputIndex]; found {
			if value == "" {
				continue
			}
			output.Text = value
		}
		result = append(result, output)
	}
	return result, records, true, nil
}

func validateCompletedSpanRanges(length int, spans []quarantinedControlSpan) error {
	previousEnd := 0
	for _, span := range spans {
		end := span.sourceStart + span.Bytes
		if span.Bytes <= 0 || span.sourceStart < previousEnd || span.sourceStart < 0 || end > length {
			return errors.New("quarantined parser produced an invalid or overlapping source range")
		}
		previousEnd = end
	}
	return nil
}

func retainCompletedTextRange(
	text string, start, end int, spans []quarantinedControlSpan,
) string {
	var result strings.Builder
	cursor := start
	for _, span := range spans {
		spanStart := max(start, span.sourceStart)
		spanEnd := min(end, span.sourceStart+span.Bytes)
		if spanStart >= spanEnd {
			continue
		}
		if cursor < spanStart {
			result.WriteString(text[cursor-start : spanStart-start])
		}
		cursor = max(cursor, spanEnd)
	}
	if cursor < end {
		result.WriteString(text[cursor-start:])
	}
	return result.String()
}

func completedSpanOutputIndex(ranges []completedTextRange, offset int) (int, bool) {
	for _, textRange := range ranges {
		if offset >= textRange.start && offset < textRange.end {
			return textRange.outputIndex, true
		}
	}
	return 0, false
}

func aggregatePreparedText(outputs []cognitionelements.PreparedOutput) (string, string) {
	var assistant, reasoning strings.Builder
	for _, output := range outputs {
		switch output.Kind {
		case cognitionelements.PreparedAssistant:
			assistant.WriteString(output.Text)
		case cognitionelements.PreparedReasoning:
			reasoning.WriteString(output.Text)
		}
	}
	return assistant.String(), reasoning.String()
}

func (runner *controlSerializationQuarantineRunner) publishQuarantinedSpans(
	ctx context.Context, cause element.Envelope, runID string,
	source ControlSerializationSource, outputIndex int, blockIndex *int,
	spans []quarantinedControlSpan,
) error {
	for _, span := range spans {
		*blockIndex++
		record := ControlSerializationQuarantine{
			RunID: runID, Source: source, SourceItemID: cause.ItemID,
			OutputIndex: outputIndex, BlockIndex: *blockIndex,
			Syntax: span.Syntax, Disposition: span.Disposition,
			Bytes: span.Bytes, SHA256: span.SHA256,
		}
		envelope := cause.Clone()
		envelope.Type = controlQuarantineType
		envelope.ItemID = fmt.Sprintf("%s:quarantined-control:%d", cause.ItemID, *blockIndex)
		envelope.RunID = runID
		envelope.Sequence = uint64(*blockIndex)
		envelope.CausalParents = appendUniqueString(envelope.CausalParents, cause.ItemID)
		envelope.Payload = record
		if err := broadcastInteraction(ctx, runner.quarantinedOutput, envelope); err != nil {
			return err
		}
	}
	return nil
}

func receiveControlQuarantineInput(
	ctx context.Context, kind controlQuarantineInputKind, input element.InputPort,
	output chan<- controlQuarantineInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalInteractionReceive(ctx, err) {
			return
		}
		if err != nil {
			sendInteractionFailure(ctx, failures, err)
			return
		}
		select {
		case output <- controlQuarantineInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}
