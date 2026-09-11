package acoustic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
	"github.com/bojieli/OpenRealtime/perception/noisefilter"
)

const noiseFilterRuntimeID = "builtin://openrealtime/elements/acoustic.NoiseFilter"

// NoiseFilterDescriptor places the waveform transform ahead of BOTH energy
// admission and ASR. A parallel evidence lane cannot provide this ordering.
func NoiseFilterDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "acoustic.NoiseFilter", Revision: 1,
		Ports: []element.Port{
			{Name: "audio", Direction: element.Input, Type: rawAudioType, Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "filtered", Direction: element.Output, Type: rawAudioType, Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction:     element.Reaction{Triggers: []string{"audio"}, Outcomes: []string{"filtered"}, MaxConcurrency: 1},
		ConfigSchema: "schema://openrealtime/acoustic/noise-filter-config/v1",
		Effects:      []element.Effect{{Name: "audio-filter-service", External: true, Authority: "audio.InputFrame"}},
	}
}

type noiseFilterFactory struct{}

func (noiseFilterFactory) Descriptor() element.Descriptor { return NoiseFilterDescriptor() }
func decodeNoiseFilter(source json.RawMessage) (noisefilter.Config, error) {
	var config noisefilter.Config
	if err := elementconfig.Decode(source, &config); err != nil {
		return config, err
	}
	return config, config.Validate()
}
func (noiseFilterFactory) ValidateConfig(source json.RawMessage) error {
	_, err := decodeNoiseFilter(source)
	return err
}
func (noiseFilterFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	config, err := decodeNoiseFilter(mount.Config)
	if err != nil {
		return nil, err
	}
	input, err := mount.Ports.Input("audio")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("filtered")
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		if err := reportBuiltIn(mount.Resolution, noiseFilterRuntimeID, "implementation:1"); err != nil {
			return err
		}
		client, err := noisefilter.New(config)
		if err != nil {
			return err
		}
		defer client.Close()
		lastState := "healthy"
		for {
			envelope, err := input.Receive(ctx)
			if terminalReceive(ctx, err) {
				return nil
			}
			if err != nil {
				return err
			}
			frame, ok := envelope.Payload.(InputFrame)
			if !ok {
				return fmt.Errorf("noise filter expected audio.InputFrame, got %T", envelope.Payload)
			}
			pcm, err := client.Process(ctx, frame.Frame.PCM16LE, frame.Frame.SampleRateHz)
			if err != nil {
				return err
			}

			status := client.Status()
			if status.State != lastState {
				slog.WarnContext(ctx, "target voice delivery state changed", "element", mount.InstanceID, "state", status.State, "reason", status.Reason, "late_packets", status.LatePackets, "muted_ms", status.MutedMS)
				lastState = status.State
			}
			frame.Frame.PCM16LE = pcm
			envelope = derivedEnvelope(envelope, rawAudioType, ":filtered", frame)
			if _, err := output.Broadcast(ctx, envelope); err != nil {
				return err
			}
		}
	}), nil
}
