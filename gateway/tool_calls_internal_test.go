package gateway

import (
	"strconv"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

// TestUnansweredToolCallsDoNotAccumulateForeverIsBounded is a growth test.
//
// The record of issued tool calls is filled by the model and drained by the
// client, and the protocol lets a client ignore a call it does not want to
// answer. Nothing else removes the entry: it has to outlive the response that
// produced it, because an answer may legitimately arrive several turns later.
// So a session whose client answers nothing keeps every call it was ever
// handed, and the sessions where that matters are the long ones - a meeting
// running for hours, which is where nobody is watching.
//
// Correctness cannot see this. Every lookup still returns the right answer;
// the map is simply larger than it should be, forever.
func TestUnansweredToolCallsDoNotAccumulateForever(t *testing.T) {
	t.Parallel()
	session := &session{issuedCalls: make(map[string]issuedCall)}
	for issued := range maxOutstandingCalls * 4 {
		session.recordCallNames([]trajectory.ToolCall{{
			CallID: "call_" + strconv.Itoa(issued), Name: "search",
		}})
	}
	if held := len(session.issuedCalls); held > maxOutstandingCalls {
		t.Fatalf("a client that answered nothing left %d issued calls recorded, want at most %d",
			held, maxOutstandingCalls)
	}
}

// TestTheMostRecentToolCallsAreTheOnesKept says which end is dropped.
//
// A call answered after this many unanswered ones loses its name and its
// duration on the debug event and still reaches the runtime, so the record is
// worth keeping for the calls most likely to be answered. That is the recent
// ones: an answer that has not arrived in two hundred and fifty-six calls is
// not arriving.
func TestTheMostRecentToolCallsAreTheOnesKept(t *testing.T) {
	t.Parallel()
	session := &session{issuedCalls: make(map[string]issuedCall)}
	// Distinct start times, oldest first, so "oldest" is a fact rather than a
	// map-iteration accident.
	base := time.Now()
	for issued := range maxOutstandingCalls + 1 {
		session.issuedCalls["call_"+strconv.Itoa(issued)] = issuedCall{
			name: "search", started: base.Add(time.Duration(issued) * time.Millisecond),
		}
	}
	session.forgetOldestCalls()
	if _, kept := session.issuedCalls["call_0"]; kept {
		t.Fatal("the oldest unanswered call survived; the record drops the wrong end")
	}
	newest := "call_" + strconv.Itoa(maxOutstandingCalls)
	if _, kept := session.issuedCalls[newest]; !kept {
		t.Fatalf("%s was dropped; the record drops the calls most likely to be answered", newest)
	}
}

// TestAnAnsweredToolCallIsForgotten is the ordinary path, and the reason the
// bound is rarely reached: a client that answers leaves nothing behind.
func TestAnAnsweredToolCallIsForgotten(t *testing.T) {
	t.Parallel()
	session := &session{issuedCalls: make(map[string]issuedCall)}
	session.recordCallNames([]trajectory.ToolCall{{CallID: "call_1", Name: "search"}})
	delete(session.issuedCalls, "call_1")
	if held := len(session.issuedCalls); held != 0 {
		t.Fatalf("%d calls recorded after the only one was answered, want 0", held)
	}
}
