package scenario

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	reviewFormatVersion = 1
	reviewSampleRateHz  = uint32(24_000)
	reviewChannels      = uint16(2)
	maxReviewImageBytes = int64(64 << 20)
)

var (
	reviewBearerSecret = regexp.MustCompile(`(?i)(bearer[[:space:]]+)[^[:space:],;]+`)
	reviewNamedSecret  = regexp.MustCompile(`(?i)(token|api[_-]?key|secret|credential|authorization)(["']?[[:space:]]*[:=][[:space:]]*["']?)([^&[:space:],;"']+)`)
)

// ReviewOptions declares a create-only scenario review run. Directory is the
// final run directory, not a parent into which an implicit timestamped name is
// invented. That makes the evidence location explicit and lets automation
// reserve collision-free run identities itself.
type ReviewOptions struct {
	Directory            string
	Scenarios            []Scenario
	Repeats              int
	ExecutionRequirement bench.ExecutionRequirement
	// Secrets are exact in-memory values to redact from error and transcript
	// prose. They are never serialized into the review bundle.
	Secrets []string
}

// ReviewManifest is the machine-readable index written as manifest.json.
// Media is typed so a later video or screen-trace capture can coexist with the
// stereo audio and still images without changing attempt identity.
type ReviewManifest struct {
	Format               string                      `json:"format"`
	FormatVersion        int                         `json:"format_version"`
	Suite                string                      `json:"suite"`
	Complete             bool                        `json:"complete"`
	Reportable           bool                        `json:"reportable"`
	EvidencePolicy       string                      `json:"execution_evidence_policy"`
	ExecutionRequirement *bench.ExecutionRequirement `json:"execution_requirement,omitempty"`
	ReportabilityErrors  []string                    `json:"reportability_errors,omitempty"`
	Expected             int                         `json:"expected_attempts"`
	Missing              []ReviewMissing             `json:"missing,omitempty"`
	Cases                []ReviewCase                `json:"cases"`
	Attempts             []ReviewAttempt             `json:"attempts"`
}

// ReviewCase identifies one authored scenario and immutable input media.
type ReviewCase struct {
	Ordinal int           `json:"ordinal"`
	Name    string        `json:"name"`
	Inputs  []ReviewMedia `json:"inputs,omitempty"`
}

// ReviewMissing identifies an expected attempt not retained by the run.
type ReviewMissing struct {
	Scenario string `json:"scenario"`
	Trial    int    `json:"trial"`
}

// ReviewAttempt is one result and the media needed to inspect it.
type ReviewAttempt struct {
	Scenario           string                  `json:"scenario"`
	Trial              int                     `json:"trial"`
	Passed             bool                    `json:"passed"`
	Failures           []string                `json:"failures,omitempty"`
	RunError           string                  `json:"run_error,omitempty"`
	ProtocolError      string                  `json:"protocol_error,omitempty"`
	EvidenceError      string                  `json:"evidence_error,omitempty"`
	EvidenceStatus     string                  `json:"execution_evidence_status"`
	Evidence           *ReviewEvidenceIdentity `json:"execution_evidence,omitempty"`
	Reportable         bool                    `json:"reportable"`
	ReportabilityError string                  `json:"reportability_error,omitempty"`
	Inputs             []ReviewInputUse        `json:"inputs,omitempty"`
	Audio              ReviewMedia             `json:"audio"`
}

// ReviewEvidenceIdentity binds an attempt to live execution proof without
// duplicating the full architecture record or retaining inspection authority.
type ReviewEvidenceIdentity struct {
	Kind        bench.ExecutionKind `json:"kind"`
	Scope       string              `json:"scope,omitempty"`
	Fingerprint string              `json:"fingerprint"`
}

// ReviewInputUse records whether the session driver actually submitted an
// authored input. A copied still is not silently described as transmitted if
// the attempt failed before its scheduled cue.
type ReviewInputUse struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// ReviewMedia identifies a retained review artifact by content, format, and
// semantic role. Audio and image details are mutually exclusive today; future
// video capture can add a Video field while keeping the common identity.
type ReviewMedia struct {
	Kind      string           `json:"kind"`
	Role      string           `json:"role"`
	Path      string           `json:"path"`
	SHA256    string           `json:"sha256"`
	MediaType string           `json:"media_type"`
	CueMS     int              `json:"cue_ms,omitempty"`
	Note      string           `json:"note,omitempty"`
	Audio     *ReviewAudioSpec `json:"audio,omitempty"`
}

// ReviewAudioSpec makes channel meaning and exact duration explicit.
type ReviewAudioSpec struct {
	Container     string   `json:"container"`
	Encoding      string   `json:"encoding"`
	SampleRateHz  uint32   `json:"sample_rate_hz"`
	Channels      uint16   `json:"channels"`
	ChannelLayout []string `json:"channel_layout"`
	Frames        uint64   `json:"frames"`
	DurationMS    float64  `json:"duration_ms"`
}

type reviewCaseState struct {
	manifest ReviewCase
	item     Scenario
}

// ReviewRun owns one exclusive directory until Close writes its indexes.
type ReviewRun struct {
	directory   string
	root        *os.Root
	repeats     int
	secrets     []string
	requirement bench.ExecutionRequirement
	cases       []reviewCaseState
	byName      map[string]int
	attempts    map[string]ReviewAttempt
	results     map[string]Result
	closed      bool
	closeErr    error
}

type stagedReviewImage struct {
	media   ReviewMedia
	payload []byte
}

// NewReviewRun creates Directory exclusively after validating all authored
// still inputs. Existing paths, symlinked parents, missing parents, traversal,
// duplicate cases, and unreadable media are rejected before benchmark work.
func NewReviewRun(options ReviewOptions) (*ReviewRun, error) {
	if options.Repeats <= 0 {
		return nil, errors.New("review repeats must be positive")
	}
	if len(options.Scenarios) == 0 {
		return nil, errors.New("a review run needs at least one scenario")
	}
	if err := options.ExecutionRequirement.Validate(); err != nil {
		return nil, fmt.Errorf("review execution requirement: %w", err)
	}
	directory, err := validateNewReviewDirectory(options.Directory)
	if err != nil {
		return nil, err
	}
	if _, err := os.Lstat(directory); err == nil {
		return nil, fmt.Errorf("create review run %q exclusively: path already exists", options.Directory)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("inspect new review run %q: %w", options.Directory, err)
	}
	run := &ReviewRun{
		directory:   directory,
		repeats:     options.Repeats,
		requirement: options.ExecutionRequirement,
		byName:      make(map[string]int, len(options.Scenarios)),
		attempts:    make(map[string]ReviewAttempt, len(options.Scenarios)*options.Repeats),
		results:     make(map[string]Result, len(options.Scenarios)*options.Repeats),
	}
	for _, secret := range options.Secrets {
		if strings.TrimSpace(secret) != "" {
			run.secrets = append(run.secrets, secret)
		}
	}

	var staged []stagedReviewImage
	for index, item := range options.Scenarios {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return nil, errors.New("review scenario names must not be empty")
		}
		if name != item.Name {
			return nil, fmt.Errorf("review scenario name %q must not have surrounding whitespace", item.Name)
		}
		if _, duplicate := run.byName[name]; duplicate {
			return nil, fmt.Errorf("review scenario %q is duplicated", name)
		}
		run.byName[name] = index
		state := reviewCaseState{
			item:     item,
			manifest: ReviewCase{Ordinal: index + 1, Name: name},
		}
		for sightIndex, sight := range item.Sees {
			media, payload, err := stageReviewImage(index, sightIndex, item, sight)
			if err != nil {
				return nil, err
			}
			state.manifest.Inputs = append(state.manifest.Inputs, media)
			staged = append(staged, stagedReviewImage{media: media, payload: payload})
		}
		run.cases = append(run.cases, state)
	}

	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create review run %q exclusively: %w", options.Directory, err)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		_ = os.Remove(directory)
		return nil, fmt.Errorf("open contained review run %q: %w", options.Directory, err)
	}
	run.root = root
	created := make([]string, 0, len(staged))
	cleanup := func() {
		for index := len(created) - 1; index >= 0; index-- {
			_ = root.Remove(created[index])
		}
		_ = root.Close()
		_ = os.Remove(directory)
	}
	for _, image := range staged {
		if err := writeReviewFile(root, image.media.Path, image.payload); err != nil {
			cleanup()
			return nil, fmt.Errorf("retain scenario still %q: %w", image.media.Path, err)
		}
		created = append(created, image.media.Path)
	}
	return run, nil
}

// Directory returns the absolute create-only run directory.
func (run *ReviewRun) Directory() string {
	if run == nil {
		return ""
	}
	return run.directory
}

// Record writes one stereo WAV immediately and retains its sanitized result
// metadata for Close. A failed attempt still gets a valid WAV; when no agent
// audio arrived its right channel is silent.
func (run *ReviewRun) Record(
	scenarioName string, trial int, capture bench.SessionAudioCapture, result Result, runErr error,
) error {
	if run == nil {
		return errors.New("review run is nil")
	}
	if run.closed {
		return errors.New("review run is already closed")
	}
	caseIndex, ok := run.byName[scenarioName]
	if !ok {
		return fmt.Errorf("scenario %q is not expected by this review run", scenarioName)
	}
	if trial < 1 || trial > run.repeats {
		return fmt.Errorf("scenario %q trial %d is outside 1..%d", scenarioName, trial, run.repeats)
	}
	key := reviewAttemptKey(scenarioName, trial)
	if _, exists := run.attempts[key]; exists {
		return fmt.Errorf("scenario %q trial %d was already retained", scenarioName, trial)
	}
	if result.Scenario != "" && result.Scenario != scenarioName {
		return fmt.Errorf("scenario %q trial %d result belongs to %q", scenarioName, trial, result.Scenario)
	}

	wav, spec, err := encodeReviewStereoWAV(capture)
	if err != nil {
		return fmt.Errorf("encode scenario %q trial %d review audio: %w", scenarioName, trial, err)
	}
	filename := fmt.Sprintf("%02d-%s-trial-%02d.stereo.wav",
		caseIndex+1, reviewSlug(scenarioName), trial)
	if err := writeReviewFile(run.root, filename, wav); err != nil {
		return fmt.Errorf("retain scenario %q trial %d review audio: %w", scenarioName, trial, err)
	}
	digest := sha256.Sum256(wav)
	attempt := ReviewAttempt{
		Scenario:       scenarioName,
		Trial:          trial,
		Passed:         result.Passed && runErr == nil,
		Failures:       run.redactMany(result.Failures),
		EvidenceStatus: "unavailable",
		Audio: ReviewMedia{
			Kind: "audio", Role: "time_aligned_room_and_agent", Path: filename,
			SHA256: "sha256:" + hex.EncodeToString(digest[:]), MediaType: "audio/wav", Audio: &spec,
		},
	}
	if runErr != nil {
		attempt.RunError = run.redact(runErr.Error())
	}
	attempt.ProtocolError = run.redact(result.Transcript.Failure)
	attempt.EvidenceError = run.redact(result.Transcript.ExecutionError)
	if attempt.EvidenceError != "" {
		attempt.EvidenceStatus = "error"
	}
	if evidence := result.Transcript.Execution; evidence != nil {
		if err := evidence.Validate(); err != nil {
			attempt.EvidenceStatus = "invalid"
			invalid := run.redact("invalid execution evidence: " + err.Error())
			if attempt.EvidenceError == "" {
				attempt.EvidenceError = invalid
			} else {
				attempt.EvidenceError += "; " + invalid
			}
		} else {
			attempt.EvidenceStatus = "attested"
			attempt.Evidence = &ReviewEvidenceIdentity{
				Kind: evidence.Kind, Scope: evidence.Scope, Fingerprint: evidence.Fingerprint,
			}
		}
	}
	switch {
	case !run.requirement.Required():
		attempt.ReportabilityError = "no execution requirement was supplied; retained media is unattested"
	case runErr != nil:
		attempt.ReportabilityError = "the benchmark attempt ended with an infrastructure error"
	case strings.TrimSpace(result.Transcript.Failure) != "":
		attempt.ReportabilityError = "the Realtime session reported a protocol/runtime failure"
	case strings.TrimSpace(result.Transcript.ExecutionError) != "":
		attempt.ReportabilityError = "execution attestation failed"
	default:
		if err := run.requirement.Match(result.Transcript.Execution); err != nil {
			attempt.ReportabilityError = run.redact(err.Error())
		} else {
			attempt.Reportable = true
		}
	}
	submitted := make(map[string]struct{}, len(run.cases[caseIndex].manifest.Inputs))
	for _, moment := range result.Transcript.Moments {
		if moment.Kind == bench.MomentScheduled && strings.HasPrefix(moment.Name, "scenario.sight.") {
			submitted[moment.Name] = struct{}{}
		}
	}
	for index, media := range run.cases[caseIndex].manifest.Inputs {
		status := "not_observed"
		if _, ok := submitted[fmt.Sprintf("scenario.sight.%d", index+1)]; ok {
			status = "submitted"
		}
		attempt.Inputs = append(attempt.Inputs, ReviewInputUse{Path: media.Path, Status: status})
	}
	run.attempts[key] = attempt
	run.results[key] = sanitizedReviewResult(result, run.redact)
	return nil
}

// Close writes REVIEW.md first and manifest.json last. The manifest is the
// create-only commit marker, so a complete manifest is never visible without
// the required human review. Close is idempotent and returns the first close
// result on subsequent calls.
func (run *ReviewRun) Close() error {
	if run == nil {
		return errors.New("review run is nil")
	}
	if run.closed {
		return run.closeErr
	}
	run.closed = true
	finish := func(err error) error {
		run.closeErr = err
		if run.root != nil {
			if closeErr := run.root.Close(); closeErr != nil && run.closeErr == nil {
				run.closeErr = fmt.Errorf("close review directory: %w", closeErr)
			}
			run.root = nil
		}
		return run.closeErr
	}
	manifest := ReviewManifest{
		Format:         "openrealtime.review",
		FormatVersion:  reviewFormatVersion,
		Suite:          "scenario",
		Expected:       len(run.cases) * run.repeats,
		Cases:          make([]ReviewCase, len(run.cases)),
		EvidencePolicy: "unattested",
	}
	if run.requirement.Required() {
		requirement := run.requirement
		manifest.EvidencePolicy = "required"
		manifest.ExecutionRequirement = &requirement
	}
	for index, state := range run.cases {
		manifest.Cases[index] = state.manifest
		for trial := 1; trial <= run.repeats; trial++ {
			key := reviewAttemptKey(state.item.Name, trial)
			attempt, ok := run.attempts[key]
			if !ok {
				manifest.Missing = append(manifest.Missing, ReviewMissing{Scenario: state.item.Name, Trial: trial})
				continue
			}
			manifest.Attempts = append(manifest.Attempts, attempt)
		}
	}
	manifest.Complete = len(manifest.Missing) == 0 && len(manifest.Attempts) == manifest.Expected
	manifest.Reportable = manifest.Complete
	if !manifest.Complete {
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			"the review bundle is missing expected attempts")
	}
	for _, attempt := range manifest.Attempts {
		if attempt.Reportable {
			continue
		}
		manifest.Reportable = false
		manifest.ReportabilityErrors = append(manifest.ReportabilityErrors,
			fmt.Sprintf("%s#%d: %s", attempt.Scenario, attempt.Trial, attempt.ReportabilityError))
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return finish(fmt.Errorf("encode review manifest: %w", err))
	}
	encoded = append(encoded, '\n')
	markdown := run.renderMarkdown(manifest)
	if err := writeReviewFile(run.root, "REVIEW.md", []byte(markdown)); err != nil {
		return finish(fmt.Errorf("write review report: %w", err))
	}
	if err := writeReviewFile(run.root, "manifest.json", encoded); err != nil {
		return finish(fmt.Errorf("write review manifest commit marker: %w", err))
	}
	if !manifest.Complete {
		return finish(fmt.Errorf("review run retained %d of %d expected attempts",
			len(manifest.Attempts), manifest.Expected))
	}
	return finish(nil)
}

func (run *ReviewRun) renderMarkdown(manifest ReviewManifest) string {
	var output strings.Builder
	output.WriteString("# Scenario review\n\n")
	if manifest.Complete {
		fmt.Fprintf(&output, "Complete: yes (%d/%d attempts retained).\n\n",
			len(manifest.Attempts), manifest.Expected)
	} else {
		fmt.Fprintf(&output, "Complete: no (%d/%d attempts retained).\n\n",
			len(manifest.Attempts), manifest.Expected)
	}
	if manifest.Reportable {
		output.WriteString("Behavioral reportable: yes; every completed attempt matches the required execution evidence.\n\n")
	} else {
		output.WriteString("Behavioral reportable: no. Captured media is reviewable but must not be presented as candidate behavior evidence.\n\n")
		for _, problem := range manifest.ReportabilityErrors {
			fmt.Fprintf(&output, "- Reportability: %s\n", markdownText(problem))
		}
		output.WriteByte('\n')
	}
	output.WriteString("Audio is 24 kHz PCM16 stereo: scripted room/user audio is the left channel; agent output is the right channel. Input stills are content-addressed below, and each attempt states whether submission was observed.\n\n")
	for _, state := range run.cases {
		fmt.Fprintf(&output, "## %02d. %s\n\n", state.manifest.Ordinal, markdownText(state.item.Name))
		if len(state.manifest.Inputs) > 0 {
			output.WriteString("Scripted visual inputs:\n\n")
			for _, media := range state.manifest.Inputs {
				fmt.Fprintf(&output, "- [%s](%s), cue %d ms, `%s`, %s; %s\n",
					markdownText(filepath.Base(media.Path)), media.Path, media.CueMS,
					media.MediaType, media.SHA256, markdownText(media.Note))
			}
			output.WriteByte('\n')
		}
		for trial := 1; trial <= run.repeats; trial++ {
			key := reviewAttemptKey(state.item.Name, trial)
			attempt, ok := run.attempts[key]
			if !ok {
				fmt.Fprintf(&output, "### Trial %d — MISSING\n\nNo attempt artifact was retained.\n\n", trial)
				continue
			}
			status := "FAIL"
			if attempt.Passed {
				status = "PASS"
			} else if attempt.RunError != "" || attempt.ProtocolError != "" {
				status = "ERROR"
			}
			fmt.Fprintf(&output, "### Trial %d — %s\n\n", trial, status)
			fmt.Fprintf(&output, "- Audio: [%s](%s), %s, %.3f ms\n",
				attempt.Audio.Path, attempt.Audio.Path, attempt.Audio.SHA256,
				attempt.Audio.Audio.DurationMS)
			if len(attempt.Inputs) > 0 {
				for _, input := range attempt.Inputs {
					fmt.Fprintf(&output, "- Visual input: [%s](%s) — %s\n",
						filepath.Base(input.Path), input.Path, input.Status)
				}
			}
			if attempt.Evidence != nil {
				fmt.Fprintf(&output, "- Execution evidence: `%s`, scope `%s`, %s\n",
					attempt.Evidence.Kind, markdownCode(attempt.Evidence.Scope), attempt.Evidence.Fingerprint)
			} else {
				fmt.Fprintf(&output, "- Execution evidence: %s; this artifact alone is not graph-native attestation.\n",
					attempt.EvidenceStatus)
			}
			if !attempt.Reportable {
				fmt.Fprintf(&output, "- Reportability: %s\n", markdownText(attempt.ReportabilityError))
			}
			for _, entry := range []struct{ label, value string }{
				{"Run error", attempt.RunError},
				{"Protocol error", attempt.ProtocolError},
				{"Evidence error", attempt.EvidenceError},
			} {
				if entry.value != "" {
					fmt.Fprintf(&output, "- %s: %s\n", entry.label, markdownText(entry.value))
				}
			}
			for _, failure := range attempt.Failures {
				fmt.Fprintf(&output, "- Failed check: %s\n", markdownText(failure))
			}
			result := run.results[key]
			renderReviewTranscript(&output, result)
			output.WriteByte('\n')
		}
	}
	return output.String()
}

func renderReviewTranscript(output *strings.Builder, result Result) {
	for _, hold := range result.Holds {
		fmt.Fprintf(output, "\n- Acknowledgement line %d: %.0f ms active before, %.0f ms during, %.0f ms after; longest pause %.0f ms (limit %d ms); playout window %d–%d ms.\n",
			hold.Line, hold.BeforeActiveMS, hold.DuringActiveMS, hold.AfterActiveMS,
			hold.LongestGapMS, hold.GapLimitMS, hold.FromMS, hold.ToMS)
		if hold.ResponseEvidence != "" {
			fmt.Fprintf(output, "- Response evidence: %s. Terminal status describes protocol completion; it does not establish why speech ended.\n", markdownText(hold.ResponseEvidence))
		}
		for _, response := range hold.Responses {
			fmt.Fprintf(output, "- Response %s: %s; audio playout %.0f–%.0f ms",
				markdownText(response.ResponseID), markdownText(response.Status), response.AudioFromMS, response.AudioToMS)
			switch response.Status {
			case "completed", "cancelled", "incomplete", "failed":
				fmt.Fprintf(output, "; terminal event %.0f ms", response.TerminalAtMS)
				if response.Reason != "" {
					fmt.Fprintf(output, "; reason %s", markdownText(response.Reason))
				}
			}
			output.WriteString(".\n")
		}
	}
	output.WriteString("\nTranscript:\n\n")
	userTurns, agentTurns := result.Transcript.UserTurns(), result.Transcript.AgentTurns()
	if len(userTurns) == 0 {
		output.WriteString("- User transcript: unavailable\n")
	} else {
		for _, turn := range userTurns {
			fmt.Fprintf(output, "- User: %s\n", markdownText(turn))
		}
	}
	if len(agentTurns) == 0 {
		output.WriteString("- Agent transcript: unavailable\n")
	} else {
		for _, turn := range agentTurns {
			fmt.Fprintf(output, "- Agent: %s\n", markdownText(turn))
		}
	}
	for _, moment := range result.Transcript.Moments {
		switch moment.Kind {
		case bench.MomentToolCall:
			fmt.Fprintf(output, "- Tool call at %.3f ms: `%s` `%s`\n",
				moment.AtMS, markdownCode(moment.Name), markdownCode(moment.Arguments))
		case bench.MomentToolResult:
			fmt.Fprintf(output, "- Tool result at %.3f ms: `%s` `%s`\n",
				moment.AtMS, markdownCode(moment.Name), markdownCode(moment.Text))
		case bench.MomentError:
			fmt.Fprintf(output, "- Session error at %.3f ms: %s\n",
				moment.AtMS, markdownText(moment.Text))
		}
	}
	if len(result.Latencies) == 0 {
		output.WriteString("- Latencies: unavailable\n")
	} else {
		for _, latency := range result.Latencies {
			value := "unheard"
			if latency.Heard {
				value = strconv.FormatFloat(latency.MS, 'f', 3, 64) + " ms"
			}
			fmt.Fprintf(output, "- Latency after %s (cue ended %d ms): %s\n",
				markdownText(latency.After), latency.EndedMS, value)
		}
	}
}

func stageReviewImage(
	caseIndex, sightIndex int, item Scenario, sight Sight,
) (ReviewMedia, []byte, error) {
	info, err := os.Lstat(sight.Path)
	if err != nil {
		return ReviewMedia{}, nil, fmt.Errorf("inspect scenario %q still %q: %w", item.Name, sight.Path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return ReviewMedia{}, nil, fmt.Errorf("scenario %q still %q must be a regular non-symlink file", item.Name, sight.Path)
	}
	if info.Size() > maxReviewImageBytes {
		return ReviewMedia{}, nil, fmt.Errorf("scenario %q still %q exceeds %d bytes",
			item.Name, sight.Path, maxReviewImageBytes)
	}
	payload, err := os.ReadFile(sight.Path)
	if err != nil {
		return ReviewMedia{}, nil, fmt.Errorf("read scenario %q still %q: %w", item.Name, sight.Path, err)
	}
	mediaType := http.DetectContentType(payload)
	extension := ""
	switch mediaType {
	case "image/png":
		extension = ".png"
	case "image/jpeg":
		extension = ".jpg"
	default:
		return ReviewMedia{}, nil, fmt.Errorf("scenario %q still %q has unsupported media type %q",
			item.Name, sight.Path, mediaType)
	}
	filename := fmt.Sprintf("%02d-%s-input-%02d%s",
		caseIndex+1, reviewSlug(item.Name), sightIndex+1, extension)
	digest := sha256.Sum256(payload)
	return ReviewMedia{
		Kind: "image", Role: "scripted_visual_input", Path: filename,
		SHA256: "sha256:" + hex.EncodeToString(digest[:]), MediaType: mediaType,
		CueMS: sight.AtMS, Note: sight.Note,
	}, payload, nil
}

func encodeReviewStereoWAV(capture bench.SessionAudioCapture) ([]byte, ReviewAudioSpec, error) {
	rate := capture.SampleRateHz
	if rate == 0 {
		rate = reviewSampleRateHz
	}
	if rate != reviewSampleRateHz {
		return nil, ReviewAudioSpec{}, fmt.Errorf("capture sample rate is %d Hz, want %d Hz", rate, reviewSampleRateHz)
	}
	frames := uint64(len(capture.RoomPCM16))
	starts := make([]uint64, len(capture.Agent))
	for index, chunk := range capture.Agent {
		if math.IsNaN(chunk.AtMS) || math.IsInf(chunk.AtMS, 0) || chunk.AtMS < 0 {
			return nil, ReviewAudioSpec{}, fmt.Errorf("agent chunk %d has invalid start %.3f ms", index, chunk.AtMS)
		}
		startFloat := math.Round(chunk.AtMS * float64(rate) / 1000)
		if startFloat > float64(math.MaxUint64) {
			return nil, ReviewAudioSpec{}, fmt.Errorf("agent chunk %d start exceeds the WAV timeline", index)
		}
		start := uint64(startFloat)
		if uint64(len(chunk.PCM16)) > math.MaxUint64-start {
			return nil, ReviewAudioSpec{}, fmt.Errorf("agent chunk %d exceeds the WAV timeline", index)
		}
		starts[index] = start
		if end := start + uint64(len(chunk.PCM16)); end > frames {
			frames = end
		}
	}
	maxFrames := uint64(math.MaxUint32-36) / (uint64(reviewChannels) * 2)
	if frames > maxFrames {
		return nil, ReviewAudioSpec{}, errors.New("review audio exceeds the WAV container size")
	}
	dataBytes := frames * uint64(reviewChannels) * 2
	interleaved := make([]int16, int(frames)*int(reviewChannels))
	for index, sample := range capture.RoomPCM16 {
		interleaved[index*2] = sample
	}
	for chunkIndex, chunk := range capture.Agent {
		start := starts[chunkIndex]
		for sampleIndex, sample := range chunk.PCM16 {
			position := int(start+uint64(sampleIndex))*2 + 1
			interleaved[position] = saturatingAdd16(interleaved[position], sample)
		}
	}
	wav := make([]byte, 44+len(interleaved)*2)
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(36+dataBytes))
	copy(wav[8:12], "WAVE")
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)
	binary.LittleEndian.PutUint16(wav[20:22], 1)
	binary.LittleEndian.PutUint16(wav[22:24], reviewChannels)
	binary.LittleEndian.PutUint32(wav[24:28], rate)
	binary.LittleEndian.PutUint32(wav[28:32], rate*uint32(reviewChannels)*2)
	binary.LittleEndian.PutUint16(wav[32:34], reviewChannels*2)
	binary.LittleEndian.PutUint16(wav[34:36], 16)
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(dataBytes))
	for index, sample := range interleaved {
		binary.LittleEndian.PutUint16(wav[44+index*2:], uint16(sample))
	}
	spec := ReviewAudioSpec{
		Container: "wav", Encoding: "pcm_s16le", SampleRateHz: rate, Channels: reviewChannels,
		ChannelLayout: []string{"left:scripted_room_user", "right:agent_output"},
		Frames:        frames,
		DurationMS:    float64(frames) * 1000 / float64(rate),
	}
	return wav, spec, nil
}

func saturatingAdd16(left, right int16) int16 {
	sum := int32(left) + int32(right)
	if sum > math.MaxInt16 {
		return math.MaxInt16
	}
	if sum < math.MinInt16 {
		return math.MinInt16
	}
	return int16(sum)
}

func validateNewReviewDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("review directory must not be empty")
	}
	if filepath.Clean(path) != path {
		return "", fmt.Errorf("review directory %q must be a clean path without traversal", path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve review directory: %w", err)
	}
	if absolute == filepath.Dir(absolute) {
		return "", errors.New("review directory must not be a filesystem root")
	}
	parent := filepath.Dir(absolute)
	for current := parent; ; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return "", fmt.Errorf("inspect review directory parent %q: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("review directory parent %q must not be a symlink", current)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("review directory parent %q is not a directory", current)
		}
		if current == filepath.Dir(current) {
			break
		}
	}
	return absolute, nil
}

func writeReviewFile(root *os.Root, name string, payload []byte) error {
	if root == nil {
		return errors.New("review directory is closed")
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

func reviewAttemptKey(scenarioName string, trial int) string {
	return scenarioName + "\x00" + strconv.Itoa(trial)
}

func reviewSlug(value string) string {
	var slug strings.Builder
	dash := false
	for _, character := range strings.ToLower(value) {
		switch {
		case unicode.IsLetter(character) || unicode.IsDigit(character):
			slug.WriteRune(character)
			dash = false
		case slug.Len() > 0 && !dash:
			slug.WriteByte('-')
			dash = true
		}
	}
	return strings.Trim(slug.String(), "-")
}

func (run *ReviewRun) redact(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	for _, secret := range run.secrets {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	value = reviewBearerSecret.ReplaceAllString(value, `${1}[REDACTED]`)
	value = reviewNamedSecret.ReplaceAllString(value, `${1}${2}[REDACTED]`)
	return strings.Join(strings.Fields(value), " ")
}

func (run *ReviewRun) redactMany(values []string) []string {
	redacted := make([]string, 0, len(values))
	for _, value := range values {
		if value = run.redact(value); value != "" {
			redacted = append(redacted, value)
		}
	}
	return redacted
}

func sanitizedReviewResult(result Result, redact func(string) string) Result {
	copy := result
	copy.Holds = append([]HoldMeasurement(nil), result.Holds...)
	for index := range copy.Holds {
		copy.Holds[index].Responses = append([]HoldResponse(nil), result.Holds[index].Responses...)
		for j := range copy.Holds[index].Responses {
			response := &copy.Holds[index].Responses[j]
			response.ResponseID, response.Reason = redact(response.ResponseID), redact(response.Reason)
		}
	}
	copy.Failures = make([]string, len(result.Failures))
	for index, failure := range result.Failures {
		copy.Failures[index] = redact(failure)
	}
	copy.Transcript = result.Transcript
	copy.Transcript.Failure = redact(result.Transcript.Failure)
	copy.Transcript.ExecutionError = redact(result.Transcript.ExecutionError)
	copy.Transcript.Moments = append([]bench.Moment(nil), result.Transcript.Moments...)
	for index := range copy.Transcript.Moments {
		moment := &copy.Transcript.Moments[index]
		moment.Text = redact(moment.Text)
		moment.Arguments = redact(moment.Arguments)
		moment.ResponseID = redact(moment.ResponseID)
		moment.ResponseStatusReason = redact(moment.ResponseStatusReason)
	}
	return copy
}

func markdownText(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	value = strings.ReplaceAll(value, "\\", "\\\\")
	for _, character := range []string{"*", "_", "[", "]", "<", ">", "#", "|"} {
		value = strings.ReplaceAll(value, character, "\\"+character)
	}
	return value
}

func markdownCode(value string) string {
	value = strings.Join(strings.Fields(value), " ")
	return strings.ReplaceAll(value, "`", "'")
}
