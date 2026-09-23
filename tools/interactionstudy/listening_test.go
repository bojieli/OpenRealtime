package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/capability"
	"github.com/bojieli/OpenRealtime/internal/audio"
)

func TestStereoPreservesTimingAndChannelOrder(t *testing.T) {
	wav, err := stereoWAV([]byte{1, 0, 2, 0, 3, 0}, []byte{0, 0, 9, 0}, 24000)
	if err != nil {
		t.Fatal(err)
	}
	if binary.LittleEndian.Uint16(wav[22:]) != 2 || binary.LittleEndian.Uint32(wav[40:]) != 12 {
		t.Fatal("invalid stereo header")
	}
	want := []byte{1, 0, 0, 0, 2, 0, 9, 0, 3, 0, 0, 0}
	for i, b := range want {
		if wav[44+i] != b {
			t.Fatalf("channel/timeline mismatch at %d", i)
		}
	}
}

func TestSilentBranchWithoutOutputStillGetsListeningFiles(t *testing.T) {
	pair, _ := capability.PairByID("si-01")
	prepared, dir := t.TempDir(), t.TempDir()
	for _, v := range pair.Variants {
		wav, err := audio.EncodeWAVMono16(make([]byte, 480), 24000)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(prepared, v.ID+".input.wav"), wav, 0644); err != nil {
			t.Fatal(err)
		}
	}
	// No output.wav: the assistant never spoke, which a silence case rewards.
	if err := attachInput(dir, prepared, pair, pair.Variants[0]); err != nil {
		t.Fatalf("silent branch failed listening assembly: %v", err)
	}
	for _, name := range []string{"input.wav", "conversation.stereo.wav", "listening.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}
