// Package fdbench adapts the released FD-Bench audio corpus to the standard
// OpenAI Realtime protocol while preserving the upstream timestamp contract.
package fdbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/livebench"
)

const (
	BenchmarkName       = "FD-Bench"
	BenchmarkRevision   = "8a4b7df1b4dcb0fc50a7a5660247a2ffe8d394eb"
	DatasetRevision     = "995e7178445b90d57dee8fab500c6e37a2f2c49e"
	ResultSchemaVersion = "1.0.0"
	TimestampRateHz     = 16_000
)

var conversationName = regexp.MustCompile(`^conversation_([1-9][0-9]*)\.wav$`)

// Segment is an upstream-compatible half-open interval in 16 kHz sample
// indices. The released .timestamps files use this clock even though their WAV
// containers are 24 kHz.
type Segment struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

type Sample struct {
	Cell            string    `json:"cell"`
	Conversation    int       `json:"conversation"`
	ID              string    `json:"id"`
	InputPath       string    `json:"input_path"`
	TimestampPath   string    `json:"timestamp_path"`
	InputSHA256     string    `json:"input_sha256"`
	TimestampSHA256 string    `json:"timestamp_sha256"`
	InputDurationMS float64   `json:"input_duration_ms"`
	InputSegments   []Segment `json:"input_segments"`
}

// Discover finds every released conversation that has both audio and the
// corresponding timestamp sidecar. It rejects malformed, overlapping, or
// out-of-range timestamps rather than silently changing the benchmark clock.
func Discover(root string) ([]Sample, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var samples []Sample
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !entry.Type().IsRegular() {
			return nil
		}
		matches := conversationName.FindStringSubmatch(entry.Name())
		if matches == nil {
			return nil
		}
		conversation, parseErr := strconv.Atoi(matches[1])
		if parseErr != nil {
			return fmt.Errorf("parse conversation number for %s: %w", path, parseErr)
		}
		timestampPath := strings.TrimSuffix(path, ".wav") + ".timestamps"
		if info, statErr := os.Stat(timestampPath); statErr != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("FD-Bench input %s has no regular timestamp sidecar", path)
		}
		audio, readErr := livebench.ReadWAV(path)
		if readErr != nil {
			return fmt.Errorf("read FD-Bench input %s: %w", path, readErr)
		}
		segments, timestampHash, readErr := readSegments(timestampPath, audio.Duration())
		if readErr != nil {
			return readErr
		}
		inputHash, hashErr := livebench.HashFile(path)
		if hashErr != nil {
			return fmt.Errorf("hash FD-Bench input %s: %w", path, hashErr)
		}
		cell := filepath.Base(filepath.Dir(path))
		if strings.TrimSpace(cell) == "" || cell == "." {
			return fmt.Errorf("FD-Bench input %s has no condition directory", path)
		}
		samples = append(samples, Sample{
			Cell: cell, Conversation: conversation, ID: strings.TrimSuffix(entry.Name(), ".wav"),
			InputPath: path, TimestampPath: timestampPath, InputSHA256: inputHash,
			TimestampSHA256: timestampHash, InputDurationMS: durationMS(audio.Duration()), InputSegments: segments,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(samples) == 0 {
		return nil, fmt.Errorf("no released FD-Bench conversations found below %s", root)
	}
	slices.SortFunc(samples, func(left, right Sample) int {
		if value := strings.Compare(left.Cell, right.Cell); value != 0 {
			return value
		}
		return left.Conversation - right.Conversation
	})
	for index := 1; index < len(samples); index++ {
		if samples[index-1].Cell == samples[index].Cell && samples[index-1].Conversation == samples[index].Conversation {
			return nil, fmt.Errorf("duplicate FD-Bench sample %s/%s", samples[index].Cell, samples[index].ID)
		}
	}
	return samples, nil
}

func readSegments(filename string, duration time.Duration) ([]Segment, string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", fmt.Errorf("read FD-Bench timestamps %s: %w", filename, err)
	}
	var segments []Segment
	if err := json.Unmarshal(data, &segments); err != nil {
		return nil, "", fmt.Errorf("decode FD-Bench timestamps %s: %w", filename, err)
	}
	if len(segments) == 0 {
		return nil, "", fmt.Errorf("FD-Bench timestamps %s are empty", filename)
	}
	maximum := int64(math.Ceil(float64(duration) / float64(time.Second) * TimestampRateHz))
	var priorEnd int64
	for index, segment := range segments {
		if segment.Start < 0 || segment.End <= segment.Start || segment.End > maximum {
			return nil, "", fmt.Errorf("FD-Bench timestamps %s contain invalid segment %d: %+v (maximum %d)", filename, index, segment, maximum)
		}
		if index != 0 && segment.Start < priorEnd {
			return nil, "", fmt.Errorf("FD-Bench timestamps %s overlap at segment %d", filename, index)
		}
		priorEnd = segment.End
	}
	digest := sha256.Sum256(data)
	return segments, hex.EncodeToString(digest[:]), nil
}

type OutputVAD struct {
	Status               string  `json:"status"`
	Name                 string  `json:"name,omitempty"`
	PackageVersion       string  `json:"package_version,omitempty"`
	Threshold            float64 `json:"threshold,omitempty"`
	MinSilenceDurationMS int     `json:"min_silence_duration_ms,omitempty"`
	TimestampRateHz      int     `json:"timestamp_rate_hz"`
}

type Result struct {
	SchemaVersion   string                  `json:"schema_version"`
	Benchmark       string                  `json:"benchmark"`
	Revision        string                  `json:"revision"`
	DatasetRevision string                  `json:"dataset_revision"`
	Status          string                  `json:"status"`
	CompletedAt     time.Time               `json:"completed_at"`
	Provider        string                  `json:"provider"`
	Sample          Sample                  `json:"sample"`
	OutputWAV       string                  `json:"output_wav"`
	OutputSHA256    string                  `json:"output_sha256"`
	OutputSegments  []Segment               `json:"output_segments,omitempty"`
	OutputVAD       OutputVAD               `json:"output_vad"`
	Session         livebench.SessionResult `json:"session"`
}

func ResultPaths(outputRoot string, sample Sample) (string, string) {
	directory := filepath.Join(outputRoot, sample.Cell)
	return filepath.Join(directory, "audio", sample.ID+".wav"), filepath.Join(directory, "results", sample.ID+".json")
}

// RunSample streams one released conversation at wall-clock speed through a
// standard Realtime adapter. Output chunks are aligned to their observed
// arrival clock and cropped at the collection boundary, matching live
// playback. Silero segmentation is a separate, explicit finalization phase.
func RunSample(ctx context.Context, adapter livebench.Adapter, sample Sample, outputRoot, provider string) (Result, error) {
	if adapter == nil {
		return Result{}, errors.New("FD-Bench requires a Realtime adapter")
	}
	input, err := livebench.ReadWAV(sample.InputPath)
	if err != nil {
		return Result{}, fmt.Errorf("read FD-Bench sample: %w", err)
	}
	session, err := adapter.Run(ctx, input)
	if err != nil {
		return Result{}, err
	}
	aligned := livebench.AlignChunks(session.Chunks, session.Descriptor.OutputSampleRate)
	collectionDuration := time.Duration(math.Round(session.ElapsedMS * float64(time.Millisecond)))
	if collectionDuration < input.Duration() {
		collectionDuration = input.Duration()
	}
	aligned = livebench.FitDuration(aligned, collectionDuration)
	outputPath, resultPath := ResultPaths(outputRoot, sample)
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create FD-Bench audio directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		return Result{}, fmt.Errorf("create FD-Bench result directory: %w", err)
	}
	outputHash, err := livebench.WriteWAV(outputPath, aligned)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		SchemaVersion: ResultSchemaVersion, Benchmark: BenchmarkName, Revision: BenchmarkRevision,
		DatasetRevision: DatasetRevision, Status: "completed", CompletedAt: time.Now().UTC(), Provider: provider,
		Sample: sample, OutputWAV: outputPath, OutputSHA256: outputHash,
		OutputVAD: OutputVAD{Status: "pending", TimestampRateHz: TimestampRateHz}, Session: session,
	}
	if err := writeJSONAtomic(resultPath, result); err != nil {
		return Result{}, err
	}
	return result, nil
}

func LoadCompleted(sample Sample, outputRoot, provider, model string) (Result, bool, error) {
	outputPath, resultPath := ResultPaths(outputRoot, sample)
	data, err := os.ReadFile(resultPath)
	if errors.Is(err, os.ErrNotExist) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read prior FD-Bench result: %w", err)
	}
	var result Result
	if err := json.Unmarshal(data, &result); err != nil {
		return Result{}, false, fmt.Errorf("decode prior FD-Bench result: %w", err)
	}
	if result.SchemaVersion != ResultSchemaVersion || result.Benchmark != BenchmarkName ||
		result.Revision != BenchmarkRevision || result.DatasetRevision != DatasetRevision || result.Status != "completed" ||
		result.Provider != provider || result.Sample.Cell != sample.Cell || result.Sample.Conversation != sample.Conversation ||
		result.Sample.InputSHA256 != sample.InputSHA256 || result.Sample.TimestampSHA256 != sample.TimestampSHA256 ||
		result.Session.Descriptor.Model != model {
		return Result{}, false, fmt.Errorf("prior FD-Bench result %s does not match the requested immutable run", resultPath)
	}
	outputHash, err := livebench.HashFile(outputPath)
	if err != nil {
		return Result{}, false, fmt.Errorf("verify prior FD-Bench output: %w", err)
	}
	if outputHash != result.OutputSHA256 {
		return Result{}, false, fmt.Errorf("prior FD-Bench output hash mismatch for %s", outputPath)
	}
	return result, true, nil
}

func Select(samples []Sample, cells []string, limit int) []Sample {
	allowed := make(map[string]struct{}, len(cells))
	for _, cell := range cells {
		if cell = strings.TrimSpace(cell); cell != "" {
			allowed[cell] = struct{}{}
		}
	}
	selected := make([]Sample, 0, len(samples))
	for _, sample := range samples {
		if len(allowed) != 0 {
			if _, ok := allowed[sample.Cell]; !ok {
				continue
			}
		}
		selected = append(selected, sample)
		if limit > 0 && len(selected) == limit {
			break
		}
	}
	return selected
}

func writeJSONAtomic(filename string, value any) error {
	temporary, err := os.CreateTemp(filepath.Dir(filename), ".fdbench-*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary FD-Bench result: %w", err)
	}
	name := temporary.Name()
	defer os.Remove(name)
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("encode FD-Bench result: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync FD-Bench result: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close FD-Bench result: %w", err)
	}
	if err := os.Rename(name, filename); err != nil {
		return fmt.Errorf("publish FD-Bench result: %w", err)
	}
	return nil
}

func durationMS(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}
