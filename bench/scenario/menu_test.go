package scenario

import "testing"

// A fixed script cannot tell an agent that pressed once from an agent that
// pressed six times - both hear the recording to the end. Measured, one call
// took nine presses and the scenario could not see it, while a person doing
// that lands three menus deep and never reaches what they wanted.
func TestTheMenuMovesWhenAKeyIsPressed(t *testing.T) {
	menu := OrderStatusMenu()
	if menu.Reached() {
		t.Fatal("the call starts at the main menu, not at its destination")
	}
	says, moved := menu.Press("2")
	if !moved || !contains(says, "order status") {
		t.Fatalf("pressing two must reach order status: %q moved=%v", says, moved)
	}
	if !menu.Reached() {
		t.Fatal("the goal was not recognised as reached")
	}

	// And pressing again does not un-press it: the caller is somewhere else
	// now, and two is not an option here.
	says, moved = menu.Press("2")
	if moved {
		t.Fatalf("a key that is not an option here must be refused: %q", says)
	}
	if menu.Presses() != 2 {
		t.Fatalf("both presses count: %d", menu.Presses())
	}
}

// The wrong key takes the call somewhere real, which is what makes pressing
// hastily cost something rather than being free.
func TestTheWrongKeyGoesSomewhereElse(t *testing.T) {
	menu := OrderStatusMenu()
	if _, moved := menu.Press("1"); !moved {
		t.Fatal("one is an option and must move the call")
	}
	if menu.Reached() {
		t.Fatal("billing is not order status")
	}
	if where := menu.Where(); where != "billing" {
		t.Fatalf("the call is in %q", where)
	}
	// And there is no way back from here by pressing two, which is exactly
	// the cost a fixed script could not represent.
	if _, moved := menu.Press("2"); moved {
		t.Fatal("billing has no options, so the keypad does nothing")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(needle) == 0 || indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return index
		}
	}
	return -1
}
