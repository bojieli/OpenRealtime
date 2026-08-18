package realtimegateway

import (
	"context"
	"encoding/binary"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/trajectory"
)

func TestMuLawRoundTripPreservesSpeechScale(t *testing.T) {
	t.Parallel()
	input := make([]byte, 0, 8_000*2)
	for index := 0; index < 8_000; index++ {
		value := int16(12_000 * math.Sin(2*math.Pi*440*float64(index)/8_000))
		var encoded [2]byte
		binary.LittleEndian.PutUint16(encoded[:], uint16(value))
		input = append(input, encoded[:]...)
	}
	compressed, err := encodeMuLaw(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded := decodeMuLaw(compressed)
	var squared float64
	for offset := range compressed {
		want := float64(int16(binary.LittleEndian.Uint16(input[offset*2:])))
		got := float64(int16(binary.LittleEndian.Uint16(decoded[offset*2:])))
		delta := want - got
		squared += delta * delta
	}
	if rmse := math.Sqrt(squared / float64(len(compressed))); rmse > 400 {
		t.Fatalf("mu-law round-trip RMSE is %.1f", rmse)
	}
}

func TestEnergyVADUsesAcousticsAndConfiguredHysteresis(t *testing.T) {
	t.Parallel()
	detector, err := newEnergyVAD(vadConfig{
		Threshold: 0.5, PrefixPaddingMS: 40, SilenceDurationMS: 60,
	}, 8_000)
	if err != nil {
		t.Fatal(err)
	}
	silence := makePCM(160, 0)
	voice := makePCM(160, 5_000)
	if event, err := detector.Push(silence); err != nil || event.Started {
		t.Fatalf("silence started VAD: event=%#v err=%v", event, err)
	}
	event, err := detector.Push(voice)
	if err != nil || !event.Started || len(event.Audio) != len(silence)+len(voice) {
		t.Fatalf("speech start did not include prefix: event=%#v err=%v", event, err)
	}
	for index := 0; index < 2; index++ {
		event, err = detector.Push(silence)
		if err != nil || event.Stopped {
			t.Fatalf("VAD stopped before hysteresis at frame %d: %#v %v", index, event, err)
		}
	}
	event, err = detector.Push(silence)
	if err != nil || !event.Stopped {
		t.Fatalf("VAD did not stop at configured silence: %#v %v", event, err)
	}
}

func TestEncodedAudioDurationMatchesWireFormat(t *testing.T) {
	t.Parallel()
	pcmu, err := encodedAudioDuration(audioFormat{Type: formatPCMU}, 1_600)
	if err != nil || pcmu != 200*time.Millisecond {
		t.Fatalf("PCMU duration = %v, %v", pcmu, err)
	}
	pcm, err := encodedAudioDuration(audioFormat{Type: formatPCM16, Rate: 24_000}, 9_600)
	if err != nil || pcm != 200*time.Millisecond {
		t.Fatalf("PCM duration = %v, %v", pcm, err)
	}
	pcmuFrame, err := encodedAudioFrameBytes(audioFormat{Type: formatPCMU}, 100*time.Millisecond)
	if err != nil || pcmuFrame != 800 {
		t.Fatalf("PCMU frame bytes = %d, %v", pcmuFrame, err)
	}
	pcmFrame, err := encodedAudioFrameBytes(audioFormat{Type: formatPCM16, Rate: 24_000}, 100*time.Millisecond)
	if err != nil || pcmFrame != 4_800 {
		t.Fatalf("PCM frame bytes = %d, %v", pcmFrame, err)
	}
}

func TestSlowSafePointSupersedesOnlyFastSpeech(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	session := &session{ctx: ctx}
	scheduler := newSpeechScheduler(session, fakeSpeech{})
	if err := scheduler.Enqueue(speechJob{
		Phase: trajectory.PhaseFast, Text: "One moment.", AssistantIDs: []string{"fast-item"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Enqueue(speechJob{
		Phase: trajectory.PhaseSlow, Text: "The answer is four.", AssistantIDs: []string{"slow-item"},
	}); err != nil {
		t.Fatal(err)
	}
	if got := scheduler.SupersedeFast(); !slices.Equal(got, []string{"fast-item"}) {
		t.Fatalf("superseded assistant IDs = %v", got)
	}
	fast := <-scheduler.queue
	slow := <-scheduler.queue
	if fast.FastEpoch == scheduler.fastEpoch.Load() {
		t.Fatal("queued fast speech remained eligible after the slow safe point")
	}
	if slow.Epoch != scheduler.epoch.Load() {
		t.Fatal("slow speech was invalidated with provisional fast speech")
	}
	scheduler.mu.Lock()
	defer scheduler.mu.Unlock()
	if len(scheduler.outstanding) != 1 || scheduler.outstanding[slow.ID].Phase != trajectory.PhaseSlow {
		t.Fatalf("outstanding speech = %#v", scheduler.outstanding)
	}
}

func makePCM(samples int, value int16) []byte {
	result := make([]byte, samples*2)
	for offset := 0; offset < len(result); offset += 2 {
		binary.LittleEndian.PutUint16(result[offset:], uint16(value))
	}
	return result
}
