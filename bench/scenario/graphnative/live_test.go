package graphnative

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	"github.com/bojieli/OpenRealtime/bench/scenario"
)

func TestLiveExecutorRetainsExactSuccessfulScenarioAudioAndStills(t *testing.T) {
	t.Chdir("../../..")
	item := scenario.Suite()[9]
	key := AttemptKey{CaseOrdinal: 10, CaseName: item.Name, Trial: 3, TaskID: item.Name + "#3"}
	var retained AttemptCapture
	config := liveExecutorFixture(func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
		retained = cloneAttemptCaptureForTest(t, capture)
		receipts := submittedReceipts(capture.Submitted)
		capture.Result.Scenario = "retainer mutation"
		capture.Audio.RoomPCM16[0] = 900
		capture.Submitted[0].Data[0] ^= 0xff
		return liveMediaReference(capture.Key, receipts), nil
	})
	executor, err := newLiveExecutor(config, func(
		_ context.Context, _ scenario.Voice, session bench.SessionConfig, got scenario.Scenario,
	) (scenario.Result, error) {
		if !sameScenario(got, item) || session.AttestationScope != key.TaskID ||
			!session.CaptureRuntimeEvidence || session.CaptureAudio == nil ||
			session.CaptureScheduled == nil {
			t.Fatalf("live task session = %+v", session)
		}
		return successfulLiveFixture(t, session, got), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executor.execute(t.Context(), key, item)
	if err != nil {
		t.Fatalf("live execute error = %v", err)
	}
	if observation.Media == nil || observation.Result.Scenario != item.Name ||
		observation.Media.Handle != "attempt/"+key.TaskID || !retained.RunSucceeded ||
		retained.Key != key || retained.Result.Scenario != item.Name ||
		retained.Audio.SampleRateHz != 24_000 || len(retained.Audio.RoomPCM16) != 4 ||
		len(retained.Submitted) != len(item.Sees) {
		t.Fatalf("live observation/capture = %+v / %+v", observation, retained)
	}
	for index, input := range retained.Submitted {
		payload, readErr := os.ReadFile(item.Sees[index].Path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		digest := sha256.Sum256(payload)
		if input.Receipt.SightID != "scenario.sight."+strconv.Itoa(index+1) ||
			input.Receipt.CueMS != item.Sees[index].AtMS ||
			input.Receipt.SHA256 != "sha256:"+hex.EncodeToString(digest[:]) ||
			input.Receipt.SizeBytes != int64(len(payload)) ||
			input.Receipt.MediaType != "image/png" || !reflect.DeepEqual(input.Data, payload) {
			t.Fatalf("submitted input %d = %+v", index, input.Receipt)
		}
	}
}

func TestLiveExecutorInvokesAndRetainsExactElevenCaseSuite(t *testing.T) {
	t.Chdir("../../..")
	suite := scenario.Suite()
	if len(suite) != 11 {
		t.Fatalf("canonical scenario count = %d, want 11", len(suite))
	}
	retained := make([]AttemptKey, 0, len(suite))
	executor, err := newLiveExecutor(liveExecutorFixture(func(
		_ context.Context, capture AttemptCapture,
	) (MediaReference, error) {
		retained = append(retained, capture.Key)
		return liveMediaReference(capture.Key, submittedReceipts(capture.Submitted)), nil
	}), successfulLivePlay(t))
	if err != nil {
		t.Fatal(err)
	}
	for index, item := range suite {
		key := AttemptKey{
			CaseOrdinal: index + 1,
			CaseName:    item.Name,
			Trial:       1,
			TaskID:      item.Name + "#1",
		}
		observation, err := executor.execute(t.Context(), key, item)
		if err != nil {
			t.Fatalf("case %d %q: %v", index+1, item.Name, err)
		}
		if observation.Media == nil || observation.Result.Scenario != item.Name ||
			len(observation.Media.Submitted) != len(item.Sees) {
			t.Fatalf("case %d %q observation = %+v", index+1, item.Name, observation)
		}
	}
	if len(retained) != 11 {
		t.Fatalf("retained attempts = %d, want 11", len(retained))
	}
	for index, key := range retained {
		if key.CaseOrdinal != index+1 || key.CaseName != suite[index].Name ||
			key.TaskID != suite[index].Name+"#1" {
			t.Fatalf("retained attempt %d = %+v", index, key)
		}
	}
}

func TestLiveExecutorRetainsDiagnosticAttemptWithoutPromotingRunFailure(t *testing.T) {
	item := scenario.Suite()[10]
	key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	want := errors.New("fixture transport failed")
	var retained AttemptCapture
	config := liveExecutorFixture(func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
		retained = cloneAttemptCaptureForTest(t, capture)
		return liveMediaReference(capture.Key, nil), nil
	})
	executor, err := newLiveExecutor(config, func(
		_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		result := successfulLiveFixture(t, session, item)
		result.Passed = false
		return result, want
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executor.execute(t.Context(), key, item)
	if !errors.Is(err, want) || observation.Media == nil || retained.RunSucceeded {
		t.Fatalf("diagnostic observation = %+v, err=%v retained=%+v", observation, err, retained)
	}
}

func TestLiveExecutorRetainsDiagnosticAudioForMissingMalformedAndDuplicateStillEvidence(t *testing.T) {
	t.Chdir("../../..")
	item := scenario.Suite()[9]
	key := AttemptKey{CaseOrdinal: 10, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	tests := []struct {
		name string
		play func(*testing.T) scenarioPlay
	}{
		{
			name: "missing successful sends",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					captureFixtureAudio(t, session)
					return scenario.Result{Scenario: item.Name, Passed: true}, errors.New("send failed")
				}
			},
		},
		{
			name: "malformed sent event",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					captureFixtureAudio(t, session)
					err := session.CaptureScheduled(bench.SessionScheduledCapture{
						AtMS: item.Sees[0].AtMS, Name: "scenario.sight.1",
						EventType: "conversation.item.create", EventJSON: []byte(`{"type":"conversation.item.create"}`),
					})
					return scenario.Result{Scenario: item.Name}, err
				}
			},
		},
		{
			name: "response invocation before image",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					captureFixtureAudio(t, session)
					err := session.CaptureScheduled(scheduledResponseCreateFixture(item, 0))
					return scenario.Result{Scenario: item.Name}, err
				}
			},
		},
		{
			name: "missing response invocation",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					captureFixtureAudio(t, session)
					capture := scheduledFixtureCapture(t, item, 0)
					if err := session.CaptureScheduled(capture); err != nil {
						return scenario.Result{Scenario: item.Name}, err
					}
					return scenario.Result{Scenario: item.Name, Transcript: bench.Transcript{Moments: []bench.Moment{{
						Kind: bench.MomentScheduled, Name: capture.Name,
					}}}}, nil
				}
			},
		},
		{
			name: "malformed response invocation",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					captureFixtureAudio(t, session)
					if err := session.CaptureScheduled(scheduledFixtureCapture(t, item, 0)); err != nil {
						return scenario.Result{Scenario: item.Name}, err
					}
					capture := scheduledResponseCreateFixture(item, 0)
					capture.EventJSON = []byte(`{"type":"response.create","response":{"modalities":["audio"]}}`)
					err := session.CaptureScheduled(capture)
					return scenario.Result{Scenario: item.Name}, err
				}
			},
		},
		{
			name: "duplicate sent identity",
			play: func(t *testing.T) scenarioPlay {
				return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
					result := successfulLiveFixture(t, session, item)
					capture := scheduledFixtureCapture(t, item, 0)
					err := session.CaptureScheduled(capture)
					return result, err
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var retainCalls atomic.Int32
			var retained AttemptCapture
			config := liveExecutorFixture(func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
				retainCalls.Add(1)
				retained = cloneAttemptCaptureForTest(t, capture)
				return liveMediaReference(capture.Key, submittedReceipts(capture.Submitted)), nil
			})
			executor, err := newLiveExecutor(config, test.play(t))
			if err != nil {
				t.Fatal(err)
			}
			observation, err := executor.execute(t.Context(), key, item)
			if err == nil || retainCalls.Load() != 1 ||
				retained.RunSucceeded || retained.Audio.SampleRateHz != 24_000 ||
				len(retained.Audio.RoomPCM16) == 0 {
				t.Fatalf("failed still evidence observation=%+v err=%v retain=%d",
					observation, err, retainCalls.Load())
			}
		})
	}
}

func TestLiveExecutorRejectsRetentionReceiptDrift(t *testing.T) {
	t.Chdir("../../..")
	item := scenario.Suite()[9]
	key := AttemptKey{CaseOrdinal: 10, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	tests := []struct {
		name   string
		retain AttemptRetainer
	}{
		{
			name: "changed submitted digest",
			retain: func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
				receipts := submittedReceipts(capture.Submitted)
				receipts[0].SHA256 = liveDigest("forged")
				return liveMediaReference(capture.Key, receipts), nil
			},
		},
		{
			name: "missing submitted input",
			retain: func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
				return liveMediaReference(capture.Key, submittedReceipts(capture.Submitted[:1])), nil
			},
		},
		{
			name: "invalid completion receipt",
			retain: func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
				return MediaReference{Handle: "attempt", ManifestSHA256: "not-a-digest",
					Submitted: submittedReceipts(capture.Submitted)}, nil
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, err := newLiveExecutor(liveExecutorFixture(test.retain), successfulLivePlay(t))
			if err != nil {
				t.Fatal(err)
			}
			observation, err := executor.execute(t.Context(), key, item)
			if err == nil || observation.Media != nil {
				t.Fatalf("drifted receipt observation=%+v err=%v", observation, err)
			}
		})
	}
}

func TestLiveExecutorRejectsEveryTaskOwnedSessionOverrideBeforePlugins(t *testing.T) {
	base := liveExecutorFixture(func(context.Context, AttemptCapture) (MediaReference, error) {
		return MediaReference{}, nil
	})
	tests := []struct {
		name   string
		mutate func(*LiveExecutorConfig)
	}{
		{"instructions", func(config *LiveExecutorConfig) { config.Session.Instructions = "override" }},
		{"tools", func(config *LiveExecutorConfig) { config.Session.Tools = []json.RawMessage{json.RawMessage(`{}`)} }},
		{"respond", func(config *LiveExecutorConfig) {
			config.Session.Respond = func(string, json.RawMessage) (json.RawMessage, error) { return nil, nil }
		}},
		{"handle tool", func(config *LiveExecutorConfig) {
			config.Session.HandleTool = func(context.Context, bench.ToolRequest) (json.RawMessage, error) { return nil, nil }
		}},
		{"concurrent tools", func(config *LiveExecutorConfig) { config.Session.ConcurrentTools = true }},
		{"realtime", func(config *LiveExecutorConfig) { config.Session.Realtime = true }},
		{"trailing silence", func(config *LiveExecutorConfig) { config.Session.TrailingSilence = time.Millisecond }},
		{"audio capture", func(config *LiveExecutorConfig) {
			config.Session.CaptureAudio = func(bench.SessionAudioCapture) error { return nil }
		}},
		{"video capture", func(config *LiveExecutorConfig) {
			config.Session.CaptureVideo = func(bench.SessionVideoCapture) error { return nil }
		}},
		{"scheduled capture", func(config *LiveExecutorConfig) {
			config.Session.CaptureScheduled = func(bench.SessionScheduledCapture) error { return nil }
		}},
		{"scheduled events", func(config *LiveExecutorConfig) { config.Session.Scheduled = []bench.ScheduledEvent{{}} }},
		{"video", func(config *LiveExecutorConfig) { config.Session.Video = []bench.VideoStream{{}} }},
		{"ready", func(config *LiveExecutorConfig) { config.Session.Ready = func(context.Context) error { return nil } }},
		{"scope", func(config *LiveExecutorConfig) { config.Session.AttestationScope = "wrong" }},
		{"transport", func(config *LiveExecutorConfig) { config.Session.Transport = "other" }},
		{"negative timeout", func(config *LiveExecutorConfig) { config.Session.Timeout = -time.Second }},
		{"nil attestor", func(config *LiveExecutorConfig) { config.Session.RuntimeAttestor = nil }},
		{"typed nil attestor", func(config *LiveExecutorConfig) {
			config.Session.RuntimeAttestor = bench.RuntimeAttestorFunc(nil)
		}},
		{"nil voice", func(config *LiveExecutorConfig) { config.Voice = nil }},
		{"typed nil voice", func(config *LiveExecutorConfig) {
			config.Voice = (*typedNilLiveVoiceFixture)(nil)
		}},
		{"nil retainer", func(config *LiveExecutorConfig) { config.Retain = nil }},
		{"nil evidence context", func(config *LiveExecutorConfig) { config.EvidenceContext = nil }},
		{"done evidence context", func(config *LiveExecutorConfig) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			config.EvidenceContext = ctx
		}},
		{"zero evidence timeout", func(config *LiveExecutorConfig) { config.EvidenceTimeout = 0 }},
		{"long evidence timeout", func(config *LiveExecutorConfig) { config.EvidenceTimeout = 3 * time.Minute }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := base
			test.mutate(&config)
			var plays atomic.Int32
			_, err := newLiveExecutor(config, func(context.Context, scenario.Voice, bench.SessionConfig, scenario.Scenario) (scenario.Result, error) {
				plays.Add(1)
				return scenario.Result{}, nil
			})
			if err == nil || plays.Load() != 0 {
				t.Fatalf("override constructor error=%v plays=%d", err, plays.Load())
			}
		})
	}
}

func TestLiveExecutorRejectsKeyFixtureAndCancellationWithoutRetention(t *testing.T) {
	item := scenario.Suite()[10]
	valid := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	var plays, retains atomic.Int32
	config := liveExecutorFixture(func(context.Context, AttemptCapture) (MediaReference, error) {
		retains.Add(1)
		return MediaReference{}, nil
	})
	executor, err := newLiveExecutor(config, func(context.Context, scenario.Voice, bench.SessionConfig, scenario.Scenario) (scenario.Result, error) {
		plays.Add(1)
		return scenario.Result{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	drifted := item
	drifted.Instructions = "changed"
	if _, err := executor.execute(t.Context(), valid, drifted); err == nil {
		t.Fatal("drifted fixture passed live executor")
	}
	wrong := valid
	wrong.TaskID = "another-task"
	if _, err := executor.execute(t.Context(), wrong, item); err == nil {
		t.Fatal("drifted key passed live executor")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := executor.execute(ctx, valid, item); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled executor error = %v", err)
	}
	if plays.Load() != 0 || retains.Load() != 0 {
		t.Fatalf("rejected live attempts invoked play=%d retain=%d", plays.Load(), retains.Load())
	}
}

func TestLiveExecutorCancellationAfterRetentionReturnsCompletedReceipt(t *testing.T) {
	item := scenario.Suite()[10]
	key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	ctx, cancel := context.WithCancel(t.Context())
	config := liveExecutorFixture(func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
		cancel()
		return liveMediaReference(capture.Key, nil), nil
	})
	executor, err := newLiveExecutor(config, successfulLivePlay(t))
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executor.execute(ctx, key, item)
	if !errors.Is(err, context.Canceled) || observation.Media == nil {
		t.Fatalf("canceled retention observation=%+v err=%v", observation, err)
	}
}

func TestLiveExecutorCancellationAfterPlayRetainsCapturedEvidence(t *testing.T) {
	item := scenario.Suite()[10]
	key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	ctx, cancel := context.WithCancel(t.Context())
	var retains atomic.Int32
	executor, err := newLiveExecutor(liveExecutorFixture(func(
		_ context.Context, capture AttemptCapture,
	) (MediaReference, error) {
		retains.Add(1)
		if capture.RunSucceeded {
			t.Fatal("canceled attempt was promoted to a successful retention capture")
		}
		return liveMediaReference(capture.Key, nil), nil
	}), func(
		_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		if err := session.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1},
		}); err != nil {
			return scenario.Result{Scenario: item.Name}, err
		}
		cancel()
		return scenario.Result{Scenario: item.Name, Passed: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executor.execute(ctx, key, item)
	if !errors.Is(err, context.Canceled) || observation.Media == nil || retains.Load() != 1 {
		t.Fatalf("post-play cancellation observation=%+v err=%v retains=%d",
			observation, err, retains.Load())
	}
}

func TestLiveExecutorSuiteCancellationStopsEvidencePlugin(t *testing.T) {
	item := scenario.Suite()[10]
	key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
	evidenceContext, cancelEvidence := context.WithCancel(t.Context())
	var retains atomic.Int32
	config := liveExecutorFixture(func(
		context.Context, AttemptCapture,
	) (MediaReference, error) {
		retains.Add(1)
		return MediaReference{}, errors.New("suite-canceled retainer must not run")
	})
	config.EvidenceContext = evidenceContext
	executor, err := newLiveExecutor(config, func(
		_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		captureFixtureAudio(t, session)
		cancelEvidence()
		return scenario.Result{Scenario: item.Name}, errors.New("fixture run failed")
	})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executor.execute(t.Context(), key, item)
	if !errors.Is(err, context.Canceled) || observation.Media != nil || retains.Load() != 0 {
		t.Fatalf("suite cancellation observation=%+v err=%v retains=%d",
			observation, err, retains.Load())
	}
}

func TestLiveExecutorQueuedCancellationDoesNotInvokeSecondAttempt(t *testing.T) {
	item := scenario.Suite()[10]
	entered := make(chan struct{})
	release := make(chan struct{})
	var plays atomic.Int32
	executor, err := newLiveExecutor(liveExecutorFixture(func(
		_ context.Context, capture AttemptCapture,
	) (MediaReference, error) {
		return liveMediaReference(capture.Key, nil), nil
	}), func(
		ctx context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario,
	) (scenario.Result, error) {
		if plays.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return scenario.Result{Scenario: item.Name}, context.Cause(ctx)
			}
		}
		if err := session.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1},
		}); err != nil {
			return scenario.Result{Scenario: item.Name}, err
		}
		return scenario.Result{Scenario: item.Name, Passed: true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 1, TaskID: item.Name + "#1"}
		_, err := executor.execute(t.Context(), key, item)
		firstDone <- err
	}()
	<-entered
	queued, cancel := context.WithCancel(t.Context())
	cancel()
	key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: 2, TaskID: item.Name + "#2"}
	observation, queuedErr := executor.execute(queued, key, item)
	close(release)
	firstErr := <-firstDone
	if !errors.Is(queuedErr, context.Canceled) || observation.Media != nil || firstErr != nil ||
		plays.Load() != 1 {
		t.Fatalf("queued cancellation observation=%+v err=%v first=%v plays=%d",
			observation, queuedErr, firstErr, plays.Load())
	}
}

func TestLiveExecutorSerializesSharedPlugins(t *testing.T) {
	item := scenario.Suite()[10]
	var active, maximum atomic.Int32
	play := func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		if err := session.CaptureAudio(bench.SessionAudioCapture{
			SampleRateHz: 24_000, RoomPCM16: []int16{1, 2, 3, 4},
		}); err != nil {
			return scenario.Result{Scenario: item.Name}, err
		}
		return scenario.Result{Scenario: item.Name, Passed: true}, nil
	}
	executor, err := newLiveExecutor(liveExecutorFixture(func(_ context.Context, capture AttemptCapture) (MediaReference, error) {
		return liveMediaReference(capture.Key, nil), nil
	}), play)
	if err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	for trial := 1; trial <= 2; trial++ {
		trial := trial
		wait.Add(1)
		go func() {
			defer wait.Done()
			key := AttemptKey{CaseOrdinal: 11, CaseName: item.Name, Trial: trial,
				TaskID: item.Name + "#" + strconv.Itoa(trial)}
			_, err := executor.execute(t.Context(), key, item)
			errorsSeen <- err
		}()
	}
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Fatalf("concurrent live execution error = %v", err)
		}
	}
	if maximum.Load() != 1 {
		t.Fatalf("shared plug-in concurrency = %d, want 1", maximum.Load())
	}
}

func liveExecutorFixture(retain AttemptRetainer) LiveExecutorConfig {
	return LiveExecutorConfig{
		Voice: liveVoiceFixture{},
		Session: bench.SessionConfig{
			Endpoint: "ws://fixture.invalid/v1/realtime",
			RuntimeAttestor: bench.RuntimeAttestorFunc(func(context.Context, bench.AttestationRequest) (bench.ExecutionEvidence, error) {
				return bench.ExecutionEvidence{}, errors.New("fixture attestor should not be invoked")
			}),
		},
		Retain: retain, EvidenceContext: context.Background(), EvidenceTimeout: time.Second,
	}
}

type liveVoiceFixture struct{}

func (liveVoiceFixture) Speak(context.Context, string, string) ([]int16, error) {
	return []int16{1}, nil
}

type typedNilLiveVoiceFixture struct{}

func (*typedNilLiveVoiceFixture) Speak(context.Context, string, string) ([]int16, error) {
	return []int16{1}, nil
}

func successfulLivePlay(t *testing.T) scenarioPlay {
	t.Helper()
	return func(_ context.Context, _ scenario.Voice, session bench.SessionConfig, item scenario.Scenario) (scenario.Result, error) {
		return successfulLiveFixture(t, session, item), nil
	}
}

func successfulLiveFixture(t *testing.T, session bench.SessionConfig, item scenario.Scenario) scenario.Result {
	t.Helper()
	captureFixtureAudio(t, session)
	transcript := bench.Transcript{}
	for index := range item.Sees {
		input := scheduledFixtureCapture(t, item, index)
		if err := session.CaptureScheduled(input); err != nil {
			t.Fatalf("capture scheduled fixture: %v", err)
		}
		transcript.Moments = append(transcript.Moments, bench.Moment{
			Kind: bench.MomentScheduled, Name: input.Name,
		})
		create := scheduledResponseCreateFixture(item, index)
		if err := session.CaptureScheduled(create); err != nil {
			t.Fatalf("capture scheduled response fixture: %v", err)
		}
		transcript.Moments = append(transcript.Moments, bench.Moment{
			Kind: bench.MomentScheduled, Name: create.Name,
		})
	}
	return scenario.Result{Scenario: item.Name, Passed: true, Transcript: transcript}
}

func captureFixtureAudio(t *testing.T, session bench.SessionConfig) {
	t.Helper()
	if session.CaptureAudio == nil {
		t.Fatal("live fixture received no audio capture")
	}
	if err := session.CaptureAudio(bench.SessionAudioCapture{
		SampleRateHz: 24_000, RoomPCM16: []int16{1, 2, 3, 4},
		Agent: []bench.TimedAudioChunk{{AtMS: 0, PCM16: []int16{5, 6}}},
	}); err != nil {
		t.Fatalf("capture audio fixture: %v", err)
	}
}

func scheduledFixtureCapture(
	t *testing.T, item scenario.Scenario, index int,
) bench.SessionScheduledCapture {
	t.Helper()
	payload, err := os.ReadFile(item.Sees[index].Path)
	if err != nil {
		t.Fatal(err)
	}
	event, err := json.Marshal(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{
				"type":      "input_image",
				"image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(payload),
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return bench.SessionScheduledCapture{
		AtMS: item.Sees[index].AtMS, Name: "scenario.sight." + strconv.Itoa(index+1),
		EventType: "conversation.item.create", EventJSON: event,
	}
}

func scheduledResponseCreateFixture(
	item scenario.Scenario, index int,
) bench.SessionScheduledCapture {
	return bench.SessionScheduledCapture{
		AtMS:      item.Sees[index].AtMS,
		Name:      "scenario.sight." + strconv.Itoa(index+1) + ".response-create",
		EventType: "response.create", EventJSON: []byte(`{"type":"response.create"}`),
	}
}

func liveMediaReference(key AttemptKey, submitted []SubmittedInputReceipt) MediaReference {
	return MediaReference{
		Handle: "attempt/" + key.TaskID, ManifestSHA256: liveDigest("media/" + key.TaskID),
		Submitted: slices.Clone(submitted),
	}
}

func liveDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(digest[:])
}

func cloneAttemptCaptureForTest(t *testing.T, capture AttemptCapture) AttemptCapture {
	t.Helper()
	payload, err := json.Marshal(capture.Result)
	if err != nil {
		t.Fatal(err)
	}
	var result scenario.Result
	if err := json.Unmarshal(payload, &result); err != nil {
		t.Fatal(err)
	}
	capture.Result = result
	capture.Audio = cloneSessionAudio(capture.Audio)
	capture.Submitted = cloneSubmittedCaptures(capture.Submitted)
	return capture
}
