package sidecarbinding

import (
	"context"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/binding"
)

type textTokenSink struct {
	binding.Sink
	parts  []string
	begins int
}

func (s *textTokenSink) SpeechBegin(context.Context, action.Utterance) error {
	s.begins++
	return nil
}

func (s *textTokenSink) SpeechText(_ context.Context, _ action.Utterance, text string) error {
	s.parts = append(s.parts, text)
	return nil
}

func TestTextTokensPreserveSeparatorsWithoutRepeatingFinal(t *testing.T) {
	sink := &textTokenSink{}
	r := &runtime{ctx: context.Background(), sink: sink}
	parts := []string{"Hello", " ", "world", "\n", "again"}
	whole := strings.Join(parts, "")
	for _, part := range parts {
		if err := r.forwardText(part); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(sink.parts, ""); got != whole {
		t.Fatalf("streamed text %q, want %q", got, whole)
	}
	if err := r.forwardRemainingText(whole); err != nil {
		t.Fatal(err)
	}
	if len(sink.parts) != len(parts) || sink.begins != 1 {
		t.Fatalf("final repeated text or turn: parts=%q begins=%d", sink.parts, sink.begins)
	}
}
