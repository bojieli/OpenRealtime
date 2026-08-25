package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/bojieli/OpenRealtime/interaction"
)

// newShadowRecorder writes every shadowed decision to a file.
//
// One line per decision, carrying the situation verbatim. That is the whole
// value of it: a disagreement between the interaction model and the predicates
// is only diagnosable next to what the model was actually shown, and a record
// that summarises the moment instead of quoting it leaves the reader
// reconstructing a different one.
//
// A recorder that failed would take the session down with it, which is the
// wrong trade for something that decides nothing. Write errors stop the
// recording and leave the conversation alone.
func newShadowRecorder(path string) (func(interaction.ShadowDecision), error) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open the shadow record: %w", err)
	}
	var mu sync.Mutex
	var broken bool
	return func(record interaction.ShadowDecision) {
		encoded, err := json.Marshal(record)
		if err != nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if broken {
			return
		}
		if _, err := fmt.Fprintf(file, "%s\n", encoded); err != nil {
			broken = true
		}
	}, nil
}
