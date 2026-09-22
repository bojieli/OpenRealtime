package capability_test

import (
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/bench/capability"
)

func TestPilotIsWellFormed(t *testing.T) {
	if err := capability.ValidatePairs(capability.Pilot()); err != nil {
		t.Fatalf("the pilot suite violates the study's contract:\n%v", err)
	}
}

func TestPilotCoversEveryFamilyEqually(t *testing.T) {
	counted := map[capability.Family]int{}
	for _, pair := range capability.Pilot() {
		counted[pair.Family]++
	}
	for _, family := range capability.Families {
		if counted[family] != 3 {
			t.Errorf("family %q has %d pairs, want 3", family, counted[family])
		}
	}
	if total := len(capability.Pilot()); total != 24 {
		t.Errorf("pilot has %d pairs, want the preregistered 24", total)
	}
}

// The prefix is one slice shared by both branches rather than two copies, so
// it cannot drift. This checks the consequence that actually matters: the
// evidence a policy receives before the branches diverge is identical, under
// every cell, once the release schedule has been computed.
func TestBothBranchesReceiveTheIdenticalPrefix(t *testing.T) {
	for _, pair := range capability.Pilot() {
		for _, cell := range capability.Cells() {
			var first []capability.Admission
			for _, variant := range pair.Variants {
				source := capability.NewSource(pair.Branch(variant), cell.Channels, capability.DefaultDelays)
				shared := []capability.Admission{}
				for _, admission := range source.Schedule() {
					if admission.SourceStart < pair.PrefixEnd() {
						shared = append(shared, admission)
					}
				}
				if first == nil {
					first = shared
					continue
				}
				if !slices.Equal(first, shared) {
					t.Errorf("%s cell %s: branch %q sees a different prefix:\n got %+v\nwant %+v",
						pair.ID, cell.ID, variant.ID, shared, first)
				}
			}
		}
	}
}

// Nothing may be released before it happened. For words and annotations that
// means after they finished; for acoustic activity, after it began, because
// that a voice has started is exactly the evidence a trailing recogniser
// cannot give.
func TestNoEvidenceIsReleasedBeforeItHappened(t *testing.T) {
	for _, pair := range capability.Pilot() {
		for _, variant := range pair.Variants {
			for _, cell := range capability.Cells() {
				source := capability.NewSource(pair.Branch(variant), cell.Channels, capability.DefaultDelays)
				for _, admission := range source.Schedule() {
					bound := admission.SourceEnd
					if admission.Mark == capability.MarkSoundOnset {
						bound = admission.SourceStart
					}
					if admission.At < bound {
						t.Errorf("%s/%s/%s: %s of %q released at %s, before %s",
							pair.ID, variant.ID, cell.ID, admission.Mark, admission.EventID,
							admission.At, bound)
					}
				}
			}
		}
	}
}

// A check that has never rejected anything is not known to check anything.
func TestValidationRejectsClairvoyantEvidence(t *testing.T) {
	pair, ok := capability.PairByID("sc-01")
	if !ok {
		t.Fatal("sc-01 is missing from the pilot")
	}
	broken := pair
	broken.Prefix = slices.Clone(pair.Prefix)
	for index, event := range broken.Prefix {
		if event.Kind == capability.KindWord {
			broken.Prefix[index].AvailableAt = event.SourceStart
			break
		}
	}
	if err := broken.Validate(); err == nil {
		t.Fatal("a word available before it finished was accepted")
	}
}

func TestValidationRejectsABranchThatReachesIntoTheSharedPrefix(t *testing.T) {
	pair, _ := capability.PairByID("sc-01")
	broken := pair
	broken.Variants = slices.Clone(pair.Variants)
	broken.Variants[0].Events = slices.Clone(pair.Variants[0].Events)
	for index := range broken.Variants[0].Events {
		broken.Variants[0].Events[index].SourceStart = 0
		broken.Variants[0].Events[index].SourceEnd = 100 * time.Millisecond
		broken.Variants[0].Events[index].AvailableAt = time.Second
	}
	if err := broken.Validate(); err == nil {
		t.Fatal("a branch speaking during the shared prefix was accepted")
	}
}

func TestValidationRejectsAnExpectationAnAcknowledgementWouldPass(t *testing.T) {
	pair, _ := capability.PairByID("sc-01")
	broken := pair
	broken.Variants = slices.Clone(pair.Variants)
	broken.Variants[0].Expect.MinWords = 0
	if err := broken.Validate(); err == nil {
		t.Fatal("an expectation with no floor on how much is said was accepted")
	}
}

// The negative control has to remove the authored difference and nothing else.
func TestWithoutFeedbackRemovesOnlyTheAuthoredPhrase(t *testing.T) {
	for _, pair := range capability.Pilot() {
		for _, variant := range pair.Variants {
			control, err := pair.WithoutFeedback(variant)
			if err != nil {
				t.Fatalf("%s/%s: %v", pair.ID, variant.ID, err)
			}
			for _, event := range control.Events {
				if event.Group == variant.Feedback {
					t.Errorf("%s/%s: %q survived the control", pair.ID, variant.ID, event.ID)
				}
			}
			removed := len(variant.Events) - len(control.Events)
			if removed == 0 {
				t.Errorf("%s/%s: the control removed nothing", pair.ID, variant.ID)
			}
			for _, event := range variant.Events {
				if event.Group == variant.Feedback {
					continue
				}
				if !slices.Contains(control.Events, event) {
					t.Errorf("%s/%s: the control also dropped %q", pair.ID, variant.ID, event.ID)
				}
			}
		}
	}
}

// Every pair must have at least one branch that requires the agent to speak.
// A pair of two silent expectations would be passed by a system that never
// says anything.
func TestEveryPairPenalisesStayingSilent(t *testing.T) {
	for _, pair := range capability.Pilot() {
		if !slices.ContainsFunc(pair.Variants, func(variant capability.Variant) bool {
			return !variant.Expect.Silent
		}) {
			t.Errorf("%s: no branch requires the agent to say anything", pair.ID)
		}
	}
}

var timeInText = regexp.MustCompile(`[-+]?\d+\.\d+\s*s\b`)

// A cell that does not declare a channel must not see it in any form. The
// test reads the exact text handed to the model, because that is the only
// place the guarantee can be broken.
func TestUndeclaredChannelsNeverReachTheModel(t *testing.T) {
	pair, _ := capability.PairByID("pr-01")
	variant := pair.Variants[1] // the cue-bearing branch
	for _, cell := range capability.Cells() {
		listener := capability.NewListener(cell.Channels)
		source := capability.NewSource(pair.Branch(variant), cell.Channels, capability.DefaultDelays)
		listener.Admit(source.Advance(source.Ends() + time.Second))
		rendered := cell.Render(listener.Observe(source.Ends()+time.Second), capability.Self{
			Heard: "spoken already", Pending: "queued", Plan: "planned",
		})
		if !cell.Channels.Cues && strings.Contains(rendered, "rising pitch") {
			t.Errorf("cell %s does not declare cues but was shown one:\n%s", cell.ID, rendered)
		}
		if !cell.Channels.Timing && timeInText.MatchString(rendered) {
			t.Errorf("cell %s does not declare timing but was shown a time:\n%s", cell.ID, rendered)
		}
		if !cell.Channels.Speakers && strings.Contains(rendered, "another person") {
			t.Errorf("cell %s does not declare speakers but was told who spoke:\n%s", cell.ID, rendered)
		}
	}
}

// A1 and A2 are the narrow timing contrast: the same words, the same task, the
// same release schedule, and only the arrangement in time differs.
func TestTimingAblationChangesOnlyTheTiming(t *testing.T) {
	one, _ := capability.CellByID("A1")
	two, _ := capability.CellByID("A2")
	if one.Channels.Timing || !two.Channels.Timing {
		t.Fatal("A1/A2 no longer differ in the timing channel")
	}
	withoutTiming := two.Channels
	withoutTiming.Timing = false
	if withoutTiming != one.Channels {
		t.Errorf("A1 and A2 differ in more than timing: %+v vs %+v", one.Channels, two.Channels)
	}
	if one.Schedule != two.Schedule || one.Planning != two.Planning || one.Joint != two.Joint {
		t.Error("A1 and A2 differ in schedule, planning or joint decision as well as timing")
	}
	pair, _ := capability.PairByID("sc-01")
	for _, variant := range pair.Variants {
		first := capability.NewSource(pair.Branch(variant), one.Channels, capability.DefaultDelays).Schedule()
		second := capability.NewSource(pair.Branch(variant), two.Channels, capability.DefaultDelays).Schedule()
		if !slices.Equal(first, second) {
			t.Errorf("%s/%s: A1 and A2 release different evidence", pair.ID, variant.ID)
		}
	}
}

// A2 and A3 run the identical schedule; the acoustic rows are withheld in one
// and admitted in the other, and the trace says which.
func TestAcousticAblationWithholdsRatherThanReschedules(t *testing.T) {
	two, _ := capability.CellByID("A2")
	three, _ := capability.CellByID("A3")
	pair, _ := capability.PairByID("pr-01")
	variant := pair.Variants[1]
	plain := capability.NewSource(pair.Branch(variant), two.Channels, capability.DefaultDelays).Schedule()
	acoustic := capability.NewSource(pair.Branch(variant), three.Channels, capability.DefaultDelays).Schedule()
	if len(plain) != len(acoustic) {
		t.Fatalf("A2 has %d rows and A3 has %d: the schedule changed, not just the channels",
			len(plain), len(acoustic))
	}
	withheld := 0
	for index := range plain {
		if plain[index].At != acoustic[index].At || plain[index].EventID != acoustic[index].EventID {
			t.Fatalf("row %d differs in time or identity between A2 and A3", index)
		}
		if plain[index].Admitted() != acoustic[index].Admitted() {
			withheld++
		}
	}
	if withheld == 0 {
		t.Error("A2 and A3 admitted exactly the same rows, so the contrast is empty")
	}
}

func TestDelayInjectionMovesAvailabilityAndNotSourceTime(t *testing.T) {
	pair, _ := capability.PairByID("sc-01")
	cell, _ := capability.CellByID("A2")
	branch := pair.Branch(pair.Variants[0])
	base := capability.NewSource(branch, cell.Channels, capability.DefaultDelays).Schedule()
	delayed := capability.DefaultDelays
	delayed.Words += 400 * time.Millisecond
	slow := capability.NewSource(branch, cell.Channels, delayed).Schedule()
	moved := 0
	for _, was := range base {
		index := slices.IndexFunc(slow, func(is capability.Admission) bool {
			return is.EventID == was.EventID && is.Mark == was.Mark
		})
		if index < 0 {
			t.Fatalf("%s vanished when evidence was delayed", was.EventID)
		}
		is := slow[index]
		if is.SourceStart != was.SourceStart || is.SourceEnd != was.SourceEnd {
			t.Errorf("%s: delaying availability moved when it was said", was.EventID)
		}
		if was.Mark == capability.MarkWord && is.At != was.At+400*time.Millisecond {
			t.Errorf("%s: released at %s, want %s", was.EventID, is.At, was.At+400*time.Millisecond)
		}
		if is.At != was.At {
			moved++
		}
	}
	if moved == 0 {
		t.Error("injecting a delay changed nothing")
	}
}

// Replay must never hand back evidence the clock has already passed, and
// must never deliver twice.
func TestAdvanceReleasesEachThingOnceAndOnlyWhenDue(t *testing.T) {
	pair, _ := capability.PairByID("si-01")
	cell, _ := capability.CellByID("A2")
	source := capability.NewSource(pair.Branch(pair.Variants[0]), cell.Channels, capability.DefaultDelays)
	seen := map[string]int{}
	for now := time.Duration(0); now <= source.Ends()+time.Second; now += capability.Tick {
		for _, admission := range source.Advance(now) {
			if admission.At > now {
				t.Fatalf("%s released at %s, before it was due at %s", admission.EventID, now, admission.At)
			}
			seen[admission.EventID+string(admission.Mark)]++
		}
		// Going backwards must release nothing at all.
		if replayed := source.Advance(now - capability.Tick); len(replayed) != 0 {
			t.Fatalf("moving the clock backwards released %d rows", len(replayed))
		}
	}
	for key, count := range seen {
		if count != 1 {
			t.Errorf("%s was released %d times", key, count)
		}
	}
	if len(seen) != len(source.Schedule()) {
		t.Errorf("released %d of %d rows", len(seen), len(source.Schedule()))
	}
}
