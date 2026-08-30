package tauvoice

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/review/candidate"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	maximumTauSimulationBytes = 64 << 20
	maximumTauAudioBytes      = 128 << 20
)

type tauCandidateContext struct {
	Harness              string    `json:"harness"`
	HarnessRevision      string    `json:"harness_revision"`
	Domain               string    `json:"domain"`
	TaskID               string    `json:"task_id"`
	TrialIndex           int       `json:"trial_index"`
	Condition            Condition `json:"speech_condition"`
	CadenceSeconds       float64   `json:"cadence_seconds"`
	AgentModel           string    `json:"agent_model"`
	UserModel            string    `json:"user_model"`
	SynthesisProvider    string    `json:"synthesis_provider"`
	SynthesisModel       string    `json:"synthesis_model"`
	SynthesisVoice       string    `json:"synthesis_voice"`
	SimulationSHA256     string    `json:"simulation_sha256,omitempty"`
	SimulationBytes      int       `json:"simulation_bytes,omitempty"`
	DeterministicScoring string    `json:"deterministic_scoring"`
}

func (config *Config) retainCandidateOutcome(
	ctx context.Context, lifecycle *candidate.Lifecycle, domain, runName string,
	outcome bench.TaskOutcome,
) error {
	if ctx == nil || lifecycle == nil {
		return errors.New("retain tau-Voice candidate outcome: missing context or lifecycle")
	}
	simulationID := outcome.Notes["simulation_id"]
	taskID := outcome.Notes["task_id"]
	var identityErr error
	if simulationID == "" || strings.TrimSpace(simulationID) != simulationID {
		identityErr = errors.Join(identityErr, errors.New("tau-Voice outcome has no canonical upstream simulation ID"))
	}
	if taskID == "" || strings.TrimSpace(taskID) != taskID {
		identityErr = errors.Join(identityErr, errors.New("tau-Voice outcome has no canonical upstream task ID"))
	}
	trialIndex, trialErr := strconv.Atoi(outcome.Notes["trial_index"])
	if trialErr != nil || trialIndex < 0 {
		trialErr = errors.New("tau-Voice outcome has an invalid upstream trial index")
		trialIndex = 0
	}
	saveTo := config.simulationDir(runName)
	var simulation []byte
	var simulationErr error
	if identityErr == nil {
		simulation, simulationErr = readTauArtifact(
			saveTo, maximumTauSimulationBytes, "simulations", simulationID+".json",
		)
	} else {
		simulationErr = identityErr
	}
	simulationDigest := ""
	if simulationErr == nil {
		if err := validateTauSimulation(simulation); err != nil {
			simulationErr = err
		} else {
			digest := sha256.Sum256(simulation)
			simulationDigest = "sha256:" + hex.EncodeToString(digest[:])
		}
	}
	attempt, err := lifecycle.BeginExternal(outcome.ID, trialIndex+1, tauCandidateContext{
		Harness: "tau2-bench", HarnessRevision: PinnedRevision,
		Domain: domain, TaskID: taskID, TrialIndex: trialIndex,
		Condition: config.Condition, CadenceSeconds: config.Cadence,
		AgentModel: config.Model, UserModel: config.UserModel,
		SynthesisProvider: config.SynthesisProvider, SynthesisModel: config.SynthesisModel,
		SynthesisVoice:   config.SynthesisVoice,
		SimulationSHA256: simulationDigest, SimulationBytes: len(simulation),
		DeterministicScoring: "tau2 database and communicated-information reward; pass when reward is at least one",
	})
	if err != nil {
		return err
	}
	if trialErr != nil {
		_ = attempt.RecordFailure("read upstream trial identity", trialErr)
	}
	if identityErr != nil {
		_ = attempt.RecordFailure("read upstream artifact identity", identityErr)
	}
	if simulationErr != nil {
		_ = attempt.RecordFailure("read upstream simulation", simulationErr)
	}
	var artifactErr error
	if simulationErr == nil {
		artifactErr = attempt.CaptureArtifact(candidate.CapturedArtifact{
			Name: "simulation.json", Kind: "trace", Role: "upstream_simulation",
			ContentType: "application/json", Bytes: simulation,
		})
	}
	var audio []byte
	var audioErr error
	if identityErr == nil {
		audio, audioErr = readTauArtifact(
			saveTo, maximumTauAudioBytes,
			"artifacts", "task_"+taskID, "sim_"+simulationID, "audio", "both.wav",
		)
	} else {
		audioErr = identityErr
	}
	if audioErr != nil {
		_ = attempt.RecordFailure("read upstream conversation audio", audioErr)
	} else if err := attempt.CaptureMedia(candidate.CapturedMedia{
		Name: "conversation.stereo.wav", Kind: "audio",
		Role: "time_aligned_user_left_agent_right", MediaType: "audio/wav", Bytes: audio,
	}); err != nil {
		audioErr = err
	}
	transcript := bench.Transcript{
		PlaybackMS: outcome.Metrics["simulation_duration_ms"],
		Execution:  outcome.Execution, ExecutionError: outcome.ExecutionError,
	}
	return errors.Join(
		identityErr, trialErr, simulationErr, artifactErr, audioErr,
		attempt.Complete(outcome, transcript),
	)
}

func readTauArtifact(rootPath string, maximum int64, components ...string) ([]byte, error) {
	if maximum <= 0 || maximum > maximumTauAudioBytes {
		return nil, errors.New("tau-Voice artifact byte bound is invalid")
	}
	if len(components) == 0 {
		return nil, errors.New("tau-Voice artifact path is empty")
	}
	for _, component := range components {
		if !safeTauArtifactComponent(component) {
			return nil, errors.New("tau-Voice artifact identity is unsafe")
		}
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, fmt.Errorf("open tau-Voice artifact root: %w", err)
	}
	defer root.Close()
	relative := filepath.Join(components...)
	identities := make([]os.FileInfo, len(components))
	for index := range components {
		prefix := filepath.Join(components[:index+1]...)
		info, err := root.Lstat(prefix)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("tau-Voice artifact path is missing or contains a symlink")
		}
		if index < len(components)-1 && !info.IsDir() {
			return nil, errors.New("tau-Voice artifact path has a non-directory ancestor")
		}
		identities[index] = info
	}
	file, err := root.Open(relative)
	if err != nil {
		return nil, fmt.Errorf("open tau-Voice artifact: %w", err)
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || before.Size() <= 0 || before.Size() > maximum {
		return nil, errors.New("tau-Voice artifact is not a bounded regular file")
	}
	if !os.SameFile(identities[len(identities)-1], before) {
		return nil, errors.New("tau-Voice artifact changed before it was opened")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) != before.Size() || int64(len(payload)) > maximum {
		return nil, errors.New("read complete bounded tau-Voice artifact")
	}
	after, err := file.Stat()
	if err != nil || after.Size() != before.Size() || !os.SameFile(before, after) {
		return nil, errors.New("tau-Voice artifact changed while it was read")
	}
	for index := range components {
		prefix := filepath.Join(components[:index+1]...)
		current, err := root.Lstat(prefix)
		if err != nil || current.Mode()&os.ModeSymlink != 0 ||
			!os.SameFile(identities[index], current) {
			return nil, errors.New("tau-Voice artifact path changed while it was read")
		}
	}
	return payload, nil
}

func validateTauSimulation(source []byte) error {
	if err := strictjson.ValidateWithLimits(source, strictjson.Limits{
		MaxInputBytes: maximumTauSimulationBytes, MaxDepth: 128, MaxTokens: 5_000_000,
		MaxObjectMembers: 1_000_000, MaxArrayElements: 2_000_000,
		MaxKeyBytes: 64 << 10, MaxTotalKeyBytes: maximumTauSimulationBytes,
		MaxWorkBytes: 256 << 20,
	}); err != nil {
		return fmt.Errorf("validate pinned tau2 simulation JSON: %w", err)
	}
	trimmed := bytes.TrimSpace(source)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return errors.New("pinned tau2 simulation JSON is not an object")
	}
	return nil
}

func safeTauArtifactComponent(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) ||
		strings.TrimSpace(value) != value || value == "." || value == ".." ||
		filepath.IsAbs(value) || filepath.VolumeName(value) != "" ||
		filepath.Base(value) != value || strings.ContainsAny(value, `/\`) {
		return false
	}
	for _, symbol := range value {
		if unicode.IsControl(symbol) {
			return false
		}
	}
	return true
}
