// Command openrealtime exposes protocol conformance and deterministic replay tools.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"

	"github.com/bojieli/OpenRealtime/adapters/reference"
	"github.com/bojieli/OpenRealtime/baseline"
	"github.com/bojieli/OpenRealtime/internal/audio"
	openaiwire "github.com/bojieli/OpenRealtime/protocol/openai"
	"github.com/bojieli/OpenRealtime/replay"
	"github.com/bojieli/OpenRealtime/trace"
	"github.com/bojieli/OpenRealtime/visualization/timeline"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "openrealtime:", err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return usageError()
	}
	switch arguments[0] {
	case "fixture":
		return runFixture(arguments[1:], output)
	case "replay":
		return runReplay(arguments[1:], output)
	case "protocol":
		return runProtocol(arguments[1:], output)
	case "trace":
		return runTrace(arguments[1:], output)
	case "benchmark":
		return runBenchmark(arguments[1:], output)
	default:
		return usageError()
	}
}

func usageError() error {
	return errors.New("usage: openrealtime <fixture|replay|protocol|trace|benchmark> <command> [options]")
}

func runFixture(arguments []string, output io.Writer) error {
	if len(arguments) == 0 || arguments[0] != "generate" {
		return errors.New("usage: openrealtime fixture generate <output.wav>")
	}
	flags := flag.NewFlagSet("fixture generate", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("fixture generate requires one output path")
	}
	path := flags.Arg(0)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	digest, err := audio.GenerateFixture(path)
	if err != nil {
		return err
	}
	return writeJSON(output, map[string]any{
		"path":           path,
		"sha256":         hex.EncodeToString(digest[:]),
		"sample_rate_hz": audio.OpenAIPCMSampleRate,
		"sample_count":   audio.OpenAIPCMSampleRate,
		"duration_ns":    uint64(1_000_000_000),
	})
}

func runReplay(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime replay <input.wav> --events <events.jsonl> --trace <trace.jsonl> [--frame-ms 20]")
	}
	inputPath := arguments[0]
	flags := flag.NewFlagSet("replay", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	eventsPath := flags.String("events", "", "OpenAI-compatible client event JSONL output")
	tracePath := flags.String("trace", "", "timed OpenRealtime trace JSONL output")
	sessionID := flags.String("session-id", "m0-replay", "deterministic session ID")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *eventsPath == "" || *tracePath == "" {
		return errors.New("usage: openrealtime replay <input.wav> --events <events.jsonl> --trace <trace.jsonl> [--frame-ms 20]")
	}
	if *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms exceeds uint32")
	}
	events, err := newAtomicOutput(*eventsPath)
	if err != nil {
		return err
	}
	defer events.Abort()
	traces, err := newAtomicOutput(*tracePath)
	if err != nil {
		return err
	}
	defer traces.Abort()
	summary, err := replay.WAV(inputPath, replay.Options{
		SessionID:       *sessionID,
		FrameDurationMS: uint32(*frameMS),
		Events:          events.File,
		Trace:           traces.File,
	})
	if err != nil {
		return err
	}
	if err := events.Commit(); err != nil {
		return err
	}
	if err := traces.Commit(); err != nil {
		return err
	}
	return writeJSON(output, summary)
}

func runProtocol(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("usage: openrealtime protocol <inventory|validate>")
	}
	switch arguments[0] {
	case "inventory":
		definitions := openaiwire.Definitions()
		counts := make(map[string]int)
		for _, definition := range definitions {
			counts[string(definition.Profile)+"/"+string(definition.Direction)]++
		}
		return writeJSON(output, map[string]any{
			"definition_count": len(definitions),
			"counts":           counts,
			"definitions":      definitions,
		})
	case "validate":
		flags := flag.NewFlagSet("protocol validate", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		profileValue := flags.String("profile", "realtime", "realtime, transcription, translation, or beta")
		directionValue := flags.String("direction", "client", "client or server")
		if err := flags.Parse(arguments[1:]); err != nil {
			return err
		}
		if flags.NArg() != 1 {
			return errors.New("protocol validate requires one JSONL path")
		}
		profile, err := parseProfile(*profileValue)
		if err != nil {
			return err
		}
		direction, err := parseDirection(*directionValue)
		if err != nil {
			return err
		}
		file, err := os.Open(flags.Arg(0))
		if err != nil {
			return err
		}
		defer file.Close()
		validator := openaiwire.NewValidator()
		count, err := eachJSONLine(file, func(line []byte, number uint64) error {
			message, err := openaiwire.Decode(line)
			if err != nil {
				return fmt.Errorf("line %d: %w", number, err)
			}
			if err := validator.Validate(profile, direction, message); err != nil {
				return fmt.Errorf("line %d: %w", number, err)
			}
			return nil
		})
		if err != nil {
			return err
		}
		return writeJSON(output, map[string]any{"events": count, "profile": profile, "direction": direction})
	default:
		return errors.New("usage: openrealtime protocol <inventory|validate>")
	}
}

func runTrace(arguments []string, output io.Writer) error {
	if len(arguments) < 2 || (arguments[0] != "validate" && arguments[0] != "summarize") {
		return errors.New("usage: openrealtime trace <validate|summarize> <trace.jsonl>")
	}
	file, err := os.Open(arguments[1])
	if err != nil {
		return err
	}
	defer file.Close()
	validator := openaiwire.NewValidator()
	typeCounts := make(map[string]uint64)
	var firstNS, lastNS uint64
	var sessionID string
	count, err := trace.Read(file, func(record trace.Record) error {
		message, err := openaiwire.Decode(record.Message)
		if err != nil {
			return err
		}
		if err := validator.Validate(record.Profile, record.Direction, message); err != nil {
			return err
		}
		if sessionID == "" {
			sessionID = record.SessionID
			firstNS = record.MonotonicNS
		}
		lastNS = record.MonotonicNS
		typeCounts[string(message.Type())]++
		return nil
	})
	if err != nil {
		return err
	}
	if arguments[0] == "validate" {
		return writeJSON(output, map[string]any{"records": count})
	}
	keys := make([]string, 0, len(typeCounts))
	for key := range typeCounts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	orderedCounts := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		orderedCounts = append(orderedCounts, map[string]any{"type": key, "count": typeCounts[key]})
	}
	return writeJSON(output, map[string]any{
		"session_id":  sessionID,
		"records":     count,
		"duration_ns": lastNS - firstNS,
		"type_counts": orderedCounts,
	})
}

func runBenchmark(arguments []string, output io.Writer) error {
	if len(arguments) == 0 || arguments[0] != "m1" {
		return errors.New("usage: openrealtime benchmark m1 --fixture <audio.wav> --manifest <manifest.json> --output <directory>")
	}
	flags := flag.NewFlagSet("benchmark m1", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	fixturePath := flags.String("fixture", "", "24 kHz PCM16 fixture")
	manifestPath := flags.String("manifest", "", "reference adapter manifest")
	outputDirectory := flags.String("output", "", "benchmark artifact directory")
	trials := flags.Uint64("trials", 30, "number of paired deterministic trials")
	seed := flags.Uint64("seed", 20260817, "base deterministic random seed")
	frameMS := flags.Uint("frame-ms", 20, "input frame duration in milliseconds")
	if err := flags.Parse(arguments[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 || *fixturePath == "" || *manifestPath == "" || *outputDirectory == "" {
		return errors.New("benchmark m1 requires --fixture, --manifest, and --output")
	}
	if *frameMS == 0 || *frameMS > uint(^uint32(0)) {
		return errors.New("frame-ms must fit a positive uint32")
	}
	if err := prepareEmptyDirectory(*outputDirectory); err != nil {
		return err
	}
	manifest, err := reference.LoadManifest(*manifestPath)
	if err != nil {
		return err
	}
	report, err := baseline.Run(context.Background(), baseline.Config{
		FixturePath: *fixturePath,
		Manifest:    manifest,
		Trials:      *trials,
		Seed:        *seed,
		FrameMS:     uint32(*frameMS),
		Timing:      baseline.DefaultTimingModel(),
	})
	if err != nil {
		return err
	}
	if err := os.Mkdir(filepath.Join(*outputDirectory, "traces"), 0o755); err != nil {
		return err
	}
	if err := writeAtomicJSON(filepath.Join(*outputDirectory, "report.json"), report); err != nil {
		return err
	}
	for _, trial := range report.Trials {
		path := filepath.Join(*outputDirectory, "traces", fmt.Sprintf("trial-%04d.jsonl", trial.Index))
		artifact, err := newAtomicOutput(path)
		if err != nil {
			return err
		}
		writer := trace.NewWriter(artifact.File)
		writeErr := error(nil)
		for _, record := range trial.Trace {
			if writeErr = writer.Write(record); writeErr != nil {
				break
			}
		}
		if writeErr == nil {
			writeErr = writer.Flush()
		}
		if writeErr == nil {
			writeErr = artifact.Commit()
		}
		if writeErr != nil {
			artifact.Abort()
			return writeErr
		}
	}
	timelineArtifact, err := newAtomicOutput(filepath.Join(*outputDirectory, "timeline-trial-0000.html"))
	if err != nil {
		return err
	}
	if err := timeline.Render(timelineArtifact.File, "M1 endpointed reference — trial 0000", report.Trials[0].Trace); err != nil {
		timelineArtifact.Abort()
		return err
	}
	if err := timelineArtifact.Commit(); err != nil {
		timelineArtifact.Abort()
		return err
	}
	return writeJSON(output, map[string]any{
		"output": *outputDirectory, "trials": len(report.Trials),
		"condition": report.Condition, "timing_mode": report.TimingMode,
		"distributions": report.Distributions,
	})
}

func prepareEmptyDirectory(path string) error {
	entries, err := os.ReadDir(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(path, 0o755); err != nil {
			return fmt.Errorf("create benchmark output directory: %w", err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect benchmark output directory: %w", err)
	}
	if len(entries) != 0 {
		return errors.New("benchmark output directory must be empty to prevent stale evidence")
	}
	return nil
}

func parseProfile(value string) (openaiwire.Profile, error) {
	profile := openaiwire.Profile(value)
	switch profile {
	case openaiwire.ProfileRealtime, openaiwire.ProfileTranscription, openaiwire.ProfileTranslation, openaiwire.ProfileBeta:
		return profile, nil
	default:
		return "", fmt.Errorf("unknown protocol profile %q", value)
	}
}

func parseDirection(value string) (openaiwire.Direction, error) {
	direction := openaiwire.Direction(value)
	switch direction {
	case openaiwire.DirectionClient, openaiwire.DirectionServer:
		return direction, nil
	default:
		return "", fmt.Errorf("unknown event direction %q", value)
	}
}

func eachJSONLine(input io.Reader, visit func([]byte, uint64) error) (uint64, error) {
	reader := bufio.NewReader(input)
	var count uint64
	for {
		line, err := reader.ReadBytes('\n')
		line = bytes.TrimSpace(line)
		if len(line) > 0 {
			count++
			if visitErr := visit(line, count); visitErr != nil {
				return count - 1, visitErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return count, err
		}
	}
	if count == 0 {
		return 0, errors.New("JSONL input must contain at least one event")
	}
	return count, nil
}

func writeJSON(output io.Writer, value any) error {
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func writeAtomicJSON(path string, value any) error {
	artifact, err := newAtomicOutput(path)
	if err != nil {
		return err
	}
	defer artifact.Abort()
	if err := writeJSON(artifact.File, value); err != nil {
		return err
	}
	return artifact.Commit()
}

type atomicOutput struct {
	File      *os.File
	temporary string
	target    string
	committed bool
}

func newAtomicOutput(target string) (*atomicOutput, error) {
	directory := filepath.Dir(target)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	file, err := os.CreateTemp(directory, ".openrealtime-*")
	if err != nil {
		return nil, err
	}
	return &atomicOutput{File: file, temporary: file.Name(), target: target}, nil
}

func (output *atomicOutput) Commit() error {
	if output.committed {
		return nil
	}
	if err := output.File.Sync(); err != nil {
		return err
	}
	if err := output.File.Chmod(0o644); err != nil {
		return err
	}
	if err := output.File.Close(); err != nil {
		return err
	}
	if err := os.Rename(output.temporary, output.target); err != nil {
		return err
	}
	output.committed = true
	return nil
}

func (output *atomicOutput) Abort() {
	if output == nil || output.committed {
		return
	}
	output.File.Close()
	_ = os.Remove(output.temporary)
}
