package cascade

import "github.com/bojieli/OpenRealtime/interaction"

// carryOut performs the part of an act that does not depend on where it was
// decided.
//
// An act arrives at two places - the pause path in audio.go and the projection
// path in floor.go - and each used to decide for itself what the act meant.
// They drifted, twice, and both times the failure was silence rather than an
// error: an act was chosen, matched no branch at that site, and nothing
// happened. Nine speak-throughs were decided and dropped at a pause before the
// branch was copied there, and copying it is what made the next one inevitable
// - cutting in was added to projection alone and was dropped at a pause in
// exactly the same way.
//
// So the meaning lives here and both sites carry it out. Adding an act to the
// vocabulary now means adding it here once, and the test beside this file
// fails if an act is left in neither list, because the thing that must not
// happen again is an act quietly meaning nothing.
func (runtime *runtime) carryOut(decision interaction.Context, act interaction.Act) {
	switch act {
	case interaction.ActActSilently:
		runtime.actSilently(decision)
	case interaction.ActSpeakThrough, interaction.ActInterrupt:
		// Both produce speech over somebody who still holds the floor, and
		// differ in why. The reason travels with the request so the voice
		// knows whether it is correcting something that cannot wait or
		// carrying out a standing policy.
		runtime.interjectFor(decision, act)
	}
}

// tookTheFloor reports whether an endpoint was taken from somebody who was
// still speaking rather than offered by them finishing.
//
// The voice is told which it got, because what belongs in a turn you took
// differs from what belongs in one you were given. This is a second thing an
// act means, so it lives beside the first rather than being spelled out at
// each site - it was written twice, once per dispatch site, which is the same
// shape as the defect above and one edit away from the same drift.
func tookTheFloor(act interaction.Act) bool {
	return act == interaction.ActInterrupt
}

// actsCarriedOut are the acts carryOut performs. actsElsewhere are the ones it
// deliberately does not, each because something else owns them: answering ends
// the turn through the endpoint, listening is the absence of an act, and the
// two speaking acts are the barge-in policy's while the agent holds the floor.
//
// Written down so the test can require that every act in the vocabulary
// appears in exactly one of them.
var (
	actsCarriedOut = []interaction.Act{
		interaction.ActActSilently,
		interaction.ActSpeakThrough,
		interaction.ActInterrupt,
	}
	actsElsewhere = []interaction.Act{
		interaction.ActStaySilent,
		interaction.ActAnswer,
		interaction.ActKeepSpeaking,
		interaction.ActStopSpeaking,
	}
)
