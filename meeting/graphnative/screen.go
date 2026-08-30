package graphnative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/element"
	modelelements "github.com/bojieli/OpenRealtime/elements/model"
	videograph "github.com/bojieli/OpenRealtime/elements/video"
)

type screenForkFactory struct{}

func (screenForkFactory) Descriptor() element.Descriptor { return ScreenForkDescriptor() }
func (screenForkFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeScreenForkConfig(source)
	return err
}

func (screenForkFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	config, err := decodeScreenForkConfig(mount.Config)
	if err != nil {
		return nil, fmt.Errorf("meeting.ScreenFork %s config: %w", mount.InstanceID, err)
	}
	video, err := mount.Ports.Input("video")
	if err != nil {
		return nil, err
	}
	outputs := make(map[string]element.OutputPort, 5)
	for _, name := range []string{"foreground", "source", "frame", "tick", "end"} {
		output, outputErr := mount.Ports.Output(name)
		if outputErr != nil {
			return nil, outputErr
		}
		outputs[name] = output
	}
	return &screenForkRunner{
		config: config, input: video, outputs: outputs, resolution: mount.Resolution,
	}, nil
}

type screenStream struct {
	sessionID, streamID, mimeType string
	revision, index, capturedNS   uint64
	haveFrame                     bool
}

type screenForkRunner struct {
	config     ScreenForkConfig
	input      element.InputPort
	outputs    map[string]element.OutputPort
	resolution element.ResolutionReporter
	active     *screenStream
	next       uint64
}

func (runner *screenForkRunner) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run meeting screen fork: nil context")
	}
	if err := reportRuntime(runner.resolution, screenForkRuntimeID); err != nil {
		return err
	}
	for {
		envelope, err := runner.input.Receive(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("receive meeting screen frame: %w", err)
		}
		if err := runner.accept(ctx, envelope); err != nil {
			return err
		}
	}
}

func (runner *screenForkRunner) accept(ctx context.Context, envelope element.Envelope) error {
	if !canonicalText(envelope.ItemID) || !canonicalText(envelope.SessionID) {
		return errors.New("meeting screen frame requires canonical item and session identities")
	}
	input, err := meetingVideoInput(envelope.Payload)
	if err != nil {
		return fmt.Errorf("meeting screen frame %q: %w", envelope.ItemID, err)
	}
	if !canonicalText(input.StreamID) {
		return fmt.Errorf("meeting screen frame %q requires a canonical stream ID", envelope.ItemID)
	}
	if input.Frame.Kind != "image" || input.Frame.Source != runner.config.Source {
		return fmt.Errorf("meeting screen frame %q requires the configured image source", envelope.ItemID)
	}
	if input.Frame.MIMEType != "image/jpeg" && input.Frame.MIMEType != "image/png" {
		return fmt.Errorf("meeting screen frame %q has an unsupported MIME type", envelope.ItemID)
	}
	if err := input.Frame.Validate(); err != nil {
		return fmt.Errorf("meeting screen frame %q: %w", envelope.ItemID, err)
	}
	if input.Frame.CapturedNS == 0 ||
		envelope.CaptureNS != input.Frame.CapturedNS ||
		len(input.Frame.Image) > runner.config.MaxFrameBytes {
		return fmt.Errorf("meeting screen frame %q is not a bounded, externally clocked image", envelope.ItemID)
	}
	if input.FrameRateMilliHz < 1 || input.FrameRateMilliHz > 1_000_000 {
		return fmt.Errorf("meeting screen frame %q has invalid frame rate %d",
			envelope.ItemID, input.FrameRateMilliHz)
	}
	input.Frame.Image = slices.Clone(input.Frame.Image)
	if runner.active != nil && runner.active.sessionID != envelope.SessionID {
		return fmt.Errorf("meeting screen frame %q crossed session %q into %q",
			envelope.ItemID, runner.active.sessionID, envelope.SessionID)
	}
	if runner.active == nil || runner.active.streamID != input.StreamID {
		if runner.active != nil {
			if err := runner.publishEnd(ctx, envelope, input.Frame.CapturedNS, "stream-replaced"); err != nil {
				return err
			}
		}
		runner.next++
		runner.active = &screenStream{
			sessionID: envelope.SessionID, streamID: input.StreamID,
			mimeType: input.Frame.MIMEType, revision: runner.next,
		}
		if err := runner.publishSource(ctx, envelope, input); err != nil {
			return err
		}
	}
	if runner.active.mimeType != input.Frame.MIMEType ||
		(runner.active.haveFrame && input.Frame.CapturedNS <= runner.active.capturedNS) ||
		(runner.active.haveFrame && input.Frame.Index <= runner.active.index) {
		return fmt.Errorf("meeting screen frame %q changes media or regresses stream time/index", envelope.ItemID)
	}
	runner.active.index = input.Frame.Index
	runner.active.capturedNS = input.Frame.CapturedNS
	runner.active.haveFrame = true

	foreground := envelope.Clone()
	foreground.Type = modelelements.VideoInputType()
	foreground.Payload = input
	if _, err := runner.outputs["foreground"].Broadcast(ctx, foreground); err != nil {
		return fmt.Errorf("forward meeting screen frame to foreground: %w", err)
	}

	frame := derivedMeetingEnvelope(envelope, "observe-frame", videograph.InlineFrameType())
	frame.CaptureNS = input.Frame.CapturedNS
	frame.SourceID = runner.config.Source
	frame.CancellationScope = input.StreamID
	frame.Payload = videograph.InlineFrame{
		StreamID: input.StreamID, SourceRevision: runner.active.revision, Frame: input.Frame,
	}
	if _, err := runner.outputs["frame"].Broadcast(ctx, frame); err != nil {
		return fmt.Errorf("publish meeting observation frame: %w", err)
	}

	tick := derivedMeetingEnvelope(envelope, "observe-tick", videograph.TimingTickType())
	tick.CaptureNS = input.Frame.CapturedNS
	tick.SourceID = runner.config.Source
	tick.CancellationScope = input.StreamID
	tick.Payload = videograph.TimingTick{
		Source: runner.config.Source, StreamID: input.StreamID,
		SourceRevision: runner.active.revision, NowNS: input.Frame.CapturedNS,
	}
	if _, err := runner.outputs["tick"].Broadcast(ctx, tick); err != nil {
		return fmt.Errorf("publish meeting observation tick: %w", err)
	}
	return nil
}

func (runner *screenForkRunner) publishSource(
	ctx context.Context, cause element.Envelope, input modelelements.VideoInputFrame,
) error {
	envelope := derivedMeetingEnvelope(cause, "source", videograph.SourceStartType())
	envelope.CaptureNS = input.Frame.CapturedNS
	envelope.SourceID = runner.config.Source
	envelope.CancellationScope = input.StreamID
	envelope.Payload = videograph.SourceStart{
		Source: runner.config.Source, StreamID: input.StreamID, Kind: runner.config.Kind,
		SourceRevision: runner.active.revision, OpenedNS: input.Frame.CapturedNS,
		NonBackpressurable: true, ExpectedMIMEType: input.Frame.MIMEType,
	}
	_, err := runner.outputs["source"].Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("publish meeting screen source: %w", err)
	}
	return nil
}

func (runner *screenForkRunner) publishEnd(
	ctx context.Context, cause element.Envelope, at uint64, reason string,
) error {
	if runner.active == nil {
		return nil
	}
	if at < runner.active.capturedNS {
		at = runner.active.capturedNS
	}
	envelope := derivedMeetingEnvelope(cause, "end", videograph.SourceEndType())
	envelope.SessionID = runner.active.sessionID
	envelope.CaptureNS = at
	envelope.SourceID = runner.config.Source
	envelope.CancellationScope = runner.active.streamID
	envelope.Payload = videograph.SourceEnd{
		Source: runner.config.Source, StreamID: runner.active.streamID,
		SourceRevision: runner.active.revision, EndedNS: at, Reason: reason,
	}
	_, err := runner.outputs["end"].Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("publish meeting screen end: %w", err)
	}
	return nil
}

func meetingVideoInput(payload any) (modelelements.VideoInputFrame, error) {
	var input modelelements.VideoInputFrame
	switch typed := payload.(type) {
	case modelelements.VideoInputFrame:
		input = typed
	case *modelelements.VideoInputFrame:
		if typed == nil {
			return input, errors.New("nil video input")
		}
		input = *typed
	default:
		return input, fmt.Errorf("payload has type %T, want model.VideoInputFrame", payload)
	}
	return input, nil
}

func derivedMeetingEnvelope(cause element.Envelope, suffix string, valueType element.Type) element.Envelope {
	result := cause.Clone()
	result.Type = valueType
	result.ItemID = cause.ItemID + "/meeting-" + suffix
	result.CausalParents = appendUnique(result.CausalParents, cause.ItemID)
	return result
}

func appendUnique(source []string, value string) []string {
	for _, existing := range source {
		if existing == value {
			return source
		}
	}
	return append(source, value)
}
