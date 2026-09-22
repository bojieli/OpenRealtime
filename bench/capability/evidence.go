package capability

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Channels is what a cell is allowed to perceive. It is declared per cell and
// enforced here, so a text-only condition cannot quietly decide on acoustic
// evidence: an undeclared channel is not filtered out at the prompt, it is
// never admitted, and the trace records that it was withheld.
type Channels struct {
	// Timing shows the model when each word was said and how long the gaps
	// were. Removing it is the narrow ablation of temporal evidence: the
	// words and the task are unchanged, only their arrangement in time.
	Timing bool `json:"timing"`
	// Sound admits acoustic activity - that somebody is making sound now,
	// before any word has been decoded.
	Sound bool `json:"sound"`
	// Cues admits acoustic annotations: intonation, hesitancy, emphasis.
	Cues bool `json:"cues"`
	// Speakers admits who said what. It is acoustic evidence, not textual:
	// one microphone in a room produces one stream of words, and telling the
	// person being helped from somebody else talking nearby is done on
	// voice, direction and distance. A cell without it hears the room as one
	// speaker, which is what a single-channel cascade actually has.
	Speakers bool `json:"speakers"`
	// Provisional admits recogniser hypotheses before they are committed. A
	// cell without it waits for commitment and pays the wait.
	Provisional bool `json:"provisional"`
}

// Mark says what an admission revealed. Sound is split into onset and offset
// because admitting one event at its start would hand the policy the moment
// the sound was going to stop, which it cannot know while it is still going.
type Mark string

const (
	MarkWord        Mark = "word"
	MarkRevision    Mark = "revision"
	MarkCommit      Mark = "commit"
	MarkCue         Mark = "cue"
	MarkSoundOnset  Mark = "sound-onset"
	MarkSoundOffset Mark = "sound-offset"
)

// Admission is one moment at which evidence became visible to a policy, or
// the record that it was withheld from this cell.
//
// It is the row written to admitted-evidence.jsonl. Keeping the withheld rows
// is what lets two cells be compared: A2 and A3 run the same schedule, and the
// trace shows the cue rows admitted in one and withheld in the other rather
// than two schedules that might have differed for some other reason.
type Admission struct {
	At          time.Duration `json:"at"`
	Mark        Mark          `json:"mark"`
	EventID     string        `json:"event_id"`
	Kind        Kind          `json:"kind"`
	Speaker     string        `json:"speaker"`
	Text        string        `json:"text,omitempty"`
	Cue         string        `json:"cue,omitempty"`
	SourceStart time.Duration `json:"source_start"`
	SourceEnd   time.Duration `json:"source_end"`
	Provisional bool          `json:"provisional,omitempty"`
	Replaces    string        `json:"replaces,omitempty"`
	// Withheld names the channel this cell did not declare, and is empty
	// when the evidence was admitted.
	Withheld string `json:"withheld,omitempty"`
	// InjectedDelay is how much later than the annotation this was released,
	// for the evidence-delay sweep.
	InjectedDelay time.Duration `json:"injected_delay,omitempty"`
}

// Admitted reports whether the policy actually saw this.
func (admission Admission) Admitted() bool { return admission.Withheld == "" }

// Delays is the injected availability delay, by kind. It is a struct rather
// than one number because the kinds are not delayed by the same thing: words
// wait for a recogniser, cues for an annotator, and acoustic activity for a
// detector that needs only a frame or two.
type Delays struct {
	// Words delays decoded text, the recogniser's trail.
	Words time.Duration `json:"words"`
	// Cues delays acoustic annotations.
	Cues time.Duration `json:"cues"`
	// SoundOnset is how long a detector takes to call a sound started.
	SoundOnset time.Duration `json:"sound_onset"`
	// SoundOffset is the hangover before a detector calls it stopped. It is
	// declared rather than assumed: a policy that treats the hangover as
	// silence is waiting longer than the annotation says.
	SoundOffset time.Duration `json:"sound_offset"`
}

// DefaultDelays matches the detector the existing micro-turn sidecar runs, so
// an annotated diagnostic is not accidentally more responsive than anything
// that could be built.
var DefaultDelays = Delays{SoundOnset: 20 * time.Millisecond, SoundOffset: 200 * time.Millisecond}

// Source releases annotated evidence on the study clock.
//
// The whole schedule is computed up front from the events, the channels, and
// the injected delays, and Advance only walks it. That is deliberate: what a
// policy could know at any moment is then a pure function of the fixture and
// the cell, decided before the run started and unable to depend on anything
// the model did.
type Source struct {
	schedule []Admission
	cursor   int
}

// NewSource builds the release schedule for one branch under one cell.
func NewSource(events []Event, channels Channels, delays Delays) *Source {
	schedule := make([]Admission, 0, len(events)+2)
	for _, event := range events {
		switch event.Kind {
		case KindSound:
			onset := Admission{
				At: event.AvailableAt + delays.SoundOnset, Mark: MarkSoundOnset,
				InjectedDelay: delays.SoundOnset,
			}
			offset := Admission{
				At: event.SourceEnd + delays.SoundOffset, Mark: MarkSoundOffset,
				InjectedDelay: delays.SoundOffset,
			}
			for _, admission := range []Admission{onset, offset} {
				admission = fill(admission, event)
				if !channels.Sound {
					admission.Withheld = "sound"
				}
				schedule = append(schedule, admission)
			}
		case KindCue:
			admission := fill(Admission{
				At: event.AvailableAt + delays.Cues, Mark: MarkCue, InjectedDelay: delays.Cues,
			}, event)
			if !channels.Cues {
				admission.Withheld = "cues"
			}
			schedule = append(schedule, admission)
		case KindCommit:
			schedule = append(schedule, fill(Admission{
				At: event.AvailableAt + delays.Words, Mark: MarkCommit, InjectedDelay: delays.Words,
			}, event))
		default:
			mark := MarkWord
			if event.Replaces != "" {
				mark = MarkRevision
			}
			admission := fill(Admission{
				At: event.AvailableAt + delays.Words, Mark: mark, InjectedDelay: delays.Words,
			}, event)
			// A cell that does not admit provisional text waits for the
			// commit that finalises the word instead of seeing it early.
			if event.Provisional && !channels.Provisional {
				admission.Withheld = "provisional"
			}
			schedule = append(schedule, admission)
		}
	}
	slices.SortStableFunc(schedule, func(left, right Admission) int { return int(left.At - right.At) })
	return &Source{schedule: schedule}
}

func fill(admission Admission, event Event) Admission {
	admission.EventID = event.ID
	admission.Kind = event.Kind
	admission.Speaker = event.speaker()
	admission.Text = event.Text
	admission.Cue = event.Cue
	admission.SourceStart = event.SourceStart
	admission.SourceEnd = event.SourceEnd
	admission.Provisional = event.Provisional
	admission.Replaces = event.Replaces
	return admission
}

// Schedule is the whole release plan, including withheld rows. It is what the
// run writes to admitted-evidence.jsonl once the run is over.
func (source *Source) Schedule() []Admission { return slices.Clone(source.schedule) }

// Ends is when the last evidence in this branch is released.
func (source *Source) Ends() time.Duration {
	if len(source.schedule) == 0 {
		return 0
	}
	return source.schedule[len(source.schedule)-1].At
}

// Advance releases everything due at or before now and returns it. Calling it
// with a time earlier than the previous call releases nothing: the clock only
// moves forward, and replay never changes what the model knew at a past tick.
func (source *Source) Advance(now time.Duration) []Admission {
	start := source.cursor
	for source.cursor < len(source.schedule) && source.schedule[source.cursor].At <= now {
		source.cursor++
	}
	return source.schedule[start:source.cursor:source.cursor]
}

// Observation is the evidence state at one tick: everything admitted so far,
// arranged the way the cell's declared channels allow it to be seen.
//
// It distinguishes the four situations the study requires to stay apart:
// observed silence, sound without decoded words, no new transcript since the
// last tick, and a channel this cell never had.
type Observation struct {
	Now time.Duration
	// Words is the transcript as the policy may see it, with revisions
	// applied and superseded hypotheses dropped.
	Words []Heard
	// Sounding is how long the current run of sound has been going, or zero
	// when the user is not audibly speaking. Only meaningful with the sound
	// channel.
	Sounding time.Duration
	// Quiet is how long it has been since sound stopped. It is negative when
	// nothing has been heard yet, which is not the same as silence observed.
	Quiet time.Duration
	// Cues are the acoustic annotations admitted so far.
	Cues []Heard
	// FreshSince lists what arrived since the previous tick, so "no new
	// transcript" is a statement the encoder can make rather than an absence
	// the model has to infer.
	FreshSince []Admission
	// Channels is what this cell declared, carried along so a rendered input
	// can say which silences were observed and which were never listened for.
	Channels Channels
	// SoundObserved is whether any acoustic activity has ever been admitted.
	SoundObserved bool
}

// Heard is one piece of evidence as the policy sees it.
type Heard struct {
	EventID     string
	Speaker     string
	Text        string
	SourceStart time.Duration
	SourceEnd   time.Duration
	AdmittedAt  time.Duration
	Provisional bool
}

// Listener accumulates admissions into the observation a policy is shown.
type Listener struct {
	channels  Channels
	words     []Heard
	cues      []Heard
	replaced  map[string]bool
	sounding  bool
	soundFrom time.Duration
	soundTo   time.Duration
	heard     bool
	fresh     []Admission
}

// NewListener starts an empty observation for one cell.
func NewListener(channels Channels) *Listener {
	return &Listener{channels: channels, replaced: map[string]bool{}, soundTo: -1}
}

// Admit folds newly released evidence into the observation. Withheld rows are
// skipped here and kept only in the trace.
func (listener *Listener) Admit(admissions []Admission) {
	for _, admission := range admissions {
		if !admission.Admitted() {
			continue
		}
		listener.fresh = append(listener.fresh, admission)
		switch admission.Mark {
		case MarkWord, MarkRevision:
			if admission.Replaces != "" {
				listener.replaced[admission.Replaces] = true
			}
			listener.words = append(listener.words, Heard{
				EventID: admission.EventID, Speaker: admission.Speaker, Text: admission.Text,
				SourceStart: admission.SourceStart, SourceEnd: admission.SourceEnd,
				AdmittedAt: admission.At, Provisional: admission.Provisional,
			})
		case MarkCommit:
			for index := range listener.words {
				if listener.words[index].EventID == admission.Replaces {
					listener.words[index].Provisional = false
				}
			}
		case MarkCue:
			listener.cues = append(listener.cues, Heard{
				EventID: admission.EventID, Speaker: admission.Speaker, Text: admission.Cue,
				SourceStart: admission.SourceStart, SourceEnd: admission.SourceEnd,
				AdmittedAt: admission.At,
			})
		case MarkSoundOnset:
			listener.sounding, listener.soundFrom, listener.heard = true, admission.At, true
		case MarkSoundOffset:
			listener.sounding, listener.soundTo = false, admission.At
		}
	}
}

// Observe is the state at now, and clears the fresh list for the next tick.
func (listener *Listener) Observe(now time.Duration) Observation {
	observation := Observation{
		Now: now, Channels: listener.channels, FreshSince: listener.fresh,
		Quiet: -1, SoundObserved: listener.heard,
	}
	listener.fresh = nil
	for _, word := range listener.words {
		if !listener.replaced[word.EventID] {
			observation.Words = append(observation.Words, word)
		}
	}
	observation.Cues = listener.cues
	switch {
	case listener.sounding:
		observation.Sounding = now - listener.soundFrom
	case listener.soundTo >= 0:
		observation.Quiet = now - listener.soundTo
	}
	return observation
}

// Said is the visible transcript of one speaker, in the order it was said.
func (observation Observation) Said(speaker string) string {
	words := make([]Heard, 0, len(observation.Words))
	for _, word := range observation.Words {
		if word.Speaker == speaker {
			words = append(words, word)
		}
	}
	slices.SortStableFunc(words, func(left, right Heard) int { return int(left.SourceStart - right.SourceStart) })
	parts := make([]string, 0, len(words))
	for _, word := range words {
		parts = append(parts, word.Text)
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Speakers is every speaker whose words have been admitted, user first.
func (observation Observation) Speakers() []string {
	speakers := []string{}
	for _, word := range observation.Words {
		if !slices.Contains(speakers, word.Speaker) {
			speakers = append(speakers, word.Speaker)
		}
	}
	slices.SortStableFunc(speakers, func(left, right string) int {
		switch {
		case left == right:
			return 0
		case left == SpeakerUser:
			return -1
		case right == SpeakerUser:
			return 1
		}
		return strings.Compare(left, right)
	})
	return speakers
}

// String renders an observation for a log line. The model-visible rendering
// lives with the cell, which decides how much of this it is allowed to show.
func (observation Observation) String() string {
	return fmt.Sprintf("at %s: %d words, sounding %s, quiet %s, %d cues",
		observation.Now, len(observation.Words), observation.Sounding, observation.Quiet, len(observation.Cues))
}
