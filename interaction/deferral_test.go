package interaction_test

import (
	"strings"
	"sync/atomic"
	"testing"

	"github.com/bojieli/OpenRealtime/eventloop"
	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/session"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type preservingManualDeferral struct{}

func (preservingManualDeferral) Name() string                         { return "preserving-manual" }
func (preservingManualDeferral) Conditions() []session.TransitionKind { return nil }
func (preservingManualDeferral) Admit(waiting interaction.Waiting) (bool, string) {
	if waiting.AutonomousObservation || waiting.Requested {
		return true, ""
	}
	return false, "waiting for a request"
}
func (preservingManualDeferral) ConsumesResponseRequest(waiting interaction.Waiting) bool {
	return !waiting.AutonomousObservation
}

type recordingWaker struct{ calls atomic.Int32 }

func (waker *recordingWaker) Wake(string) { waker.calls.Add(1) }

func observationBatch(authority trajectory.Authority) eventloop.Batch {
	phase := trajectory.PhaseUser
	observer := "microphone"
	if authority == trajectory.AuthorityObserver {
		phase = trajectory.PhaseObserver
		observer = "screen"
	}
	return eventloop.Batch{Items: []trajectory.Item{{
		Kind: trajectory.KindObservation, Producer: trajectory.Producer{Phase: phase},
		Observation: &trajectory.ObservationMeta{
			Observer: observer, Source: observer, Authority: authority,
		},
	}}}
}

func TestGateLetsComposedPolicyPreserveRequestAcrossPassiveObservation(t *testing.T) {
	waker := &recordingWaker{}
	gate, err := interaction.Bind(
		preservingManualDeferral{}, session.NewDuplex(session.DuplexConfig{}), waker,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer gate.Close()
	gate.RequestResponse()

	observer := observationBatch(trajectory.AuthorityObserver)
	if !interaction.AutonomousObservation(observer) {
		t.Fatal("observer-only batch was not classified as autonomous")
	}
	if admitted, reason := gate.AdmitRun(t.Context(), observer); !admitted || reason != "" {
		t.Fatalf("observer admission = %t, %q", admitted, reason)
	}
	user := observationBatch(trajectory.AuthorityUser)
	if interaction.AutonomousObservation(user) {
		t.Fatal("user-authority observation was classified as autonomous")
	}
	if admitted, reason := gate.AdmitRun(t.Context(), user); !admitted || reason != "" {
		t.Fatalf("preserved request admission = %t, %q", admitted, reason)
	}
	if admitted, reason := gate.AdmitRun(t.Context(), user); admitted ||
		!strings.Contains(reason, "waiting") {
		t.Fatalf("consumed request authorized a second run = %t, %q", admitted, reason)
	}
	if waker.calls.Load() != 1 {
		t.Fatalf("request wake count = %d, want 1", waker.calls.Load())
	}
}
