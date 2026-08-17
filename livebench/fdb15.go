package livebench

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
)

const (
	FullDuplexBenchRevision = "3e799c45a045256f47d5f1c9cda90157e2d2ec9e"
	FullDuplexBenchName     = "full-duplex-bench-v1.5"
)

var FDB15Scenarios = []string{
	"background_speech",
	"talking_to_other",
	"user_backchannel",
	"user_interruption",
}

type fdbMetadata struct {
	Timestamps []float64 `json:"timestamps"`
}

// DiscoverFDB15 recognizes both the upstream v1_5/<scenario>/<id> layout and
// a directory that directly contains one scenario's sample directories.
func DiscoverFDB15(root string) ([]Sample, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var samples []Sample
	for _, scenario := range FDB15Scenarios {
		candidates := []string{
			filepath.Join(root, "v1_5", scenario),
			filepath.Join(root, scenario),
		}
		if filepath.Base(root) == scenario {
			candidates = append(candidates, root)
		}
		var scenarioRoot string
		for _, candidate := range candidates {
			if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
				scenarioRoot = candidate
				break
			}
		}
		if scenarioRoot == "" {
			continue
		}
		entries, readErr := os.ReadDir(scenarioRoot)
		if readErr != nil {
			return nil, fmt.Errorf("read %s: %w", scenarioRoot, readErr)
		}
		for _, entry := range entries {
			if !entry.IsDir() || entry.Name()[0] == '.' {
				continue
			}
			sampleRoot := filepath.Join(scenarioRoot, entry.Name())
			input := filepath.Join(sampleRoot, "input.wav")
			clean := filepath.Join(sampleRoot, "clean_input.wav")
			metadata := filepath.Join(sampleRoot, "metadata.json")
			if !regularFile(input) || !regularFile(clean) || !regularFile(metadata) {
				continue
			}
			sample, sampleErr := loadFDB15Sample(scenario, entry.Name(), input, clean, metadata)
			if sampleErr != nil {
				return nil, sampleErr
			}
			samples = append(samples, sample)
		}
	}
	if len(samples) == 0 {
		return nil, errors.New("no Full-Duplex-Bench v1.5 samples found")
	}
	slices.SortFunc(samples, func(left, right Sample) int {
		if left.Scenario != right.Scenario {
			if left.Scenario < right.Scenario {
				return -1
			}
			return 1
		}
		if left.ID < right.ID {
			return -1
		}
		if left.ID > right.ID {
			return 1
		}
		return 0
	})
	return samples, nil
}

func loadFDB15Sample(scenario, id, input, clean, metadata string) (Sample, error) {
	data, err := os.ReadFile(metadata)
	if err != nil {
		return Sample{}, fmt.Errorf("read metadata for %s/%s: %w", scenario, id, err)
	}
	var annotation fdbMetadata
	if err := json.Unmarshal(data, &annotation); err != nil {
		return Sample{}, fmt.Errorf("parse metadata for %s/%s: %w", scenario, id, err)
	}
	if len(annotation.Timestamps) != 2 || annotation.Timestamps[0] < 0 || annotation.Timestamps[1] < annotation.Timestamps[0] {
		return Sample{}, fmt.Errorf("invalid overlap timestamps for %s/%s", scenario, id)
	}
	inputHash, err := HashFile(input)
	if err != nil {
		return Sample{}, err
	}
	cleanHash, err := HashFile(clean)
	if err != nil {
		return Sample{}, err
	}
	metadataHash, err := HashFile(metadata)
	if err != nil {
		return Sample{}, err
	}
	start, end := annotation.Timestamps[0], annotation.Timestamps[1]
	return Sample{
		Benchmark: FullDuplexBenchName, Revision: FullDuplexBenchRevision,
		Scenario: scenario, ID: id, InputPath: input, CleanInputPath: clean,
		MetadataPath: metadata, OverlapStartS: &start, OverlapEndS: &end,
		InputSHA256: inputHash, CleanSHA256: cleanHash, MetadataSHA256: metadataHash,
	}, nil
}

func regularFile(filename string) bool {
	info, err := os.Stat(filename)
	return err == nil && info.Mode().IsRegular()
}
