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

// And across speakers too. The caller asking for the call and the recording
// answering it ran together into one utterance - "and find out where my order
// has got to Thank you for calling Press one for billing" - and the agent
// pressed a key at the person who had asked for the call to be made.
func TestOneSpeakerDoesNotRunIntoTheNext(t *testing.T) {
	item := Scenario{Script: []Line{
		{Speaker: "user", AtMS: 0, Text: "one"},
		{Speaker: "other", AtMS: 5_000, Text: "two"},
	}}
	timeline, err := Compose(context.Background(), fixedVoice{ms: 9_000}, item)
	if err != nil {
		t.Fatal(err)
	}
	if gap := timeline.Spans[1].StartMS - timeline.Spans[0].EndMS; gap < breathMS {
		t.Fatalf("the second speaker starts %dms after the first stopped", gap)
	}
}
