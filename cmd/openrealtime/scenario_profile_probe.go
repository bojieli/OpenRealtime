package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/bench"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/gateway"
	"github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
)

type scenarioProfileProbeRuntime interface {
	legacy.Runtime
	Live() inspect.Live
	Done() <-chan struct{}
}

// probeScenarioExpectedResolution mounts the exact selected scenario
// application without sending user input. Only identities reported through the
// live runtime, capability, and deployment inspection boundaries enter the
// reviewed benchmark contract.
func probeScenarioExpectedResolution(
	ctx context.Context,
	binding gateway.SessionBinding,
	plan *config.Plan,
	configuration bench.ArtifactIdentity,
	settings legacy.Settings,
) (resolution bench.LiveResolution, resultErr error) {
	if ctx == nil {
		return bench.LiveResolution{}, errors.New("probe scenario expected resolution: nil context")
	}
	if binding == nil || plan == nil {
		return bench.LiveResolution{}, errors.New(
			"probe scenario expected resolution: binding and plan are required",
		)
	}
	if err := context.Cause(ctx); err != nil {
		return bench.LiveResolution{}, err
	}

	probeContext, cancelProbe := context.WithTimeout(ctx, time.Minute)
	defer cancelProbe()
	runtimeValue, err := binding.Start(probeContext, legacy.Options{
		Sink: scenarioProfileProbeSink{}, Settings: settings,
		SessionID: "scenario-profile-resolution-probe",
	})
	if err != nil {
		return bench.LiveResolution{}, fmt.Errorf("probe scenario expected resolution: start: %w", err)
	}
	runtime, ok := runtimeValue.(scenarioProfileProbeRuntime)
	if !ok {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		return bench.LiveResolution{}, errors.Join(
			errors.New("probe scenario expected resolution: runtime has no graph inspection surface"),
			runtimeValue.Close(closeContext, errors.New("profile resolution probe refused runtime")),
		)
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if closeErr := runtime.Close(
			closeContext, errors.New("profile resolution probe complete"),
		); closeErr != nil {
			resolution = bench.LiveResolution{}
			resultErr = errors.Join(resultErr, closeErr)
		}
	}()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := runtime.Live()
		if scenarioProfileProbeIsLive(plan, snapshot) {
			resolution, err = bench.AuthorExpectedResolutionFromInspection(
				plan.Graph(), configuration, snapshot,
			)
			if err != nil {
				return bench.LiveResolution{}, fmt.Errorf(
					"probe scenario expected resolution: freeze: %w", err,
				)
			}
			return resolution, nil
		}
		select {
		case <-probeContext.Done():
			return bench.LiveResolution{}, fmt.Errorf(
				"probe scenario expected resolution: readiness: %w", context.Cause(probeContext),
			)
		case <-runtime.Done():
			return bench.LiveResolution{}, errors.New(
				"probe scenario expected resolution: runtime stopped before every live identity was reported",
			)
		case <-ticker.C:
		}
	}
}

func scenarioProfileProbeIsLive(plan *config.Plan, snapshot inspect.Live) bool {
	if plan == nil || snapshot.State != "running" || snapshot.Deployment == nil ||
		len(snapshot.Nodes) != len(plan.Graph().Nodes) {
		return false
	}
	for _, graphNode := range plan.Graph().Nodes {
		node, found := snapshot.Nodes[graphNode.ID]
		if !found || node.State != "running" || node.Resolution == nil ||
			node.Resolution.RuntimeEvidence != inspect.EvidenceLive ||
			node.Resolution.CapabilitiesEvidence != inspect.EvidenceLive {
			return false
		}
	}
	return true
}

type scenarioProfileProbeSink struct{}

func (scenarioProfileProbeSink) TurnBegin(context.Context) error { return nil }
func (scenarioProfileProbeSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	return nil
}
func (scenarioProfileProbeSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (scenarioProfileProbeSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (scenarioProfileProbeSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (scenarioProfileProbeSink) SpeechBegin(context.Context, action.Utterance) error { return nil }
func (scenarioProfileProbeSink) SpeechText(context.Context, action.Utterance, string) error {
	return nil
}
func (scenarioProfileProbeSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (scenarioProfileProbeSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (scenarioProfileProbeSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (scenarioProfileProbeSink) Failed(context.Context, legacy.ErrorEvent)             {}
