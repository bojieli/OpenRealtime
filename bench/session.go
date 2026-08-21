package bench

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/internal/audio"
	"github.com/bojieli/OpenRealtime/pcm"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// Every suite reduces to the same thing: play audio into a session and judge
// what came out. This is that, once, so five suites do not each grow their own
// slightly different session driver - and so a timing number from one is
// comparable with a timing number from another.

// Moment is one thing that happened, with when it happened.
//
// A suite judges a conversation from this record rather than from the wire,
// which is what lets the same recording be scored for endpointing, overlap,
// and tool use without three different clients.
type Moment struct {
	// AtMS is milliseconds from the start of playback, so a moment can be
	// compared against a recording's own annotations.
	AtMS float64 `json:"at_ms"`
	Kind string  `json:"kind"`
	Text string  `json:"text,omitempty"`
	// AudioMS is how much audio a speech moment carried.
	AudioMS float64 `json:"audio_ms,omitempty"`
	Name    string  `json:"name,omitempty"`
}

// Moment kinds.
const (
	MomentSpeechStarted = "user_speech_started"
	MomentSpeechStopped = "user_speech_stopped"
	MomentTranscript    = "transcript"
	MomentAgentText     = "agent_text"
	MomentAgentAudio    = "agent_audio"
	MomentResponseDone  = "response_done"
	MomentToolCall      = "tool_call"
	MomentError         = "error"
)

// Transcript is the complete timed record of one conversation.
type Transcript struct {
	Moments []Moment `json:"moments"`
	// PlaybackMS is how long the input recording was.
	PlaybackMS float64 `json:"playback_ms"`
	Failure    string  `json:"failure,omitempty"`
}

// UserTurns returns what the user was heard to say, in order.
func (transcript Transcript) UserTurns() []string {
	var turns []string
	for _, moment := range transcript.Moments {
		if moment.Kind == MomentTranscript && strings.TrimSpace(moment.Text) != "" {
			turns = append(turns, moment.Text)
		}
	}
	return turns
}

// AgentTurns returns what the agent said, one entry per response.
func (transcript Transcript) AgentTurns() []string {
	var turns []string
	current := strings.Builder{}
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case MomentAgentText:
			current.WriteString(moment.Text)
		case MomentResponseDone:
			if strings.TrimSpace(current.String()) != "" {
				turns = append(turns, current.String())
			}
			current.Reset()
		}
	}
	if strings.TrimSpace(current.String()) != "" {
		turns = append(turns, current.String())
	}
	return turns
}

// ToolCalls returns the names of calls handed to the client.
func (transcript Transcript) ToolCalls() []string {
	var names []string
	for _, moment := range transcript.Moments {
		if moment.Kind == MomentToolCall {
			names = append(names, moment.Name)
		}
	}
	return names
}

// AudioBetween totals the agent audio emitted in a window, which is how
// overlap and barge-in are judged: whether the agent was making sound while
// the user was talking, and how quickly it stopped.
func (transcript Transcript) AudioBetween(fromMS, toMS float64) float64 {
	total := 0.0
	for _, moment := range transcript.Moments {
		if moment.Kind != MomentAgentAudio || moment.AtMS < fromMS || moment.AtMS > toMS {
			continue
		}
		total += moment.AudioMS
	}
	return total
}

// FirstAudioAfter is the latency from a moment in the recording to the next
// audio the agent produced.
func (transcript Transcript) FirstAudioAfter(fromMS float64) (float64, bool) {
	for _, moment := range transcript.Moments {
		if moment.Kind == MomentAgentAudio && moment.AtMS >= fromMS {
			return moment.AtMS - fromMS, true
		}
	}
	return 0, false
}

// SessionConfig configures one conversation.
type SessionConfig struct {
	// Endpoint is the protocol endpoint.
	Endpoint string
	Token    string
	Model    string
	// Instructions is the agent instruction for this task.
	Instructions string
	// Tools are declared to the session. Their results come from Respond.
	Tools []json.RawMessage
	// Respond answers a tool call. Returning an error ends the task; a nil
	// function refuses every call, which is correct for a suite with no tools.
	Respond func(name string, arguments json.RawMessage) (json.RawMessage, error)
	// Realtime plays audio at its own rate. Turning it off makes a suite
	// faster and its timing numbers meaningless, so it stays on for anything
	// that reports latency.
	Realtime bool
	// TrailingSilence is appended so server endpointing fires on the last
	// utterance. Zero selects 1200 ms.
	TrailingSilence time.Duration
	// Timeout bounds one conversation.
	Timeout time.Duration
	// Quiet suppresses per-task progress.
	Quiet bool
}

// Play drives one recording through a session and returns the timed record.
func Play(ctx context.Context, config SessionConfig, wavPath string) (Transcript, error) {
	samples, err := loadPCM24k(wavPath)
	if err != nil {
		return Transcript{}, err
	}
	return PlaySamples(ctx, config, samples)
}

// PlaySamples drives 24 kHz PCM16 samples through a session.
func PlaySamples(ctx context.Context, config SessionConfig, samples []int16) (Transcript, error) {
	if strings.TrimSpace(config.Endpoint) == "" {
		return Transcript{}, errors.New("a session needs an endpoint")
	}
	if config.Timeout <= 0 {
		config.Timeout = 3 * time.Minute
	}
	if config.TrailingSilence <= 0 {
		config.TrailingSilence = 1200 * time.Millisecond
	}
	samples = append(samples, make([]int16, int(config.TrailingSilence.Seconds()*24_000))...)

	timed, cancel := context.WithTimeout(ctx, config.Timeout)
	defer cancel()
	client, err := realtimeclient.Dial(timed, realtimeclient.Config{
		URL: config.Endpoint, Token: config.Token, Model: config.Model,
	})
	if err != nil {
		return Transcript{}, err
	}
	defer client.Close()

	recorder := &recorder{started: time.Now()}
	collected := make(chan Transcript, 1)
	go func() { collected <- recorder.collect(timed, client, config) }()

	update := map[string]any{
		"type": "realtime",
		"audio": map[string]any{
			"input":  map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
			"output": map[string]any{"format": map[string]any{"type": "audio/pcm", "rate": 24000}},
		},
	}
	if strings.TrimSpace(config.Instructions) != "" {
		update["instructions"] = config.Instructions
	}
	if len(config.Tools) > 0 {
		tools := make([]json.RawMessage, len(config.Tools))
		copy(tools, config.Tools)
		update["tools"] = tools
	}
	if err := client.Send(timed, map[string]any{"type": "session.update", "session": update}); err != nil {
		return Transcript{}, err
	}

	const frameSamples = 2400 // 100 ms
	started := time.Now()
	for offset := 0; offset < len(samples); offset += frameSamples {
		end := min(offset+frameSamples, len(samples))
		if err := client.Send(timed, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(encodePCM(samples[offset:end])),
		}); err != nil {
			return Transcript{}, err
		}
		if config.Realtime {
			elapsed := time.Duration(end) * time.Second / 24_000
			if wait := elapsed - time.Since(started); wait > 0 {
				select {
				case <-time.After(wait):
				case <-timed.Done():
					break
				}
			}
		}
	}
	recorder.playbackDone(float64(len(samples)) / 24.0)

	select {
	case transcript := <-collected:
		return transcript, nil
	case <-timed.Done():
		return recorder.snapshot(), errors.New("the conversation did not finish before the timeout")
	}
}

type recorder struct {
	mu         sync.Mutex
	started    time.Time
	moments    []Moment
	playbackMS float64
	failure    string
}

func (recorder *recorder) at() float64 {
	return float64(time.Since(recorder.started).Microseconds()) / 1000
}

func (recorder *recorder) add(moment Moment) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	moment.AtMS = recorder.at()
	recorder.moments = append(recorder.moments, moment)
}

func (recorder *recorder) playbackDone(milliseconds float64) {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.playbackMS = milliseconds
}

func (recorder *recorder) snapshot() Transcript {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	moments := append([]Moment(nil), recorder.moments...)
	sort.SliceStable(moments, func(left, right int) bool {
		return moments[left].AtMS < moments[right].AtMS
	})
	return Transcript{Moments: moments, PlaybackMS: recorder.playbackMS, Failure: recorder.failure}
}

// collect reads the session until it goes quiet after playback.
//
// "Quiet after playback" rather than "a fixed number of responses": a suite
// recording may contain one turn or five, and counting responses would make
// the driver suite-specific.
func (recorder *recorder) collect(
	ctx context.Context, client *realtimeclient.Client, config SessionConfig,
) Transcript {
	idle := time.NewTimer(time.Hour)
	defer idle.Stop()
	const quietFor = 3 * time.Second

	for {
		select {
		case <-ctx.Done():
			return recorder.snapshot()
		case <-idle.C:
			return recorder.snapshot()
		case event, open := <-client.Events():
			if !open {
				return recorder.snapshot()
			}
			recorder.mu.Lock()
			playbackFinished := recorder.playbackMS > 0
			recorder.mu.Unlock()
			if playbackFinished {
				idle.Reset(quietFor)
			}
			recorder.handle(ctx, client, config, event)
		}
	}
}

func (recorder *recorder) handle(
	ctx context.Context, client *realtimeclient.Client,
	config SessionConfig, event realtimeclient.Event,
) {
	switch event.Type {
	case "input_audio_buffer.speech_started":
		recorder.add(Moment{Kind: MomentSpeechStarted})
	case "input_audio_buffer.speech_stopped":
		recorder.add(Moment{Kind: MomentSpeechStopped})
	case "conversation.item.input_audio_transcription.completed":
		var decoded struct {
			Transcript string `json:"transcript"`
		}
		_ = event.Decode(&decoded)
		recorder.add(Moment{Kind: MomentTranscript, Text: decoded.Transcript})
	case "response.output_audio_transcript.delta":
		var decoded struct {
			Delta string `json:"delta"`
		}
		_ = event.Decode(&decoded)
		if strings.TrimSpace(decoded.Delta) != "" {
			recorder.add(Moment{Kind: MomentAgentText, Text: decoded.Delta})
		}
	case "response.output_audio.delta":
		var decoded struct {
			Delta string `json:"delta"`
		}
		_ = event.Decode(&decoded)
		payload, err := base64.StdEncoding.DecodeString(decoded.Delta)
		if err == nil && len(payload) > 0 {
			recorder.add(Moment{Kind: MomentAgentAudio, AudioMS: float64(len(payload)/2) / 24.0})
		}
	case "response.done":
		recorder.add(Moment{Kind: MomentResponseDone})
	case "response.function_call_arguments.done":
		var decoded struct {
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		_ = event.Decode(&decoded)
		recorder.add(Moment{Kind: MomentToolCall, Name: decoded.Name})
		recorder.answer(ctx, client, config, decoded.CallID, decoded.Name, decoded.Arguments)
	case "error":
		var decoded struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = event.Decode(&decoded)
		recorder.add(Moment{Kind: MomentError, Text: decoded.Error.Message})
		recorder.mu.Lock()
		recorder.failure = decoded.Error.Message
		recorder.mu.Unlock()
	}
}

// answer returns a tool result.
//
// A suite with no tools still answers, with a refusal. Leaving a call
// unanswered would hang the turn and make every task in the cell time out,
// which would look like the system failing rather than the harness.
func (recorder *recorder) answer(
	ctx context.Context, client *realtimeclient.Client,
	config SessionConfig, callID, name string, arguments json.RawMessage,
) {
	output := json.RawMessage(`{"error":"no tools are available in this task"}`)
	if config.Respond != nil {
		produced, err := config.Respond(name, arguments)
		if err != nil {
			output = json.RawMessage(fmt.Sprintf("{%q:%q}", "error", err.Error()))
		} else if len(produced) > 0 {
			output = produced
		}
	}
	_ = client.Send(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "function_call_output", "call_id": callID, "output": string(output),
		},
	})
	_ = client.Send(ctx, map[string]any{"type": "response.create"})
}

func encodePCM(samples []int16) []byte {
	encoded := make([]byte, len(samples)*2)
	for index, sample := range samples {
		binary.LittleEndian.PutUint16(encoded[index*2:], uint16(sample))
	}
	return encoded
}

// loadPCM24k reads a WAV file as 24 kHz mono PCM16, resampling if needed.
func loadPCM24k(path string) ([]int16, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	decoded, err := audio.ReadFile(path)
	if err != nil {
		return nil, err
	}
	payload := decoded.PCM16LE
	if decoded.Metadata.SampleRateHz != 24_000 {
		resampler, err := pcm.NewResampler(decoded.Metadata.SampleRateHz, 24_000)
		if err != nil {
			return nil, err
		}
		converted, err := resampler.Push(payload)
		if err != nil {
			return nil, err
		}
		terminal, err := resampler.Finalize()
		if err != nil {
			return nil, err
		}
		payload = append(converted, terminal...)
	}
	samples := make([]int16, len(payload)/2)
	for index := range samples {
		samples[index] = int16(binary.LittleEndian.Uint16(payload[index*2:]))
	}
	return samples, nil
}
