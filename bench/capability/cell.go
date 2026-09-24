package capability

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Schedule is how often the policy gets to act, which is a separate treatment
// from what it gets to see. The two current micro-turn profiles change both at
// once; the study cells change one at a time.
type Schedule string

const (
	// ScheduleWholeTurn asks the policy once, after the user's turn has
	// ended: conventional endpoint-triggered answering.
	ScheduleWholeTurn Schedule = "whole-turn"
	// ScheduleMicroTurn asks on a fixed clock whether or not anything was
	// said, so silence is an input rather than the absence of one.
	ScheduleMicroTurn Schedule = "micro-turn"
)

// Planning is how much of the answer is decided before any of it is spoken,
// and whether what remains unspoken may still change. It is Study B's factor
// and is held fixed across Study A.
type Planning string

const (
	// PlanningFixed writes the whole answer once and speaks it unchanged.
	PlanningFixed Planning = "fixed-full"
	// PlanningRevisable writes the whole answer but may rewrite whatever has
	// not yet been spoken. A long tentative plan need not be a long
	// irrevocable utterance, and this cell is what shows the difference.
	PlanningRevisable Planning = "revisable-full"
	// PlanningIncremental formulates only the next bounded continuation and
	// reconsiders after every admitted observation.
	PlanningIncremental Planning = "incremental"
)

// Availability keeps a desired cell in the matrix when its resource is
// missing, so the study does not silently shrink to whatever happened to run.
type Availability string

const (
	AvailabilityRunnable    Availability = "runnable"
	AvailabilityUnavailable Availability = "unavailable"
)

// Cell is one experimental condition: a declared schedule, a declared set of
// input channels, and a declared planning mode. Nothing about a run may differ
// from what the cell declares, and the run records the declaration beside its
// results so a comparison can be checked rather than trusted.
type Cell struct {
	ID    string `json:"id"`
	Study string `json:"study"`
	// Purpose is what this cell is for, in the study's own terms. It is
	// stored with the results because a cell's meaning is the contrast it
	// takes part in, not its name.
	Purpose string `json:"purpose"`
	// Contrast names the cell this one is read against and the single factor
	// that differs. A cell that changes two things at once has to say so.
	Contrast string `json:"contrast,omitempty"`

	Schedule     Schedule      `json:"schedule"`
	Channels     Channels      `json:"channels"`
	Planning     Planning      `json:"planning"`
	TickInterval time.Duration `json:"tick_interval"`
	// Joint says the policy chooses an action and its next content in one
	// decision. The alternative is the existing arrangement: an enumerated
	// control question, then a separate answer stream that new evidence can
	// only stop, never change.
	Joint bool `json:"joint"`
	// Deliberation adds thinking-enabled decisions. "background" keeps the
	// per-tick fast policy and runs a thinking request from the snapshot at
	// new user words, admitting its result as a proposal at a later tick if
	// no newer user words arrived meanwhile. "synchronous" makes every tick's
	// decision a blocking thinking request. Empty means neither.
	Deliberation string `json:"deliberation,omitempty"`

	Availability Availability `json:"availability"`
	// Unavailable is why, and is required when the cell is not runnable.
	Unavailable string `json:"unavailable,omitempty"`
}

// Tick is the study's clock. 500 ms first, as the existing micro-turn profiles
// use; the 200/500/1000 ms sweep is a diagnostic to run after the principal
// contrast works, not a way to find a setting that makes one cell look good.
const Tick = 500 * time.Millisecond

// Cells is the frozen catalogue. IDs are stable: a result file names one of
// these and means exactly what is written here.
func Cells() []Cell {
	microturn := func(id, purpose, contrast string, channels Channels) Cell {
		return Cell{
			ID: id, Study: "A", Purpose: purpose, Contrast: contrast,
			Schedule: ScheduleMicroTurn, Channels: channels,
			Planning: PlanningIncremental, TickInterval: Tick, Joint: true,
			Availability: AvailabilityRunnable,
		}
	}
	return []Cell{{
		ID: "A0", Study: "A",
		Purpose:      "conventional behaviour: one answer per completed user turn",
		Contrast:     "A2 differs in both schedule and evidence; A1/A2 is the narrow timing contrast",
		Schedule:     ScheduleWholeTurn,
		Channels:     Channels{Timing: false, Sound: true},
		Planning:     PlanningFixed,
		TickInterval: Tick,
		Availability: AvailabilityRunnable,
	}, microturn("A1",
		"incremental transcript with its clock and empty intervals removed",
		"A2, which differs only in whether the same evidence carries its times",
		Channels{Timing: false, Sound: true},
	), microturn("A2",
		"sparse temporal representation: timed text, observed silence, own heard speech, a revisable plan",
		"A1 for timing; A3 for acoustic annotation",
		Channels{Timing: true, Sound: true},
	), func() Cell {
		c := microturn("A2D",
			"A2 plus background deliberation: thinking requests run across ticks and revise pending content",
			"A2, identical evidence and fast policy; A2T, the same thinking without parallelism",
			Channels{Timing: true, Sound: true})
		c.Deliberation = "background"
		return c
	}(), func() Cell {
		c := microturn("A2T",
			"A2 with every tick's decision a blocking thinking request",
			"A2D, which runs the same thinking in parallel with the per-tick policy",
			Channels{Timing: true, Sound: true})
		c.Deliberation = "synchronous"
		return c
	}(), microturn("A3",
		"A2 plus causally available acoustic evidence, testing what transcription loses",
		"A2, which runs the identical schedule with the acoustic rows withheld",
		Channels{Timing: true, Sound: true, Cues: true, Speakers: true},
	), {
		ID: "A4", Study: "A",
		Purpose:      "audio-native reference through its supported interface",
		Contrast:     "none: a system-level reference, not a matched architecture ablation",
		Schedule:     ScheduleMicroTurn,
		Channels:     Channels{Timing: true, Sound: true, Cues: true, Speakers: true},
		Planning:     PlanningIncremental,
		TickInterval: Tick,
		Availability: AvailabilityUnavailable,
		Unavailable: "no audio-native model on this host admits annotated evidence on a" +
			" controlled schedule; a native run is a separate system-level reference",
	}, {
		ID: "C1", Study: "C",
		Purpose:      "the existing arrangement: an enumerated control question, then a separate answer stream",
		Contrast:     "C2, which differs only in whether action and content are one decision",
		Schedule:     ScheduleMicroTurn,
		Channels:     Channels{Timing: true, Sound: true},
		Planning:     PlanningFixed,
		TickInterval: Tick,
		Joint:        false,
		Availability: AvailabilityRunnable,
	}, {
		ID: "C2", Study: "C",
		Purpose:      "joint micro-turn policy: one decision chooses the act and the next bounded continuation",
		Contrast:     "C1, which differs only in whether action and content are one decision",
		Schedule:     ScheduleMicroTurn,
		Channels:     Channels{Timing: true, Sound: true},
		Planning:     PlanningIncremental,
		TickInterval: Tick,
		Joint:        true,
		Availability: AvailabilityRunnable,
	}}
}

// CellByID returns one frozen cell.
func CellByID(id string) (Cell, error) {
	index := slices.IndexFunc(Cells(), func(cell Cell) bool { return cell.ID == id })
	if index < 0 {
		return Cell{}, fmt.Errorf("no cell %q in the frozen catalogue", id)
	}
	return Cells()[index], nil
}

// Self is what the agent knows about its own speech and plan.
//
// Heard, Pending, and Plan are kept apart because conflating them is the
// failure the study exists to catch: text handed to a synthesiser is not text
// the user heard, and a plan that was never sent is neither.
type Self struct {
	Speaking bool
	// Began is when the current run of speech started playing.
	Began time.Duration
	// Heard is what has audibly played, from playback marks.
	Heard string
	// Partial describes partially played or interrupted segments, whose exact
	// heard words are unknown.
	Partial string
	// Pending is text accepted by the synthesiser that has not yet played.
	Pending string
	// Plan is content the policy intends to say and has not yet sent.
	Plan string
}

// Render is the exact text handed to the policy at one tick.
//
// It is built only from channels the cell declared. A quantity the cell may
// not see is not shown in a vaguer form; it is absent, and the closing line
// says which channels were silent so the model is not left to guess whether a
// gap was silence or deafness.
func (cell Cell) Render(observation Observation, self Self) string {
	var lines []string
	if cell.Channels.Timing {
		lines = append(lines, "Conversation so far, with the time each phrase was said relative to now:")
		lines = append(lines, cell.timedTranscript(observation)...)
	} else {
		lines = append(lines, "Conversation so far, in order. No timing information is available:")
		lines = append(lines, cell.untimedTranscript(observation)...)
	}
	if cell.Channels.Cues && len(observation.Cues) > 0 {
		lines = append(lines, "", "Acoustic cues, as they became available:")
		for _, cue := range observation.Cues {
			if cell.Channels.Timing {
				lines = append(lines, fmt.Sprintf("  %+.1fs  %s: %s",
					(cue.SourceEnd-observation.Now).Seconds(), cell.speakerOf(cue), cue.Text))
			} else {
				lines = append(lines, fmt.Sprintf("  %s: %s", cell.speakerOf(cue), cue.Text))
			}
		}
	}
	lines = append(lines, "", cell.microphone(observation))
	lines = append(lines, "", cell.own(observation, self))
	if withheld := cell.withheld(); withheld != "" {
		lines = append(lines, "", withheld)
	}
	return strings.Join(lines, "\n")
}

// speakerOf is how a cell may refer to whoever said a word. Without the
// speaker channel every voice in the room is one undifferentiated stream,
// which is what a single microphone and one transcript give.
func (cell Cell) speakerOf(word Heard) string {
	if cell.Channels.Speakers {
		return word.Speaker
	}
	return "heard"
}

func (cell Cell) untimedTranscript(observation Observation) []string {
	words := slices.Clone(observation.Words)
	slices.SortStableFunc(words, func(left, right Heard) int { return int(left.SourceStart - right.SourceStart) })
	var lines []string
	var run []Heard
	flush := func() {
		if len(run) == 0 {
			return
		}
		parts := make([]string, 0, len(run))
		for _, word := range run {
			parts = append(parts, word.Text)
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", cell.speakerOf(run[0]), strings.Join(parts, " ")))
		run = nil
	}
	for _, word := range words {
		if len(run) > 0 && cell.speakerOf(word) != cell.speakerOf(run[len(run)-1]) {
			flush()
		}
		run = append(run, word)
	}
	flush()
	if len(lines) == 0 {
		lines = append(lines, "  (nothing recognised yet)")
	}
	return lines
}

func (cell Cell) timedTranscript(observation Observation) []string {
	words := slices.Clone(observation.Words)
	slices.SortStableFunc(words, func(left, right Heard) int { return int(left.SourceStart - right.SourceStart) })
	var lines []string
	var run []Heard
	flush := func() {
		if len(run) == 0 {
			return
		}
		parts := make([]string, 0, len(run))
		for _, word := range run {
			parts = append(parts, word.Text)
		}
		line := fmt.Sprintf("  %+.1fs to %+.1fs  %s: %s",
			(run[0].SourceStart - observation.Now).Seconds(),
			(run[len(run)-1].SourceEnd - observation.Now).Seconds(),
			cell.speakerOf(run[0]), strings.Join(parts, " "))
		if slices.ContainsFunc(run, func(word Heard) bool { return word.Provisional }) {
			line += "  [not yet final]"
		}
		lines = append(lines, line)
		run = nil
	}
	for _, word := range words {
		// A gap wide enough to be a pause is shown as a pause. Where the
		// words came from matters too: a change of speaker is a new line.
		if len(run) > 0 {
			previous := run[len(run)-1]
			gap := word.SourceStart - previous.SourceEnd
			if cell.speakerOf(word) != cell.speakerOf(previous) || word.Provisional != previous.Provisional || gap >= 400*time.Millisecond {
				flush()
				if gap >= 400*time.Millisecond {
					lines = append(lines, fmt.Sprintf("  (%.1f s with nothing said)", gap.Seconds()))
				}
			}
		}
		run = append(run, word)
	}
	flush()
	if len(lines) == 0 {
		lines = append(lines, "  (nothing recognised yet)")
	}
	return lines
}

func (cell Cell) microphone(observation Observation) string {
	if !cell.Channels.Sound {
		return "You have no microphone-activity signal: you cannot tell silence from a recogniser that has said nothing."
	}
	switch {
	case observation.Sounding > 0 && cell.Channels.Timing:
		return fmt.Sprintf("Microphone: the user is making sound right now, and has been for %.1f s.", observation.Sounding.Seconds())
	case observation.Sounding > 0:
		return "Microphone: the user is making sound right now."
	case !observation.SoundObserved:
		return "Microphone: no sound from the user yet."
	case cell.Channels.Timing:
		return fmt.Sprintf("Microphone: quiet for %.1f s.", observation.Quiet.Seconds())
	default:
		return "Microphone: quiet."
	}
}

func (cell Cell) own(observation Observation, self Self) string {
	var lines []string
	if self.Speaking && cell.Channels.Timing {
		lines = append(lines, fmt.Sprintf("You are SPEAKING. You started %.1f s ago.", (observation.Now-self.Began).Seconds()))
	} else if self.Speaking {
		lines = append(lines, "You are SPEAKING.")
	} else {
		lines = append(lines, "You are SILENT.")
	}
	lines = append(lines, fmt.Sprintf("The user has actually heard you say: %q", self.Heard))
	lines = append(lines, "Partially played or interrupted speech (not a complete transcript): "+self.Partial)
	lines = append(lines, fmt.Sprintf("Cancelable speech state (partial playback is described above): %q", self.Pending))
	lines = append(lines, fmt.Sprintf("Your plan for what to say next, not yet sent and still changeable: %q", self.Plan))
	return strings.Join(lines, "\n")
}

func (cell Cell) withheld() string {
	var missing []string
	if !cell.Channels.Timing {
		missing = append(missing, "when anything was said")
	}
	if !cell.Channels.Sound {
		missing = append(missing, "microphone activity")
	}
	if !cell.Channels.Cues {
		missing = append(missing, "how it was said (intonation, emphasis, hesitancy)")
	}
	if !cell.Channels.Speakers {
		missing = append(missing, "who said it, or whether it was addressed to you")
	}
	if !cell.Channels.Provisional {
		missing = append(missing, "words the recogniser has not finalised")
	}
	if len(missing) == 0 {
		return ""
	}
	return "You cannot observe: " + strings.Join(missing, "; ") + "."
}
