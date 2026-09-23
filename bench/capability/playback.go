package capability

import (
	"fmt"
	"strings"
	"time"
)

// Segment is one piece of assistant speech from request to playback. Generated
// text is not heard text: only samples acknowledged by the playback sink count
// as played, and only a segment whose every generated sample played is heard
// in full. Exact heard words of a partially played segment are unknown.
type Segment struct {
	ID               string         `json:"id"`
	Text             string         `json:"text"`
	GeneratedAt      time.Duration  `json:"generated_at"`
	SampleRate       int            `json:"sample_rate"`
	GeneratedSamples int64          `json:"generated_samples"`
	PlayedSamples    int64          `json:"played_samples"`
	Ended            bool           `json:"ended"`
	Cut              bool           `json:"cut"`
	FirstPlayedAt    *time.Duration `json:"first_played_at,omitempty"`
	LastPlayedAt     *time.Duration `json:"last_played_at,omitempty"`
	CompletedAt      *time.Duration `json:"completed_at,omitempty"`
}

// Playback is the ledger of every segment in one trial. It is not safe for
// concurrent use; the caller serialises access.
type Playback struct {
	Segments []Segment `json:"segments"`
}

func (p *Playback) find(id string) (*Segment, error) {
	for i := range p.Segments {
		if p.Segments[i].ID == id {
			return &p.Segments[i], nil
		}
	}
	return nil, fmt.Errorf("unknown segment %q", id)
}

// Add records text handed to synthesis at generatedAt. IDs are unique.
func (p *Playback) Add(id, text string, generatedAt time.Duration, sampleRate int) error {
	if _, err := p.find(id); err == nil {
		return fmt.Errorf("duplicate segment %q", id)
	}
	if sampleRate <= 0 {
		return fmt.Errorf("segment %q: invalid sample rate %d", id, sampleRate)
	}
	p.Segments = append(p.Segments, Segment{ID: id, Text: text, GeneratedAt: generatedAt, SampleRate: sampleRate})
	return nil
}

// Generated adds synthesized samples that have not necessarily played.
func (p *Playback) Generated(id string, samples int64) error {
	s, err := p.find(id)
	if err != nil {
		return err
	}
	if s.Ended || s.Cut || samples < 0 {
		return fmt.Errorf("segment %q: generation after end or cut", id)
	}
	s.GeneratedSamples += samples
	return nil
}

// Mark records the cumulative samples acknowledged as played at time at.
func (p *Playback) Mark(id string, played int64, at time.Duration) error {
	s, err := p.find(id)
	if err != nil {
		return err
	}
	if s.Cut {
		return fmt.Errorf("segment %q: playback after cut", id)
	}
	if played < s.PlayedSamples || played > s.GeneratedSamples {
		return fmt.Errorf("segment %q: played %d outside [%d, %d]", id, played, s.PlayedSamples, s.GeneratedSamples)
	}
	if at < s.GeneratedAt || (s.LastPlayedAt != nil && at < *s.LastPlayedAt) {
		return fmt.Errorf("segment %q: playback mark out of order", id)
	}
	if played > 0 && s.FirstPlayedAt == nil {
		// The first chunk began playing one chunk duration before its mark.
		first := at - time.Duration(played)*time.Second/time.Duration(s.SampleRate)
		first = max(first, s.GeneratedAt)
		s.FirstPlayedAt = &first
	}
	s.PlayedSamples = played
	if played > 0 {
		last := at
		s.LastPlayedAt = &last
	}
	return nil
}

// End records that synthesis produced all of the segment's audio.
func (p *Playback) End(id string) error {
	s, err := p.find(id)
	if err != nil {
		return err
	}
	if s.Cut {
		return fmt.Errorf("segment %q: end after cut", id)
	}
	s.Ended = true
	return nil
}

// FinishPlayback completes a segment whose generated audio all played.
func (p *Playback) FinishPlayback(id string, at time.Duration) error {
	s, err := p.find(id)
	if err != nil {
		return err
	}
	if !s.Ended || s.Cut || s.PlayedSamples != s.GeneratedSamples || s.PlayedSamples == 0 {
		return fmt.Errorf("segment %q: cannot complete (ended=%t cut=%t played=%d generated=%d)",
			id, s.Ended, s.Cut, s.PlayedSamples, s.GeneratedSamples)
	}
	if s.LastPlayedAt == nil || at < *s.LastPlayedAt {
		return fmt.Errorf("segment %q: completion before last playback", id)
	}
	done := at
	s.CompletedAt = &done
	return nil
}

// Cancel cuts a segment that has not completed. Unknown or completed segments
// are left unchanged: what was heard in full stays heard.
func (p *Playback) Cancel(id string) {
	s, err := p.find(id)
	if err != nil || s.CompletedAt != nil {
		return
	}
	s.Cut = true
}

// Self is the model-visible state of the assistant's own speech (playback
// state v2): completed text as heard, partially played or cut segments with
// their exact heard words marked unknown, and the cancelable active segment.
func (p *Playback) Self() Self {
	var self Self
	var heard, partial, pending []string
	for _, s := range p.Segments {
		switch {
		case s.CompletedAt != nil:
			heard = append(heard, s.Text)
		case s.Cut && s.PlayedSamples > 0:
			partial = append(partial, fmt.Sprintf("[%s] CANCELED historical segment; no audio remains queued; interrupted after %d samples at %d Hz; exact heard words unknown; generated segment: %q\n",
				s.ID, s.PlayedSamples, s.SampleRate, s.Text))
		case s.Cut:
		case s.PlayedSamples > 0:
			self.Speaking, self.Began = true, *s.FirstPlayedAt
			partial = append(partial, fmt.Sprintf("[%s] partially played; exact heard words unknown; generated segment: %q\n", s.ID, s.Text))
			pending = append(pending, fmt.Sprintf("[%s] active segment with a partially played prefix; only its remaining audio can be canceled; generated full text (not a heard transcript): %q\n", s.ID, s.Text))
		default:
			pending = append(pending, fmt.Sprintf("[%s] no playback acknowledged yet; generated text: %q\n", s.ID, s.Text))
		}
	}
	self.Heard = strings.Join(heard, " ")
	self.Partial = strings.Join(partial, "")
	self.Pending = strings.Join(pending, "")
	return self
}
