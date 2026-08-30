package graphnative

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	videograph "github.com/bojieli/OpenRealtime/elements/video"
	"github.com/bojieli/OpenRealtime/internal/elementconfig"
)

const (
	defaultMaxFrameBytes     = 8 << 20
	maximumMaxFrameBytes     = 16 << 20
	defaultMaxBackgroundRuns = 64
	maximumMaxBackgroundRuns = 4096
	// ContextInjection uses the protocol-v4 JSON payload boundary. Keeping the
	// raw text at or below 80 KiB leaves room under its 512 KiB wire cap even
	// when every byte requires JSON's longest six-byte escape.
	defaultMaxBackgroundBytes = 64 << 10
	maximumBackgroundBytes    = 80 << 10
	defaultTerminalMemory     = 512
	maximumTerminalMemory     = 65_536
	maximumInstructionBytes   = 64 << 10
)

type ScreenForkConfig struct {
	Source        string                `json:"source"`
	Kind          videograph.SourceKind `json:"kind"`
	MaxFrameBytes int                   `json:"max_frame_bytes,omitempty"`
}

func decodeScreenForkConfig(source json.RawMessage) (ScreenForkConfig, error) {
	config := ScreenForkConfig{MaxFrameBytes: defaultMaxFrameBytes}
	if err := elementconfig.Decode(source, &config); err != nil {
		return ScreenForkConfig{}, err
	}
	if !canonicalText(config.Source) {
		return ScreenForkConfig{}, errors.New("screen fork requires a canonical source")
	}
	switch config.Kind {
	case videograph.SourceScreen, videograph.SourceCamera, videograph.SourceVideo:
	default:
		return ScreenForkConfig{}, errors.New("screen fork has an unsupported source kind")
	}
	if config.MaxFrameBytes < 1 || config.MaxFrameBytes > maximumMaxFrameBytes {
		return ScreenForkConfig{}, fmt.Errorf("screen fork max_frame_bytes must be between 1 and %d", maximumMaxFrameBytes)
	}
	return config, nil
}

type BackgroundInjectionConfig struct {
	Role            string `json:"role"`
	Instruction     string `json:"instruction"`
	MaxOutputTokens int    `json:"max_output_tokens,omitempty"`
	MaxRuns         int    `json:"max_runs,omitempty"`
	MaxTextBytes    int    `json:"max_text_bytes,omitempty"`
	TerminalMemory  int    `json:"terminal_memory,omitempty"`
}

func decodeBackgroundInjectionConfig(source json.RawMessage) (BackgroundInjectionConfig, error) {
	config := BackgroundInjectionConfig{
		MaxOutputTokens: 512,
		MaxRuns:         defaultMaxBackgroundRuns,
		MaxTextBytes:    defaultMaxBackgroundBytes,
		TerminalMemory:  defaultTerminalMemory,
	}
	if err := elementconfig.Decode(source, &config); err != nil {
		return BackgroundInjectionConfig{}, err
	}
	if config.Role != "background" {
		return BackgroundInjectionConfig{}, errors.New("background injection role must be background")
	}
	if len(config.Instruction) == 0 || len(config.Instruction) > maximumInstructionBytes {
		return BackgroundInjectionConfig{}, fmt.Errorf("background instruction must contain 1 to %d bytes", maximumInstructionBytes)
	}
	if config.Instruction != strings.TrimSpace(config.Instruction) ||
		strings.ContainsRune(config.Instruction, '\x00') {
		return BackgroundInjectionConfig{}, errors.New("background instruction must be canonical")
	}
	if config.MaxOutputTokens < 1 || config.MaxOutputTokens > 1_000_000 {
		return BackgroundInjectionConfig{}, errors.New("background max_output_tokens must be between 1 and 1000000")
	}
	if config.MaxRuns < 1 || config.MaxRuns > maximumMaxBackgroundRuns {
		return BackgroundInjectionConfig{}, fmt.Errorf("background max_runs must be between 1 and %d", maximumMaxBackgroundRuns)
	}
	if config.MaxTextBytes < 1 || config.MaxTextBytes > maximumBackgroundBytes {
		return BackgroundInjectionConfig{}, fmt.Errorf("background max_text_bytes must be between 1 and %d", maximumBackgroundBytes)
	}
	if config.TerminalMemory < 1 || config.TerminalMemory > maximumTerminalMemory {
		return BackgroundInjectionConfig{}, fmt.Errorf("background terminal_memory must be between 1 and %d", maximumTerminalMemory)
	}
	return config, nil
}

func canonicalText(value string) bool {
	return value != "" && len(value) <= 1024 && value == strings.TrimSpace(value) &&
		!strings.ContainsAny(value, "\x00\r\n")
}
