package speech

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

type cancellationMemory struct {
	maximum int
	order   []string
	reasons map[string]string
}

func newCancellationMemory(maximum int) *cancellationMemory {
	return &cancellationMemory{maximum: maximum, reasons: make(map[string]string)}
}

func (memory *cancellationMemory) remember(id, reason string) {
	id = strings.TrimSpace(id)
	if id == "" {
		return
	}
	reason = firstNonempty(strings.TrimSpace(reason), "cancelled")
	if _, exists := memory.reasons[id]; exists {
		memory.reasons[id] = reason
		return
	}
	if len(memory.order) == memory.maximum {
		delete(memory.reasons, memory.order[0])
		copy(memory.order, memory.order[1:])
		memory.order[len(memory.order)-1] = id
	} else {
		memory.order = append(memory.order, id)
	}
	memory.reasons[id] = reason
}

// peek reports a remembered cancellation without consuming it: a run stays
// cancelled for every utterance that follows.
func (memory *cancellationMemory) peek(id string) (string, bool) {
	reason, found := memory.reasons[strings.TrimSpace(id)]
	return reason, found
}

func (memory *cancellationMemory) take(id string) (string, bool) {
	reason, found := memory.reasons[id]
	if !found {
		return "", false
	}
	delete(memory.reasons, id)
	index := slices.Index(memory.order, id)
	if index >= 0 {
		memory.order = slices.Delete(memory.order, index, index+1)
	}
	return reason, true
}

func receiveInterrupts(
	ctx context.Context, input element.InputPort, output chan<- element.Envelope,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminal(ctx, err) {
			return
		}
		if err != nil {
			sendFailure(ctx, failures, err)
			return
		}
		select {
		case output <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func sendFailure(ctx context.Context, output chan<- error, err error) {
	select {
	case output <- err:
	case <-ctx.Done():
	}
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

// cancelsRun reports whether a cancel addressed to a run names this one.
func cancelsRun(request Cancel, runID string) bool {
	return strings.TrimSpace(request.RunID) != "" && strings.TrimSpace(request.RunID) == strings.TrimSpace(runID)
}

func cancelTarget(envelope element.Envelope, request Cancel) string {
	return firstNonempty(
		strings.TrimSpace(request.UtteranceID), strings.TrimSpace(envelope.CancellationScope),
		strings.TrimSpace(envelope.RunID), strings.TrimSpace(envelope.SourceID),
	)
}

func childEnvelope(
	cause element.Envelope, typeOf element.Type, itemID, sourceID string,
) element.Envelope {
	envelope := cause.Clone()
	envelope.Type = typeOf
	envelope.ItemID = itemID
	if sourceID != "" {
		envelope.SourceID = sourceID
		envelope.CancellationScope = sourceID
	}
	if cause.ItemID != "" && !slices.Contains(envelope.CausalParents, cause.ItemID) {
		envelope.CausalParents = append(envelope.CausalParents, cause.ItemID)
	}
	return envelope
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
