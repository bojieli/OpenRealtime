package video

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/element"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const maximumFrameBytes = 64 << 20

// FrameIngressDescriptor is a deliberately tiny scheduling boundary. A graph
// connects its outputs with => to make capture loss visible in Graph IR while
// its receiver keeps an external lossless boundary drained with no policy or
// provider work on that path.
func FrameIngressDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "video.FrameIngress",
		Revision:      1,
		Ports: []element.Port{
			{Name: "frame_in", Direction: element.Input, Type: inlineFrameType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 64},
			{Name: "reference_in", Direction: element.Input, Type: frameRefType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 64},
			{Name: "frames", Direction: element.Output, Type: inlineFrameType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "references", Direction: element.Output, Type: frameRefType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
		},
		Reaction: element.Reaction{
			Triggers: []string{"frame_in", "reference_in"},
			Outcomes: []string{"frames", "references"}, MaxConcurrency: 2,
		},
	}
}

func AdaptiveObservationDescriptor() element.Descriptor {
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion,
		Name:          "video.AdaptiveObservation",
		Revision:      1,
		Ports: []element.Port{
			{Name: "source", Direction: element.Input, Type: sourceStartType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "frames", Direction: element.Input, Type: inlineFrameType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "references", Direction: element.Input, Type: frameRefType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "tick", Direction: element.Input, Type: timingTickType,
				Cardinality: element.One, Required: true, DefaultDepth: 16},
			{Name: "refresh", Direction: element.Input, Type: refreshType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "end", Direction: element.Input, Type: sourceEndType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "cancel", Direction: element.Input, Type: cancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "observe", Direction: element.Output, Type: imageBatchType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "observe_reference", Direction: element.Output, Type: referenceBatchType,
				Cardinality: element.One, Required: true, DefaultDepth: 4},
			{Name: "observer_refresh", Direction: element.Output, Type: visualRefreshType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "observer_close", Direction: element.Output, Type: visualCloseType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "observer_cancel", Direction: element.Output, Type: visualCancelType,
				Cardinality: element.One, Required: true, DefaultDepth: 8},
			{Name: "state", Direction: element.Output, Type: policyStateType,
				Cardinality: element.One, Required: true, LossAllowed: true, DefaultDepth: 1},
			{Name: "decision", Direction: element.Output, Type: decisionType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
			{Name: "outcome", Direction: element.Output, Type: policyOutcomeType,
				Cardinality: element.One, Required: true, DefaultDepth: 32},
		},
		Reaction: element.Reaction{
			Triggers:   []string{"source", "frames", "references", "tick", "refresh", "end"},
			Interrupts: []string{"cancel"},
			Outcomes: []string{
				"observe", "observe_reference", "observer_refresh", "observer_close",
				"observer_cancel", "state", "decision", "outcome",
			},
			MaxConcurrency: 1,
		},
		StateSchema:  "schema://openrealtime/video/adaptive-observation-state/v1",
		ConfigSchema: "schema://openrealtime/video/adaptive-observation-config/v1",
		Dependencies: []element.Dependency{{Name: graphruntime.SequenceServiceName}},
		Effects:      []element.Effect{{Name: "video.latest-frame.memory", Reversible: true}},
	}
}

type AdaptiveObservationConfig struct {
	Source           string      `json:"source"`
	Mode             CadenceMode `json:"mode,omitempty"`
	FixedIntervalMS  int64       `json:"fixed_interval_ms,omitempty"`
	MinIntervalMS    int64       `json:"min_interval_ms,omitempty"`
	MaxIntervalMS    int64       `json:"max_interval_ms,omitempty"`
	ChangeThreshold  float64     `json:"change_threshold,omitempty"`
	MaxFrameBytes    int         `json:"max_frame_bytes,omitempty"`
	MaxChangeSamples int         `json:"max_change_samples,omitempty"`
	MaxMetadataBytes int         `json:"max_metadata_bytes,omitempty"`
	TerminalMemory   int         `json:"terminal_memory,omitempty"`
}

func decodeAdaptiveObservationConfig(source json.RawMessage) (AdaptiveObservationConfig, error) {
	config := AdaptiveObservationConfig{
		Mode: CadenceAdaptive, FixedIntervalMS: 1000,
		MinIntervalMS: 250, MaxIntervalMS: 2000, ChangeThreshold: 0.02,
		MaxFrameBytes: 8 << 20, MaxChangeSamples: 512,
		MaxMetadataBytes: 64 << 10, TerminalMemory: 256,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return AdaptiveObservationConfig{}, err
	}
	config.Source = strings.TrimSpace(config.Source)
	if config.Source == "" || strings.ContainsAny(config.Source, "\x00\r\n") {
		return AdaptiveObservationConfig{}, errors.New("adaptive video policy requires a canonical source")
	}
	if err := config.Mode.validate(); err != nil {
		return AdaptiveObservationConfig{}, err
	}
	if config.FixedIntervalMS < 1 || config.FixedIntervalMS > 24*60*60*1000 {
		return AdaptiveObservationConfig{}, errors.New("fixed_interval_ms must be between 1 and 86400000")
	}
	if config.MinIntervalMS < 1 || config.MinIntervalMS > 24*60*60*1000 {
		return AdaptiveObservationConfig{}, errors.New("min_interval_ms must be between 1 and 86400000")
	}
	if config.MaxIntervalMS < config.MinIntervalMS || config.MaxIntervalMS > 24*60*60*1000 {
		return AdaptiveObservationConfig{}, errors.New("max_interval_ms must be between min_interval_ms and 86400000")
	}
	if config.ChangeThreshold < 0 || config.ChangeThreshold > 1 {
		return AdaptiveObservationConfig{}, errors.New("change_threshold must be between zero and one")
	}
	if config.MaxFrameBytes < 1 || config.MaxFrameBytes > maximumFrameBytes {
		return AdaptiveObservationConfig{}, fmt.Errorf("max_frame_bytes must be between 1 and %d", maximumFrameBytes)
	}
	if config.MaxChangeSamples < 16 || config.MaxChangeSamples > 4096 {
		return AdaptiveObservationConfig{}, errors.New("max_change_samples must be between 16 and 4096")
	}
	if config.MaxMetadataBytes < 256 || config.MaxMetadataBytes > 1<<20 {
		return AdaptiveObservationConfig{}, errors.New("max_metadata_bytes must be between 256 and 1048576")
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > 1_000_000 {
		return AdaptiveObservationConfig{}, errors.New("terminal_memory must be between 1 and 1000000")
	}
	return config, nil
}
