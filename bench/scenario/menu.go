package scenario

import (
	"encoding/json"
	"strings"
	"sync"
)

// Menu is a recorded phone menu that answers a keypress.
//
// The first version of the phone-menu scenario read a fixed script whatever
// the agent did, which makes two different behaviours indistinguishable: an
// agent that presses the right key once and an agent that presses it six times
// both hear the same recording to the end. Measured, one call took nine
// presses and the scenario could not see it - a person doing that lands three
// menus deep and never reaches what they wanted.
//
// So the menu has states. Pressing a key moves it, an unexpected key is
// refused the way a real one refuses, and what plays next depends on where the
// agent has got to. That is what makes "pressed into the next option" a thing
// the harness can observe rather than a sentence in a note.
type Menu struct {
	// States maps a state name to what the menu says on entering it and where
	// each key leads. The zero name "" is where the call starts.
	States map[string]MenuState
	// Goal is the state the call is trying to reach.
	Goal string

	mu      sync.Mutex
	at      string
	visited []string
	presses int
}

// MenuState is one prompt and the keys it accepts.
type MenuState struct {
	// Says is read aloud on entering. Empty means the state is terminal and
	// the scenario's own script carries the audio.
	Says string
	// Keys maps a digit to the state it leads to. A digit not here is refused.
	Keys map[string]string
}

// Press moves the menu, and reports what it says next.
//
// An unrecognised key leaves the menu where it is and says so, which is what a
// real one does: the caller has not moved, and pressing again is not the same
// as pressing correctly the first time.
func (menu *Menu) Press(digit string) (says string, moved bool) {
	menu.mu.Lock()
	defer menu.mu.Unlock()
	menu.presses++
	state, known := menu.States[menu.at]
	if !known {
		return "", false
	}
	next, ok := state.Keys[strings.TrimSpace(digit)]
	if !ok {
		return "That is not one of the options. " + state.Says, false
	}
	menu.at = next
	menu.visited = append(menu.visited, next)
	return menu.States[next].Says, true
}

// Reached reports whether the call got where it was going.
func (menu *Menu) Reached() bool {
	menu.mu.Lock()
	defer menu.mu.Unlock()
	return menu.at == menu.Goal
}

// Presses is how many keys were sent, which is the number that separates an
// agent that navigated from one that hammered the keypad.
func (menu *Menu) Presses() int {
	menu.mu.Lock()
	defer menu.mu.Unlock()
	return menu.presses
}

// Where names the state the call ended in, for a failure worth reading.
func (menu *Menu) Where() string {
	menu.mu.Lock()
	defer menu.mu.Unlock()
	if menu.at == "" {
		return "the main menu"
	}
	return menu.at
}

// Respond answers a press_key call as the menu would.
func (menu *Menu) Respond(_ string, arguments json.RawMessage) (json.RawMessage, error) {
	var call struct {
		Digit string `json:"digit"`
	}
	_ = json.Unmarshal(arguments, &call)
	says, moved := menu.Press(call.Digit)
	return json.Marshal(map[string]any{
		"ok": moved, "menu_says": says, "at": menu.Where(),
	})
}

// OrderStatusMenu is the menu the phone scenario dials.
func OrderStatusMenu() *Menu {
	return &Menu{
		Goal: "order-status",
		States: map[string]MenuState{
			"": {
				Says: "Press one for billing. Press two for order status. Press three for technical support.",
				Keys: map[string]string{"1": "billing", "2": "order-status", "3": "support"},
			},
			"billing":      {Says: "You have reached billing. Please hold."},
			"order-status": {Says: "You have reached order status. Please say your order number."},
			"support":      {Says: "You have reached technical support. Please hold."},
		},
	}
}
