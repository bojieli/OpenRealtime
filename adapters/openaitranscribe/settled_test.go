package openaitranscribe

import "testing"

// The unit of agreement is the script's. Splitting on whitespace makes a
// Chinese utterance one token, so it agrees with the previous revision only
// when nothing changed and stable is empty on every partial - and a caller
// that falls back to the whole revision then acts on the unsettled tail this
// exists to hold back.
func TestStabilityFollowsTheScript(t *testing.T) {
	// English is unchanged: whole words, and a changed tail is unstable.
	stable, unstable := settled("ship it by the", "ship it by the thirteenth")
	if stable != "ship it by the" || unstable != " thirteenth" {
		t.Fatalf("English split as %q | %q", stable, unstable)
	}
	// A single English word must not be cut in half. "Hel" is not a settled
	// prefix of "Hello" in a script that marks its own word boundaries.
	if stable, _ := settled("Hel", "Hello"); stable != "" {
		t.Fatalf("half an English word was reported settled: %q", stable)
	}
	// Chinese agrees by character, and the character still being decided is
	// held back: a recogniser that has heard 很高 may yet write 很高兴 or 高雄.
	stable, unstable = settled("你好很高", "你好很高兴见到你")
	if stable == "" {
		t.Fatal("a Chinese partial reported nothing settled, which is the defect")
	}
	if len([]rune(stable)) >= len([]rune("你好很高兴见到你")) {
		t.Fatalf("the unsettled tail was reported as settled: %q", stable)
	}
	if stable+unstable != "你好很高兴见到你" {
		t.Fatalf("the split lost or invented text: %q + %q", stable, unstable)
	}
	// A revision that changes its own tail must not carry the change into
	// stable: 高雄 and 高兴 differ at the last character.
	stable, _ = settled("你好很高兴", "你好很高雄")
	if []rune(stable)[len([]rune(stable))-1] == '兴' {
		t.Fatalf("a character the recogniser took back stayed settled: %q", stable)
	}
	// Nothing in common is nothing settled.
	if stable, unstable := settled("你好", "再见"); stable != "" || unstable != "再见" {
		t.Fatalf("an unrelated revision split as %q | %q", stable, unstable)
	}
}
