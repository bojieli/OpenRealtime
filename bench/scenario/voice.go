package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// SpeechVoice synthesises through an OpenAI-shaped speech endpoint.
//
// Speakers are given different voices where the backend offers them. A
// scenario with a waiter or a phone menu in it is not testing anything if the
// second party is acoustically identical to the first: the agent's job includes
// telling them apart, and a harness that hands it two copies of one voice has
// removed the difficulty it meant to pose.
type SpeechVoice struct {
	Endpoint string
	Model    string
	// Voices maps a speaker name to a backend voice. A speaker with no entry
	// uses Default.
	Voices  map[string]string
	Default string
	Client  *http.Client
}

// CacheDir holds synthesised speech between runs.
//
// A benchmark holds its input constant. Re-synthesising every line on every run
// re-rolls the dice on two things at once: the timing, because a synthesiser
// does not produce the same durations twice and every check window is anchored
// to where a line ends, and the recognition, because a slightly different
// waveform is heard slightly differently - measured, "the build" came back as
// "the bill" in two runs of five and cost both of them.
//
// That variance is why four scenarios in this suite spanned nearly their whole
// range across passes on identical code, and why a single pass could not rank
// two models. The audio is the same audio either way; caching it only stops
// asking for a new roll.
var CacheDir = ".runtime/speech-cache"

func (voice SpeechVoice) Speak(ctx context.Context, speaker, text string) ([]int16, error) {
	name := voice.Default
	if chosen, ok := voice.Voices[speaker]; ok && chosen != "" {
		name = chosen
	}
	key := cacheKey(voice.Model, name, text)
	if samples, ok := readCached(key); ok {
		return samples, nil
	}
	body, err := json.Marshal(map[string]any{
		"model": voice.Model, "input": text, "voice": name,
		"response_format": "wav", "sample_rate": 24000,
	})
	if err != nil {
		return nil, err
	}
	timed, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(timed, http.MethodPost, voice.Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	client := voice.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("speech endpoint returned %s: %s", response.Status, truncate(payload))
	}
	samples, err := decodeWAV(payload, 24_000)
	if err != nil {
		return nil, err
	}
	writeCached(key, samples)
	return samples, nil
}

// decodeWAV pulls 16-bit mono samples out of a RIFF file and resamples them to
// the rate the session speaks.
//
// It reads the format chunk rather than assuming one. The first version did
// not, and asking the backend for 24 kHz was enough to convince me it had been
// given: it returns 44.1 kHz and ignores the field. Feeding those samples to a
// 24 kHz pipeline stretches everything by 1.84 and drops it an octave, which
// does not sound like a bug in a log - it sounds like a recogniser that is bad
// at its job. "A heron landed on the far bank" came back as "the horn mounted
// on the far bank", and every scenario measured through it was measuring the
// wrong thing.
//
// It also walks the chunk list rather than assuming data starts at byte 44,
// because synthesis backends emit LIST and fact chunks and a fixed offset reads
// those as audio - which sounds like a click and measures like speech.
func decodeWAV(payload []byte, want int) ([]int16, error) {
	if len(payload) < 12 || string(payload[0:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return nil, fmt.Errorf("speech endpoint did not return a WAV")
	}
	rate, channels := 0, 1
	offset := 12
	for offset+8 <= len(payload) {
		id := string(payload[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(payload[offset+4 : offset+8]))
		start := offset + 8
		if size < 0 || start+size > len(payload) {
			size = len(payload) - start
		}
		switch id {
		case "fmt ":
			if size >= 16 {
				channels = int(binary.LittleEndian.Uint16(payload[start+2:]))
				rate = int(binary.LittleEndian.Uint32(payload[start+4:]))
				if bits := binary.LittleEndian.Uint16(payload[start+14:]); bits != 16 {
					return nil, fmt.Errorf("speech endpoint returned %d-bit audio, want 16", bits)
				}
			}
		case "data":
			if rate == 0 {
				return nil, fmt.Errorf("WAV carried no format chunk before its data")
			}
			samples := make([]int16, size/2)
			for index := range samples {
				samples[index] = int16(binary.LittleEndian.Uint16(payload[start+index*2:]))
			}
			if channels > 1 {
				samples = downmix(samples, channels)
			}
			return resample(samples, rate, want), nil
		}
		offset = start + size
		if size%2 == 1 {
			offset++
		}
	}
	return nil, fmt.Errorf("WAV carried no data chunk")
}

// downmix averages interleaved channels into one.
func downmix(samples []int16, channels int) []int16 {
	mono := make([]int16, len(samples)/channels)
	for index := range mono {
		sum := 0
		for channel := 0; channel < channels; channel++ {
			sum += int(samples[index*channels+channel])
		}
		mono[index] = int16(sum / channels)
	}
	return mono
}

// resample converts between rates by linear interpolation.
//
// Crude, and adequate: what is being resampled is synthesised speech about to
// be transcribed, and the artefacts of linear interpolation sit far above the
// band a recogniser cares about. Anything better would be a signal-processing
// dependency in a test harness.
func resample(samples []int16, from, to int) []int16 {
	if from == to || from <= 0 || len(samples) == 0 {
		return samples
	}
	count := len(samples) * to / from
	out := make([]int16, count)
	for index := range out {
		position := float64(index) * float64(from) / float64(to)
		left := int(position)
		if left >= len(samples)-1 {
			out[index] = samples[len(samples)-1]
			continue
		}
		fraction := position - float64(left)
		out[index] = int16(float64(samples[left])*(1-fraction) + float64(samples[left+1])*fraction)
	}
	return out
}

func truncate(payload []byte) string {
	if len(payload) > 200 {
		return string(payload[:200])
	}
	return string(payload)
}

// ResampleForTest exposes the rate conversion. It is exported for a test
// rather than tested through the network, because what went wrong was
// arithmetic and the endpoint had nothing to do with it.
func ResampleForTest(samples []int16, from, to int) []int16 { return resample(samples, from, to) }

// cacheKey names one line of synthesised speech.
//
// The model and the voice are in it because changing either changes the audio,
// and a cache that returned the old sound for a new voice would quietly undo
// the reason speakers have different ones.
func cacheKey(model, voiceName, text string) string {
	sum := sha256.Sum256([]byte(model + "\x00" + voiceName + "\x00" + text))
	return hex.EncodeToString(sum[:16])
}

func readCached(key string) ([]int16, bool) {
	payload, err := os.ReadFile(filepath.Join(CacheDir, key+".pcm"))
	if err != nil || len(payload) < 2 {
		return nil, false
	}
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2:]))
	}
	return samples, true
}

// writeCached stores the audio, and says nothing when it cannot.
//
// A benchmark that fails because a cache directory is read-only is a benchmark
// measuring the wrong thing. The synthesiser answered; the run proceeds.
func writeCached(key string, samples []int16) {
	if err := os.MkdirAll(CacheDir, 0o755); err != nil {
		return
	}
	payload := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(payload[index*2:], uint16(sample))
	}
	// Written beside and renamed, so a run interrupted mid-write does not
	// leave a truncated line that every later run then treats as the audio.
	temporary := filepath.Join(CacheDir, key+".part")
	if err := os.WriteFile(temporary, payload, 0o644); err != nil {
		return
	}
	_ = os.Rename(temporary, filepath.Join(CacheDir, key+".pcm"))
}
