package cascade

import (
	"context"
	"strconv"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/pcm"
)

const (
	turnEndRateHz = uint32(16_000)
	// turnEndWindow is what an acoustic end-of-turn classifier reads: Smart
	// Turn takes up to eight seconds ending at the pause.
	turnEndWindow = 8 * time.Second
)

// recordTurnEndAudio keeps the rolling window the acoustic end-of-turn
// classifier reads. Every frame is kept, silence included, because the
// classifier judges the pause itself as well as the speech before it.
func (runtime *runtime) recordTurnEndAudio(frame []byte, rateHz uint32) {
	if runtime.config.TurnEnd == nil {
		return
	}
	runtime.turnEndMu.Lock()
	defer runtime.turnEndMu.Unlock()
	if runtime.turnEndResampler == nil || runtime.turnEndResampler.InputRate() != rateHz {
		resampler, err := pcm.NewResampler(rateHz, turnEndRateHz)
		if err != nil {
			return
		}
		runtime.turnEndResampler = resampler
		runtime.turnEndWindow = runtime.turnEndWindow[:0]
	}
	converted, err := runtime.turnEndResampler.Push(frame)
	if err != nil {
		runtime.turnEndResampler = nil
		return
	}
	runtime.turnEndWindow = append(runtime.turnEndWindow, converted...)
	limit := int(turnEndWindow.Seconds()*float64(turnEndRateHz)) * 2
	if excess := len(runtime.turnEndWindow) - limit; excess > 0 {
		runtime.turnEndWindow = append(runtime.turnEndWindow[:0], runtime.turnEndWindow[excess:]...)
	}
}

// acousticEndpoint asks the classifier about the pause a floor decision is
// about to judge, and records the answer on the timeline whatever becomes of
// it. A failure is recorded too and yields no evidence, which leaves the
// floor's ordinary silence rule in charge.
func (runtime *runtime) acousticEndpoint(revisionID uint64, transcript string, silenceNS uint64) *interaction.AcousticEndpoint {
	if runtime.config.TurnEnd == nil {
		return nil
	}
	runtime.turnEndMu.Lock()
	window := append([]byte(nil), runtime.turnEndWindow...)
	runtime.turnEndMu.Unlock()
	if len(window) == 0 {
		return nil
	}
	started := time.Now()
	evidence, err := runtime.config.TurnEnd.Evaluate(runtime.ctx, window, transcript)
	attributes := map[string]any{
		"classifier": runtime.config.TurnEnd.Name(),
		"silence_ms": float64(silenceNS) / float64(time.Millisecond),
		"latency_ms": float64(time.Since(started).Microseconds()) / 1000,
		"window_ms":  float64(len(window)/2) * 1000 / float64(turnEndRateHz),
	}
	if err != nil {
		attributes["error"] = err.Error()
	} else {
		attributes["probability"] = evidence.Probability
		attributes["model"] = evidence.Model
	}
	runtime.debug(context.Background(), binding.DebugEvent{
		Category: "policy", Name: "turn_end.acoustic", Phase: "decision",
		CorrelationID: strconv.FormatUint(revisionID, 10), Attributes: attributes,
	})
	if err != nil {
		return nil
	}
	return &evidence
}
