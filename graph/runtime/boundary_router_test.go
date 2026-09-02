package runtime

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestBoundaryCachedPortsAndLanesFollowOnePublishedGeneration(t *testing.T) {
	valueType := boundaryTestType()
	oldInput, oldInputQueue := boundaryTestOutput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 4, nil)
	oldOutput, oldOutputQueue := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 4, nil)
	router, err := newBoundaryRouter(
		map[string]*outputPort{"input": oldInput},
		map[string]*inputPort{"output": oldOutput},
	)
	if err != nil {
		t.Fatal(err)
	}

	input, found := router.input("input")
	if !found {
		t.Fatal("stable input boundary is missing")
	}
	if again, _ := router.input("input"); input != again {
		t.Fatal("repeated boundary lookup returned a different stable port")
	}
	inputLanes := input.Lanes()
	if again := input.Lanes(); len(again) != 1 || inputLanes[0] != again[0] {
		t.Fatal("repeated Lanes returned a different stable sender")
	}
	output, found := router.output("output")
	if !found {
		t.Fatal("stable output boundary is missing")
	}
	outputLanes := output.Lanes()
	if again := output.Lanes(); len(again) != 1 || outputLanes[0] != again[0] {
		t.Fatal("repeated Lanes returned a different stable receiver")
	}

	if result, sendErr := input.Broadcast(context.Background(), envelope(valueType, "old-port")); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("old port send = %+v, %v", result, sendErr)
	}
	boundaryRequireItem(t, oldInputQueue[0], "old-port")
	if result, sendErr := inputLanes[0].Send(context.Background(), envelope(valueType, "old-lane")); sendErr != nil || result != element.Delivered {
		t.Fatalf("old lane send = %s, %v", result, sendErr)
	}
	boundaryRequireItem(t, oldInputQueue[0], "old-lane")
	boundaryRequireSend(t, oldOutputQueue[0], valueType, "old-output")
	if got, receiveErr := outputLanes[0].Receive(context.Background()); receiveErr != nil || got.ItemID != "old-output" {
		t.Fatalf("old lane receive = %+v, %v", got, receiveErr)
	}

	newInput, newInputQueue := boundaryTestOutput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 4, nil)
	newOutput, newOutputQueue := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 4, nil)
	if err := router.replace(
		context.Background(),
		map[string]*outputPort{"input": newInput},
		map[string]*inputPort{"output": newOutput},
		nil,
	); err != nil {
		t.Fatal(err)
	}

	if result, sendErr := input.Broadcast(context.Background(), envelope(valueType, "new-port")); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("new port send = %+v, %v", result, sendErr)
	}
	boundaryRequireItem(t, newInputQueue[0], "new-port")
	if result, sendErr := inputLanes[0].Send(context.Background(), envelope(valueType, "new-lane")); sendErr != nil || result != element.Delivered {
		t.Fatalf("new lane send = %s, %v", result, sendErr)
	}
	boundaryRequireItem(t, newInputQueue[0], "new-lane")
	boundaryRequireSend(t, newOutputQueue[0], valueType, "new-output")
	if got, receiveErr := output.Receive(context.Background()); receiveErr != nil || got.ItemID != "new-output" {
		t.Fatalf("new port receive = %+v, %v", got, receiveErr)
	}
	if oldInputQueue[0].snapshot().Occupancy != 0 || oldOutputQueue[0].snapshot().Occupancy != 0 {
		t.Fatal("a cached handle routed back into the retired generation")
	}
}

func TestBoundaryBlockedLosslessSendRetriesOnlyOnCandidate(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	boundaryRequireSend(t, oldQueues[0], valueType, "occupy-old")
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	lane := stable.Lanes()[0]

	type outcome struct {
		result element.DeliveryResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, sendErr := lane.Send(context.Background(), envelope(valueType, "candidate-only"))
		done <- outcome{result: result, err: sendErr}
	}()
	boundaryAwait(t, "lossless sender did not enter backpressure", func() bool {
		return oldQueues[0].snapshot().Backpressure == 1 && router.current.ingressAdmission.activeLeases() == 1
	})

	newPort, newQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	if err := router.replace(context.Background(), map[string]*outputPort{"input": newPort}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result != element.Delivered {
			t.Fatalf("retried send = %s, %v", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retired sender did not retry on the candidate")
	}
	if got := oldQueues[0].snapshot(); got.Enqueued != 1 || got.Occupancy != 1 {
		t.Fatalf("predecessor queue was mutated by the retired send: %+v", got)
	}
	if got := newQueues[0].snapshot(); got.Enqueued != 1 || got.Occupancy != 1 {
		t.Fatalf("candidate queue did not receive exactly one send: %+v", got)
	}
	boundaryRequireItem(t, oldQueues[0], "occupy-old")
	boundaryRequireItem(t, newQueues[0], "candidate-only")
}

func TestBoundaryEgressRemainsLiveDuringDrainAndBlockedReceiveRetriesAtCutover(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 2, nil)
	router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": oldPort})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.output("output")
	newPort, newQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 2, nil)

	first := make(chan element.Envelope, 1)
	firstErr := make(chan error, 1)
	go func() {
		received, receiveErr := stable.Receive(context.Background())
		first <- received
		firstErr <- receiveErr
	}()
	boundaryAwait(t, "first egress receiver did not block", func() bool {
		return router.current.egressAdmission.activeLeases() == 1
	})

	second := make(chan element.Envelope, 1)
	secondErr := make(chan error, 1)
	if err := router.replace(
		context.Background(), nil, map[string]*inputPort{"output": newPort},
		func(context.Context) error {
			boundaryRequireSend(t, oldQueues[0], valueType, "drained-old")
			select {
			case got := <-first:
				if receiveErr := <-firstErr; receiveErr != nil || got.ItemID != "drained-old" {
					t.Fatalf("drain receive = %+v, %v", got, receiveErr)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("egress was not live during ingress drain")
			}
			go func() {
				received, receiveErr := stable.Lanes()[0].Receive(context.Background())
				second <- received
				secondErr <- receiveErr
			}()
			boundaryAwait(t, "second egress receiver did not block on predecessor", func() bool {
				return router.current.egressAdmission.activeLeases() == 1
			})
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	boundaryRequireSend(t, newQueues[0], valueType, "candidate-output")
	select {
	case got := <-second:
		if receiveErr := <-secondErr; receiveErr != nil || got.ItemID != "candidate-output" {
			t.Fatalf("candidate receive = %+v, %v", got, receiveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked predecessor receiver did not retry on candidate")
	}
	if got := oldQueues[0].snapshot(); got.Enqueued != 1 || got.Dequeued != 1 || got.Occupancy != 0 {
		t.Fatalf("predecessor drain telemetry = %+v", got)
	}
}

func TestBoundaryBroadcastRetirementNeverPartiallyPublishesPredecessor(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestOutput(
		t, "input", []string{"left", "right"}, valueType, ir.Lossless, 1, nil,
	)
	boundaryRequireSend(t, oldQueues[0], valueType, "occupy-left")
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")

	type outcome struct {
		result element.SendResult
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, sendErr := stable.Broadcast(context.Background(), envelope(valueType, "broadcast"))
		done <- outcome{result: result, err: sendErr}
	}()
	boundaryAwait(t, "broadcast did not block on full lossless branch", func() bool {
		return oldQueues[0].snapshot().Backpressure == 1 && router.current.ingressAdmission.activeLeases() == 1
	})
	if got := oldQueues[1].snapshot().Occupancy; got != 0 {
		t.Fatalf("predecessor received partial broadcast before retirement: occupancy %d", got)
	}

	newPort, newQueues := boundaryTestOutput(
		t, "input", []string{"left", "right"}, valueType, ir.Lossless, 1, nil,
	)
	if err := router.replace(context.Background(), map[string]*outputPort{"input": newPort}, nil, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-done:
		if got.err != nil || got.result.Delivered != 2 || got.result.Dropped != 0 {
			t.Fatalf("candidate broadcast = %+v, %v", got.result, got.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("broadcast did not retry on candidate")
	}
	if got := oldQueues[1].snapshot(); got.Enqueued != 0 || got.Occupancy != 0 {
		t.Fatalf("broadcast partially reached predecessor branch: %+v", got)
	}
	for _, queue := range newQueues {
		if got := queue.snapshot(); got.Enqueued != 1 || got.Occupancy != 1 {
			t.Fatalf("candidate broadcast branch %s = %+v", queue.id, got)
		}
		boundaryRequireItem(t, queue, "broadcast")
	}
}

func TestBoundaryCompletedLossyDropIsNeverReplayed(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossy, 1, nil)
	boundaryRequireSend(t, oldQueues[0], valueType, "kept")
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	result, err := stable.Lanes()[0].Send(context.Background(), envelope(valueType, "drop-once"))
	if err != nil || result != element.Dropped {
		t.Fatalf("lossy send = %s, %v", result, err)
	}

	newPort, newQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossy, 1, nil)
	if err := router.replace(context.Background(), map[string]*outputPort{"input": newPort}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if got := oldQueues[0].snapshot(); got.Dropped != 1 || got.Enqueued != 1 {
		t.Fatalf("predecessor lossy telemetry = %+v", got)
	}
	if got := newQueues[0].snapshot(); got.Enqueued != 0 || got.Dropped != 0 || got.Occupancy != 0 {
		t.Fatalf("completed drop was replayed to candidate: %+v", got)
	}
}

func TestBoundaryLossyRetirementLinearizesToExactlyOneOutcome(t *testing.T) {
	valueType := boundaryTestType()
	for iteration := 0; iteration < 100; iteration++ {
		oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossy, 1, nil)
		boundaryRequireSend(t, oldQueues[0], valueType, fmt.Sprintf("kept-%d", iteration))
		router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
		if err != nil {
			t.Fatal(err)
		}
		stable, _ := router.input("input")
		newPort, newQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossy, 1, nil)

		start := make(chan struct{})
		type outcome struct {
			result element.DeliveryResult
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			<-start
			result, sendErr := stable.Lanes()[0].Send(
				context.Background(), envelope(valueType, fmt.Sprintf("racing-%d", iteration)),
			)
			done <- outcome{result: result, err: sendErr}
		}()
		close(start)
		if err := router.replace(context.Background(), map[string]*outputPort{"input": newPort}, nil, nil); err != nil {
			t.Fatal(err)
		}
		got := <-done
		if got.err != nil {
			t.Fatalf("iteration %d send error: %v", iteration, got.err)
		}
		oldDropped := oldQueues[0].snapshot().Dropped
		newOccupancy := newQueues[0].snapshot().Occupancy
		switch got.result {
		case element.Dropped:
			if oldDropped != 1 || newOccupancy != 0 {
				t.Fatalf("iteration %d drop was duplicated/replayed: old drops=%d new occupancy=%d",
					iteration, oldDropped, newOccupancy)
			}
		case element.Delivered:
			if oldDropped != 0 || newOccupancy != 1 {
				t.Fatalf("iteration %d delivery was lost/duplicated: old drops=%d new occupancy=%d",
					iteration, oldDropped, newOccupancy)
			}
		default:
			t.Fatalf("iteration %d unexpected result %q", iteration, got.result)
		}
	}
}

func TestBoundaryReplacementRejectsExactContractMismatchBeforePause(t *testing.T) {
	valueType := boundaryTestType()
	otherType := element.Event(element.Named("test.Other"))
	baseInput, _ := boundaryTestOutput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 2, nil)
	baseOutput, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)

	tests := []struct {
		name    string
		ingress func() map[string]*outputPort
		egress  func() map[string]*inputPort
		want    string
	}{
		{
			name:    "missing name",
			ingress: func() map[string]*outputPort { return nil },
			egress: func() map[string]*inputPort {
				port, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": port}
			},
			want: "missing graph input boundary",
		},
		{
			name: "extra name",
			ingress: func() map[string]*outputPort {
				port, _ := boundaryTestOutput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 2, nil)
				extra, _ := boundaryTestOutput(t, "extra", []string{"extra-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*outputPort{"input": port, "extra": extra}
			},
			egress: func() map[string]*inputPort {
				port, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": port}
			},
			want: "unexpected graph input boundary",
		},
		{
			name:    "direction",
			ingress: func() map[string]*outputPort { return nil },
			egress: func() map[string]*inputPort {
				output, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				input, _ := boundaryTestInput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": output, "input": input}
			},
			want: "missing graph input boundary",
		},
		{
			name: "type",
			ingress: func() map[string]*outputPort {
				port, _ := boundaryTestOutput(t, "input", []string{"input-lane"}, otherType, ir.Lossless, 2, nil)
				return map[string]*outputPort{"input": port}
			},
			egress: func() map[string]*inputPort {
				port, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": port}
			},
			want: "has type",
		},
		{
			name: "cardinality",
			ingress: func() map[string]*outputPort {
				port, _ := boundaryTestOutput(t, "input", []string{"input-lane", "second"}, valueType, ir.Lossless, 2, nil)
				return map[string]*outputPort{"input": port}
			},
			egress: func() map[string]*inputPort {
				port, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": port}
			},
			want: "has cardinality",
		},
		{
			name: "lane ID",
			ingress: func() map[string]*outputPort {
				port, _ := boundaryTestOutput(t, "input", []string{"renamed-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*outputPort{"input": port}
			},
			egress: func() map[string]*inputPort {
				port, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 2, nil)
				return map[string]*inputPort{"output": port}
			},
			want: "has lane IDs",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			router, err := newBoundaryRouter(
				map[string]*outputPort{"input": baseInput},
				map[string]*inputPort{"output": baseOutput},
			)
			if err != nil {
				t.Fatal(err)
			}
			err = router.replace(context.Background(), test.ingress(), test.egress(), nil)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("replace error = %v, want substring %q", err, test.want)
			}
			if router.current.sequence != 1 {
				t.Fatalf("contract mismatch changed generation to %d", router.current.sequence)
			}
			lease, admitted := router.current.ingressAdmission.acquire()
			if !admitted {
				t.Fatal("contract mismatch paused predecessor ingress")
			}
			lease.release()
		})
	}
}

func TestBoundaryMismatchDoesNotWakeBlockedPredecessorSend(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	boundaryRequireSend(t, oldQueues[0], valueType, "occupy")
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	done := make(chan error, 1)
	go func() {
		_, sendErr := stable.Lanes()[0].Send(context.Background(), envelope(valueType, "still-old"))
		done <- sendErr
	}()
	boundaryAwait(t, "sender did not block", func() bool {
		return oldQueues[0].snapshot().Backpressure == 1
	})
	wrong, wrongQueues := boundaryTestOutput(t, "input", []string{"wrong-lane"}, valueType, ir.Lossless, 1, nil)
	if err := router.replace(context.Background(), map[string]*outputPort{"input": wrong}, nil, nil); err == nil {
		t.Fatal("lane mismatch replacement succeeded")
	}
	select {
	case err := <-done:
		t.Fatalf("mismatch woke blocked predecessor sender: %v", err)
	default:
	}
	boundaryRequireItem(t, oldQueues[0], "occupy")
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("predecessor sender did not resume after capacity opened")
	}
	boundaryRequireItem(t, oldQueues[0], "still-old")
	if got := wrongQueues[0].snapshot(); got.Enqueued != 0 {
		t.Fatalf("rejected candidate was mutated: %+v", got)
	}
}

func TestBoundaryDrainFailureResumesPredecessorWithoutReplay(t *testing.T) {
	valueType := boundaryTestType()
	oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 2, nil)
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	newPort, newQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 2, nil)
	drainFailure := errors.New("drain refused")
	done := make(chan error, 1)
	err = router.replace(
		context.Background(), map[string]*outputPort{"input": newPort}, nil,
		func(context.Context) error {
			go func() {
				_, sendErr := stable.Broadcast(context.Background(), envelope(valueType, "after-refusal"))
				done <- sendErr
			}()
			select {
			case sendErr := <-done:
				t.Fatalf("ingress was admitted during drain: %v", sendErr)
			case <-time.After(20 * time.Millisecond):
			}
			return drainFailure
		},
	)
	if !errors.Is(err, drainFailure) {
		t.Fatalf("replace error = %v, want %v", err, drainFailure)
	}
	select {
	case sendErr := <-done:
		if sendErr != nil {
			t.Fatal(sendErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("predecessor admission did not resume after drain refusal")
	}
	boundaryRequireItem(t, oldQueues[0], "after-refusal")
	if got := newQueues[0].snapshot(); got.Enqueued != 0 || got.Occupancy != 0 {
		t.Fatalf("refused candidate received replay: %+v", got)
	}
	if router.current.sequence != 1 {
		t.Fatalf("refusal published generation %d", router.current.sequence)
	}
}

func TestBoundaryCloseWakesCachedPortAndLaneOperationsAndIsIdempotent(t *testing.T) {
	valueType := boundaryTestType()
	ingress, ingressQueues := boundaryTestOutput(t, "input", []string{"input-lane"}, valueType, ir.Lossless, 1, nil)
	boundaryRequireSend(t, ingressQueues[0], valueType, "occupy")
	egress, _ := boundaryTestInput(t, "output", []string{"output-lane"}, valueType, ir.Lossless, 1, nil)
	router, err := newBoundaryRouter(
		map[string]*outputPort{"input": ingress}, map[string]*inputPort{"output": egress},
	)
	if err != nil {
		t.Fatal(err)
	}
	stableIngress, _ := router.input("input")
	stableEgress, _ := router.output("output")

	operationErrors := make(chan error, 4)
	go func() {
		_, sendErr := stableIngress.Broadcast(context.Background(), envelope(valueType, "port-send"))
		operationErrors <- sendErr
	}()
	go func() {
		_, sendErr := stableIngress.Lanes()[0].Send(context.Background(), envelope(valueType, "lane-send"))
		operationErrors <- sendErr
	}()
	go func() {
		_, _, receiveErr := stableEgress.ReceiveAny(context.Background())
		operationErrors <- receiveErr
	}()
	go func() {
		_, receiveErr := stableEgress.Lanes()[0].Receive(context.Background())
		operationErrors <- receiveErr
	}()
	boundaryAwait(t, "cached operations did not block", func() bool {
		return router.current.ingressAdmission.activeLeases() == 2 &&
			router.current.egressAdmission.activeLeases() == 2
	})

	const closers = 8
	closeErrors := make(chan error, closers)
	for range closers {
		go func() { closeErrors <- router.close(context.Background()) }()
	}
	for range closers {
		if closeErr := <-closeErrors; closeErr != nil {
			t.Fatalf("concurrent close: %v", closeErr)
		}
	}
	for range 4 {
		if operationErr := <-operationErrors; !errors.Is(operationErr, ErrChannelClosed) {
			t.Fatalf("cached operation error = %v, want channel closed", operationErr)
		}
	}
	if got := router.current.ingressAdmission.activeLeases(); got != 0 {
		t.Fatalf("ingress retained %d route leases", got)
	}
	if got := router.current.egressAdmission.activeLeases(); got != 0 {
		t.Fatalf("egress retained %d route leases", got)
	}
	if err := router.close(context.Background()); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

func TestMountedShutdownClosesStableBoundaryBeforeConcreteQueues(t *testing.T) {
	valueType := boundaryTestType()
	egress, egressQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": egress})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.output("output")
	done := make(chan error, 1)
	go func() {
		_, receiveErr := stable.Lanes()[0].Receive(context.Background())
		done <- receiveErr
	}()
	boundaryAwait(t, "mounted cached receiver did not block", func() bool {
		return router.current.egressAdmission.activeLeases() == 1
	})
	mounted := &Mounted{
		boundaries: router,
		queues:     map[string]*queue{egressQueues[0].id: egressQueues[0]},
		timeout:    time.Second,
	}
	if err := mounted.shutdownResources(); err != nil {
		t.Fatal(err)
	}
	select {
	case receiveErr := <-done:
		if !errors.Is(receiveErr, ErrChannelClosed) {
			t.Fatalf("cached receiver error = %v", receiveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Mounted shutdown did not wake cached receiver")
	}
	if _, receiveErr := egressQueues[0].receive(context.Background()); !errors.Is(receiveErr, ErrChannelClosed) {
		t.Fatalf("concrete queue remained open after shutdown: %v", receiveErr)
	}
}

func TestMountedShutdownPreservesBufferedTerminalEgress(t *testing.T) {
	valueType := boundaryTestType()
	egress, egressQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	boundaryRequireSend(t, egressQueues[0], valueType, "terminal")
	router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": egress})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.output("output")
	mounted := &Mounted{
		boundaries: router,
		queues:     map[string]*queue{egressQueues[0].id: egressQueues[0]},
		timeout:    time.Second,
	}
	if err := mounted.shutdownResources(); err != nil {
		t.Fatal(err)
	}
	got, receiveErr := stable.Receive(context.Background())
	if receiveErr != nil || got.ItemID != "terminal" {
		t.Fatalf("buffered terminal receive after shutdown = %+v, %v", got, receiveErr)
	}
	if _, receiveErr := stable.Receive(context.Background()); !errors.Is(receiveErr, ErrChannelClosed) {
		t.Fatalf("drained stable egress remained open: %v", receiveErr)
	}
}

func TestBoundaryQueueShutdownRacingReplacementReopensPredecessorEgress(t *testing.T) {
	valueType := boundaryTestType()
	oldEgress, oldQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	boundaryRequireSend(t, oldQueues[0], valueType, "terminal")
	router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": oldEgress})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.output("output")
	newEgress, _ := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	draining := make(chan struct{})
	releaseDrain := make(chan struct{})
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- router.replace(
			context.Background(), nil, map[string]*inputPort{"output": newEgress},
			func(context.Context) error {
				close(draining)
				<-releaseDrain
				return nil
			},
		)
	}()
	select {
	case <-draining:
	case <-time.After(2 * time.Second):
		t.Fatal("replacement did not reach egress drain")
	}
	if err := router.beginQueueShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	oldQueues[0].close()
	close(releaseDrain)
	if replaceErr := <-replaceDone; !errors.Is(replaceErr, ErrChannelClosed) {
		t.Fatalf("replacement racing shutdown = %v, want channel closed", replaceErr)
	}
	got, receiveErr := stable.Receive(context.Background())
	if receiveErr != nil || got.ItemID != "terminal" {
		t.Fatalf("predecessor terminal egress after race = %+v, %v", got, receiveErr)
	}
}

func TestBoundaryCanceledEgressRetirementDuringShutdownReopensDrain(t *testing.T) {
	valueType := boundaryTestType()
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	var holdObserver sync.Once
	oldEgress, oldQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	oldEgress.observe = func(element.Envelope) {
		holdObserver.Do(func() {
			close(observerStarted)
			<-releaseObserver
		})
	}
	boundaryRequireSend(t, oldQueues[0], valueType, "held")
	router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": oldEgress})
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.output("output")
	receiveDone := make(chan element.Envelope, 1)
	receiveErr := make(chan error, 1)
	go func() {
		got, err := stable.Receive(context.Background())
		receiveDone <- got
		receiveErr <- err
	}()
	select {
	case <-observerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("predecessor receive did not reach the held observer")
	}
	newEgress, _ := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	replaceCtx, cancelReplace := context.WithCancelCause(context.Background())
	replaceDone := make(chan error, 1)
	go func() {
		replaceDone <- router.replace(replaceCtx, nil, map[string]*inputPort{"output": newEgress}, nil)
	}()
	boundaryAwait(t, "replacement did not retire predecessor egress", func() bool {
		return !boundaryAdmissionOpen(router.current.egressAdmission)
	})
	if err := router.beginQueueShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	cancelCause := errors.New("cancel replacement")
	cancelReplace(cancelCause)
	if err := <-replaceDone; !errors.Is(err, cancelCause) {
		t.Fatalf("canceled replacement = %v, want %v", err, cancelCause)
	}
	close(releaseObserver)
	select {
	case got := <-receiveDone:
		if err := <-receiveErr; err != nil || got.ItemID != "held" {
			t.Fatalf("held receive after canceled cutover = %+v, %v", got, err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled cutover did not reopen drainable predecessor egress")
	}
	boundaryRequireSend(t, oldQueues[0], valueType, "terminal")
	oldQueues[0].close()
	got, terminalErr := stable.Receive(context.Background())
	if terminalErr != nil || got.ItemID != "terminal" {
		t.Fatalf("terminal drain after canceled cutover = %+v, %v", got, terminalErr)
	}
}

func TestBoundaryRetiredReceiveRetriesWhenPredecessorQueueAlsoCloses(t *testing.T) {
	valueType := boundaryTestType()
	for iteration := 0; iteration < 50; iteration++ {
		oldEgress, oldQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
		router, err := newBoundaryRouter(nil, map[string]*inputPort{"output": oldEgress})
		if err != nil {
			t.Fatal(err)
		}
		stable, _ := router.output("output")
		received := make(chan element.Envelope, 1)
		receiveErr := make(chan error, 1)
		go func() {
			got, err := stable.Receive(context.Background())
			received <- got
			receiveErr <- err
		}()
		boundaryAwait(t, "predecessor receiver did not block", func() bool {
			return router.current.egressAdmission.activeLeases() == 1
		})
		newEgress, newQueues := boundaryTestInput(t, "output", []string{"lane"}, valueType, ir.Lossless, 1, nil)
		candidate, contracts, err := prepareBoundaryGeneration(nil, map[string]*inputPort{"output": newEgress})
		if err != nil {
			t.Fatal(err)
		}
		if err := validateBoundaryContracts(router.contracts, contracts); err != nil {
			t.Fatal(err)
		}
		predecessor := router.current
		_, drained := predecessor.egressAdmission.stop()
		oldQueues[0].close()
		select {
		case <-drained:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d predecessor lease did not drain", iteration)
		}
		router.mu.Lock()
		candidate.sequence = predecessor.sequence + 1
		router.current = candidate
		router.mu.Unlock()
		router.changed.signal()
		boundaryRequireSend(t, newQueues[0], valueType, fmt.Sprintf("new-%d", iteration))
		select {
		case got := <-received:
			if err := <-receiveErr; err != nil || got.ItemID != fmt.Sprintf("new-%d", iteration) {
				t.Fatalf("iteration %d receive = %+v, %v", iteration, got, err)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d receiver did not follow candidate", iteration)
		}
	}
}

func TestBoundaryStableWrappersDoNotDuplicateQueueTelemetry(t *testing.T) {
	valueType := boundaryTestType()
	var traces atomic.Int64
	trace := func(*queue, TraceKind, element.Envelope, int) { traces.Add(1) }
	changed := newCondition()
	channel, err := newQueue("lane", valueType, ir.Lossless, 2, changed, func() uint64 { return 1 }, trace)
	if err != nil {
		t.Fatal(err)
	}
	ingress := &outputPort{name: "input", typ: valueType, queues: []*queue{channel}, changed: changed}
	egress := &inputPort{name: "output", typ: valueType, queues: []*queue{channel}, changed: changed}
	router, err := newBoundaryRouter(
		map[string]*outputPort{"input": ingress}, map[string]*inputPort{"output": egress},
	)
	if err != nil {
		t.Fatal(err)
	}
	stableIngress, _ := router.input("input")
	stableEgress, _ := router.output("output")
	if result, sendErr := stableIngress.Broadcast(context.Background(), envelope(valueType, "once")); sendErr != nil || result.Delivered != 1 {
		t.Fatalf("send = %+v, %v", result, sendErr)
	}
	if got, receiveErr := stableEgress.Receive(context.Background()); receiveErr != nil || got.ItemID != "once" {
		t.Fatalf("receive = %+v, %v", got, receiveErr)
	}
	if got := channel.snapshot(); got.Enqueued != 1 || got.Dequeued != 1 || got.Dropped != 0 {
		t.Fatalf("queue telemetry was duplicated: %+v", got)
	}
	if got := traces.Load(); got != 2 {
		t.Fatalf("trace callbacks = %d, want one enqueue plus one dequeue", got)
	}
}

func TestBoundaryRecoveredTraceOrObserverPanicDoesNotLeakRouteLease(t *testing.T) {
	for _, test := range []struct {
		name     string
		trace    bool
		observer bool
	}{
		{name: "trace", trace: true},
		{name: "observer", observer: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			valueType := boundaryTestType()
			var trace func(*queue, TraceKind, element.Envelope, int)
			if test.trace {
				trace = func(*queue, TraceKind, element.Envelope, int) { panic("trace panic") }
			}
			port, queues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 1, trace)
			if test.observer {
				port.observe = func(element.Envelope) { panic("observer panic") }
			}
			router, err := newBoundaryRouter(map[string]*outputPort{"input": port}, nil)
			if err != nil {
				t.Fatal(err)
			}
			stable, _ := router.input("input")
			func() {
				defer func() {
					if recovered := recover(); recovered == nil {
						t.Fatal("callback panic did not propagate")
					}
				}()
				_, _ = stable.Lanes()[0].Send(context.Background(), envelope(valueType, "committed"))
			}()
			if got := queues[0].snapshot().Enqueued; got != 1 {
				t.Fatalf("committed enqueue count = %d, want 1", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := router.close(ctx); err != nil {
				t.Fatalf("close after recovered callback panic: %v", err)
			}
			if got := router.current.ingressAdmission.activeLeases(); got != 0 {
				t.Fatalf("callback panic leaked %d route leases", got)
			}
		})
	}
}

func TestBoundaryQueueShutdownStopsIngressBeforeWaitingForCommittedObserver(t *testing.T) {
	valueType := boundaryTestType()
	observerStarted := make(chan struct{})
	releaseObserver := make(chan struct{})
	port, _ := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, 1, nil)
	port.observe = func(element.Envelope) {
		close(observerStarted)
		<-releaseObserver
	}
	router, err := newBoundaryRouter(map[string]*outputPort{"input": port}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	sendDone := make(chan error, 1)
	go func() {
		_, sendErr := stable.Broadcast(context.Background(), envelope(valueType, "committed"))
		sendDone <- sendErr
	}()
	select {
	case <-observerStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("observer did not start")
	}
	if err := router.beginQueueShutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case sendErr := <-sendDone:
		t.Fatalf("blocked observer unexpectedly returned: %v", sendErr)
	default:
	}
	if _, sendErr := stable.Broadcast(context.Background(), envelope(valueType, "rejected")); !errors.Is(sendErr, ErrChannelClosed) {
		t.Fatalf("new ingress after shutdown = %v, want channel closed", sendErr)
	}
	close(releaseObserver)
	if sendErr := <-sendDone; sendErr != nil {
		t.Fatal(sendErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := router.waitIngressShutdown(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestBoundaryConcurrentSendsAreConservedAcrossPublication(t *testing.T) {
	valueType := boundaryTestType()
	const sends = 256
	oldPort, oldQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, sends, nil)
	router, err := newBoundaryRouter(map[string]*outputPort{"input": oldPort}, nil)
	if err != nil {
		t.Fatal(err)
	}
	stable, _ := router.input("input")
	newPort, newQueues := boundaryTestOutput(t, "input", []string{"lane"}, valueType, ir.Lossless, sends, nil)
	start := make(chan struct{})
	errorsSeen := make(chan error, sends)
	var workers sync.WaitGroup
	workers.Add(sends)
	for index := range sends {
		go func() {
			defer workers.Done()
			<-start
			result, sendErr := stable.Lanes()[0].Send(
				context.Background(), envelope(valueType, fmt.Sprintf("item-%d", index)),
			)
			if sendErr != nil || result != element.Delivered {
				errorsSeen <- fmt.Errorf("send %d = %s, %w", index, result, sendErr)
			}
		}()
	}
	close(start)
	if err := router.replace(context.Background(), map[string]*outputPort{"input": newPort}, nil, nil); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	close(errorsSeen)
	for sendErr := range errorsSeen {
		t.Error(sendErr)
	}
	oldMetrics := oldQueues[0].snapshot()
	newMetrics := newQueues[0].snapshot()
	if got := oldMetrics.Enqueued + newMetrics.Enqueued; got != sends {
		t.Fatalf("committed sends across generations = %d, want %d (old=%+v new=%+v)",
			got, sends, oldMetrics, newMetrics)
	}
}

func boundaryTestType() element.Type {
	return element.Event(element.Named("test.Value"))
}

func boundaryTestOutput(
	t *testing.T,
	name string,
	laneIDs []string,
	valueType element.Type,
	delivery ir.Delivery,
	depth int,
	trace func(*queue, TraceKind, element.Envelope, int),
) (*outputPort, []*queue) {
	t.Helper()
	changed := newCondition()
	queues := boundaryTestQueues(t, laneIDs, valueType, delivery, depth, changed, trace)
	return &outputPort{name: name, typ: valueType, queues: queues, changed: changed}, queues
}

func boundaryTestInput(
	t *testing.T,
	name string,
	laneIDs []string,
	valueType element.Type,
	delivery ir.Delivery,
	depth int,
	trace func(*queue, TraceKind, element.Envelope, int),
) (*inputPort, []*queue) {
	t.Helper()
	changed := newCondition()
	queues := boundaryTestQueues(t, laneIDs, valueType, delivery, depth, changed, trace)
	return &inputPort{name: name, typ: valueType, queues: queues, changed: changed}, queues
}

func boundaryTestQueues(
	t *testing.T,
	laneIDs []string,
	valueType element.Type,
	delivery ir.Delivery,
	depth int,
	changed *condition,
	trace func(*queue, TraceKind, element.Envelope, int),
) []*queue {
	t.Helper()
	queues := make([]*queue, len(laneIDs))
	for index, id := range laneIDs {
		channel, err := newQueue(id, valueType, delivery, depth, changed, func() uint64 { return 1 }, trace)
		if err != nil {
			t.Fatal(err)
		}
		queues[index] = channel
	}
	return queues
}

func boundaryRequireSend(t *testing.T, queue *queue, valueType element.Type, id string) {
	t.Helper()
	if result, err := queue.send(context.Background(), envelope(valueType, id)); err != nil || result != element.Delivered {
		t.Fatalf("seed send %s = %s, %v", id, result, err)
	}
}

func boundaryRequireItem(t *testing.T, queue *queue, want string) {
	t.Helper()
	got, err := queue.receive(context.Background())
	if err != nil || got.ItemID != want {
		t.Fatalf("queue %s receive = %+v, %v; want %s", queue.id, got, err, want)
	}
}

func boundaryAwait(t *testing.T, description string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatal(description)
		}
		time.Sleep(time.Millisecond)
	}
}

func boundaryAdmissionOpen(admission *routeAdmission) bool {
	admission.mu.Lock()
	defer admission.mu.Unlock()
	return admission.open
}
