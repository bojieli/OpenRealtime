package interaction_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
	"github.com/bojieli/OpenRealtime/trajectory"
)

func turn(id string, words int) trajectory.Item {
	return trajectory.Item{
		ID: id, Kind: trajectory.KindObservation,
		Producer: trajectory.Producer{Phase: trajectory.PhaseUser},
		Content:  id + " " + strings.TrimSpace(strings.Repeat("word ", words)),
	}
}

func conversation(count, words int) []trajectory.Item {
	items := make([]trajectory.Item, count)
	for index := range items {
		items[index] = turn(fmt.Sprintf("t%d", index), words)
	}
	return items
}

// The property the whole type exists for: appending must not move the start,
// because the start is where a cached prefix begins.
func TestWindowDoesNotRollOnEveryTurn(t *testing.T) {
	window := &interaction.Window{LowerBudget: 100, UpperBudget: 250}
	items := conversation(1, 20)
	first := window.Lines(items)[0]
	moves := 0
	for count := 2; count <= 12; count++ {
		items = conversation(count, 20)
		lines := window.Lines(items)
		if lines[0] != first {
			moves++
			first = lines[0]
		}
	}
	if moves == 0 {
		t.Fatal("the window never truncated across eleven turns; the budgets are not being applied")
	}
	if moves > 3 {
		t.Fatalf("the window start moved %d times in eleven turns, so the prefix cache is invalidated almost every turn", moves)
	}
}

// A truncation goes all the way to the lower bound, so the next one is far
// away rather than one turn later.
func TestWindowTruncatesToTheLowerBoundNotJustUnderTheUpper(t *testing.T) {
	window := &interaction.Window{LowerBudget: 100, UpperBudget: 250}
	items := conversation(40, 20)
	lines := window.Lines(items)
	size := 0
	for _, line := range lines {
		size += len([]rune(line))/4 + 2
	}
	if size > 100 {
		t.Fatalf("after truncation the window is %d tokens, above the lower bound of 100", size)
	}
	if size == 0 {
		t.Fatal("truncation emptied the window")
	}
}

// The trajectory is pruned from the front, so an index would silently come to
// mean a different item.
func TestWindowStartSurvivesPruningFromTheFront(t *testing.T) {
	window := &interaction.Window{LowerBudget: 100, UpperBudget: 250}
	items := conversation(30, 20)
	before := strings.Join(window.Lines(items), "\n")
	after := strings.Join(window.Lines(items[2:]), "\n")
	if before != after {
		t.Fatalf("pruning items the window had already passed moved it.\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// What an interaction model needs from the past is who said what. Reasoning
// and tool plumbing are the agent talking to itself.
func TestWindowCarriesOnlyConversation(t *testing.T) {
	lines := (&interaction.Window{}).Lines([]trajectory.Item{
		turn("u1", 3),
		{ID: "r1", Kind: trajectory.KindReasoning, Content: "considering the options"},
		{ID: "a1", Kind: trajectory.KindAssistant, Content: "here you go",
			Producer: trajectory.Producer{Phase: trajectory.PhaseFast}},
		{ID: "s1", Kind: trajectory.KindObservation, Content: "a dialog is open",
			Producer:    trajectory.Producer{Phase: trajectory.PhaseObserver},
			Observation: &trajectory.ObservationMeta{Observer: "video", Authority: trajectory.AuthorityObserver}},
	})
	joined := strings.Join(lines, "\n")
	if strings.Contains(joined, "considering the options") {
		t.Fatalf("reasoning reached the window:\n%s", joined)
	}
	if !strings.Contains(joined, "agent: here you go") || !strings.Contains(joined, "user: u1 word") {
		t.Fatalf("conversation missing from the window:\n%s", joined)
	}
	if !strings.Contains(joined, "seen: a dialog is open") {
		t.Fatalf("an observer's report was dropped or mislabelled as speech:\n%s", joined)
	}
}
