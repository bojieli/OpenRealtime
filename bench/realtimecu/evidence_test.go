package realtimecu

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

type fixtureEvidencePlugin struct {
	begin  func(context.Context, EvidenceAttempt) (AttemptEvidence, error)
	finish func(context.Context, bench.Result) error
}

func (plugin fixtureEvidencePlugin) BeginAttempt(
	ctx context.Context, attempt EvidenceAttempt,
) (AttemptEvidence, error) {
	return plugin.begin(ctx, attempt)
}

func (plugin fixtureEvidencePlugin) FinishSuite(ctx context.Context, result bench.Result) error {
	return plugin.finish(ctx, result)
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
	}
	drifted.Task.Deadline++
	if err := drifted.validate(); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("drifted evidence attempt error = %v", err)
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
	outcome := runCase(context.Background(), environment, Options{
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
	finishCalls := 0
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
	if !errors.Is(err, want) || finishCalls != 1 || result.Summary.Complete {
		t.Fatalf("Run result=%+v error=%v finish calls=%d", result, err, finishCalls)
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
				config.Video[0].Source != "screen" {
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
	outcome := runCase(context.Background(), environment, Options{
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
		!strings.Contains(outcome.Error, want.Error()) {
		t.Fatalf("outcome=%+v aborts=%d", outcome, aborts)
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
