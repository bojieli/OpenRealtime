package realtimecu

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench"
)

// Exercise the runner's real dispatcher and serialized state/action/state
// boundary. An execution error must reach the agent and remain distinguishable
// from either missing page evidence or the later action that actually succeeds.
func TestRealtimeCURunnerPreservesFailedEffectsThroughRecovery(t *testing.T) {
	for _, test := range []struct {
		name          string
		recover       bool
		effectOnError bool
		wantPassed    bool
		wantMissing   float64
	}{
		{name: "failed effect remains an explicit failure"},
		{name: "later successful effect recovers", recover: true, wantPassed: true},
		{name: "error-bearing success cannot settle", effectOnError: true, wantMissing: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			item := Case{Task: Suite()[0], Grounding: GroundingPixel}
			started := time.Unix(200, 0)
			clock := started
			page := PageResult{}
			failure := errors.New("target moved before the click was confirmed")
			effects := 0
			surface := &failedEffectSurface{click: func() error {
				effects++
				if effects > 1 || test.effectOnError {
					page = PageResult{
						Complete: true, Success: true, Actions: 1,
						Reason: "target reached", CompletedAtMS: milliseconds(clock.Sub(started)),
					}
				}
				if effects == 1 {
					return failure
				}
				return nil
			}}
			environment := realtimeCURunEnvironment{episode: func(context.Context, Case) (realtimeCURunEpisode, error) {
				return realtimeCURunEpisode{
					ready: func(context.Context) error { return nil }, started: func() time.Time { return started },
					surface:       surface,
					captureScreen: func(context.Context) ([]byte, error) { return fixturePNG, nil },
					captureCamera: func(context.Context) ([]byte, error) { return fixturePNG, nil },
					result:        func(context.Context) (PageResult, error) { return page, nil },
				}, nil
			}}
			transcript := bench.Transcript{Moments: []bench.Moment{{Kind: bench.MomentReady}}}
			outcome, evidenceErr := runCase(t.Context(), environment, Options{
				Endpoint: "ws://hermetic.invalid/v1/realtime", Cell: ReferenceCell(),
				FrameRate: 3, Timeout: 10 * time.Second,
				dependencies: &runDependencies{
					now: func() time.Time { return clock.Add(time.Millisecond) },
					playSamples: func(ctx context.Context, config bench.SessionConfig, _ []int16) (bench.Transcript, error) {
						calls := 1
						if test.recover {
							calls++
						}
						for index := range calls {
							clock = started.Add(item.Task.CueAt + time.Duration(index+1)*100*time.Millisecond)
							request := bench.ToolRequest{
								CallID: fmt.Sprintf("effect-%d", index), Name: "computer.click_normalized",
								Arguments: json.RawMessage(fmt.Sprintf(`{"source":"screen","x":%d,"y":500}`, 500+index*10)),
								Received:  clock,
							}
							output, err := config.HandleTool(ctx, request)
							if index == 0 {
								if err == nil || !strings.Contains(err.Error(), failure.Error()) || len(output) != 0 {
									t.Fatalf("failed effect was not returned to the agent: output=%s err=%v", output, err)
								}
							} else if err != nil || len(output) == 0 {
								t.Fatalf("recovery did not return a successful tool result: output=%s err=%v", output, err)
							}
							transcript.Moments = append(transcript.Moments, bench.Moment{
								Kind: bench.MomentToolCall, AtMS: milliseconds(clock.Sub(started)),
								CallID: request.CallID, Name: request.Name, Arguments: string(request.Arguments),
							})
						}
						return transcript, nil
					},
				},
				evidenceOrigin: EvidenceRunOrigin{Kind: EvidenceOriginHermetic},
			}, item)
			if evidenceErr != nil || !outcome.Completed || outcome.Error != "" {
				t.Fatalf("failed effect became an incomplete evaluation: outcome=%+v err=%v", outcome, evidenceErr)
			}
			if outcome.Passed != test.wantPassed || outcome.Metrics["task_success_rate"] != truth(test.wantPassed) ||
				outcome.Metrics["settlement_evidence_missing_count"] != test.wantMissing ||
				outcome.Metrics["invalid_action_count"] != 1 || outcome.Metrics["action_count"] != float64(effects) ||
				outcome.Metrics["correct_action_rate"] != truth(test.recover || test.effectOnError) {
				t.Fatalf("failed-effect diagnosis lost the distinction between error, recovery, and success: %+v", outcome)
			}
			var actions []ActionRecord
			if err := json.Unmarshal([]byte(outcome.Notes["actions"]), &actions); err != nil {
				t.Fatal(err)
			}
			if len(actions) != effects || actions[0].Error != failure.Error() ||
				actions[0].CallID != "effect-0" || actions[0].PageBefore == nil || actions[0].PageAfter == nil {
				t.Fatalf("failed action lost its exact identity, error, or page consequence: %+v", actions)
			}
			if test.recover {
				if actions[0].PageAfter.Complete || actions[1].Error != "" || actions[1].CallID != "effect-1" ||
					actions[1].PageBefore == nil || actions[1].PageBefore.Complete ||
					actions[1].PageAfter == nil || !actions[1].PageAfter.Success {
					t.Fatalf("recovery was attributed to the failed action: %+v", actions)
				}
				// Losing the failed action's consequence is missing evidence. The
				// explicit error itself was sufficient and did not erase recovery.
				actions[0].PageAfter = nil
				rescored := score(bench.TaskOutcome{Metrics: map[string]float64{}, Notes: map[string]string{}}, item, page, actions, transcript, false)
				if rescored.Passed || rescored.Metrics["settlement_evidence_missing_count"] != 1 {
					t.Fatalf("missing failed-action consequence passed: %+v", rescored)
				}
			}
		})
	}
}

type failedEffectSurface struct {
	fixtureRunSurface
	click func() error
}

func (surface *failedEffectSurface) Click(context.Context, int, int, string) error {
	return surface.click()
}
