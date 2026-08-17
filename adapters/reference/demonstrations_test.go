package reference

import (
	"path/filepath"
	"testing"
)

func TestLoadDemonstrations(t *testing.T) {
	t.Parallel()
	demonstrations, err := LoadDemonstrations(filepath.Join("..", "..", "tests", "fixtures", "m5-demonstrations.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(demonstrations.Translation.Segments) != 5 || len(demonstrations.Game.Rounds) != 4 {
		t.Fatalf("unexpected demonstrations: %+v", demonstrations)
	}
}
