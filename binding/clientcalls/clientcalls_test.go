package clientcalls_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/binding/clientcalls"
	"github.com/bojieli/OpenRealtime/internal/clock"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type recorder struct {
	mu        sync.Mutex
	batches   map[string][]trajectory.ToolResult
	expired   map[string][]trajectory.ToolCall
	failNext  bool
	committed int
}

func newRecorder() *recorder {
	return &recorder{
		batches: make(map[string][]trajectory.ToolResult),
		expired: make(map[string][]trajectory.ToolCall),
	}
}

func (record *recorder) commit(invocationID string, results []trajectory.ToolResult) error {
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.failNext {
		record.failNext = false
		return errors.New("the event loop refused the batch")
	}
	record.batches[invocationID] = results
	record.committed++
	return nil
}

func (record *recorder) expire(invocationID string, unanswered []trajectory.ToolCall) {
	record.mu.Lock()
	defer record.mu.Unlock()
	record.expired[invocationID] = unanswered
}

func (record *recorder) batch(invocationID string) []trajectory.ToolResult {
	record.mu.Lock()
	defer record.mu.Unlock()
	return record.batches[invocationID]
}

func newTracker(t *testing.T, record *recorder, timeout time.Duration, scheduler clock.Scheduler) *clientcalls.Tracker {
	t.Helper()
	tracker, err := clientcalls.New(clientcalls.Config{
		Timeout: timeout, Scheduler: scheduler,
		Commit: record.commit, Expired: record.expire,
	})
	if err != nil {
		t.Fatalf("new tracker: %v", err)
	}
	t.Cleanup(tracker.Close)
	return tracker
}

func calls(names ...string) []trajectory.ToolCall {
	result := make([]trajectory.ToolCall, 0, len(names))
	for _, name := range names {
		result = append(result, trajectory.ToolCall{CallID: "call_" + name, Name: name})
	}
	return result
}

func TestABatchCommitsOnlyWhenEveryCallHasAResult(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	tracker := newTracker(t, record, 0, clock.NewManual(0))
	if err := tracker.Track("inv_1", calls("read", "write")); err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read", Output: []byte(`"ok"`)}); err != nil {
		t.Fatalf("first result: %v", err)
	}
	if record.batch("inv_1") != nil {
		t.Fatal("a partial batch must not commit")
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_write", Output: []byte(`"ok"`)}); err != nil {
		t.Fatalf("second result: %v", err)
	}
	batch := record.batch("inv_1")
	if len(batch) != 2 || batch[0].CallID != "call_read" || batch[1].CallID != "call_write" {
		t.Fatalf("the batch must arrive in call order: %v", batch)
	}
	// The name comes from the call rather than from the client, which sends
	// only an identifier and an output.
	if batch[0].Name != "read" || batch[1].Name != "write" {
		t.Fatalf("results must carry the tool name: %v", batch)
	}
	if tracker.Outstanding() != 0 {
		t.Fatal("a committed invocation must not stay outstanding")
	}
}

// The defect this package exists for: a client that never answers used to
// leave the invocation outstanding for the life of the session.
func TestAnUnansweredInvocationCommitsWhenItsDeadlinePasses(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, 30*time.Second, scheduler)
	if err := tracker.Track("inv_1", calls("read", "write")); err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read", Output: []byte(`"ok"`)}); err != nil {
		t.Fatalf("result: %v", err)
	}

	scheduler.AdvanceNS(uint64(29 * time.Second))
	if record.batch("inv_1") != nil {
		t.Fatal("the deadline had not passed yet")
	}
	scheduler.AdvanceNS(uint64(2 * time.Second))

	batch := record.batch("inv_1")
	if len(batch) != 2 {
		t.Fatalf("the whole batch must commit: %v", batch)
	}
	// What the client did answer is kept; what it did not says so.
	if batch[0].Error != "" || string(batch[0].Output) != `"ok"` {
		t.Fatalf("an answered call must keep its result: %v", batch[0])
	}
	if !strings.Contains(batch[1].Error, "did not return a result") {
		t.Fatalf("an unanswered call must say so: %v", batch[1])
	}
	if batch[1].Name != "write" {
		t.Fatalf("a synthesised result must still identify its tool: %v", batch[1])
	}
	if tracker.Outstanding() != 0 {
		t.Fatal("an expired invocation must not stay outstanding")
	}

	record.mu.Lock()
	unanswered := record.expired["inv_1"]
	record.mu.Unlock()
	if len(unanswered) != 1 || unanswered[0].Name != "write" {
		t.Fatalf("the session must learn which calls went unanswered: %v", unanswered)
	}
}

func TestACompletedInvocationDoesNotAlsoExpire(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, 30*time.Second, scheduler)
	if err := tracker.Track("inv_1", calls("read")); err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read", Output: []byte(`"ok"`)}); err != nil {
		t.Fatalf("result: %v", err)
	}
	scheduler.AdvanceNS(uint64(time.Minute))

	record.mu.Lock()
	defer record.mu.Unlock()
	if record.committed != 1 {
		t.Fatalf("expected exactly one commit, got %d", record.committed)
	}
	if len(record.expired) != 0 {
		t.Fatal("a completed invocation has nothing to expire")
	}
}

// A split batch: some calls run here, the rest run on the client, and the two
// halves commit together or not at all.
func TestLocalResultsWaitForTheClientsHalfOfTheBatch(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	tracker := newTracker(t, record, 0, clock.NewManual(0))
	batch := calls("computer.click", "read_file")
	if err := tracker.Track("inv_1", batch); err != nil {
		t.Fatalf("track: %v", err)
	}
	tracker.Hold("inv_1", []trajectory.ToolResult{
		{CallID: "call_computer.click", Name: "computer.click", Output: []byte(`"clicked"`)},
	})
	if record.batch("inv_1") != nil {
		t.Fatal("the local half must not commit on its own")
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read_file", Output: []byte(`"contents"`)}); err != nil {
		t.Fatalf("client result: %v", err)
	}
	committed := record.batch("inv_1")
	if len(committed) != 2 || string(committed[0].Output) != `"clicked"` {
		t.Fatalf("both halves must commit together: %v", committed)
	}
}

func TestTheDeadlineCoversTheClientsHalfOfASplitBatch(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, 10*time.Second, scheduler)
	if err := tracker.Track("inv_1", calls("computer.click", "read_file")); err != nil {
		t.Fatalf("track: %v", err)
	}
	tracker.Hold("inv_1", []trajectory.ToolResult{
		{CallID: "call_computer.click", Name: "computer.click", Output: []byte(`"clicked"`)},
	})
	scheduler.AdvanceNS(uint64(11 * time.Second))
	committed := record.batch("inv_1")
	if len(committed) != 2 {
		t.Fatalf("the batch must commit: %v", committed)
	}
	if string(committed[0].Output) != `"clicked"` {
		t.Fatalf("an action that did happen must be recorded as having happened: %v", committed[0])
	}
	if committed[1].Error == "" {
		t.Fatalf("the unanswered half must say so: %v", committed[1])
	}
}

func TestAResultThatCannotBeCommittedLeavesTheInvocationRetryable(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, 30*time.Second, scheduler)
	if err := tracker.Track("inv_1", calls("read")); err != nil {
		t.Fatalf("track: %v", err)
	}
	record.mu.Lock()
	record.failNext = true
	record.mu.Unlock()
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read"}); err == nil {
		t.Fatal("a refused commit must be reported")
	}
	if tracker.Outstanding() != 1 {
		t.Fatal("a refused commit leaves the invocation outstanding")
	}
	// The deadline is still armed, so the session recovers rather than
	// holding the invocation forever because one commit failed.
	scheduler.AdvanceNS(uint64(time.Minute))
	if record.batch("inv_1") == nil {
		t.Fatal("the deadline must still be able to commit the batch")
	}
}

func TestMalformedResultsAreRefused(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	tracker := newTracker(t, record, 0, clock.NewManual(0))
	if err := tracker.Track("inv_1", calls("read")); err != nil {
		t.Fatalf("track: %v", err)
	}
	if err := tracker.Track("inv_1", calls("read")); err == nil {
		t.Fatal("an invocation cannot be tracked twice")
	}
	if err := tracker.Result(trajectory.ToolResult{}); err == nil {
		t.Fatal("a result with no call ID is not addressable")
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_unknown"}); err == nil {
		t.Fatal("a result for a call nobody made is not addressable")
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read"}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := tracker.Result(trajectory.ToolResult{CallID: "call_read"}); err == nil {
		t.Fatal("a completed invocation cannot take another result")
	}
}

func TestANegativeTimeoutDisablesTheDeadline(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, -1, scheduler)
	if err := tracker.Track("inv_1", calls("read")); err != nil {
		t.Fatalf("track: %v", err)
	}
	scheduler.AdvanceNS(uint64(time.Hour))
	if record.batch("inv_1") != nil {
		t.Fatal("a disabled deadline must never fire")
	}
	if tracker.Outstanding() != 1 {
		t.Fatal("the invocation is still waiting on the client")
	}
}

func TestClosingStopsEveryDeadline(t *testing.T) {
	t.Parallel()
	record := newRecorder()
	scheduler := clock.NewManual(0)
	tracker := newTracker(t, record, time.Second, scheduler)
	if err := tracker.Track("inv_1", calls("read")); err != nil {
		t.Fatalf("track: %v", err)
	}
	tracker.Close()
	scheduler.AdvanceNS(uint64(time.Minute))
	record.mu.Lock()
	defer record.mu.Unlock()
	if record.committed != 0 {
		t.Fatal("a closed session must not commit into a runtime that is shutting down")
	}
}

func TestATrackerRequiresSomewhereToCommit(t *testing.T) {
	t.Parallel()
	if _, err := clientcalls.New(clientcalls.Config{}); err == nil {
		t.Fatal("a tracker with no commit has nowhere to put a completed batch")
	}
}
