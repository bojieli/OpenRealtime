package cascade

import (
	"context"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
)

// debug emits implementation evidence only when the session sink supports
// the optional developer stream. Ordinary sinks and test harnesses require no
// additional methods and pay no wire cost.
func (runtime *runtime) debug(ctx context.Context, event binding.DebugEvent) {
	sink, enabled := runtime.sink.(binding.DebugSink)
	if !enabled {
		return
	}
	_ = sink.Debug(ctx, event)
}

func elapsedMS(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}
