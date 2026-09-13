package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/internal/audio"
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
	// Listen is the separate endpoint used to score what the agent was
	// audibly saying. Unset leaves that claim unmade rather than assumed.
	Listen Hearing
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
		return levelled(samples), nil
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
	return levelled(samples), nil
}

// levelled brings a synthesised line to the loudness people speak at in the
// same room. Synthesis voices differ by a lot - measured, the "other"
// speaker's Mandarin came out four times quieter than the user's English
// (peak 0.28 against 0.93 of full scale) - and the room's energy gate, set
// for a person at a normal level, opened on a fraction of the quiet line and
// gave the recogniser a 600 ms fragment of "你好，很高兴见到你". The scenario
// is about translating what was said, not about a voice that mumbles.
//
// The measure is the RMS of the voiced part (10 ms frames above one percent
// of full scale), scaled to a fixed target with the peak kept clear of
// clipping; silence and pauses are untouched.
func levelled(samples []int16) []int16 {
	const (
		frame     = 240 // 10 ms at 24 kHz
		voiced    = 0.01 * 32768
		targetRMS = 0.25 * 32768
		peakLimit = 0.95 * 32768
	)
	sum, count := 0.0, 0
	peak := 0.0
	for start := 0; start+frame <= len(samples); start += frame {
		energy := 0.0
		for _, sample := range samples[start : start+frame] {
			value := float64(sample)
			energy += value * value
			peak = max(peak, math.Abs(value))
		}
		if rms := math.Sqrt(energy / frame); rms > voiced {
			sum += energy
			count += frame
		}
	}
	if count == 0 || peak == 0 {
		return samples
	}
	gain := targetRMS / math.Sqrt(sum/float64(count))
	gain = min(gain, peakLimit/peak)
	if math.Abs(gain-1) < 0.05 {
		return samples
	}
	out := make([]int16, len(samples))
	for index, sample := range samples {
		out[index] = int16(max(-32768, min(32767, math.Round(float64(sample)*gain))))
	}
	return out
}

// hearWithGemini sends the window's WAV to Gemini and returns what it heard.
func (listen Hearing) hearWithGemini(ctx context.Context, container []byte) (string, error) {
	model := listen.GeminiModel
	if model == "" {
		model = "gemini-3.7-flash"
	}
	endpoint := listen.GeminiEndpoint
	if endpoint == "" {
		endpoint = "https://generativelanguage.googleapis.com/v1beta"
	}
	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{{"role": "user", "parts": []map[string]any{
			{"inlineData": map[string]any{"mimeType": "audio/wav", "data": base64.StdEncoding.EncodeToString(container)}},
			{"text": geminiHearingPrompt},
		}}},
		"generationConfig": map[string]any{"temperature": 0, "thinkingConfig": map[string]any{"thinkingBudget": 0}},
	})
	if err != nil {
		return "", err
	}
	client := listen.Client
	if client == nil {
		client = &http.Client{Timeout: 120 * time.Second}
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint+"/models/"+model+":generateContent", bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("x-goog-api-key", listen.GeminiAPIKey)
		response, err := client.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		_ = response.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		if response.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("gemini hearing returned %s: %s", response.Status, truncate(payload))
			if response.StatusCode < 500 && response.StatusCode != http.StatusTooManyRequests {
				return "", lastErr
			}
			time.Sleep(2 * time.Second)
			continue
		}
		var reply struct {
			Candidates []struct {
				Content struct {
					Parts []struct {
						Text string `json:"text"`
					} `json:"parts"`
				} `json:"content"`
			} `json:"candidates"`
		}
		if err := json.Unmarshal(payload, &reply); err != nil {
			return "", fmt.Errorf("decode gemini hearing: %w", err)
		}
		var text strings.Builder
		for _, candidate := range reply.Candidates {
			for _, part := range candidate.Content.Parts {
				text.WriteString(part.Text)
			}
		}
		return strings.TrimSpace(text.String()), nil
	}
	return "", lastErr
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
	// The temporary is this writer's own: scenarios play in parallel, and
	// two of them synthesising the same line must not share one.
	temporary, err := os.CreateTemp(CacheDir, key+".*.part")
	if err != nil {
		return
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		_ = os.Remove(temporary.Name())
		return
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporary.Name())
		return
	}
	_ = os.Rename(temporary.Name(), filepath.Join(CacheDir, key+".pcm"))
}

// Transcribe is where the harness sends the agent's own audio when a check has
// to be scored against what a loudspeaker produced.
//
// It is deliberately a second endpoint rather than the session's recogniser.
// The point of the check it serves is that nothing involved in producing the
// audio gets to say what was in it, and the session's recogniser is the most
// involved thing there is.
type Hearing struct {
	Endpoint string
	Model    string
	Language string
	Client   *http.Client
	// GeminiAPIKey, when set, has Gemini listen to the recording instead of
	// the transcription endpoint. A speech recogniser built for dictation
	// hears a lone "One." as "One eight" and "Eighteen." as "eight teen";
	// a model asked what was said in a short clip does not. GeminiModel
	// defaults to gemini-3.7-flash; GeminiEndpoint to the public API.
	GeminiAPIKey   string
	GeminiModel    string
	GeminiEndpoint string
}

// geminiHearingPrompt asks for the words alone. Numbers as words, because
// the counting checks read them either way and the room's voice says them
// as words; nothing invented for silence, because a window with nothing in
// it is a finding the checks rely on.
const geminiHearingPrompt = "Transcribe exactly the words spoken in this recording, in the language they are " +
	"spoken in, with numbers written as words. Reply with the transcript only. If nothing is spoken, reply " +
	"with an empty line."

// Hear transcribes one stretch of the agent's audio.
func (voice SpeechVoice) Hear(ctx context.Context, samples []int16, rateHz int) (string, error) {
	if voice.Listen.Endpoint == "" {
		return "", errors.New("no transcription endpoint is configured for the harness")
	}
	return voice.Listen.hear(ctx, samples, rateHz)
}

func (listen Hearing) hear(ctx context.Context, samples []int16, rateHz int) (string, error) {
	if len(samples) == 0 {
		return "", nil
	}
	pcm := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(pcm[index*2:], uint16(sample))
	}
	container, err := audio.EncodeWAVMono16(pcm, uint32(rateHz))
	if err != nil {
		return "", err
	}
	if listen.GeminiAPIKey != "" {
		return listen.hearWithGemini(ctx, container)
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "agent.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(container); err != nil {
		return "", err
	}
	fields := [][2]string{{"model", listen.Model}, {"response_format", "json"}}
	if listen.Language != "" {
		fields = append(fields, [2]string{"language", listen.Language})
	}
	for _, field := range fields {
		if field[1] == "" {
			continue
		}
		if err := writer.WriteField(field[0], field[1]); err != nil {
			return "", err
		}
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	timed, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(timed, http.MethodPost, listen.Endpoint, &body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Accept", "application/json")
	client := listen.Client
	if client == nil {
		client = &http.Client{Timeout: 180 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcription endpoint returned %s: %s",
			response.Status, truncate(payload))
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("decode transcription response: %w", err)
	}
	return strings.TrimSpace(decoded.Text), nil
}

var _ Ears = SpeechVoice{}
