package fixture

import (
	"path/filepath"
	"testing"
)

func TestLoadCanonicalFixture(t *testing.T) {
	t.Parallel()
	input, err := Load(filepath.Join("..", "..", "tests", "fixtures", "m0-tone.wav"), 20, "fixture-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(input.Frames) != 50 || len(input.Records) != 51 || input.EndpointNS != 1_000_000_000 || input.SampleCount != 24_000 {
		t.Fatalf("unexpected fixture: %+v", input)
	}
}
