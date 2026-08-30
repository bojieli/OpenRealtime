package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	archbench "github.com/bojieli/OpenRealtime/bench/architecture"
	"github.com/bojieli/OpenRealtime/bench/scenario"
	"github.com/bojieli/OpenRealtime/bench/scenario/graphnative"
	launchprofile "github.com/bojieli/OpenRealtime/graph/launch/profile"
)

const (
	scenarioGraphMediaFormat        = "openrealtime.scenario.graphnative.media"
	scenarioGraphMediaFormatVersion = 1
	maximumScenarioGraphMediaBytes  = 4 << 20
)

type scenarioGraphSelection struct {
	Contract graphnative.Contract
	Profile  launchprofile.Document
}

type scenarioGraphAttempt struct {
	Key    graphnative.AttemptKey
	Result scenario.Result
	Err    error
}

type scenarioGraphOutcome struct {
	Checklist graphnative.Checklist
	Attempts  []scenarioGraphAttempt
}

type scenarioGraphMediaArtifact struct {
	Path      string `json:"path"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
	MediaType string `json:"media_type"`
	Role      string `json:"role"`
}

type scenarioGraphSubmittedArtifact struct {
	Receipt graphnative.SubmittedInputReceipt `json:"receipt"`
	Media   scenarioGraphMediaArtifact        `json:"media"`
}

type scenarioGraphMediaManifest struct {
	Format        string                           `json:"format"`
	FormatVersion int                              `json:"format_version"`
	Key           graphnative.AttemptKey           `json:"key"`
	Audio         scenarioGraphMediaArtifact       `json:"audio"`
	Submitted     []scenarioGraphSubmittedArtifact `json:"submitted_inputs,omitempty"`
}

// scenarioGraphReviewBundle is the CLI-selected local evidence plug-in. The
// graph-native harness sees only its Retain, Verify, and Sink callbacks; it
// does not know about paths, WAVs, Markdown, or this concrete implementation.
type scenarioGraphReviewBundle struct {
	mu       sync.Mutex
	review   *scenario.ReviewRun
	root     *os.Root
	closed   bool
	closeErr error
}

func prepareScenarioGraphSelection(
	path string,
	cell archbench.Cell,
	requirement bench.ExecutionRequirement,
	repetitions int,
) (scenarioGraphSelection, error) {
	if strings.TrimSpace(path) == "" {
		return scenarioGraphSelection{}, errors.New(
			"graph-native scenario execution requires -launch-profile",
		)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return scenarioGraphSelection{}, fmt.Errorf("read scenario launch profile: %w", err)
	}
	profile, err := launchprofile.ParseYAML(path, payload)
	if err != nil {
		return scenarioGraphSelection{}, fmt.Errorf("parse scenario launch profile: %w", err)
	}
	contract, err := graphnative.BuildContract()
	if err != nil {
		return scenarioGraphSelection{}, err
	}
	if profile.Adapter.ProfileName != cell.Architecture.RuntimeBinding {
		return scenarioGraphSelection{}, fmt.Errorf(
			"scenario launch profile adapter %q differs from architecture binding %q",
			profile.Adapter.ProfileName, cell.Architecture.RuntimeBinding,
		)
	}
	selection := graphnative.ChecklistConfig{
		Contract: contract, Profile: profile,
		AdapterProfileFingerprint: cell.Architecture.Profile,
		ExecutionRequirement:      requirement,
		Repetitions:               repetitions,
		RequireMedia:              true,
		Executor: func(
			context.Context, graphnative.AttemptKey, scenario.Scenario,
		) (graphnative.AttemptObservation, error) {
			return graphnative.AttemptObservation{}, errors.New(
				"scenario selection validator must not invoke the executor",
			)
		},
		VerifyMedia: func(
			context.Context, graphnative.AttemptKey, graphnative.CaseRequirement,
			graphnative.MediaReference,
		) (graphnative.VerifiedMedia, error) {
			return graphnative.VerifiedMedia{}, errors.New(
				"scenario selection validator must not invoke the media verifier",
			)
		},
	}
	if err := graphnative.ValidateChecklistConfig(selection); err != nil {
		return scenarioGraphSelection{}, err
	}
	return scenarioGraphSelection{Contract: contract, Profile: profile}, nil
}

func newScenarioGraphReviewBundle(
	directory string,
	repetitions int,
	requirement bench.ExecutionRequirement,
	secrets []string,
) (*scenarioGraphReviewBundle, error) {
	review, err := scenario.NewReviewRun(scenario.ReviewOptions{
		Directory: directory, Scenarios: scenario.Suite(), Repeats: repetitions,
		ExecutionRequirement: requirement, Secrets: slices.Clone(secrets),
	})
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(review.Directory())
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("open graph-native scenario review bundle: %w", err),
			review.Close(),
		)
	}
	return &scenarioGraphReviewBundle{review: review, root: root}, nil
}

func (bundle *scenarioGraphReviewBundle) Directory() string {
	if bundle == nil || bundle.review == nil {
		return ""
	}
	return bundle.review.Directory()
}

func (bundle *scenarioGraphReviewBundle) Close() error {
	if bundle == nil {
		return errors.New("graph-native scenario review bundle is nil")
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.closed {
		return bundle.closeErr
	}
	bundle.closed = true
	var result error
	if bundle.review != nil {
		result = errors.Join(result, bundle.review.Close())
	}
	if bundle.root != nil {
		result = errors.Join(result, bundle.root.Close())
		bundle.root = nil
	}
	bundle.closeErr = result
	return bundle.closeErr
}

func (bundle *scenarioGraphReviewBundle) Retain(
	ctx context.Context, capture graphnative.AttemptCapture,
) (graphnative.MediaReference, error) {
	if ctx == nil {
		return graphnative.MediaReference{}, errors.New("retain scenario media: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return graphnative.MediaReference{}, cause
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.closed || bundle.root == nil || bundle.review == nil {
		return graphnative.MediaReference{}, errors.New("scenario review bundle is closed")
	}
	var runErr error
	if !capture.RunSucceeded {
		runErr = errors.New("scenario attempt ended with an infrastructure error; inspect the checklist")
	}
	if err := bundle.review.Record(
		capture.Key.CaseName, capture.Key.Trial, capture.Audio, capture.Result, runErr,
	); err != nil {
		return graphnative.MediaReference{}, err
	}
	audio, err := bundle.findAttemptAudio(capture.Key)
	if err != nil {
		return graphnative.MediaReference{}, err
	}
	manifest := scenarioGraphMediaManifest{
		Format: scenarioGraphMediaFormat, FormatVersion: scenarioGraphMediaFormatVersion,
		Key: capture.Key, Audio: audio,
	}
	for index, input := range capture.Submitted {
		if cause := context.Cause(ctx); cause != nil {
			return graphnative.MediaReference{}, cause
		}
		digest := scenarioGraphDigest(input.Data)
		if digest != input.Receipt.SHA256 || int64(len(input.Data)) != input.Receipt.SizeBytes ||
			http.DetectContentType(input.Data) != input.Receipt.MediaType {
			return graphnative.MediaReference{}, fmt.Errorf(
				"scenario submitted input %d differs from its transport receipt", index+1,
			)
		}
		extension, err := scenarioGraphImageExtension(input.Receipt.MediaType)
		if err != nil {
			return graphnative.MediaReference{}, err
		}
		name := fmt.Sprintf("%02d-trial-%03d-submitted-%02d%s",
			capture.Key.CaseOrdinal, capture.Key.Trial, index+1, extension)
		if err := writeScenarioGraphFile(bundle.root, name, input.Data); err != nil {
			return graphnative.MediaReference{}, fmt.Errorf("retain submitted scenario input: %w", err)
		}
		manifest.Submitted = append(manifest.Submitted, scenarioGraphSubmittedArtifact{
			Receipt: input.Receipt,
			Media: scenarioGraphMediaArtifact{
				Path: name, SHA256: digest, SizeBytes: int64(len(input.Data)),
				MediaType: input.Receipt.MediaType, Role: "submitted_visual_input",
			},
		})
	}
	payload, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return graphnative.MediaReference{}, fmt.Errorf("encode scenario media manifest: %w", err)
	}
	payload = append(payload, '\n')
	handle := scenarioGraphAttemptFilename(capture.Key, "media.json")
	if err := writeScenarioGraphFile(bundle.root, handle, payload); err != nil {
		return graphnative.MediaReference{}, fmt.Errorf("retain scenario media manifest: %w", err)
	}
	return graphnative.MediaReference{
		Handle: handle, ManifestSHA256: scenarioGraphDigest(payload),
		Submitted: scenarioGraphSubmittedReceipts(manifest.Submitted),
	}, nil
}

func (bundle *scenarioGraphReviewBundle) Verify(
	ctx context.Context,
	key graphnative.AttemptKey,
	requirement graphnative.CaseRequirement,
	reference graphnative.MediaReference,
) (graphnative.VerifiedMedia, error) {
	if ctx == nil {
		return graphnative.VerifiedMedia{}, errors.New("verify scenario media: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return graphnative.VerifiedMedia{}, cause
	}
	bundle.mu.Lock()
	defer bundle.mu.Unlock()
	if bundle.closed || bundle.root == nil {
		return graphnative.VerifiedMedia{}, errors.New("scenario review bundle is closed")
	}
	if requirement.Name != key.CaseName ||
		reference.Handle != scenarioGraphAttemptFilename(key, "media.json") {
		return graphnative.VerifiedMedia{}, errors.New("scenario media reference has a drifted attempt identity")
	}
	payload, err := bundle.root.ReadFile(reference.Handle)
	if err != nil {
		return graphnative.VerifiedMedia{}, fmt.Errorf("read scenario media manifest: %w", err)
	}
	if len(payload) == 0 || len(payload) > maximumScenarioGraphMediaBytes ||
		scenarioGraphDigest(payload) != reference.ManifestSHA256 {
		return graphnative.VerifiedMedia{}, errors.New("scenario media manifest digest or size is invalid")
	}
	var manifest scenarioGraphMediaManifest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return graphnative.VerifiedMedia{}, fmt.Errorf("decode scenario media manifest: %w", err)
	}
	if err := requireScenarioGraphJSONEOF(decoder); err != nil {
		return graphnative.VerifiedMedia{}, err
	}
	if manifest.Format != scenarioGraphMediaFormat ||
		manifest.FormatVersion != scenarioGraphMediaFormatVersion || manifest.Key != key {
		return graphnative.VerifiedMedia{}, errors.New("scenario media manifest identity is invalid")
	}
	if err := bundle.verifyArtifact(manifest.Audio, "audio/wav", "stereo_room_and_agent"); err != nil {
		return graphnative.VerifiedMedia{}, err
	}
	audioPayload, err := bundle.root.ReadFile(manifest.Audio.Path)
	if err != nil {
		return graphnative.VerifiedMedia{}, err
	}
	if err := validateScenarioGraphStereoWAV(audioPayload); err != nil {
		return graphnative.VerifiedMedia{}, err
	}
	receipts := make([]graphnative.SubmittedInputReceipt, 0, len(manifest.Submitted))
	for _, input := range manifest.Submitted {
		if input.Media.SHA256 != input.Receipt.SHA256 ||
			input.Media.SizeBytes != input.Receipt.SizeBytes ||
			input.Media.MediaType != input.Receipt.MediaType {
			return graphnative.VerifiedMedia{}, errors.New(
				"scenario submitted media differs from its transport receipt",
			)
		}
		if err := bundle.verifyArtifact(
			input.Media, input.Receipt.MediaType, "submitted_visual_input",
		); err != nil {
			return graphnative.VerifiedMedia{}, err
		}
		receipts = append(receipts, input.Receipt)
	}
	if !reflect.DeepEqual(receipts, reference.Submitted) {
		return graphnative.VerifiedMedia{}, errors.New(
			"scenario media manifest changed submitted-input receipts",
		)
	}
	return graphnative.VerifiedMedia{
		Handle: reference.Handle, ManifestSHA256: reference.ManifestSHA256,
		Audio: true, Submitted: receipts,
	}, nil
}

func (bundle *scenarioGraphReviewBundle) Sink() graphnative.ChecklistSink {
	return graphnative.ChecklistSink{
		Attempt: func(ctx context.Context, record graphnative.AttemptRecord) error {
			if ctx == nil {
				return errors.New("retain scenario checklist attempt: nil context")
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			payload, err := json.MarshalIndent(record, "", "  ")
			if err != nil {
				return fmt.Errorf("encode scenario checklist attempt: %w", err)
			}
			payload = append(payload, '\n')
			bundle.mu.Lock()
			defer bundle.mu.Unlock()
			if bundle.closed || bundle.root == nil {
				return errors.New("scenario review bundle is closed")
			}
			return writeScenarioGraphFile(
				bundle.root, scenarioGraphAttemptFilename(record.Key, "checklist.json"), payload,
			)
		},
		Finalize: func(ctx context.Context, checklist graphnative.Checklist) error {
			if ctx == nil {
				return errors.New("retain scenario checklist: nil context")
			}
			if cause := context.Cause(ctx); cause != nil {
				return cause
			}
			payload, err := graphnative.MarshalChecklist(checklist)
			if err != nil {
				return err
			}
			bundle.mu.Lock()
			defer bundle.mu.Unlock()
			if bundle.closed || bundle.root == nil {
				return errors.New("scenario review bundle is closed")
			}
			markdown, err := bundle.renderChecklist(checklist)
			if err != nil {
				return err
			}
			if err := writeScenarioGraphFile(bundle.root, "CHECKLIST.md", []byte(markdown)); err != nil {
				return fmt.Errorf("retain scenario checklist review: %w", err)
			}
			return writeScenarioGraphFile(bundle.root, "checklist.json", payload)
		},
	}
}

func (bundle *scenarioGraphReviewBundle) renderChecklist(
	checklist graphnative.Checklist,
) (string, error) {
	var output strings.Builder
	output.WriteString("# Graph-native scenario checklist\n\n")
	fmt.Fprintf(&output, "Complete: %t (%d/%d attempts).\n\n",
		checklist.Complete, checklist.Executed, checklist.Expected)
	fmt.Fprintf(&output, "Reportable: %t (%d/%d attempts independently attested and media-verified).\n\n",
		checklist.Reportable, checklist.ReportableAttempts, checklist.Expected)
	fmt.Fprintf(&output, "All behavioral checks passed: %t (%d/%d attempts passed).\n\n",
		checklist.Passed, checklist.PassedAttempts, checklist.Expected)
	fmt.Fprintf(&output, "Contract: `%s`; launch profile: `%s`; plan: `%s`; adapter profile: `%s`.\n\n",
		checklist.ContractFingerprint, checklist.LaunchProfileFingerprint,
		checklist.Plan.PlanFingerprint, checklist.AdapterProfileFingerprint)
	if checklist.Repetitions < graphnative.MinimumReportableRepetitions {
		fmt.Fprintf(&output,
			"This diagnostic run has %d repetition(s) per case; publication requires at least %d.\n\n",
			checklist.Repetitions, graphnative.MinimumReportableRepetitions)
	}
	for _, item := range checklist.Cases {
		fmt.Fprintf(&output, "## %02d. %s\n\n", item.Ordinal, scenarioGraphMarkdown(item.Name))
		for _, attempt := range checklist.Attempts {
			if attempt.Key.CaseOrdinal != item.Ordinal {
				continue
			}
			status := strings.ToUpper(attempt.Behavior)
			if len(attempt.Infrastructure) > 0 {
				status = "INFRASTRUCTURE FAILURE"
			}
			fmt.Fprintf(&output, "### Trial %d — %s\n\n", attempt.Key.Trial, status)
			fmt.Fprintf(&output, "- Attempt fingerprint: `%s`\n", attempt.Fingerprint)
			fmt.Fprintf(&output, "- Reportable attempt: %t\n", attempt.Reportable)
			if attempt.Media != nil {
				manifest, err := bundle.readMediaManifest(attempt.Media.Handle)
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&output, "- Audio: [%s](%s), `%s`\n",
					scenarioGraphMarkdown(manifest.Audio.Path), manifest.Audio.Path, manifest.Audio.SHA256)
				fmt.Fprintf(&output, "- Media manifest: [%s](%s), `%s`\n",
					scenarioGraphMarkdown(attempt.Media.Handle), attempt.Media.Handle,
					attempt.Media.ManifestSHA256)
				for _, input := range manifest.Submitted {
					fmt.Fprintf(&output, "- Submitted visual input: [%s](%s), cue %d ms, `%s`\n",
						scenarioGraphMarkdown(input.Media.Path), input.Media.Path,
						input.Receipt.CueMS, input.Receipt.SHA256)
				}
			}
			for _, failure := range attempt.Failures {
				fmt.Fprintf(&output, "- Failed behavioral check: %s\n", scenarioGraphMarkdown(failure))
			}
			for _, failure := range attempt.Infrastructure {
				fmt.Fprintf(&output, "- Infrastructure `%s`", failure.Code)
				if failure.Detail != "" {
					fmt.Fprintf(&output, ": %s", scenarioGraphMarkdown(failure.Detail))
				}
				output.WriteByte('\n')
			}
			output.WriteByte('\n')
		}
	}
	return output.String(), nil
}

func (bundle *scenarioGraphReviewBundle) readMediaManifest(
	handle string,
) (scenarioGraphMediaManifest, error) {
	payload, err := bundle.root.ReadFile(handle)
	if err != nil {
		return scenarioGraphMediaManifest{}, fmt.Errorf("read scenario media manifest for review: %w", err)
	}
	var manifest scenarioGraphMediaManifest
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return scenarioGraphMediaManifest{}, fmt.Errorf("decode scenario media manifest for review: %w", err)
	}
	if err := requireScenarioGraphJSONEOF(decoder); err != nil {
		return scenarioGraphMediaManifest{}, err
	}
	return manifest, nil
}

func executeScenarioGraphChecklist(
	ctx context.Context,
	selection scenarioGraphSelection,
	requirement bench.ExecutionRequirement,
	adapterProfileFingerprint string,
	repetitions int,
	timeout time.Duration,
	directory string,
	voice scenario.Voice,
	session bench.SessionConfig,
	newExecutor func(graphnative.LiveExecutorConfig) (graphnative.AttemptExecutor, error),
) (outcome scenarioGraphOutcome, returnErr error) {
	if ctx == nil {
		return outcome, errors.New("run graph-native scenarios: nil context")
	}
	if newExecutor == nil {
		return outcome, errors.New("run graph-native scenarios: nil executor factory")
	}
	bundle, err := newScenarioGraphReviewBundle(
		directory, repetitions, requirement, []string{session.Token},
	)
	if err != nil {
		return outcome, err
	}
	defer func() { returnErr = errors.Join(returnErr, bundle.Close()) }()
	executor, err := newExecutor(graphnative.LiveExecutorConfig{
		Voice: voice, Session: session, Retain: bundle.Retain,
	})
	if err != nil {
		return outcome, err
	}
	observed := make([]scenarioGraphAttempt, 0, len(selection.Contract.Cases)*repetitions)
	wrapped := func(
		attemptContext context.Context, key graphnative.AttemptKey, item scenario.Scenario,
	) (graphnative.AttemptObservation, error) {
		bounded, cancel := context.WithTimeout(attemptContext, timeout)
		defer cancel()
		observation, runErr := executor(bounded, key, item)
		observed = append(observed, scenarioGraphAttempt{
			Key: key, Result: observation.Result, Err: runErr,
		})
		return observation, runErr
	}
	checklist, err := graphnative.RunChecklist(ctx, graphnative.ChecklistConfig{
		Contract: selection.Contract, Profile: selection.Profile,
		AdapterProfileFingerprint: adapterProfileFingerprint,
		ExecutionRequirement:      requirement,
		Repetitions:               repetitions,
		RequireMedia:              true,
		Executor:                  wrapped,
		VerifyMedia:               bundle.Verify,
		SanitizeError: func(string) string {
			return "scenario attempt failed; inspect the create-only review bundle"
		},
		Sink: bundle.Sink(),
	})
	outcome.Checklist = checklist
	outcome.Attempts = observed
	return outcome, err
}

func appendScenarioGraphArchitectureAttempts(
	result *archbench.Result, attempts []scenarioGraphAttempt,
) error {
	if result == nil {
		return nil
	}
	for _, attempt := range attempts {
		result.Measurement.Tasks = append(result.Measurement.Tasks,
			scenarioTask(attempt.Key.TaskID, attempt.Result, attempt.Err))
		if attempt.Result.Transcript.Runtime != nil {
			result.Observed = append(result.Observed, archbench.Observation{
				TaskID: attempt.Key.TaskID, Status: *attempt.Result.Transcript.Runtime,
			})
		}
		record, err := json.Marshal(attempt.Result)
		if err != nil {
			return err
		}
		result.Records = append(result.Records, record)
	}
	return nil
}

func reportScenarioGraphOutcome(
	output io.Writer, outcome scenarioGraphOutcome, repetitions int,
) {
	byCase := make(map[string][]scenario.Result, len(scenario.Suite()))
	for _, attempt := range outcome.Attempts {
		if attempt.Err != nil {
			fmt.Fprintf(output, "  ERR  %-28s %v\n", attempt.Key.CaseName, attempt.Err)
		}
		byCase[attempt.Key.CaseName] = append(byCase[attempt.Key.CaseName], attempt.Result)
	}
	for _, item := range scenario.Suite() {
		attempts := byCase[item.Name]
		if len(attempts) == 0 {
			attempts = make([]scenario.Result, 0, repetitions)
		}
		reportScenario(output, item, attempts)
	}
	fmt.Fprintf(output, "\n  scenarios %d/%d\n",
		outcome.Checklist.PassedAttempts, outcome.Checklist.Executed)
	fmt.Fprintf(output, "  checklist   %d/%d reportable attempts\n",
		outcome.Checklist.ReportableAttempts, outcome.Checklist.Expected)
	if outcome.Checklist.Reportable {
		fmt.Fprintln(output, "  reportable graph-native checklist")
	} else {
		fmt.Fprintln(output, "  NOT REPORTABLE: graph-native checklist")
	}
}

func (bundle *scenarioGraphReviewBundle) findAttemptAudio(
	key graphnative.AttemptKey,
) (scenarioGraphMediaArtifact, error) {
	entries, err := fs.ReadDir(bundle.root.FS(), ".")
	if err != nil {
		return scenarioGraphMediaArtifact{}, fmt.Errorf("list scenario review audio: %w", err)
	}
	prefix := fmt.Sprintf("%02d-", key.CaseOrdinal)
	suffix := fmt.Sprintf("-trial-%02d.stereo.wav", key.Trial)
	var name string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) &&
			strings.HasSuffix(entry.Name(), suffix) {
			if name != "" {
				return scenarioGraphMediaArtifact{}, errors.New("scenario attempt has multiple review WAVs")
			}
			name = entry.Name()
		}
	}
	if name == "" {
		return scenarioGraphMediaArtifact{}, errors.New("scenario attempt review WAV is missing")
	}
	payload, err := bundle.root.ReadFile(name)
	if err != nil {
		return scenarioGraphMediaArtifact{}, fmt.Errorf("read scenario review WAV: %w", err)
	}
	if err := validateScenarioGraphStereoWAV(payload); err != nil {
		return scenarioGraphMediaArtifact{}, err
	}
	return scenarioGraphMediaArtifact{
		Path: name, SHA256: scenarioGraphDigest(payload), SizeBytes: int64(len(payload)),
		MediaType: "audio/wav", Role: "stereo_room_and_agent",
	}, nil
}

func (bundle *scenarioGraphReviewBundle) verifyArtifact(
	artifact scenarioGraphMediaArtifact, mediaType string, role string,
) error {
	if artifact.Path == "" || filepath.Base(artifact.Path) != artifact.Path ||
		artifact.MediaType != mediaType || artifact.Role != role ||
		artifact.SizeBytes < 1 {
		return errors.New("scenario media artifact identity is invalid")
	}
	info, err := bundle.root.Lstat(artifact.Path)
	if err != nil {
		return fmt.Errorf("inspect scenario media artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() != artifact.SizeBytes {
		return errors.New("scenario media artifact is not an exact regular file")
	}
	payload, err := bundle.root.ReadFile(artifact.Path)
	if err != nil {
		return fmt.Errorf("read scenario media artifact: %w", err)
	}
	if scenarioGraphDigest(payload) != artifact.SHA256 {
		return errors.New("scenario media artifact digest is invalid")
	}
	return nil
}

func scenarioGraphSubmittedReceipts(
	source []scenarioGraphSubmittedArtifact,
) []graphnative.SubmittedInputReceipt {
	result := make([]graphnative.SubmittedInputReceipt, len(source))
	for index := range source {
		result[index] = source[index].Receipt
	}
	return result
}

func scenarioGraphAttemptFilename(key graphnative.AttemptKey, suffix string) string {
	return fmt.Sprintf("%02d-trial-%03d.%s", key.CaseOrdinal, key.Trial, suffix)
}

func scenarioGraphImageExtension(mediaType string) (string, error) {
	switch mediaType {
	case "image/png":
		return ".png", nil
	case "image/jpeg":
		return ".jpg", nil
	case "image/webp":
		return ".webp", nil
	default:
		return "", fmt.Errorf("unsupported submitted scenario media type %q", mediaType)
	}
}

func scenarioGraphDigest(payload []byte) string {
	digest := sha256.Sum256(payload)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func scenarioGraphMarkdown(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, character := range []string{"`", "*", "_", "[", "]", "<", ">"} {
		value = strings.ReplaceAll(value, character, "\\"+character)
	}
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

func validateScenarioGraphStereoWAV(payload []byte) error {
	if len(payload) < 44 || string(payload[:4]) != "RIFF" || string(payload[8:12]) != "WAVE" ||
		string(payload[12:16]) != "fmt " || string(payload[36:40]) != "data" ||
		binary.LittleEndian.Uint32(payload[16:20]) != 16 ||
		binary.LittleEndian.Uint16(payload[20:22]) != 1 ||
		binary.LittleEndian.Uint16(payload[22:24]) != 2 ||
		binary.LittleEndian.Uint32(payload[24:28]) != 24_000 ||
		binary.LittleEndian.Uint16(payload[34:36]) != 16 ||
		int(binary.LittleEndian.Uint32(payload[40:44])) != len(payload)-44 ||
		int(binary.LittleEndian.Uint32(payload[4:8])) != len(payload)-8 {
		return errors.New("scenario review audio is not exact 24 kHz stereo PCM16 WAV")
	}
	return nil
}

func requireScenarioGraphJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return errors.New("decode scenario media manifest: trailing JSON value")
		}
		return fmt.Errorf("decode scenario media manifest trailing data: %w", err)
	}
	return nil
}

func writeScenarioGraphFile(root *os.Root, name string, payload []byte) error {
	if root == nil {
		return errors.New("scenario review directory is closed")
	}
	if filepath.Base(name) != name || name == "." || name == "" {
		return errors.New("scenario review filename is not contained")
	}
	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	written := false
	defer func() {
		_ = file.Close()
		if !written {
			_ = root.Remove(name)
		}
	}()
	if _, err := file.Write(payload); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	written = true
	return nil
}
