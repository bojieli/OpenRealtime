package cognition

import (
	"crypto/sha256"
	"strconv"
	"testing"
)

func TestCommittedCanceledRunMemoryIsBoundedAndIdentityIsLengthFramed(t *testing.T) {
	runner := &textModelRunner{committedCanceledRuns: make(map[[sha256.Size]byte]struct{})}
	for index := 0; index <= committedCancelMemory; index++ {
		runner.rememberCommittedCanceledRun("session-a", "run-"+strconv.Itoa(index))
	}
	if len(runner.committedCanceledRuns) != committedCancelMemory ||
		len(runner.committedCanceledRunOrder) != committedCancelMemory {
		t.Fatalf("canceled-run memory = %d/%d, want %d",
			len(runner.committedCanceledRuns), len(runner.committedCanceledRunOrder),
			committedCancelMemory)
	}
	if runner.wasCommittedRunCanceled("session-a", "run-0") {
		t.Fatal("oldest canceled-run tombstone was not evicted at the documented horizon")
	}
	if !runner.wasCommittedRunCanceled("session-a", "run-1") ||
		!runner.wasCommittedRunCanceled("session-a", "run-512") {
		t.Fatal("retained canceled-run tombstones were lost")
	}
	runner.rememberCommittedCanceledRun("session-a", "run-512")
	if len(runner.committedCanceledRuns) != committedCancelMemory ||
		len(runner.committedCanceledRunOrder) != committedCancelMemory {
		t.Fatal("duplicate canceled-run tombstone changed bounded memory")
	}
	if committedRunKey("a", "bc") == committedRunKey("ab", "c") {
		t.Fatal("canceled-run commitment does not length-frame session and run IDs")
	}
}
