package media

import (
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/bojieli/OpenRealtime/bench"
)

func BenchmarkEncodeStereoWAV(b *testing.B) {
	capture := bench.SessionAudioCapture{
		SampleRateHz: 24_000,
		RoomPCM16:    make([]int16, 24_000*10),
		Agent: []bench.TimedAudioChunk{
			{AtMS: 250, PCM16: make([]int16, 24_000*4)},
			{AtMS: 5_000, PCM16: make([]int16, 24_000*3)},
		},
	}
	b.ReportAllocs()
	b.SetBytes(int64((len(capture.RoomPCM16) + len(capture.Agent[0].PCM16) + len(capture.Agent[1].PCM16)) * 2))
	for range b.N {
		if _, _, err := EncodeStereoWAV(capture); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRecorderCaptureVideo(b *testing.B) {
	parent := b.TempDir()
	frame := pngFixture(b, 1280, 720, color.RGBA{R: 0x33, G: 0x66, B: 0x99, A: 0xff})
	b.ReportAllocs()
	b.SetBytes(int64(len(frame)))
	for index := range b.N {
		b.StopTimer()
		directory := filepath.Join(parent, formatFrameSequence(index+1))
		recorder := mustRecorder(b, directory, []string{"screen"},
			newFixtureEncoder(""), newFixtureAttestor(""), nil)
		capture := bench.SessionVideoCapture{Source: "screen", Width: 1280, Height: 720,
			MediaType: "image/png", WireTimestamp: int64(index), EpisodeAtMS: 0, Data: frame}
		b.StartTimer()
		if err := recorder.CaptureVideo(capture); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = recorder.Abort()
		_ = os.RemoveAll(directory)
	}
}

func BenchmarkRecorderFinalizeAndVerify(b *testing.B) {
	parent := b.TempDir()
	for index := range b.N {
		b.StopTimer()
		directory := filepath.Join(parent, formatFrameSequence(index+1))
		recorder := mustRecorder(b, directory, []string{"screen"},
			newFixtureEncoder(""), newFixtureAttestor(""), nil)
		captureStandard(b, recorder)
		b.StartTimer()
		receipt, err := recorder.Finalize(b.Context(), 500)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := VerifyBundle(directory, receipt.ManifestSHA256); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		_ = makeTreeOwnerWritable(directory)
		_ = os.RemoveAll(directory)
	}
}
