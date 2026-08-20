package interleave

import (
	"context"
	"errors"
	"slices"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// ToolCallSink hands an authoritative slow call batch to an asynchronous tool
// orchestrator. The sink must not fabricate results; completion returns later
// as one eventloop tool-result batch. Dispatch must be idempotent by call ID so
// recovery can retry a committed call without duplicating its external effect.
type ToolCallSink func(context.Context, string, []trajectory.ToolCall) error

// RunObserver receives each committed fast or slow safe-point result. It is an
// operational hook for speech planning and telemetry, not another agent.
type RunObserver func(trajectory.Phase, continuation.RunResult) error

type ProcessorConfig struct {
	Engine       *Engine
	Stream       StreamObserver
	ToolCallSink ToolCallSink
	RunObserver  RunObserver
}

// Processor applies the complete cognitive policy to committed event batches:
// an observation starts fast then slow; a tool-result batch resumes slow
// directly; media-only state changes do not invoke a model. It implements no
// answer/ask/yield router and does not inspect event text.
type Processor struct {
	engine       *Engine
	stream       StreamObserver
	toolCallSink ToolCallSink
	runObserver  RunObserver
}

func NewProcessor(config ProcessorConfig) (*Processor, error) {
	if config.Engine == nil {
		return nil, errors.New("interleave event processor requires an engine")
	}
	return &Processor{
		engine: config.Engine, stream: config.Stream,
		toolCallSink: config.ToolCallSink, runObserver: config.RunObserver,
	}, nil
}

func (processor *Processor) Process(ctx context.Context, batch eventloop.Batch) error {
	var hasObservation, hasToolResult bool
	var sourceRevision uint64
	for _, item := range batch.Items {
		sourceRevision = max(sourceRevision, item.SourceRevision)
		switch item.Kind {
		case trajectory.KindObservation:
			hasObservation = true
		case trajectory.KindToolResult:
			hasToolResult = true
		}
	}
	for _, item := range processor.engine.store.Snapshot().Items {
		if item.Kind == trajectory.KindObservation {
			sourceRevision = max(sourceRevision, item.SourceRevision)
		}
	}
	if !hasObservation && !hasToolResult {
		if !trajectory.BatchIntroducesPendingRepair(batch.Items) {
			return nil
		}
	}
	request := Request{SourceRevision: sourceRevision}
	var failures []error
	if hasObservation {
		fast, err := processor.engine.RunFast(ctx, request, processor.stream)
		if processor.runObserver != nil && fast.Committed {
			if observerErr := processor.runObserver(trajectory.PhaseFast, fast); observerErr != nil {
				failures = append(failures, observerErr)
			}
		}
		if err != nil {
			failures = append(failures, err)
			if ctx.Err() != nil {
				return errors.Join(failures...)
			}
		}
	}

	slow, err := processor.engine.RunSlowStep(ctx, request, processor.stream)
	if processor.runObserver != nil && slow.Committed {
		if observerErr := processor.runObserver(trajectory.PhaseSlow, slow); observerErr != nil {
			failures = append(failures, observerErr)
		}
	}
	if err != nil {
		failures = append(failures, err)
		return errors.Join(failures...)
	}
	if len(slow.ToolCalls) > 0 {
		if processor.toolCallSink == nil {
			failures = append(failures, errors.New("slow continuation produced external calls without a tool-call sink"))
		} else {
			calls := make([]trajectory.ToolCall, len(slow.ToolCalls))
			for index, call := range slow.ToolCalls {
				call.Arguments = slices.Clone(call.Arguments)
				calls[index] = call
			}
			failures = append(failures, processor.toolCallSink(ctx, slow.InvocationID, calls))
		}
	}
	return errors.Join(failures...)
}
