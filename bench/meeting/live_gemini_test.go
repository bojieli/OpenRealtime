package meeting

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	revieweval "github.com/bojieli/OpenRealtime/bench/review"
	reviewgemini "github.com/bojieli/OpenRealtime/bench/review/gemini"
	reviewmedia "github.com/bojieli/OpenRealtime/bench/review/media"
	reviewffmpeg "github.com/bojieli/OpenRealtime/bench/review/media/ffmpeg"
	"github.com/bojieli/OpenRealtime/computeruse"
)

const (
	meetingChromiumReviewGate = "OPENREALTIME_RUN_MEETING_CHROMIUM_REVIEW_E2E"
	meetingGeminiReviewGate   = "OPENREALTIME_RUN_MEETING_GEMINI_E2E"
	meetingGeminiReviewPath   = "OPENREALTIME_MEETING_GEMINI_REVIEW_DIR"
)

// TestMeetingRunChromiumAllFourRealFFmpegReviewEndToEnd exercises the actual
// browser presentation and production FFmpeg/full-decode plug-ins without a
// network reviewer. It is deliberately opt-in because the four authored
// timelines take about one minute in wall-clock time.
func TestMeetingRunChromiumAllFourRealFFmpegReviewEndToEnd(t *testing.T) {
	if os.Getenv(meetingChromiumReviewGate) != "1" {
		t.Skip("set " + meetingChromiumReviewGate + "=1 to retain all four real Chromium A/V cases")
	}
	parent := t.TempDir()
	directory := filepath.Join(parent, "chromium-all-four")
	lease := openFixtureMeetingReviewer(t, &fixtureMeetingReviewer{})
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease, VideoFactory: &realMeetingVideoFactory{},
		SourceReceiptPath:             filepath.Join(parent, "source-receipt.json"),
		EvaluationReceiptDirectory:    filepath.Join(parent, "evaluation-receipts"),
		EvaluationQuarantineDirectory: filepath.Join(parent, "evaluation-quarantine"),
	})
	if err != nil {
		t.Fatal(err)
	}
	makeMeetingReviewTreeRemovable(t, directory)
	result := runChromiumMeetingReview(t, bundle)
	requireChromiumMeetingReview(t, bundle, result, revieweval.ProviderDescriptor{
		Provider: "fixture", Model: "meeting-review-fixture",
	})
}

// TestLiveGemini37FlashReviewsAllFourRetainedMeetingAV is a provisioned
// reviewer gate over the same deterministic four-case browser run. It keeps
// its source, raw provider requests/responses, normalized assessments, exact
// media, and portable receipts at a caller-owned create-only destination.
// The deterministic scorer remains authoritative; this test separately fails
// when the advisory reviewer finds a significant issue so the recording can
// be inspected and the run improved before another create-only attempt.
func TestLiveGemini37FlashReviewsAllFourRetainedMeetingAV(t *testing.T) {
	if os.Getenv(meetingGeminiReviewGate) != "1" {
		t.Skip("set " + meetingGeminiReviewGate + "=1 to call exact Gemini 3.7 Flash on all four Meeting A/V cases")
	}
	directory, sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory :=
		liveMeetingReviewDestinations(t)
	apiKey, err := reviewgemini.EnvironmentAPIKey(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	registry, err := revieweval.NewRegistry([]revieweval.Registration{
		reviewgemini.Registration(func(ctx context.Context) (string, error) {
			if err := ctx.Err(); err != nil {
				return "", err
			}
			return apiKey, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := registry.Open(t.Context(), reviewgemini.RegistrationName)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if closeErr := lease.Close(); closeErr != nil {
			t.Errorf("close exact Gemini reviewer lease: %v", closeErr)
		}
	}()
	if lease.Descriptor() != reviewgemini.Descriptor() ||
		lease.Descriptor().Model != reviewgemini.ModelID ||
		reviewgemini.ModelID != "gemini-3.7-flash" {
		t.Fatalf("selected reviewer drifted from exact Gemini 3.7 Flash: %+v", lease.Descriptor())
	}
	bundle, err := NewReviewBundle(ReviewBundleOptions{
		Directory: directory, Reviewer: lease,
		VideoFactory: &realMeetingVideoFactory{options: reviewffmpeg.Options{
			SensitiveValues: []string{apiKey},
		}},
		SourceReceiptPath:             sourceReceiptPath,
		EvaluationReceiptDirectory:    evaluationReceiptDirectory,
		EvaluationQuarantineDirectory: evaluationQuarantineDirectory,
		SensitiveValues:               []string{apiKey},
	})
	if err != nil {
		t.Fatal(err)
	}
	result := runChromiumMeetingReview(t, bundle)
	requireChromiumMeetingReview(t, bundle, result, reviewgemini.Descriptor())
	t.Logf("retained exact %s all-four Meeting A/V review at %s", reviewgemini.ModelID, directory)
}

func runChromiumMeetingReview(t *testing.T, bundle *ReviewBundle) bench.Result {
	t.Helper()
	for _, binary := range []string{"chromium", "ffmpeg", "ffprobe", "bwrap", "espeak"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Fatalf("real Meeting review requires %s: %v", binary, err)
		}
	}
	requirement, graph := fixtureMeetingGraphRequirement(t)
	harness := newChromiumMeetingReviewHarness(t, graph)
	dependencies := productionRunDependencies()
	dependencies.playSamples = harness.play
	dependencies.now = time.Now
	cell := ReferenceCell()
	cell.Execution = requirement
	result, err := Run(t.Context(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: cell,
		FrameRate: 2, AnalysisDelay: 10 * time.Second, Timeout: 30 * time.Second,
		Evidence: bundle, dependencies: dependencies,
	})
	if err != nil {
		t.Fatalf("run all-four Chromium Meeting review: %v", err)
	}
	return result
}

func requireChromiumMeetingReview(
	t *testing.T, bundle *ReviewBundle, result bench.Result, reviewer revieweval.ProviderDescriptor,
) {
	t.Helper()
	directory := bundle.Directory()
	if !result.Summary.Complete || result.Summary.Passed != ExpectedTasks() ||
		len(result.Tasks) != ExpectedTasks() {
		t.Fatalf("all-four deterministic Meeting result = %+v", result.Summary)
	}
	for _, outcome := range result.Tasks {
		if !outcome.Completed || !outcome.Passed || outcome.Execution == nil ||
			outcome.Execution.Kind != bench.ExecutionGraphNative || outcome.Execution.Scope != outcome.ID {
			t.Fatalf("all-four deterministic Meeting outcome = %+v", outcome)
		}
	}
	receipt, err := bundle.Receipt()
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := VerifyReviewBundle(receipt.Directory, receipt.ManifestSHA256)
	if err != nil || !manifest.Complete || manifest.Reportable || manifest.CoreReportable ||
		len(manifest.Attempts) != ExpectedTasks() || len(manifest.Missing) != 0 {
		t.Fatalf("all-four Meeting review manifest = %+v, %v", manifest, err)
	}
	for _, attempt := range manifest.Attempts {
		if !attempt.Deterministic.Passed || attempt.ReviewStatus != "complete" ||
			attempt.VideoStatus != "retained-playable-video" || len(attempt.Media) != 2 ||
			attempt.Assessment == nil || !attempt.Assessment.MediaUsable ||
			!attempt.Assessment.AgreesWithDeterministic ||
			attempt.Assessment.ObservedOutcome != "pass" ||
			len(attempt.Assessment.SignificantProblems) != 0 {
			t.Fatalf("all-four Meeting reviewer found an unresolved issue in %s: %+v",
				attempt.Case, attempt)
		}
		if reviewer.Provider != "" && (attempt.Assessment.Reviewer.Provider != reviewer.Provider ||
			attempt.Assessment.Reviewer.Model != reviewer.Model) {
			t.Fatalf("Meeting reviewer provenance for %s = %+v, want %+v",
				attempt.Case, attempt.Assessment.Reviewer, reviewer)
		}
		mediaDirectory := filepath.Join(directory,
			filepath.FromSlash(filepath.Dir(attempt.MediaManifest.Path)))
		mediaManifest, err := reviewmedia.VerifyBundle(mediaDirectory, attempt.MediaManifest.SHA256)
		if err != nil || mediaManifest.Attestor == nil ||
			mediaManifest.Attestor.Descriptor.Capability != reviewmedia.FullDecodeAttestationCapability ||
			len(mediaManifest.Video) != 1 || mediaManifest.Video[0].PlayableSpec == nil ||
			mediaManifest.Video[0].PlayableSpec.EncodedWidth%2 != 0 ||
			mediaManifest.Video[0].PlayableSpec.EncodedHeight%2 != 0 {
			t.Fatalf("full-decode Meeting media for %s = %+v, %v", attempt.Case, mediaManifest, err)
		}
		if attempt.EvaluationReceipt == nil || attempt.EvaluationReceipt.ManifestSHA256 == "" ||
			attempt.SecondaryReview == nil || attempt.EvaluationManifest == nil ||
			!meetingAttemptRetainsRawProviderExchange(attempt) {
			t.Fatalf("Meeting review for %s lacks exact raw provider evidence: %+v", attempt.Case, attempt)
		}
	}
	sourceReceipt, err := bundle.SourceReceipt()
	if err != nil {
		t.Fatal(err)
	}
	externalSource, err := ReadReviewSourceReceipt(t.Context(), bundle.sourceReceiptPath)
	if err != nil || !samePortableMeetingSourceReceipt(externalSource, sourceReceipt) {
		t.Fatalf("all-four external source receipt = %+v, %v", externalSource, err)
	}
	if _, err := VerifyMeetingReviewSource(t.Context(), externalSource.Directory, externalSource); err != nil {
		t.Fatalf("verify all-four external source receipt: %v", err)
	}
	evaluations := bundle.EvaluationReceipts()
	if len(evaluations) != ExpectedTasks() {
		t.Fatalf("all-four external evaluation receipts = %d, want %d",
			len(evaluations), ExpectedTasks())
	}
	for _, evaluation := range evaluations {
		path := filepath.Join(bundle.evaluationReceiptDirectory,
			filepath.Base(evaluation.Directory)+".receipt.json")
		external, err := revieweval.ReadEvaluationBundleReceipt(t.Context(), path)
		if err != nil || external != evaluation {
			t.Fatalf("all-four external evaluation receipt = %+v, want %+v: %v",
				external, evaluation, err)
		}
		if _, err := revieweval.VerifyEvaluationBundle(t.Context(),
			revieweval.EvaluationBundleOptions{Directory: external.Directory}, external); err != nil {
			t.Fatalf("verify all-four external evaluation receipt: %v", err)
		}
	}
}

func meetingAttemptRetainsRawProviderExchange(attempt ReviewAttempt) bool {
	purposes := map[string]bool{
		"provider-request.bin":   false,
		"raw-response.bin":       false,
		"normalized-output.json": false,
		"record.json":            false,
	}
	for _, artifact := range attempt.ReviewArtifacts {
		for name := range purposes {
			if filepath.Base(filepath.FromSlash(artifact.Path)) == name &&
				artifact.SHA256 != "" && artifact.SizeBytes > 0 {
				purposes[name] = true
			}
		}
	}
	for _, retained := range purposes {
		if !retained {
			return false
		}
	}
	return true
}

func liveMeetingReviewDestinations(t *testing.T) (string, string, string, string) {
	t.Helper()
	directory := strings.TrimSpace(os.Getenv(meetingGeminiReviewPath))
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory ||
		directory == filepath.Dir(directory) {
		t.Fatal(meetingGeminiReviewPath + " must name a clean absolute create-only non-root directory")
	}
	parent := filepath.Dir(directory)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		t.Fatal(meetingGeminiReviewPath + " parent must be an existing directory")
	}
	sourceReceiptPath := directory + ".source-receipt.json"
	evaluationReceiptDirectory := directory + ".evaluation-receipts"
	evaluationQuarantineDirectory := directory + ".evaluation-quarantine"
	for _, path := range []string{
		directory, sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory,
	} {
		if _, err := os.Lstat(path); err == nil {
			t.Fatalf("live Meeting review create-only destination already exists: %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("inspect live Meeting review destination %s: %v", path, err)
		}
	}
	return directory, sourceReceiptPath, evaluationReceiptDirectory, evaluationQuarantineDirectory
}

type chromiumMeetingReviewHarness struct {
	graph  bench.GraphEvidence
	speech map[string][]int16
}

func newChromiumMeetingReviewHarness(
	t *testing.T, graph bench.GraphEvidence,
) *chromiumMeetingReviewHarness {
	t.Helper()
	texts := map[string]string{
		"open-share-present":           "The launch review is open and shared. Conversion is eighteen point four percent.",
		"follow-up-during-analysis":    "I moved to the risks slide while the analysis continued.",
		"visual-alert-before":          "Conversion is eighteen point four percent.",
		"visual-alert-after":           "Alert acknowledged. Continuing the presentation.",
		"spoken-navigation-correction": "Understood. Returning from summary to the overview.",
	}
	speech := make(map[string][]int16, len(texts))
	for id, text := range texts {
		pcm, err := synthesizeMeetingReviewSpeech(t.Context(), text)
		if err != nil {
			t.Fatalf("synthesize retained Meeting response %s: %v", id, err)
		}
		speech[id] = pcm
	}
	return &chromiumMeetingReviewHarness{graph: graph, speech: speech}
}

type scriptedMeetingEvent struct {
	atMS  float64
	order int
	run   func(float64) error
}

func (harness *chromiumMeetingReviewHarness) play(
	ctx context.Context, config bench.SessionConfig, samples []int16,
) (bench.Transcript, error) {
	if err := config.Ready(ctx); err != nil {
		return bench.Transcript{}, err
	}
	started := time.Now()
	playbackMS := float64(len(samples)) * 1000 / 24_000
	transcript := bench.Transcript{
		PlaybackMS: playbackMS,
		Moments:    []bench.Moment{{AtMS: 0, Kind: bench.MomentReady}},
	}
	var wireTimestamp int64
	captureFrame := func(_ float64) error {
		frame, err := config.Video[0].Capture(ctx)
		if err != nil {
			return err
		}
		atMS := elapsedMeetingReviewMS(started)
		wireTimestamp++
		if config.CaptureVideo != nil {
			if err := config.CaptureVideo(bench.SessionVideoCapture{
				Source: "screen", Width: config.Video[0].Width, Height: config.Video[0].Height,
				MediaType: "image/jpeg", WireTimestamp: wireTimestamp,
				EpisodeAtMS: atMS, Data: frame,
			}); err != nil {
				return err
			}
		}
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: atMS, Kind: bench.MomentVideoFrame, Source: "screen",
		})
		return nil
	}
	click := func(callID string, x, y int) func(float64) error {
		return func(_ float64) error {
			arguments := json.RawMessage(fmt.Sprintf(
				`{"source":"screen","x":%d,"y":%d,"button":"left"}`, x, y,
			))
			atMS := elapsedMeetingReviewMS(started)
			_, err := config.HandleTool(ctx, bench.ToolRequest{
				CallID: callID, Name: computeruse.ClickNormalized, Arguments: arguments,
			})
			transcript.Moments = append(transcript.Moments, bench.Moment{
				AtMS: atMS, Kind: bench.MomentToolCall, CallID: callID,
				Name: computeruse.ClickNormalized, Arguments: string(arguments),
			})
			return err
		}
	}
	tool := func(callID, name string) func(float64) error {
		return func(_ float64) error {
			atMS := elapsedMeetingReviewMS(started)
			_, err := config.HandleTool(ctx, bench.ToolRequest{
				CallID: callID, Name: name, Arguments: json.RawMessage(`{}`),
			})
			transcript.Moments = append(transcript.Moments, bench.Moment{
				AtMS: atMS, Kind: bench.MomentToolCall, CallID: callID,
				Name: name, Arguments: `{}`,
			})
			return err
		}
	}
	events := make([]scriptedMeetingEvent, 0, 40)
	var agent []bench.TimedAudioChunk
	var analysisDone chan error
	caseID := config.AttestationScope
	switch caseID {
	case "open-share-present":
		events = append(events,
			scriptedMeetingEvent{atMS: 2_000, run: click(caseID+"-open", 100, 824)},
			scriptedMeetingEvent{atMS: 3_800, run: click(caseID+"-share", 146, 609)},
			scriptedMeetingEvent{atMS: 5_200, run: tool(caseID+"-read", ToolReadLaunchReview)},
		)
		responseAt := 5_300.0
		agent = append(agent, bench.TimedAudioChunk{AtMS: responseAt, PCM16: harness.speech[caseID]})
		appendMeetingReviewResponse(&transcript, responseAt,
			"The launch review is open and shared. Conversion is 18.4 percent.", harness.speech[caseID])
	case "follow-up-during-analysis":
		analysisDone = make(chan error, 1)
		events = append(events,
			scriptedMeetingEvent{atMS: 1_000, run: func(_ float64) error {
				callID := caseID + "-analysis"
				atMS := elapsedMeetingReviewMS(started)
				transcript.Moments = append(transcript.Moments, bench.Moment{
					AtMS: atMS, Kind: bench.MomentToolCall, CallID: callID,
					Name: ToolAnalyzeLaunchReview, Arguments: `{}`,
				})
				go func() {
					_, err := config.HandleTool(ctx, bench.ToolRequest{
						CallID: callID, Name: ToolAnalyzeLaunchReview, Arguments: json.RawMessage(`{}`),
					})
					analysisDone <- err
				}()
				return nil
			}},
			scriptedMeetingEvent{atMS: 9_800, run: click(caseID+"-risks", 450, 827)},
		)
		responseAt := 11_100.0
		agent = append(agent, bench.TimedAudioChunk{AtMS: responseAt, PCM16: harness.speech[caseID]})
		appendMeetingReviewResponse(&transcript, responseAt,
			"I moved to the risks slide while the analysis continued.", harness.speech[caseID])
	case "visual-alert-during-presentation":
		events = append(events,
			scriptedMeetingEvent{atMS: 10_600, order: 9, run: captureFrame},
			scriptedMeetingEvent{atMS: 10_750, run: click(caseID+"-ack", 692, 522)},
			scriptedMeetingEvent{atMS: 10_900, order: 9, run: captureFrame},
		)
		beforeAt, afterAt := 8_300.0, 11_000.0
		agent = append(agent,
			bench.TimedAudioChunk{AtMS: beforeAt, PCM16: harness.speech["visual-alert-before"]},
			bench.TimedAudioChunk{AtMS: afterAt, PCM16: harness.speech["visual-alert-after"]},
		)
		appendMeetingReviewResponse(&transcript, beforeAt,
			"Conversion is 18.4 percent.", harness.speech["visual-alert-before"])
		appendMeetingReviewResponse(&transcript, afterAt,
			"Alert acknowledged. Continuing the presentation.", harness.speech["visual-alert-after"])
	case "spoken-navigation-correction":
		events = append(events,
			scriptedMeetingEvent{atMS: 3_000, run: click(caseID+"-summary", 538, 827)},
			scriptedMeetingEvent{atMS: 9_200, run: click(caseID+"-overview", 360, 827)},
		)
		responseAt := 9_300.0
		agent = append(agent, bench.TimedAudioChunk{AtMS: responseAt, PCM16: harness.speech[caseID]})
		appendMeetingReviewResponse(&transcript, responseAt,
			"Understood. Returning from summary to the overview.", harness.speech[caseID])
	default:
		return transcript, fmt.Errorf("unknown scripted Meeting case %q", caseID)
	}
	timelineEndMS := playbackMS
	for _, chunk := range agent {
		chunkEndMS := chunk.AtMS + float64(len(chunk.PCM16))*1000/24_000
		if chunkEndMS > timelineEndMS {
			timelineEndMS = chunkEndMS
		}
	}
	for _, event := range events {
		if event.atMS > timelineEndMS {
			timelineEndMS = event.atMS
		}
	}
	timelineEndMS += 350
	for atMS := float64(50); atMS < timelineEndMS; atMS += 500 {
		events = append(events, scriptedMeetingEvent{atMS: atMS, order: 10, run: captureFrame})
	}
	events = append(events, scriptedMeetingEvent{
		atMS: timelineEndMS, order: 10, run: captureFrame,
	})
	appendMeetingReviewUserMoments(&transcript, caseID)
	sort.SliceStable(events, func(left, right int) bool {
		if events[left].atMS == events[right].atMS {
			return events[left].order < events[right].order
		}
		return events[left].atMS < events[right].atMS
	})
	for _, event := range events {
		if err := waitUntilMeetingReview(ctx, started, event.atMS); err != nil {
			return transcript, err
		}
		if err := event.run(event.atMS); err != nil {
			return transcript, err
		}
	}
	if analysisDone != nil {
		select {
		case err := <-analysisDone:
			if err != nil {
				return transcript, err
			}
		case <-ctx.Done():
			return transcript, context.Cause(ctx)
		}
	}
	if config.CaptureAudio != nil {
		if err := config.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: append([]int16(nil), samples...), Agent: agent,
		}); err != nil {
			return transcript, err
		}
	}
	evidence, err := bench.FreezeExecutionEvidence(bench.ExecutionEvidence{
		FormatVersion: bench.AttestationFormatVersion, Kind: bench.ExecutionGraphNative,
		Scope: caseID, Graph: cloneGraphEvidence(harness.graph),
	})
	if err != nil {
		return transcript, err
	}
	sort.SliceStable(transcript.Moments, func(left, right int) bool {
		return transcript.Moments[left].AtMS < transcript.Moments[right].AtMS
	})
	transcript.Execution = &evidence
	return transcript, nil
}

func appendMeetingReviewResponse(
	transcript *bench.Transcript, atMS float64, text string, speech []int16,
) {
	durationMS := float64(len(speech)) * 1000 / 24_000
	transcript.Moments = append(transcript.Moments,
		bench.Moment{AtMS: atMS, Kind: bench.MomentAgentText, Text: text})
	for offset := 0.0; offset < durationMS; offset += 500 {
		chunkMS := min(500.0, durationMS-offset)
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: atMS + offset, Kind: bench.MomentAgentAudio, AudioMS: chunkMS,
		})
	}
	transcript.Moments = append(transcript.Moments,
		bench.Moment{AtMS: atMS + durationMS, Kind: bench.MomentResponseDone})
}

func appendMeetingReviewUserMoments(transcript *bench.Transcript, caseID string) {
	segments := map[string][]struct {
		atMS float64
		text string
	}{
		"open-share-present": {{atMS: 5_155, text: "Open the launch review, share your screen and tell everyone the latest conversion rate."}},
		"follow-up-during-analysis": {
			{atMS: 4_737, text: "Analyze the launch review in the background. Keep listening while you work."},
			{atMS: 10_920, text: "Switch to the risks slide now."},
		},
		"visual-alert-during-presentation": {{atMS: 8_034, text: "Present the launch overview. If a deployment alert appears, acknowledge it immediately without stopping your presentation."}},
		"spoken-navigation-correction": {
			{atMS: 3_437, text: "Go to the summary slide and begin presenting."},
			{atMS: 8_991, text: "Wait, go back to the overview."},
		},
	}
	for _, segment := range segments[caseID] {
		transcript.Moments = append(transcript.Moments, bench.Moment{
			AtMS: segment.atMS, Kind: bench.MomentTranscript, Text: segment.text,
		})
	}
}

func waitUntilMeetingReview(ctx context.Context, started time.Time, atMS float64) error {
	wait := time.Until(started.Add(time.Duration(atMS * float64(time.Millisecond))))
	if wait <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func elapsedMeetingReviewMS(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}

func synthesizeMeetingReviewSpeech(ctx context.Context, text string) ([]int16, error) {
	espeak := exec.CommandContext(ctx, "espeak", "--stdout", text)
	wav, err := espeak.Output()
	if err != nil {
		return nil, errors.New("run espeak")
	}
	ffmpeg := exec.CommandContext(ctx, "ffmpeg",
		"-nostdin", "-hide_banner", "-loglevel", "error", "-i", "pipe:0",
		"-f", "s16le", "-acodec", "pcm_s16le", "-ar", "24000", "-ac", "1", "pipe:1",
	)
	ffmpeg.Stdin = bytes.NewReader(wav)
	raw, err := ffmpeg.Output()
	if err != nil || len(raw) == 0 || len(raw)%2 != 0 {
		return nil, errors.New("resample espeak output to exact PCM24k")
	}
	result := make([]int16, len(raw)/2)
	for index := range result {
		result[index] = int16(binary.LittleEndian.Uint16(raw[index*2 : index*2+2]))
	}
	return result, nil
}
