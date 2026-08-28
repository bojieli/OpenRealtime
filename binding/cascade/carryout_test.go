package cascade

import (
	"os"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/interaction"
)

// Every act must be either carried out or deliberately left to something else.
// An act in neither list is one that means nothing, and meaning nothing is
// what this whole file exists to stop being silent about: twice an act was
// chosen, matched no branch at the site that decided it, and nothing happened
// - no error, no log, just a decision that evaporated.
func TestEveryActIsEitherCarriedOutOrDeliberatelyNot(t *testing.T) {
	placed := map[interaction.Act]int{}
	for _, act := range actsCarriedOut {
		placed[act]++
	}
	for _, act := range actsElsewhere {
		placed[act]++
	}
	for _, act := range interaction.AllActs() {
		switch placed[act] {
		case 0:
			t.Errorf("%q is in the vocabulary and nothing says what it does", act)
		case 1:
		default:
			t.Errorf("%q is both carried out here and left to something else", act)
		}
	}
	for act := range placed {
		found := false
		for _, known := range interaction.AllActs() {
			if act == known {
				found = true
			}
		}
		if !found {
			t.Errorf("%q is placed but is not in the vocabulary", act)
		}
	}
}

// Both places an act arrives must carry it out the same way, which means
// neither may decide for itself what an act means. The check is textual
// because the defect is textual: a branch on a specific act, at a site that
// does not own the answer.
func TestNeitherDispatchSiteDecidesWhatAnActMeans(t *testing.T) {
	for _, file := range []string{"floor.go", "audio.go"} {
		source, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(source), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			for _, act := range actsCarriedOut {
				name := actConstantName(act)
				if strings.Contains(trimmed, "interaction."+name) &&
					!strings.Contains(trimmed, "carryOut") &&
					!strings.Contains(trimmed, "tookTheFloor") {
					t.Errorf("%s:%d branches on %s instead of carrying the act out: %q",
						file, number+1, name, trimmed)
				}
			}
		}
	}
}

func actConstantName(act interaction.Act) string {
	switch act {
	case interaction.ActActSilently:
		return "ActActSilently"
	case interaction.ActSpeakThrough:
		return "ActSpeakThrough"
	case interaction.ActInterrupt:
		return "ActInterrupt"
	}
	return "ActUnknown"
}
