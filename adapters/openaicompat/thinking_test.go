package openaicompat

import "testing"

// A Qwen-class model with thinking left on writes its deliberation into the
// content field, and the fast provider's content is what gets spoken. The
// observed failure was an agent reading its own chain of thought aloud:
//
//	spoken: <think>
//	Okay, the user is asking how many cars are in the picture. But wait...
func TestReasoningWrittenIntoContentIsNotSomethingToSay(t *testing.T) {
	var filter thinkingFilter
	reasoning, assistant := filter.push("<think>\nOkay, the user is asking...\n</think>There are five cars.")
	if assistant != "There are five cars." {
		t.Fatalf("only the answer is spoken, got %q", assistant)
	}
	if reasoning != "\nOkay, the user is asking...\n" {
		t.Fatalf("the deliberation is reasoning, got %q", reasoning)
	}
	if !filter.Leaked() {
		t.Fatal("a stream that carried inline reasoning must say so")
	}
}

// The stream is chunked at arbitrary boundaries, so a filter that only
// recognised whole tags would put the first half of one on the wire as
// something to say.
func TestATagSplitAcrossChunksIsStillATag(t *testing.T) {
	var filter thinkingFilter
	var reasoning, assistant string
	for _, chunk := range []string{"<", "th", "ink", ">", "delib", "erating", "</thi", "nk>", "The answer."} {
		gotReasoning, gotAssistant := filter.push(chunk)
		reasoning += gotReasoning
		assistant += gotAssistant
	}
	flushedReasoning, flushedAssistant := filter.flush()
	reasoning += flushedReasoning
	assistant += flushedAssistant
	if assistant != "The answer." {
		t.Fatalf("assistant content was %q", assistant)
	}
	if reasoning != "deliberating" {
		t.Fatalf("reasoning was %q", reasoning)
	}
}

// A short output budget runs out mid-deliberation, so the closing tag never
// arrives. Speaking what was held would say exactly the half of the thought
// that fit - which is the worst available outcome.
func TestAnUnterminatedBlockIsStillReasoning(t *testing.T) {
	var filter thinkingFilter
	reasoning, assistant := filter.push("<think>Okay, the user is asking how many")
	flushedReasoning, flushedAssistant := filter.flush()
	if assistant+flushedAssistant != "" {
		t.Fatalf("nothing here is an answer, got %q", assistant+flushedAssistant)
	}
	if reasoning+flushedReasoning != "Okay, the user is asking how many" {
		t.Fatalf("reasoning was %q", reasoning+flushedReasoning)
	}
}

// Recognition is narrow on purpose. A model that answers normally must not
// have its words held back or reclassified, and one that happens to discuss
// the tag is answering rather than thinking.
func TestOrdinaryContentIsUntouched(t *testing.T) {
	for _, content := range []string{
		"There are five cars.",
		"The tag <think> is how some models mark reasoning.",
		"<thought>not the marker</thought> still an answer",
		"<",
		"<th",
	} {
		var filter thinkingFilter
		reasoning, assistant := filter.push(content)
		flushedReasoning, flushedAssistant := filter.flush()
		if reasoning+flushedReasoning != "" {
			t.Fatalf("%q produced reasoning %q", content, reasoning+flushedReasoning)
		}
		if assistant+flushedAssistant != content {
			t.Fatalf("%q came out as %q", content, assistant+flushedAssistant)
		}
		if filter.Leaked() {
			t.Fatalf("%q is not a leak", content)
		}
	}
}

// A block that opens mid-answer is the model talking about the tag, not
// thinking: deliberation comes first or not at all.
func TestABlockThatDoesNotOpenTheContentIsNotReasoning(t *testing.T) {
	var filter thinkingFilter
	reasoning, assistant := filter.push("Here is an example: <think>like this</think>")
	flushedReasoning, flushedAssistant := filter.flush()
	if reasoning+flushedReasoning != "" {
		t.Fatalf("nothing here is reasoning, got %q", reasoning+flushedReasoning)
	}
	if assistant+flushedAssistant != "Here is an example: <think>like this</think>" {
		t.Fatalf("assistant content was %q", assistant+flushedAssistant)
	}
}
