package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestLosslessQueueBackpressuresAndPreservesOrder(t *testing.T) {
	changed := newCondition()
	valueType := element.Event(element.Named("test.Value"))
	queue, err := newQueue("lossless", valueType, ir.Lossless, 1, changed, func() uint64 { return 0 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	first := envelope(valueType, "first")
	second := envelope(valueType, "second")
	if result, err := queue.send(context.Background(), first); err != nil || result != element.Delivered {
		t.Fatalf("first send = %s, %v", result, err)
	}
	done := make(chan error, 1)
	go func() {
		_, sendErr := queue.send(context.Background(), second)
		done <- sendErr
	}()
	select {
	case err := <-done:
		t.Fatalf("second send did not backpressure: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	got, err := queue.receive(context.Background())
	if err != nil || got.ItemID != "first" {
		t.Fatalf("first receive = %+v, %v", got, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backpressured sender did not wake")
	}
	got, err = queue.receive(context.Background())
	if err != nil || got.ItemID != "second" {
		t.Fatalf("second receive = %+v, %v", got, err)
	}
	metrics := queue.snapshot()
	if metrics.Backpressure != 1 || metrics.HighWater != 1 || metrics.Enqueued != 2 || metrics.Dequeued != 2 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestLossyQueueDropsNewestWhenFull(t *testing.T) {
	valueType := element.Stream(element.Named("video.Frame"))
	queue, err := newQueue("lossy", valueType, ir.Lossy, 1, newCondition(), func() uint64 { return 0 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.send(context.Background(), envelope(valueType, "kept")); err != nil {
		t.Fatal(err)
	}
	result, err := queue.send(context.Background(), envelope(valueType, "dropped"))
	if err != nil || result != element.Dropped {
		t.Fatalf("lossy send = %s, %v", result, err)
	}
	got, err := queue.receive(context.Background())
	if err != nil || got.ItemID != "kept" {
		t.Fatalf("receive = %+v, %v", got, err)
	}
	metrics := queue.snapshot()
	if metrics.Dropped != 1 || metrics.Enqueued != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
}

func TestQueueReportsCumulativeResidenceTime(t *testing.T) {
	valueType := element.Event(element.Named("test.Value"))
	var now uint64 = 10
	queue, err := newQueue("timed", valueType, ir.Lossless, 2, newCondition(), func() uint64 {
		return now
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := queue.send(context.Background(), envelope(valueType, "first")); err != nil {
		t.Fatal(err)
	}
	now = 35
	if _, err := queue.receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.snapshot().QueueWaitNS; got != 25 {
		t.Fatalf("queue residence time = %d, want 25", got)
	}
	now = 40
	if _, err := queue.send(context.Background(), envelope(valueType, "second")); err != nil {
		t.Fatal(err)
	}
	now = 55
	if _, err := queue.receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := queue.snapshot().QueueWaitNS; got != 40 {
		t.Fatalf("cumulative queue residence time = %d, want 40", got)
	}
}

func TestBroadcastWaitsForEveryLosslessBranchBeforeAnyAdmission(t *testing.T) {
	changed := newCondition()
	valueType := element.Event(element.Named("test.Value"))
	left, _ := newQueue("left", valueType, ir.Lossless, 1, changed, func() uint64 { return 0 }, nil)
	right, _ := newQueue("right", valueType, ir.Lossless, 1, changed, func() uint64 { return 0 }, nil)
	if _, err := left.send(context.Background(), envelope(valueType, "occupy")); err != nil {
		t.Fatal(err)
	}
	observed := make(chan string, 2)
	port := &outputPort{
		name: "out", typ: valueType, queues: []*queue{left, right}, changed: changed,
		observe: func(envelope element.Envelope) { observed <- envelope.ItemID },
	}
	done := make(chan error, 1)
	go func() {
		_, sendErr := port.Broadcast(context.Background(), envelope(valueType, "broadcast"))
		done <- sendErr
	}()
	select {
	case err := <-done:
		t.Fatalf("broadcast did not wait: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	select {
	case item := <-observed:
		t.Fatalf("blocked broadcast was observed before atomic admission: %s", item)
	default:
	}
	right.mu.Lock()
	rightSize := right.size
	right.mu.Unlock()
	if rightSize != 0 {
		t.Fatalf("unblocked branch observed a partial broadcast; size = %d", rightSize)
	}
	if _, err := left.receive(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("broadcast did not resume")
	}
	select {
	case item := <-observed:
		if item != "broadcast" {
			t.Fatalf("observed broadcast = %q", item)
		}
	case <-time.After(time.Second):
		t.Fatal("successful broadcast was not observed")
	}
	select {
	case item := <-observed:
		t.Fatalf("multi-lane broadcast was observed more than once: %s", item)
	default:
	}
	for _, queue := range []*queue{left, right} {
		got, err := queue.receive(context.Background())
		if err != nil || got.ItemID != "broadcast" {
			t.Fatalf("branch %s receive = %+v, %v", queue.id, got, err)
		}
	}
}

func TestLaneAccessPreservesPortObservationBoundary(t *testing.T) {
	changed := newCondition()
	valueType := element.Event(element.Named("test.Value"))
	laneQueue, err := newQueue("lane", valueType, ir.Lossless, 2, changed, func() uint64 { return 1 }, nil)
	if err != nil {
		t.Fatal(err)
	}
	outputObserved := make(chan string, 1)
	inputObserved := make(chan string, 1)
	output := &outputPort{
		name: "out", typ: valueType, queues: []*queue{laneQueue}, changed: changed,
		observe: func(envelope element.Envelope) { outputObserved <- envelope.ItemID },
	}
	input := &inputPort{
		name: "in", typ: valueType, queues: []*queue{laneQueue}, changed: changed,
		observe: func(envelope element.Envelope) { inputObserved <- envelope.ItemID },
	}
	message := envelope(valueType, "lane-message")
	if result, sendErr := output.Lanes()[0].Send(context.Background(), message); sendErr != nil || result != element.Delivered {
		t.Fatalf("lane send = %s, %v", result, sendErr)
	}
	if got, receiveErr := input.Lanes()[0].Receive(context.Background()); receiveErr != nil || got.ItemID != message.ItemID {
		t.Fatalf("lane receive = %+v, %v", got, receiveErr)
	}
	for name, channel := range map[string]<-chan string{"output": outputObserved, "input": inputObserved} {
		select {
		case item := <-channel:
			if item != message.ItemID {
				t.Fatalf("%s lane observation = %q", name, item)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s lane access bypassed observation", name)
		}
	}
}

func TestQueueCancellationAndCloseWakeWaiters(t *testing.T) {
	valueType := element.Event(element.Named("test.Value"))
	queue, _ := newQueue("closed", valueType, ir.Lossless, 1, newCondition(), func() uint64 { return 0 }, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	cause := errors.New("stop waiting")
	cancel(cause)
	if _, err := queue.receive(ctx); !errors.Is(err, cause) {
		t.Fatalf("receive error = %v, want %v", err, cause)
	}
	queue.close()
	if _, err := queue.receive(context.Background()); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("closed receive error = %v", err)
	}
}

func envelope(valueType element.Type, id string) element.Envelope {
	return element.Envelope{Type: valueType, ItemID: id, Payload: id}
}
