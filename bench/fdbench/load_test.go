package fdbench

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesTheReleasedTimestampRate(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(directory, "conversation.timestamps"),
		[]byte(`[{"start":16000,"end":32000}]`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	conversation, err := loadConversation(directory, "condition", "conversation")
	if err != nil {
		t.Fatal(err)
	}
	if len(conversation.Turns) != 1 {
		t.Fatalf("got %d turns, want one", len(conversation.Turns))
	}
	turn := conversation.Turns[0]
	if turn.StartMS != 1000 || turn.EndMS != 2000 {
		t.Fatalf("released 16 kHz offsets decoded to %+v", turn)
	}
}
