package fdbench

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeSampleDataset(t *testing.T, conditions map[string]int) string {
	t.Helper()
	root := t.TempDir()
	for condition, count := range conditions {
		directory := filepath.Join(root, condition)
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		for index := range count {
			name := filepath.Join(directory, "conversation_"+string(rune('a'+index/26))+string(rune('a'+index%26)))
			writeToneWAV(t, name+".wav", 1500, 0, 1000)
			_ = os.WriteFile(name+".timestamps", []byte(`[{"start": 0, "end": 16000}]`), 0o644)
		}
	}
	return root
}

func TestLoadSampleIsSeededPerConditionAndNotTheFirstNames(t *testing.T) {
	root := writeSampleDataset(t, map[string]int{"clean": 60, "noisy": 60, "tiny": 3})
	ids := func(seed int64) []string {
		conversations, err := LoadSample(root, []string{"clean", "noisy", "tiny"}, 10, seed)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, conversation := range conversations {
			out = append(out, conversation.ID)
		}
		return out
	}
	first, again, other := ids(7), ids(7), ids(8)
	if len(first) != 23 || !slices.Equal(first, again) || slices.Equal(first, other) {
		t.Fatalf("sample sizes/determinism wrong: %d %v", len(first), first)
	}
	head, err := Load(root, []string{"clean"}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var headIDs []string
	for _, conversation := range head {
		headIDs = append(headIDs, conversation.ID)
	}
	if slices.Equal(first[:10], headIDs) {
		t.Fatal("the sample is just the first ten names")
	}
}
