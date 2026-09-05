package fdbench

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/internal/audio"
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

	// The loader reads the recording to find where each turn's speech stops,
	// so the fixture has to be one. The turn is annotated 1,000-2,000 ms and
	// the speech inside it stops at 1,800.
	writeToneWAV(t, filepath.Join(directory, "conversation.wav"), 3_000, 1_000, 1_800)

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
	if turn.AudibleEndMS < 1_780 || turn.AudibleEndMS > 1_840 {
		t.Fatalf("the turn's speech stops at %.0f ms, want the 1,800 it was written at", turn.AudibleEndMS)
	}
}

// Two ways a turn gives the measurement nothing to work with, and in both the
// annotation has to be what survives.
//
// Sound throughout is the 0 dB background condition: the noise never stops, so
// the last audible block is the last block. Nothing above the floor at all is
// a turn this measurement cannot read, and reporting its start as the place
// the speech ended would move every judgement about that turn to the front of
// it.
func TestATurnTheMeasurementCannotReadKeepsItsAnnotatedEnd(t *testing.T) {
	for _, sample := range []struct {
		name               string
		toneStart, toneEnd float64
	}{
		{"sound throughout", 0, 3_000},
		{"nothing above the floor", 0, 0},
	} {
		t.Run(sample.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(directory, "conversation.timestamps"),
				[]byte(`[{"start":16000,"end":32000}]`), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			writeToneWAV(t, filepath.Join(directory, "conversation.wav"),
				3_000, sample.toneStart, sample.toneEnd)

			conversation, err := loadConversation(directory, "condition", "conversation")
			if err != nil {
				t.Fatal(err)
			}
			if got := conversation.Turns[0].AudibleEndMS; got < 1_980 || got > 2_020 {
				t.Fatalf("the turn's speech is reported as ending at %.0f ms, want its annotated 2,000", got)
			}
		})
	}
}

func writeToneWAV(tb testing.TB, path string, durationMS, toneStartMS, toneEndMS float64) {
	tb.Helper()
	const rate = 24_000
	total := int(durationMS * rate / 1000)
	pcm := make([]byte, total*2)
	for index := range total {
		at := float64(index) * 1000 / rate
		var value int16
		if at >= toneStartMS && at < toneEndMS {
			value = int16(8000 * math.Sin(2*math.Pi*220*float64(index)/rate))
		}
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(value))
	}
	encoded, err := audio.EncodeWAVMono16(pcm, rate)
	if err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		tb.Fatal(err)
	}
}
