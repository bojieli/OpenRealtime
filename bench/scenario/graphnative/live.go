package graphnative

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"os"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

// SubmittedInputCapture binds one receipt to the immutable bytes submitted in
// the corresponding successful scheduled Realtime event. Data is owned by the
// selected retention plug-in; it is deliberately separate from checklist JSON.
type SubmittedInputCapture struct {
	Receipt SubmittedInputReceipt
	Data    []byte
}

// AttemptCapture is the provider-neutral evidence handed to one caller-selected
// retention plug-in after a canonical scenario attempt. Result and Audio are
// independent copies. RunSucceeded distinguishes a behavioral result from a
// diagnostic artifact without retaining an unsanitized provider/transport
// error inside the capture contract.
type AttemptCapture struct {
	Key          AttemptKey
	Result       scenario.Result
	Audio        bench.SessionAudioCapture
	Submitted    []SubmittedInputCapture
	RunSucceeded bool
}

// AttemptRetainer owns evidence persistence. It must retain the supplied audio
// and every submitted input create-only, then return an opaque handle and final
// manifest digest. RunChecklist invokes an independently selected verifier
// before treating the reference as evidence.
type AttemptRetainer func(context.Context, AttemptCapture) (MediaReference, error)

// LiveExecutorConfig composes the existing scenario harness with an ordinary
// Realtime endpoint, exact runtime attestor, and evidence-retention plug-in.
// Voice and Retain are host-selected plug-ins; this package opens no speech,
// storage, provider, server, presentation, or credential resource itself.
type LiveExecutorConfig struct {
	Voice   scenario.Voice
	Session bench.SessionConfig
	Retain  AttemptRetainer
	// EvidenceContext is owned by the complete checklist run rather than one
	// attempt. A timed-out attempt may therefore finish retaining bytes that
	// SessionConfig already captured, while cancellation of the whole run still
	// stops filesystem or provider-side evidence work.
	EvidenceContext context.Context
	// EvidenceTimeout bounds one retention call independently of the live
	// attempt deadline. It is deployment policy, not a behavioral timeout.
	EvidenceTimeout time.Duration
}

type scenarioPlay func(
	context.Context, scenario.Voice, bench.SessionConfig, scenario.Scenario,
) (scenario.Result, error)

type liveExecutor struct {
	voice           scenario.Voice
	session         bench.SessionConfig
	retain          AttemptRetainer
	play            scenarioPlay
	evidenceContext context.Context
	evidenceTimeout time.Duration
	claim           chan struct{}
}

// NewLiveExecutor freezes the live client composition without opening a
// connection or invoking a plug-in. Attempts through one returned executor are
// serialized so stateful speech and retention plug-ins need only implement a
// single-attempt lifecycle.
func NewLiveExecutor(config LiveExecutorConfig) (AttemptExecutor, error) {
	executor, err := newLiveExecutor(config, scenario.Play)
	if err != nil {
		return nil, err
	}
	return executor.execute, nil
}

func newLiveExecutor(config LiveExecutorConfig, play scenarioPlay) (*liveExecutor, error) {
	if nilInterface(config.Voice) {
		return nil, errors.New("scenario live executor requires a voice plug-in")
	}
	if config.Retain == nil {
		return nil, errors.New("scenario live executor requires an attempt-retention plug-in")
	}
	if play == nil {
		return nil, errors.New("scenario live executor requires the scenario player")
	}
	if config.EvidenceContext == nil {
		return nil, errors.New("scenario live executor requires a run-owned evidence context")
	}
	if cause := context.Cause(config.EvidenceContext); cause != nil {
		return nil, fmt.Errorf("scenario live executor evidence context is already done: %w", cause)
	}
	if config.EvidenceTimeout <= 0 || config.EvidenceTimeout > 2*time.Minute {
		return nil, errors.New("scenario live executor evidence timeout must be in (0,2m]")
	}
	if err := validateLiveSession(config.Session); err != nil {
		return nil, err
	}
	session := config.Session
	session.CaptureRuntimeEvidence = true
	return &liveExecutor{
		voice: config.Voice, session: session, retain: config.Retain, play: play,
		evidenceContext: config.EvidenceContext, evidenceTimeout: config.EvidenceTimeout,
		claim: make(chan struct{}, 1),
	}, nil
}

func validateLiveSession(config bench.SessionConfig) error {
	if strings.TrimSpace(config.Endpoint) == "" {
		return errors.New("scenario live executor requires a Realtime endpoint")
	}
	transport := strings.ToLower(strings.TrimSpace(config.Transport))
	if transport != "" && transport != bench.TransportWebSocket && transport != bench.TransportWebRTC {
		return fmt.Errorf("scenario live executor transport must be websocket or webrtc, got %q", config.Transport)
	}
	if nilInterface(config.RuntimeAttestor) {
		return errors.New("scenario live executor requires an exact runtime attestor")
	}
	if config.Timeout < 0 || config.WorkingTimeout < 0 || config.PostPlaybackQuiet < 0 {
		return errors.New("scenario live executor session durations cannot be negative")
	}
	if config.Instructions != "" || len(config.Tools) != 0 || config.Respond != nil ||
		config.HandleTool != nil || config.ConcurrentTools || config.Realtime ||
		config.TrailingSilence != 0 || config.CaptureAudio != nil || config.CaptureVideo != nil ||
		config.CaptureScheduled != nil || len(config.Scheduled) != 0 || len(config.Video) != 0 ||
		config.Ready != nil || config.AttestationScope != "" {
		return errors.New("scenario live executor session contains a scenario-owned override")
	}
	return nil
}

func (executor *liveExecutor) execute(
	ctx context.Context, key AttemptKey, item scenario.Scenario,
) (AttemptObservation, error) {
	if ctx == nil {
		return AttemptObservation{}, errors.New("run scenario live attempt: nil context")
	}
	if cause := context.Cause(ctx); cause != nil {
		return AttemptObservation{}, cause
	}
	if err := validateLiveAttempt(key, item); err != nil {
		return AttemptObservation{}, err
	}
	select {
	case executor.claim <- struct{}{}:
		defer func() { <-executor.claim }()
	case <-ctx.Done():
		return AttemptObservation{}, context.Cause(ctx)
	}
	if cause := context.Cause(ctx); cause != nil {
		return AttemptObservation{}, cause
	}

	var captureMu sync.Mutex
	var audio bench.SessionAudioCapture
	audioCalls := 0
	submitted := make([]SubmittedInputCapture, 0, len(item.Sees))
	scheduledCalls := 0
	var captureErr error
	task := executor.session
	task.AttestationScope = key.TaskID
	task.CaptureAudio = func(value bench.SessionAudioCapture) error {
		captureMu.Lock()
		defer captureMu.Unlock()
		audioCalls++
		if audioCalls != 1 {
			captureErr = errors.Join(captureErr, errors.New("scenario session captured audio more than once"))
			return captureErr
		}
		audio = cloneSessionAudio(value)
		return nil
	}
	task.CaptureScheduled = func(value bench.SessionScheduledCapture) error {
		captureMu.Lock()
		defer captureMu.Unlock()
		index := scheduledCalls / 2
		if scheduledCalls%2 == 0 {
			input, err := decodeSubmittedInput(item, value, index)
			if err != nil {
				captureErr = errors.Join(captureErr, err)
				return err
			}
			for _, previous := range submitted {
				if previous.Receipt.SightID == input.Receipt.SightID {
					err := fmt.Errorf("scenario submitted input %q was captured more than once", input.Receipt.SightID)
					captureErr = errors.Join(captureErr, err)
					return err
				}
			}
			submitted = append(submitted, input)
		} else if err := validateSubmittedResponseCreate(item, value, index); err != nil {
			captureErr = errors.Join(captureErr, err)
			return err
		}
		scheduledCalls++
		return nil
	}

	result, runErr := executor.play(ctx, executor.voice, task, item)
	observation := AttemptObservation{Result: result}
	captureMu.Lock()
	ownedAudio := cloneSessionAudio(audio)
	ownedSubmitted := cloneSubmittedCaptures(submitted)
	calls, scheduled, retainedCaptureErr := audioCalls, scheduledCalls, captureErr
	captureMu.Unlock()
	attemptCause := context.Cause(ctx)
	var audioErr error
	if calls != 1 || ownedAudio.SampleRateHz != 24_000 || len(ownedAudio.RoomPCM16) == 0 {
		audioErr = errors.New(
			"scenario live attempt did not produce one non-empty 24 kHz session audio capture",
		)
	}
	submittedErr := validateSubmittedCaptures(item, result.Transcript, ownedSubmitted, scheduled)
	attemptErr := errors.Join(runErr, retainedCaptureErr, audioErr, submittedErr, attemptCause)
	// Retention needs valid audio because the source bundle's primary timeline
	// is a stereo WAV. Submitted inputs may be partial: preserving those bytes
	// is still useful diagnostic evidence, but the resulting reference cannot
	// satisfy validateMediaReference and therefore cannot become reportable.
	if audioErr != nil {
		return observation, attemptErr
	}

	retainedResult, err := cloneScenarioResult(result)
	if err != nil {
		return observation, errors.Join(attemptErr, err)
	}
	retentionInput := AttemptCapture{
		Key: key, Result: retainedResult, Audio: cloneSessionAudio(ownedAudio),
		Submitted: cloneSubmittedCaptures(ownedSubmitted), RunSucceeded: attemptErr == nil,
	}
	retentionContext, cancelRetention := context.WithTimeout(
		executor.evidenceContext, executor.evidenceTimeout,
	)
	if retentionCause := context.Cause(retentionContext); retentionCause != nil {
		cancelRetention()
		return observation, errors.Join(attemptErr, retentionCause)
	}
	reference, retainErr := executor.retain(retentionContext, retentionInput)
	retentionCause := context.Cause(retentionContext)
	cancelRetention()
	terminalAttemptCause := context.Cause(ctx)
	if retainErr != nil {
		return observation, errors.Join(
			attemptErr, terminalAttemptCause, retentionCause,
			fmt.Errorf("retain scenario attempt evidence: %w", retainErr),
		)
	}
	if retentionCause != nil {
		return observation, errors.Join(attemptErr, terminalAttemptCause, retentionCause)
	}
	receipts := submittedReceipts(ownedSubmitted)
	if !reflect.DeepEqual(reference.Submitted, receipts) {
		return observation, errors.Join(attemptErr, terminalAttemptCause, errors.New(
			"scenario attempt retainer changed the exact submitted-input receipts",
		))
	}
	if err := validateMediaReference(reference, item); err != nil {
		return observation, errors.Join(
			attemptErr, terminalAttemptCause,
			fmt.Errorf("scenario attempt retention receipt: %w", err),
		)
	}
	copy := cloneMediaReference(reference)
	observation.Media = &copy
	return observation, errors.Join(attemptErr, terminalAttemptCause)
}

func validateSubmittedResponseCreate(
	item scenario.Scenario, capture bench.SessionScheduledCapture, index int,
) error {
	if index >= len(item.Sees) {
		return errors.New("scenario emitted an unexpected scheduled response invocation")
	}
	wantID := "scenario.sight." + strconv.Itoa(index+1) + ".response-create"
	if capture.Name != wantID || capture.AtMS != item.Sees[index].AtMS ||
		capture.EventType != "response.create" {
		return fmt.Errorf("scenario visual response invocation %d has a drifted cue identity", index+1)
	}
	var event struct {
		Type string `json:"type"`
	}
	decoder := json.NewDecoder(bytes.NewReader(capture.EventJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil || event.Type != capture.EventType {
		return errors.New("scenario visual response invocation is not the closed response.create shape")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return errors.New("scenario visual response invocation has trailing JSON")
	}
	return nil
}

func validateLiveAttempt(key AttemptKey, item scenario.Scenario) error {
	suite := scenario.Suite()
	if key.CaseOrdinal < 1 || key.CaseOrdinal > len(suite) || key.Trial < 1 {
		return errors.New("scenario live attempt key is outside the canonical suite")
	}
	want := suite[key.CaseOrdinal-1]
	wantTaskID := want.Name + "#" + strconv.Itoa(key.Trial)
	if key.CaseName != want.Name || key.TaskID != wantTaskID || !sameScenario(item, want) {
		return errors.New("scenario live attempt key or fixture differs from the canonical suite")
	}
	return nil
}

func sameScenario(left, right scenario.Scenario) bool {
	leftMenu, rightMenu := left.Menu, right.Menu
	left.Menu, right.Menu = nil, nil
	if !reflect.DeepEqual(left, right) || (leftMenu == nil) != (rightMenu == nil) {
		return false
	}
	if leftMenu == nil {
		return true
	}
	return reflect.ValueOf(leftMenu).Pointer() == reflect.ValueOf(rightMenu).Pointer()
}

func decodeSubmittedInput(
	item scenario.Scenario, capture bench.SessionScheduledCapture, index int,
) (SubmittedInputCapture, error) {
	if index >= len(item.Sees) {
		return SubmittedInputCapture{}, errors.New("scenario emitted an unexpected scheduled input")
	}
	wantID := "scenario.sight." + strconv.Itoa(index+1)
	sight := item.Sees[index]
	if capture.Name != wantID || capture.AtMS != sight.AtMS ||
		capture.EventType != "conversation.item.create" {
		return SubmittedInputCapture{}, fmt.Errorf(
			"scenario submitted input %d has a drifted cue identity", index+1,
		)
	}
	type inputContent struct {
		Type     string `json:"type"`
		ImageURL string `json:"image_url"`
	}
	type inputItem struct {
		Type    string         `json:"type"`
		Role    string         `json:"role"`
		Content []inputContent `json:"content"`
	}
	var event struct {
		Type string    `json:"type"`
		Item inputItem `json:"item"`
	}
	decoder := json.NewDecoder(bytes.NewReader(capture.EventJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&event); err != nil {
		return SubmittedInputCapture{}, errors.New("scenario submitted input event is not the closed image-message shape")
	}
	if err := requireJSONEOF(decoder); err != nil {
		return SubmittedInputCapture{}, errors.New("scenario submitted input event has trailing JSON")
	}
	if event.Type != capture.EventType || event.Item.Type != "message" || event.Item.Role != "user" ||
		len(event.Item.Content) != 1 || event.Item.Content[0].Type != "input_image" {
		return SubmittedInputCapture{}, errors.New("scenario submitted input event is not one user image")
	}
	const prefix = "data:image/png;base64,"
	if !strings.HasPrefix(event.Item.Content[0].ImageURL, prefix) {
		return SubmittedInputCapture{}, errors.New("scenario submitted input is not an inline PNG")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(
		event.Item.Content[0].ImageURL, prefix,
	))
	if err != nil || len(payload) == 0 {
		return SubmittedInputCapture{}, errors.New("scenario submitted input has invalid PNG bytes")
	}
	wantPayload, err := os.ReadFile(sight.Path)
	if err != nil || !bytes.Equal(payload, wantPayload) {
		return SubmittedInputCapture{}, errors.New(
			"scenario submitted input bytes differ from the canonical fixture",
		)
	}
	if _, err := png.Decode(bytes.NewReader(payload)); err != nil {
		return SubmittedInputCapture{}, errors.New("scenario submitted input does not fully decode as PNG")
	}
	digest := sha256.Sum256(payload)
	return SubmittedInputCapture{
		Receipt: SubmittedInputReceipt{
			SightID: wantID, CueMS: sight.AtMS,
			SHA256: "sha256:" + hex.EncodeToString(digest[:]), SizeBytes: int64(len(payload)),
			MediaType: "image/png",
		},
		Data: slices.Clone(payload),
	}, nil
}

func validateSubmittedCaptures(
	item scenario.Scenario, transcript bench.Transcript, captured []SubmittedInputCapture,
	scheduledCalls int,
) error {
	if scheduledCalls != 2*len(item.Sees) {
		return fmt.Errorf(
			"scenario captured %d/%d authored visual protocol events", scheduledCalls, 2*len(item.Sees),
		)
	}
	if len(captured) != len(item.Sees) {
		return fmt.Errorf(
			"scenario retained %d/%d successfully submitted still inputs", len(captured), len(item.Sees),
		)
	}
	moments := make(map[string]int, len(item.Sees))
	for _, moment := range transcript.Moments {
		if moment.Kind == bench.MomentScheduled {
			moments[moment.Name]++
		}
	}
	for index, input := range captured {
		wantID := "scenario.sight." + strconv.Itoa(index+1)
		responseID := wantID + ".response-create"
		if input.Receipt.SightID != wantID || moments[wantID] != 1 || moments[responseID] != 1 {
			return fmt.Errorf("scenario submitted input %q lacks one successful transcript moment", wantID)
		}
	}
	if len(moments) != 2*len(item.Sees) {
		return errors.New("scenario transcript contains an unexpected scheduled input identity")
	}
	return nil
}

func submittedReceipts(source []SubmittedInputCapture) []SubmittedInputReceipt {
	if len(source) == 0 {
		return nil
	}
	result := make([]SubmittedInputReceipt, len(source))
	for index, input := range source {
		result[index] = input.Receipt
	}
	return result
}

func cloneSubmittedCaptures(source []SubmittedInputCapture) []SubmittedInputCapture {
	result := make([]SubmittedInputCapture, len(source))
	for index, input := range source {
		result[index] = input
		result[index].Data = slices.Clone(input.Data)
	}
	return result
}

func cloneSessionAudio(source bench.SessionAudioCapture) bench.SessionAudioCapture {
	result := source
	result.RoomPCM16 = slices.Clone(source.RoomPCM16)
	result.Agent = make([]bench.TimedAudioChunk, len(source.Agent))
	for index, chunk := range source.Agent {
		result.Agent[index] = chunk
		result.Agent[index].PCM16 = slices.Clone(chunk.PCM16)
	}
	return result
}

func cloneScenarioResult(source scenario.Result) (scenario.Result, error) {
	payload, err := json.Marshal(source)
	if err != nil {
		return scenario.Result{}, fmt.Errorf("clone scenario result for retention: %w", err)
	}
	var result scenario.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		return scenario.Result{}, fmt.Errorf("clone scenario result for retention: %w", err)
	}
	return result, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}
