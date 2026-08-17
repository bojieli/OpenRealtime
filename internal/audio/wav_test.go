package audio

import (
	"encoding/hex"
	"io"
	"path/filepath"
	"testing"
)

func TestGeneratedFixtureIsOpenAIPCM(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fixture.wav")
	digest, err := GenerateFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hex.EncodeToString(digest[:]), "e7adb582e0ea62d376f28b38d12bedb8ca78149eb35441775bf810c9ff2521af"; got != want {
		t.Fatalf("fixture SHA-256 = %s, want %s", got, want)
	}
	reader, err := OpenPCM16Mono(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	metadata := reader.Metadata()
	if metadata.SampleRateHz != 24_000 || metadata.SampleCount != 24_000 || metadata.DurationNS() != 1_000_000_000 {
		t.Fatalf("unexpected metadata: %+v", metadata)
	}
	buffer := make([]byte, 960)
	var bytesRead int
	for {
		read, err := reader.ReadFrame(buffer)
		bytesRead += read
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if bytesRead != 48_000 {
		t.Fatalf("read %d PCM bytes, want 48000", bytesRead)
	}
}
