package main

import (
	"encoding/binary"
	"testing"
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
