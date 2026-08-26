package scenario

import (
	"context"
	"testing"
)

type fixedVoice struct{ ms int }

func (v fixedVoice) Speak(context.Context, string, string) ([]int16, error) {
	return make([]int16, v.ms*24), nil
}

// TestOneSpeakerDoesNotSayTwoThingsAtOnce is the regression for a story that
// became one utterance. The script placed three sentences eight seconds apart
// against durations from a faster synthesiser; a slower one ran them together,
// the gate never closed, and the agent heard the first sentence and nothing
// after it for the remaining twenty-seven seconds.
func TestOneSpeakerDoesNotSayTwoThingsAtOnce(t *testing.T) {
	item := Scenario{Script: []Line{
		{Speaker: "user", AtMS: 0, Text: "one"},
		{Speaker: "user", AtMS: 5_000, Text: "two"},
		{Speaker: "user", AtMS: 10_000, Text: "three"},
	}}
	timeline, err := Compose(context.Background(), fixedVoice{ms: 9_000}, item)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < len(timeline.Spans); index++ {
		gap := timeline.Spans[index].StartMS - timeline.Spans[index-1].EndMS
		if gap < breathMS {
			t.Fatalf("line %d starts %dms after line %d ended, which is not a breath",
				index, gap, index-1)
		}
	}
}

// Two people, on the other hand, is the whole point of half these scenarios.
func TestTwoSpeakersStillTalkOverEachOther(t *testing.T) {
	item := Scenario{Script: []Line{
		{Speaker: "user", AtMS: 0, Text: "one"},
		{Speaker: "other", AtMS: 1_000, Text: "two"},
	}}
	timeline, err := Compose(context.Background(), fixedVoice{ms: 9_000}, item)
	if err != nil {
		t.Fatal(err)
	}
	if timeline.Spans[1].StartMS != 1_000 {
		t.Fatalf("a second speaker was pushed to %dms instead of talking over the first",
			timeline.Spans[1].StartMS)
	}
}
