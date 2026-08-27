package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// handleExtension decodes the two client-to-server extension events.
//
// They are handled before an unknown base-protocol type is treated as a
// failure, because the pinned base registry cannot know about them by
// construction. It reports whether the message was an extension event at all,
// so an actually unknown type still gets the base protocol's error.
// isExtensionEvent reports whether an event belongs to the OpenRealtime
// extension rather than to the base protocol.
//
// The check is on the read goroutine because routing has to happen before
// decoding: the pinned base registry cannot know about extension events by
// construction, so handing one to it first would make every extension event an
// invalid one.
func isExtensionEvent(input []byte) bool {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(input, &envelope); err != nil {
		return false
	}
	return strings.HasPrefix(envelope.Type, "openrealtime.")
}

func (session *session) handleExtension(input []byte) (bool, error) {
	if !isExtensionEvent(input) {
		return false, nil
	}
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(input, &envelope); err != nil {
		return false, nil
	}
	session.settingsMu.RLock()
	enabled := hasFeature(session.settings.extension, openrealtime.FeatureVideoInput)
	limits := session.settings.limits
	session.settingsMu.RUnlock()

	switch envelope.Type {
	case openrealtime.EventVideoSourceUpdate:
		if !enabled {
			return true, errors.New("video input was not negotiated for this session")
		}
		var update openrealtime.VideoSourceUpdate
		if err := json.Unmarshal(input, &update); err != nil {
			return true, err
		}
		return true, session.updateVideoSource(update, limits)
	case openrealtime.EventVideoFrameAppend:
		if !enabled {
			return true, errors.New("video input was not negotiated for this session")
		}
		var appendEvent openrealtime.VideoFrameAppend
		if err := json.Unmarshal(input, &appendEvent); err != nil {
			return true, err
		}
		return true, session.appendVideoFrame(appendEvent, limits)
	default:
		return true, fmt.Errorf("unsupported openrealtime client event %q", envelope.Type)
	}
}

func (session *session) updateVideoSource(update openrealtime.VideoSourceUpdate, limits openrealtime.Limits) error {
	if err := update.Validate(); err != nil {
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.source_update", Phase: "error",
			CorrelationID: update.Source, Message: err.Error(),
		})
		return err
	}
	if update.Width > limits.MaxDimension || update.Height > limits.MaxDimension {
		err := fmt.Errorf("video source exceeds the %d pixel limit on its long edge", limits.MaxDimension)
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.source_update", Phase: "error",
			CorrelationID: update.Source, Message: err.Error(), Attributes: map[string]any{
				"width": update.Width, "height": update.Height, "max_dimension": limits.MaxDimension,
			},
		})
		return err
	}
	session.sourcesMu.Lock()
	if update.State == openrealtime.SourceClosed {
		delete(session.sources, update.Source)
	} else {
		source, exists := session.sources[update.Source]
		if !exists {
			source = &videoSource{}
			session.sources[update.Source] = source
		}
		source.state, source.width, source.height = update.State, update.Width, update.Height
	}
	session.sourcesMu.Unlock()
	name := "video.source_updated"
	if update.State == openrealtime.SourceClosed {
		name = "video.source_closed"
	}
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugVideo), Name: name, Phase: "instant",
		CorrelationID: update.Source, Attributes: map[string]any{
			"state": update.State, "width": update.Width, "height": update.Height,
		},
	})
	return nil
}

func (session *session) appendVideoFrame(appendEvent openrealtime.VideoFrameAppend, limits openrealtime.Limits) error {
	payload, err := appendEvent.Decode(limits)
	if err != nil {
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.frame_decode", Phase: "error",
			CorrelationID: appendEvent.Source, Message: err.Error(),
		})
		return err
	}
	arrived := time.Now()
	session.sourcesMu.Lock()
	source, declared := session.sources[appendEvent.Source]
	if !declared {
		session.sourcesMu.Unlock()
		// Geometry is what makes a computer-use coordinate well-defined, so a
		// frame from an undeclared source is refused rather than guessed at.
		err := fmt.Errorf("video source %q was never declared", appendEvent.Source)
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.frame_refused", Phase: "error",
			CorrelationID: appendEvent.Source, Message: err.Error(), Attributes: map[string]any{
				"encoded_bytes": len(payload), "reason": "undeclared_source",
			},
		})
		return err
	}
	if source.state != openrealtime.SourceActive {
		state := source.state
		session.sourcesMu.Unlock()
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.frame_ignored", Phase: "instant",
			CorrelationID: appendEvent.Source, Attributes: map[string]any{
				"encoded_bytes": len(payload), "reason": "source_not_active", "state": state,
			},
		})
		return nil
	}
	// The rate cap is declared at negotiation, so it has to be enforced here:
	// a limit a client is told about and the server does not apply is not a
	// limit, and the cost of a frame that arrives too soon is paid before any
	// observer's gate gets to reject it. Excess is dropped rather than
	// refused, because a client that sends a little fast is conforming badly
	// rather than misbehaving, and failing its session over pacing would be a
	// worse answer than sampling it.
	if !source.admits(arrived, limits.FPSCap) {
		session.sourcesMu.Unlock()
		session.config.Metrics.videoFramesDropped.Add(1)
		_ = session.Debug(session.ctx, binding.DebugEvent{
			Category: string(openrealtime.DebugVideo), Name: "video.frame_dropped", Phase: "instant",
			CorrelationID: appendEvent.Source, Attributes: map[string]any{
				"encoded_bytes": len(payload), "reason": "fps_cap", "fps_cap": limits.FPSCap,
			},
		})
		return nil
	}
	source.index++
	frame := perception.Frame{
		Kind: perception.FrameImage, Source: appendEvent.Source, Index: source.index,
		Image: payload, MIMEType: mimeFor(limits.Format), Width: source.width, Height: source.height,
	}
	session.sourcesMu.Unlock()
	session.config.Metrics.videoFramesIn.Add(1)
	// Capture time is the client's if it supplied one and arrival time
	// otherwise. It is never left at zero: downstream this is the only
	// timestamp an observation carries, and an observation with no time in it
	// cannot be ordered against the speech it is supposed to accompany.
	frame.CapturedNS = uint64(arrived.UnixNano())
	if appendEvent.TimestampMS > 0 {
		frame.CapturedNS = uint64(appendEvent.TimestampMS) * uint64(time.Millisecond)
	}
	correlationID := fmt.Sprintf("%s:%d", appendEvent.Source, frame.Index)
	_ = session.Debug(session.ctx, binding.DebugEvent{
		Category: string(openrealtime.DebugVideo), Name: "video.frame_accepted", Phase: "instant",
		CorrelationID: correlationID, Attributes: map[string]any{
			"source": frame.Source, "frame_index": frame.Index, "encoded_bytes": len(payload),
			"width": frame.Width, "height": frame.Height, "captured_ms": frame.CapturedNS / 1_000_000,
		},
	})
	started := time.Now()
	err = session.runtime.Video(session.ctx, frame)
	trace := binding.DebugEvent{
		Category: string(openrealtime.DebugVideo), Name: "video.runtime_dispatch", Phase: "end",
		DurationMS: float64(time.Since(started)) / float64(time.Millisecond), CorrelationID: correlationID,
		Attributes: map[string]any{"source": frame.Source, "frame_index": frame.Index},
	}
	if err != nil {
		trace.Phase, trace.Message = "error", err.Error()
	}
	_ = session.Debug(session.ctx, trace)
	return err
}

// admits applies the declared frame-rate cap to one source.
//
// The cap is a floor on the interval rather than a count in a window: a window
// lets a client send a whole second's worth in a burst and then wait, which
// costs exactly what the cap exists to bound.
func (source *videoSource) admits(arrived time.Time, cap int) bool {
	if cap <= 0 {
		return true
	}
	interval := time.Second / time.Duration(cap)
	if !source.lastAdmitted.IsZero() && arrived.Sub(source.lastAdmitted) < interval {
		return false
	}
	source.lastAdmitted = arrived
	return true
}

func mimeFor(format string) string {
	switch strings.ToLower(strings.TrimSpace(format)) {
	case "png":
		return "image/png"
	case "webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}
