package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/bench"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/perception"
)

type meetingProfileProbeRuntime interface {
	legacy.Runtime
	Live() inspect.Live
	Done() <-chan struct{}
}

// probeMeetingExpectedResolution mounts the exact selected application without
// sending user input and freezes only identities reported through the live
// runtime, capability, and deployment inspection boundaries. Declaration-time
// plan metadata remains a required contract but is never promoted to observed
// evidence.
func probeMeetingExpectedResolution(
	ctx context.Context,
	binding legacy.Binding,
	plan *config.Plan,
	configuration bench.ArtifactIdentity,
) (resolution bench.LiveResolution, resultErr error) {
	if ctx == nil {
		return bench.LiveResolution{}, errors.New("probe Meeting expected resolution: nil context")
	}
	if binding == nil || plan == nil {
		return bench.LiveResolution{}, errors.New(
			"probe Meeting expected resolution: binding and plan are required",
		)
	}
	if err := context.Cause(ctx); err != nil {
		return bench.LiveResolution{}, err
	}

	probeContext, cancelProbe := context.WithTimeout(ctx, time.Minute)
	defer cancelProbe()
	runtimeValue, err := binding.Start(probeContext, legacy.Options{
		Sink: meetingProfileProbeSink{}, SessionID: "meeting-profile-resolution-probe",
	})
	if err != nil {
		return bench.LiveResolution{}, fmt.Errorf("probe Meeting expected resolution: start: %w", err)
	}
	runtime, ok := runtimeValue.(meetingProfileProbeRuntime)
	if !ok {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		return bench.LiveResolution{}, errors.Join(
			errors.New("probe Meeting expected resolution: runtime has no graph inspection surface"),
			runtimeValue.Close(closeContext, errors.New("profile resolution probe refused runtime")),
		)
	}
	defer func() {
		closeContext, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		resultErr = errors.Join(resultErr, runtime.Close(
			closeContext, errors.New("profile resolution probe complete"),
		))
	}()

	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := runtime.Live()
		if meetingProfileProbeIsLive(plan, snapshot) {
			resolution, err = bench.AuthorExpectedResolutionFromInspection(
				plan.Graph(), configuration, snapshot,
			)
			if err != nil {
				return bench.LiveResolution{}, fmt.Errorf(
					"probe Meeting expected resolution: freeze: %w", err,
				)
			}
			if resolution.Deployment == nil {
				return bench.LiveResolution{}, errors.New(
					"probe Meeting expected resolution: live snapshot omitted exact deployment evidence",
				)
			}
			return resolution, nil
		}
		select {
		case <-probeContext.Done():
			return bench.LiveResolution{}, fmt.Errorf(
				"probe Meeting expected resolution: readiness: %w", context.Cause(probeContext),
			)
		case <-runtime.Done():
			return bench.LiveResolution{}, errors.New(
				"probe Meeting expected resolution: runtime stopped before every live identity was reported",
			)
		case <-ticker.C:
		}
	}
}

func meetingProfileProbeIsLive(plan *config.Plan, snapshot inspect.Live) bool {
	if plan == nil || snapshot.Deployment == nil || len(snapshot.Nodes) != len(plan.Graph().Nodes) {
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

type meetingProfileProbeSink struct{}

func (meetingProfileProbeSink) TurnBegin(context.Context) error { return nil }
func (meetingProfileProbeSink) TurnEnd(context.Context, legacy.TurnOutcome) error {
	return nil
}
func (meetingProfileProbeSink) Activity(context.Context, legacy.ActivityEvent) error { return nil }
func (meetingProfileProbeSink) Transcript(context.Context, legacy.TranscriptEvent) error {
	return nil
}
func (meetingProfileProbeSink) Observation(context.Context, perception.Observation) error {
	return nil
}
func (meetingProfileProbeSink) SpeechBegin(context.Context, action.Utterance) error { return nil }
func (meetingProfileProbeSink) SpeechText(context.Context, action.Utterance, string) error {
	return nil
}
func (meetingProfileProbeSink) SpeechAudio(context.Context, action.Utterance, action.Frame) error {
	return nil
}
func (meetingProfileProbeSink) SpeechEnd(context.Context, action.Utterance, action.Outcome) error {
	return nil
}
func (meetingProfileProbeSink) ToolCalls(context.Context, legacy.ToolCallEvent) error { return nil }
func (meetingProfileProbeSink) Failed(context.Context, legacy.ErrorEvent)             {}
