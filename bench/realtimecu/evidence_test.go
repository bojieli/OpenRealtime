package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
	binding "github.com/bojieli/OpenRealtime/binding"
)

type fixtureEvidencePlugin struct {
	begin  func(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	finish func(context.Context, bench.Result) error
	close  func() error
}

func (plugin fixtureEvidencePlugin) BeginAttempt(
	ctx context.Context, attempt EvidenceAttempt,
) (AttemptEvidence, error) {
	return plugin.begin(ctx, attempt)
}

func (plugin fixtureEvidencePlugin) FinishSuite(ctx context.Context, result bench.Result) error {
	return plugin.finish(ctx, result)
}

func (plugin fixtureEvidencePlugin) Close() error {
	if plugin.close == nil {
		return nil
	}
	return plugin.close()
}

type fixtureAttemptEvidence struct {
	captureAudio func(bench.SessionAudioCapture) error
	captureVideo func(bench.SessionVideoCapture) error
	complete     func(context.Context, EvidenceCompletion) error
	abort        func() error
}

func (attempt fixtureAttemptEvidence) CaptureAudio(capture bench.SessionAudioCapture) error {
	return attempt.captureAudio(capture)
}

func (attempt fixtureAttemptEvidence) CaptureVideo(capture bench.SessionVideoCapture) error {
	return attempt.captureVideo(capture)
}

func (attempt fixtureAttemptEvidence) Complete(
	ctx context.Context, completion EvidenceCompletion,
) error {
	return attempt.complete(ctx, completion)
}

func (attempt fixtureAttemptEvidence) Abort() error { return attempt.abort() }

func TestEvidenceAttemptCoversExactlySixteenAuthoredCases(t *testing.T) {
	cases, err := Select(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 16 {
		t.Fatalf("authored cases = %d, want 16", len(cases))
	}
	origin := EvidenceRunOrigin{
		Kind: EvidenceOriginProduction, Live: true, Transport: bench.TransportWebSocket,
		EndpointSHA256: endpointIdentity("ws://127.0.0.1:8765/v1/realtime"),
	}
	seen := make(map[string]struct{}, len(cases))
	for _, item := range cases {
		attempt := EvidenceAttempt{
			Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
			Grounding: item.Grounding, Origin: origin,
			Observers: []string{"fixture.graph-native-observer"},
		}
		if err := attempt.validate(); err != nil {
			t.Fatalf("validate %s: %v", item.ID(), err)
		}
		seen[item.ID()] = struct{}{}
	}
	if len(seen) != 16 {
		t.Fatalf("unique evidence cases = %d, want 16", len(seen))
	}

	drifted := EvidenceAttempt{
		Suite: SuiteName, Case: cases[0].ID(), Trial: 1, Task: cloneCase(cases[0]).Task,
		Grounding: cases[0].Grounding, Origin: origin,
		Observers: []string{"fixture.graph-native-observer"},
	}
	drifted.Task.Deadline++
	if err := drifted.validate(); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("drifted evidence attempt error = %v", err)
	}
}

func TestRunCaseReturnsActionBudgetFailureThroughProtocolErrorChannel(t *testing.T) {
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	item.Task.MaxActions = 1
	started := time.Unix(1_000, 0)
	secondFailed := false
	environment := realtimeCURunEnvironment{episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
		return realtimeCURunEpisode{
			ready: func(context.Context) error { return nil }, started: func() time.Time { return started },
			surface:       &fixtureRunSurface{},
			captureScreen: func(context.Context) ([]byte, error) { return fixturePNG, nil },
			captureCamera: func(context.Context) ([]byte, error) { return fixturePNG, nil },
			result: func(context.Context) (PageResult, error) {
				return PageResult{Reason: "fixture action budget terminal"}, nil
			},
		}, nil
	}}
	outcome, evidenceErr := runCase(context.Background(), environment, Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
		FrameRate: 3, Timeout: 10 * time.Second,
		dependencies: &runDependencies{
			playSamples: func(ctx context.Context, config bench.SessionConfig, _ []int16) (bench.Transcript, error) {
				if err := config.Ready(ctx); err != nil {
					return bench.Transcript{}, err
				}
				arguments := json.RawMessage(`{"source":"screen","x":10,"y":20}`)
				if output, err := config.HandleTool(ctx, bench.ToolRequest{
					CallID: "action-1", Name: "computer.click", Arguments: arguments,
					Received: started.Add(time.Second),
				}); err != nil || len(output) == 0 {
					t.Fatalf("first action output=%s error=%v", output, err)
				}
				if output, err := config.HandleTool(ctx, bench.ToolRequest{
					CallID: "action-2", Name: "computer.click", Arguments: arguments,
					Received: started.Add(2 * time.Second),
				}); err == nil || len(output) != 0 ||
					!strings.Contains(err.Error(), "task action budget of 1 is exhausted") {
					t.Fatalf("second action output=%s error=%v", output, err)
				} else {
					secondFailed = true
				}
				return bench.Transcript{PlaybackMS: 3_000, Moments: []bench.Moment{
					{Kind: bench.MomentReady, AtMS: 0},
					{Kind: bench.MomentToolCall, CallID: "action-1", Name: "computer.click", AtMS: 1_000},
					{Kind: bench.MomentToolCall, CallID: "action-2", Name: "computer.click", AtMS: 2_000},
				}}, nil
			},
			now: func() time.Time { return started.Add(3 * time.Second) },
		},
	}, item)
	if evidenceErr != nil || !secondFailed || !outcome.Completed ||
		outcome.Metrics["action_count"] != 2 || outcome.Metrics["invalid_action_count"] != 1 ||
		!strings.Contains(outcome.Notes["actions"], "task action budget of 1 is exhausted") {
		t.Fatalf("outcome=%+v evidence=%v second_failed=%t", outcome, evidenceErr, secondFailed)
	}
}

func TestLiveEvidenceRejectsUnspecifiedObserverSelection(t *testing.T) {
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	attempt := EvidenceAttempt{
		Suite: SuiteName, Case: item.ID(), Trial: 1, Task: cloneCase(item).Task,
		Grounding: item.Grounding,
		Origin: EvidenceRunOrigin{
			Kind: EvidenceOriginProduction, Live: true, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://fixture.invalid/v1/realtime"),
		},
	}
	if err := attempt.validate(); err == nil || !strings.Contains(err.Error(), "observer") {
		t.Fatalf("unspecified live observer selection error = %v", err)
	}
}

func TestLiveEvidenceRequiresExactNegotiatedObserverSelection(t *testing.T) {
	origin := EvidenceRunOrigin{
		Kind: EvidenceOriginProduction, Live: true, Transport: bench.TransportWebSocket,
		EndpointSHA256: endpointIdentity("ws://fixture.invalid/v1/realtime"),
	}
	requested := []string{"fixture.graph-native-observer"}
	for _, transcript := range []bench.Transcript{
		{},
		{Runtime: &binding.Status{Observers: []string{"different"}}},
	} {
		if err := validateNegotiatedObservers(origin, requested, transcript); err == nil {
			t.Fatalf("observer drift was accepted: %+v", transcript.Runtime)
		}
	}
	if err := validateNegotiatedObservers(origin, requested, bench.Transcript{
		Runtime: &binding.Status{Observers: slices.Clone(requested)},
	}); err != nil {
		t.Fatal(err)
	}
	observerErr := validateNegotiatedObservers(origin, requested, bench.Transcript{})
	combined, tolerated := realtimeCUPlaybackFailure(bench.ErrConversationTimeout, observerErr)
	if tolerated || !errors.Is(combined, bench.ErrConversationTimeout) ||
		!strings.Contains(combined.Error(), "observer") {
		t.Fatalf("timeout with missing observer proof combined=%v tolerated=%t", combined, tolerated)
	}
	combined, tolerated = realtimeCUPlaybackFailure(bench.ErrConversationTimeout, nil)
	if !tolerated || !errors.Is(combined, bench.ErrConversationTimeout) {
		t.Fatalf("attested conversation timeout combined=%v tolerated=%t", combined, tolerated)
	}
}

func TestRunnerRejectsCaptureRateThatDriftsFromCellIdentity(t *testing.T) {
	_, err := Run(t.Context(), Options{
		Endpoint: "ws://fixture.invalid/v1/realtime", Cell: ReferenceCell(), FrameRate: 10,
		Observers: []string{"fixture.graph-native-observer"},
	})
	if err == nil || !strings.Contains(err.Error(), "capture is 10fps") {
		t.Fatalf("capture-rate drift error = %v", err)
	}
}

func TestEvidenceAttemptPrecedesEpisodeAndRefusalStopsExecution(t *testing.T) {
	want := errors.New("fixture evidence refusal")
	episodeCalls := 0
	plugin := fixtureEvidencePlugin{
		begin: func(_ context.Context, attempt EvidenceAttempt) (AttemptEvidence, error) {
			if attempt.Case != "static-control/pixel" || attempt.Origin.Kind != EvidenceOriginHermetic ||
				attempt.Origin.Live {
				t.Fatalf("evidence attempt = %+v", attempt)
			}
			return nil, want
		},
		finish: func(context.Context, bench.Result) error { return nil },
	}
	environment := realtimeCURunEnvironment{episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
		episodeCalls++
		return realtimeCURunEpisode{}, nil
	}}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	outcome, _ := runCase(context.Background(), environment, Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
		FrameRate: 3, Timeout: time.Second, Evidence: plugin,
		dependencies: &runDependencies{playSamples: bench.PlaySamples, now: time.Now},
		evidenceOrigin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Live: false, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
		},
	}, item)
	if episodeCalls != 0 || outcome.Completed || !strings.Contains(outcome.Error, want.Error()) {
		t.Fatalf("runCase outcome=%+v episode calls=%d", outcome, episodeCalls)
	}
}

func TestRunFinishesEvidenceOnEnvironmentFailure(t *testing.T) {
	want := errors.New("environment unavailable")
	finishCalls, closeCalls := 0, 0
	plugin := fixtureEvidencePlugin{
		begin: func(context.Context, EvidenceAttempt) (AttemptEvidence, error) {
			t.Fatal("attempt began after environment creation failed")
			return nil, nil
		},
		finish: func(_ context.Context, result bench.Result) error {
			finishCalls++
			if result.Suite != SuiteName || result.Expected != 16 || len(result.Tasks) != 0 ||
				result.Summary.Complete {
				t.Fatalf("finished result = %+v", result)
			}
			return nil
		},
		close: func() error { closeCalls++; return nil },
	}
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Evidence: plugin,
		dependencies: &runDependencies{
			newEnvironment: func(context.Context, EnvironmentConfig) (realtimeCURunEnvironment, error) {
				return realtimeCURunEnvironment{}, want
			},
			playSamples: bench.PlaySamples, now: time.Now,
		},
	})
	if !errors.Is(err, want) || finishCalls != 1 || closeCalls != 1 || result.Summary.Complete {
		t.Fatalf("Run result=%+v error=%v finish calls=%d close calls=%d",
			result, err, finishCalls, closeCalls)
	}
}

func TestHermeticRunnerWiresExactMediaAndFreezesEvidence(t *testing.T) {
	surface := &fixtureRunSurface{}
	started := time.Unix(100, 0)
	audioCalls, videoCalls, completeCalls, abortCalls, finishCalls := 0, 0, 0, 0, 0
	plugin := fixtureEvidencePlugin{
		begin: func(_ context.Context, attempt EvidenceAttempt) (AttemptEvidence, error) {
			if attempt.Origin.Kind != EvidenceOriginHermetic || attempt.Origin.Live ||
				attempt.Origin.Transport != bench.TransportWebSocket {
				t.Fatalf("hermetic origin = %+v", attempt.Origin)
			}
			return fixtureAttemptEvidence{
				captureAudio: func(capture bench.SessionAudioCapture) error {
					audioCalls++
					if capture.SampleRateHz != 24_000 || len(capture.RoomPCM16) == 0 {
						t.Fatalf("audio capture = %+v", capture)
					}
					return nil
				},
				captureVideo: func(capture bench.SessionVideoCapture) error {
					videoCalls++
					if capture.Source != "screen" || capture.MediaType != "image/png" || len(capture.Data) == 0 {
						t.Fatalf("video capture = %+v", capture)
					}
					return nil
				},
				complete: func(_ context.Context, completion EvidenceCompletion) error {
					completeCalls++
					if !completion.Outcome.Completed || !completion.Outcome.Passed ||
						completion.Page.Reason != "fixture success" || completion.Attempt.Case != "static-control/pixel" {
						t.Fatalf("completion = %+v", completion)
					}
					completion.Outcome.Metrics["task_success_rate"] = 0
					completion.Transcript.Moments[0].Kind = "mutated"
					return nil
				},
				abort: func() error { abortCalls++; return nil },
			}, nil
		},
		finish: func(_ context.Context, result bench.Result) error {
			finishCalls++
			if len(result.Tasks) != 1 || result.Tasks[0].Metrics["task_success_rate"] != 1 ||
				result.Tasks[0].Passed != true {
				t.Fatalf("suite evidence was mutated by attempt callback: %+v", result.Tasks)
			}
			result.Tasks[0].Passed = false
			return nil
		},
	}
	dependencies := &runDependencies{
		newEnvironment: func(context.Context, EnvironmentConfig) (realtimeCURunEnvironment, error) {
			return realtimeCURunEnvironment{
				episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
					return realtimeCURunEpisode{
						ready: func(context.Context) error { return nil }, started: func() time.Time { return started },
						surface:       surface,
						captureScreen: func(context.Context) ([]byte, error) { return fixturePNG, nil },
						captureCamera: func(context.Context) ([]byte, error) { return fixturePNG, nil },
						result: func(context.Context) (PageResult, error) {
							return PageResult{Complete: true, Success: true, Reason: "fixture success", CompletedAtMS: 4000}, nil
						},
					}, nil
				},
				close: func() error { return nil },
			}, nil
		},
		playSamples: func(ctx context.Context, config bench.SessionConfig, samples []int16) (bench.Transcript, error) {
			if config.CaptureAudio == nil || config.CaptureVideo == nil || len(config.Video) != 1 ||
				config.Video[0].Source != "screen" ||
				!slices.Equal(config.Observers, []string{"fixture.graph-native-observer"}) {
				t.Fatalf("session evidence callbacks = %+v", config)
			}
			if err := config.Ready(ctx); err != nil {
				return bench.Transcript{}, err
			}
			if err := config.CaptureVideo(bench.SessionVideoCapture{
				Source: "screen", Width: 1280, Height: 720, MediaType: "image/png",
				WireTimestamp: 100, EpisodeAtMS: 10, Data: append([]byte(nil), fixturePNG...),
			}); err != nil {
				return bench.Transcript{}, err
			}
			if err := config.CaptureAudio(bench.SessionAudioCapture{
				SampleRateHz: 24_000, RoomPCM16: append([]int16(nil), samples...),
			}); err != nil {
				return bench.Transcript{}, err
			}
			return bench.Transcript{PlaybackMS: 5000, Moments: []bench.Moment{
				{Kind: bench.MomentReady, AtMS: 0},
				{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 10},
				{Kind: bench.MomentToolCall, Name: "computer.click", AtMS: 4000},
			}}, nil
		},
		now: func() time.Time { return started.Add(4 * time.Second) },
	}
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
		Observers:  []string{"fixture.graph-native-observer"},
		Groundings: []Grounding{GroundingPixel}, Categories: []string{"control"}, Limit: 1,
		FrameRate: 3, Timeout: 10 * time.Second, Evidence: plugin, dependencies: dependencies,
	})
	if err != nil {
		t.Fatal(err)
	}
	if audioCalls != 1 || videoCalls != 1 || completeCalls != 1 || abortCalls != 0 || finishCalls != 1 {
		t.Fatalf("callback counts audio=%d video=%d complete=%d abort=%d finish=%d",
			audioCalls, videoCalls, completeCalls, abortCalls, finishCalls)
	}
	if len(result.Tasks) != 1 || !result.Tasks[0].Passed || result.Tasks[0].Metrics["task_success_rate"] != 1 {
		t.Fatalf("Run result was mutated by evidence plug-in: %+v", result)
	}
	if result.Summary.Complete || result.Expected != 16 {
		t.Fatalf("hermetic filtered run became complete: %+v", result.Summary)
	}
}

func TestEvidenceCompleteFailureFailsClosedAndAborts(t *testing.T) {
	want := errors.New("retain evidence failed")
	aborts := 0
	plugin := fixtureEvidencePlugin{
		begin: func(context.Context, EvidenceAttempt) (AttemptEvidence, error) {
			return fixtureAttemptEvidence{
				captureAudio: func(bench.SessionAudioCapture) error { return nil },
				captureVideo: func(bench.SessionVideoCapture) error { return nil },
				complete:     func(context.Context, EvidenceCompletion) error { return want },
				abort:        func() error { aborts++; return nil },
			}, nil
		},
		finish: func(context.Context, bench.Result) error { return nil },
	}
	item := Case{Task: Suite()[0], Grounding: GroundingPixel}
	started := time.Now()
	environment := realtimeCURunEnvironment{episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
		return realtimeCURunEpisode{
			ready: func(context.Context) error { return nil }, started: func() time.Time { return started },
			surface:       &fixtureRunSurface{},
			captureScreen: func(context.Context) ([]byte, error) { return fixturePNG, nil },
			captureCamera: func(context.Context) ([]byte, error) { return fixturePNG, nil },
			result:        func(context.Context) (PageResult, error) { return PageResult{}, errors.New("fixture setup failure") },
		}, nil
	}}
	outcome, evidenceErr := runCase(context.Background(), environment, Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
		FrameRate: 3, Timeout: time.Second, Evidence: plugin,
		dependencies: &runDependencies{
			playSamples: func(context.Context, bench.SessionConfig, []int16) (bench.Transcript, error) {
				return bench.Transcript{}, errors.New("fixture transport failure")
			},
			now: time.Now,
		},
		evidenceOrigin: EvidenceRunOrigin{
			Kind: EvidenceOriginHermetic, Transport: bench.TransportWebSocket,
			EndpointSHA256: endpointIdentity("ws://hermetic.invalid/v1/realtime"),
		},
	}, item)
	if outcome.Completed || outcome.Passed || aborts != 1 ||
		!strings.Contains(outcome.Error, "fixture transport failure") ||
		strings.Contains(outcome.Error, want.Error()) || !errors.Is(evidenceErr, want) {
		t.Fatalf("outcome=%+v evidence error=%v aborts=%d", outcome, evidenceErr, aborts)
	}
}

func TestEvidenceCaptureFailureDoesNotRewriteDeterministicOutcome(t *testing.T) {
	want := errors.New("fixture evidence capture failed")
	aborts, completes := 0, 0
	plugin := fixtureEvidencePlugin{
		begin: func(context.Context, EvidenceAttempt) (AttemptEvidence, error) {
			return fixtureAttemptEvidence{
				captureAudio: func(bench.SessionAudioCapture) error { return nil },
				captureVideo: func(bench.SessionVideoCapture) error { return want },
				complete: func(context.Context, EvidenceCompletion) error {
					completes++
					return nil
				},
				abort: func() error { aborts++; return nil },
			}, nil
		},
		finish: func(_ context.Context, result bench.Result) error {
			if len(result.Tasks) != 1 || !result.Tasks[0].Passed || result.Tasks[0].Error != "" {
				t.Fatalf("capture failure rewrote frozen deterministic result: %+v", result.Tasks)
			}
			return nil
		},
	}
	started := time.Unix(200, 0)
	dependencies := &runDependencies{
		newEnvironment: func(context.Context, EnvironmentConfig) (realtimeCURunEnvironment, error) {
			return realtimeCURunEnvironment{
				episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
					return realtimeCURunEpisode{
						ready:   func(context.Context) error { return nil },
						started: func() time.Time { return started }, surface: &fixtureRunSurface{},
						captureScreen: func(context.Context) ([]byte, error) { return fixturePNG, nil },
						captureCamera: func(context.Context) ([]byte, error) { return fixturePNG, nil },
						result: func(context.Context) (PageResult, error) {
							return PageResult{
								Complete: true, Success: true, Reason: "fixture success", CompletedAtMS: 4_000,
							}, nil
						},
					}, nil
				},
				close: func() error { return nil },
			}, nil
		},
		playSamples: func(ctx context.Context, config bench.SessionConfig, _ []int16) (bench.Transcript, error) {
			if err := config.Ready(ctx); err != nil {
				return bench.Transcript{}, err
			}
			if err := config.CaptureVideo(bench.SessionVideoCapture{
				Source: "screen", Width: 1280, Height: 720, MediaType: "image/png",
				WireTimestamp: 1, EpisodeAtMS: 10, Data: slices.Clone(fixturePNG),
			}); err != nil {
				t.Fatalf("evidence wrapper leaked capture failure into scoring transport: %v", err)
			}
			if err := config.CaptureAudio(bench.SessionAudioCapture{
				SampleRateHz: 24_000, RoomPCM16: make([]int16, 2_400),
			}); err != nil {
				t.Fatal(err)
			}
			return bench.Transcript{PlaybackMS: 5_000, Moments: []bench.Moment{
				{Kind: bench.MomentReady, AtMS: 0},
				{Kind: bench.MomentVideoFrame, Source: "screen", AtMS: 10},
				{Kind: bench.MomentToolCall, Name: "computer.click", AtMS: 4_000},
			}}, nil
		},
		now: func() time.Time { return started.Add(4 * time.Second) },
	}
	result, err := Run(context.Background(), Options{
		Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
		Groundings: []Grounding{GroundingPixel}, Categories: []string{"control"}, Limit: 1,
		FrameRate: 3, Timeout: 10 * time.Second, Evidence: plugin, dependencies: dependencies,
	})
	if !errors.Is(err, want) || len(result.Tasks) != 1 || !result.Tasks[0].Passed ||
		result.Tasks[0].Error != "" || aborts != 1 || completes != 0 {
		t.Fatalf("result=%+v error=%v aborts=%d completes=%d", result, err, aborts, completes)
	}
}

func TestCloneResultPreservesEmptyCollectionIdentityAndOwnership(t *testing.T) {
	source := bench.Result{
		Suite:    SuiteName,
		Cell:     bench.Cell{Name: "empty-collections", Levels: map[bench.Factor]string{}},
		Expected: 1,
		Tasks: []bench.TaskOutcome{{
			ID: "task", Metrics: map[string]float64{}, Notes: map[string]string{},
		}},
		Summary: bench.Summary{Distributions: map[string]bench.Distribution{}},
	}
	cloned, err := cloneResult(source)
	if err != nil || !reflect.DeepEqual(cloned, source) {
		t.Fatalf("cloneResult()=%+v error=%v, want %+v", cloned, err, source)
	}
	cloned.Cell.Levels[bench.FactorBinding] = "changed"
	cloned.Tasks[0].Metrics["changed"] = 1
	cloned.Tasks[0].Notes["changed"] = "yes"
	cloned.Summary.Distributions["changed"] = bench.Distribution{Count: 1}
	if len(source.Cell.Levels) != 0 || len(source.Tasks[0].Metrics) != 0 ||
		len(source.Tasks[0].Notes) != 0 || len(source.Summary.Distributions) != 0 {
		t.Fatalf("cloneResult() retained aliases: %+v", source)
	}
}

type fixtureRunSurface struct{}

func (*fixtureRunSurface) Name() string                                     { return "benchmark-browser" }
func (*fixtureRunSurface) Viewport(context.Context) (int, int, error)       { return 1280, 720, nil }
func (*fixtureRunSurface) Click(context.Context, int, int, string) error    { return nil }
func (*fixtureRunSurface) DoubleClick(context.Context, int, int) error      { return nil }
func (*fixtureRunSurface) Move(context.Context, int, int) error             { return nil }
func (*fixtureRunSurface) Drag(context.Context, int, int, int, int) error   { return nil }
func (*fixtureRunSurface) Type(context.Context, string) error               { return nil }
func (*fixtureRunSurface) Key(context.Context, []string) error              { return nil }
func (*fixtureRunSurface) Scroll(context.Context, int, int, int, int) error { return nil }
func (*fixtureRunSurface) Screenshot(context.Context) error                 { return nil }
func (*fixtureRunSurface) ClickElement(context.Context, string) error       { return nil }

// fixturePNG is a complete one-pixel PNG. The evidence seam treats bytes as
// opaque; production media validation independently decodes retained frames.
var fixturePNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x02, 0x00, 0x00, 0x00, 0x90, 0x77, 0x53, 0xde,
	0x00, 0x00, 0x00, 0x0c, 0x49, 0x44, 0x41, 0x54,
	0x08, 0xd7, 0x63, 0xf8, 0xcf, 0xc0, 0x00, 0x00, 0x03, 0x01, 0x01, 0x00, 0x18, 0xdd, 0x8d, 0xb0,
	0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae, 0x42, 0x60, 0x82,
}
