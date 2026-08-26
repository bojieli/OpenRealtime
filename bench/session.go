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
	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
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
	CallID  string  `json:"call_id,omitempty"`
	// Arguments preserves the action the model actually grounded. Accuracy
	// cannot be reconstructed from a tool name alone.
	Arguments string `json:"arguments,omitempty"`
	Source    string `json:"source,omitempty"`
	Observer  string `json:"observer,omitempty"`
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
	MomentToolResult    = "tool_result"
	MomentVideoFrame    = "video_frame_sent"
	MomentObservation   = "observation"
	MomentReady         = "environment_ready"
	// MomentScheduled marks a non-audio event the harness injected, so a
	// transcript shows why the agent spoke when nobody had said anything.
	MomentScheduled = "scheduled"
	MomentError     = "error"
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

// AudioStartedBetween is agent audio from turns that began inside the window.
//
// A silence check asks whether something in the window made the agent speak,
// and audio still playing from a turn that began before it cannot have. The
// distinction is not academic: an agent asked to report a build finishing says
// briefly that it will, and the tail of that sentence was being counted as a
// reaction to the first screen it saw three seconds later. Counting it that
// way also contradicts the scenario next to it, where the agent keeping the
// floor through somebody's "mhm" is the behaviour being asked for.
//
// A turn's audio begins at its first frame after the last response, which is
// the protocol's own boundary rather than a gap this has to guess at.
func (transcript Transcript) AudioStartedBetween(fromMS, toMS float64) float64 {
	total, startedAt := 0.0, -1.0
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case MomentResponseDone:
			startedAt = -1
		case MomentAgentAudio:
			if startedAt < 0 {
				startedAt = moment.AtMS
			}
			if startedAt < fromMS || startedAt > toMS {
				continue
			}
			if moment.AtMS < fromMS || moment.AtMS > toMS {
				continue
			}
			total += moment.AudioMS
		}
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

// FirstToolCallAfter is the wait from a moment in the recording to the next
// time a named tool was called.
//
// A silent act is still an answer. Measuring how soon it happened as a wait
// for speech asks an agent whose right move is to press a key and say nothing
// to fail either the latency check or the silence one beside it.
func (transcript Transcript) FirstToolCallAfter(name string, fromMS float64) (float64, bool) {
	for _, moment := range transcript.Moments {
		if moment.Kind == MomentToolCall && moment.Name == name && moment.AtMS >= fromMS {
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
	// HandleTool is the context-aware form used by interactive environments.
	// It takes precedence over Respond and receives the call identity so an
	// evaluator can retain an exact action trace and propagate idempotency.
	HandleTool func(context.Context, ToolRequest) (json.RawMessage, error)
	// Realtime plays audio at its own rate. Turning it off makes a suite
	// faster and its timing numbers meaningless, so it stays on for anything
	// that reports latency.
	Realtime bool
	// TrailingSilence is appended so server endpointing fires on the last
	// utterance. Zero selects 1200 ms.
	TrailingSilence time.Duration
	// WorkingTimeout bounds silence while the agent still owes a response, as
	// distinct from the short quiet that means it has finished. Zero selects
	// thirty seconds.
	WorkingTimeout time.Duration
	// Timeout bounds one conversation.
	Timeout time.Duration
	// Quiet suppresses per-task progress.
	Quiet bool
	// Scheduled are protocol events to send at points in the playback.
	//
	// A conversation is not only speech. A screen changes, a camera sees
	// something, a system event lands - and each of those has a moment,
	// exactly as an utterance does. Scheduling them on the same timeline is
	// what lets a scenario ask whether the agent spoke because of something it
	// saw while nobody was talking, which no amount of audio can express.
	Scheduled []ScheduledEvent
	// Video streams live frames until the conversation finishes. Unlike a
	// Scheduled event, a stream keeps observing while the agent acts, which is
	// necessary for multi-step computer use and transient visual tasks.
	Video []VideoStream
	// Ready runs after session configuration and video-source declarations but
	// before audio playback and frame capture. Browser tasks reset their clock
	// here so cue-to-action latency excludes connection setup.
	Ready func(context.Context) error
}

// ToolRequest is one complete model action received over the protocol.
type ToolRequest struct {
	CallID    string
	Name      string
	Arguments json.RawMessage
	Received  time.Time
}

// VideoStream is one declared source sampled for the duration of a task.
// Capture returns encoded JPEG or PNG bytes. A nil frame skips this tick.
type VideoStream struct {
	Source   string
	Width    int
	Height   int
	Interval time.Duration
	Capture  func(context.Context) ([]byte, error)
}

// ScheduledEvent is one protocol event and when to send it.
type ScheduledEvent struct {
	// AtMS is measured from the start of playback, like everything else in a
	// transcript, so a scheduled event and an utterance can be placed against
	// each other.
	AtMS  int
	Event map[string]any
}

// ErrConversationTimeout means a connected session continued working beyond
// its configured conversation horizon. Suites with a deterministic evaluator
// may score the state reached at that horizon as a completed negative outcome;
// setup, transport, and capture errors remain distinct infrastructure errors.
var ErrConversationTimeout = errors.New("the conversation did not finish before the timeout")

// ErrSessionFailure means the connected endpoint emitted a protocol error.
// Unlike ErrConversationTimeout, this is not an agent reaching a scoring
// horizon: ASR, model, engine, or transport work failed, so a suite must leave
// the task incomplete rather than publish the outage as a capability result.
var ErrSessionFailure = errors.New("the session reported a failure")

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
	for index := range config.Video {
		stream := &config.Video[index]
		if strings.TrimSpace(stream.Source) == "" || stream.Width <= 0 || stream.Height <= 0 {
			return Transcript{}, fmt.Errorf("video stream %d requires a source and positive geometry", index)
		}
		if stream.Capture == nil {
			return Transcript{}, fmt.Errorf("video stream %q requires capture", stream.Source)
		}
		if stream.Interval <= 0 {
			stream.Interval = time.Second / 3
		}
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
	if len(config.Video) > 0 {
		update["openrealtime"] = map[string]any{
			"version": openrealtime.Version,
			"supports": []string{
				string(openrealtime.FeatureVideoInput),
				string(openrealtime.FeatureObservations),
				string(openrealtime.FeatureComputerUse),
			},
			"observers": []string{"audio", "video"},
		}
	}
	if err := client.Send(timed, map[string]any{"type": "session.update", "session": update}); err != nil {
		return Transcript{}, err
	}
	for _, stream := range config.Video {
		if err := client.Send(timed, map[string]any{
			"type": openrealtime.EventVideoSourceUpdate, "source": stream.Source,
			"state": openrealtime.SourceActive, "width": stream.Width, "height": stream.Height,
		}); err != nil {
			return Transcript{}, err
		}
	}
	if config.Ready != nil {
		if err := config.Ready(timed); err != nil {
			return Transcript{}, fmt.Errorf("prepare session environment: %w", err)
		}
	}
	recorder.add(Moment{Kind: MomentReady})

	// A conversation and its video workers have different shutdown edges. A
	// normal conversation may finish while Capture is inside a multi-command
	// operation (for example, installing, capturing, and removing a set-of-mark
	// overlay). Canceling that operation and returning immediately lets the
	// orphaned worker race the next benchmark case and can invalidate a shared
	// browser connection. Stop scheduling frames, let the one already in flight
	// finish under the caller's run-wide context, and join every worker before
	// the session or environment can be reused.
	videoStop := make(chan struct{})
	videoErrors := make(chan error, max(1, len(config.Video)))
	var videoWorkers sync.WaitGroup
	for _, stream := range config.Video {
		stream := stream
		videoWorkers.Add(1)
		go func() {
			defer videoWorkers.Done()
			streamVideo(ctx, videoStop, client, recorder, stream, videoErrors)
		}()
	}
	var stopVideoOnce sync.Once
	stopVideo := func() {
		stopVideoOnce.Do(func() { close(videoStop) })
		videoWorkers.Wait()
	}
	defer stopVideo()

	const frameSamples = 2400 // 100 ms
	started := time.Now()
	scheduled := append([]ScheduledEvent(nil), config.Scheduled...)
	sort.SliceStable(scheduled, func(i, j int) bool { return scheduled[i].AtMS < scheduled[j].AtMS })
	sent := 0
	for offset := 0; offset < len(samples); offset += frameSamples {
		end := min(offset+frameSamples, len(samples))
		// Anything due by this point in the playback goes first, so a scheduled
		// event lands before the audio that follows it rather than after.
		atMS := offset * 1000 / 24_000
		for sent < len(scheduled) && scheduled[sent].AtMS <= atMS {
			if err := client.Send(timed, scheduled[sent].Event); err != nil {
				return Transcript{}, err
			}
			recorder.add(Moment{AtMS: float64(scheduled[sent].AtMS), Kind: MomentScheduled})
			sent++
		}
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
		stopVideo()
		if strings.TrimSpace(transcript.Failure) != "" {
			return transcript, fmt.Errorf("%w: %s", ErrSessionFailure, transcript.Failure)
		}
		return transcript, nil
	case err := <-videoErrors:
		stopVideo()
		return recorder.snapshot(), err
	case <-timed.Done():
		stopVideo()
		transcript := recorder.snapshot()
		if strings.TrimSpace(transcript.Failure) != "" {
			return transcript, fmt.Errorf("%w: %s", ErrSessionFailure, transcript.Failure)
		}
		return transcript, ErrConversationTimeout
	}
}

func streamVideo(
	ctx context.Context, stop <-chan struct{}, client *realtimeclient.Client, recorder *recorder,
	stream VideoStream, failures chan<- error,
) {
	send := func() error {
		frame, err := stream.Capture(ctx)
		if err != nil {
			return fmt.Errorf("capture video source %q: %w", stream.Source, err)
		}
		if len(frame) == 0 {
			return nil
		}
		if err := client.Send(ctx, map[string]any{
			"type": openrealtime.EventVideoFrameAppend, "source": stream.Source,
			"frame":        base64.StdEncoding.EncodeToString(frame),
			"timestamp_ms": time.Now().UnixMilli(),
		}); err != nil {
			return fmt.Errorf("send video source %q: %w", stream.Source, err)
		}
		recorder.add(Moment{Kind: MomentVideoFrame, Source: stream.Source})
		return nil
	}
	if err := send(); err != nil {
		select {
		case failures <- err:
		case <-stop:
		case <-ctx.Done():
		}
		return
	}
	ticker := time.NewTicker(stream.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := send(); err != nil {
				select {
				case failures <- err:
				case <-stop:
				case <-ctx.Done():
				}
				return
			}
		}
	}
}

type recorder struct {
	mu         sync.Mutex
	started    time.Time
	moments    []Moment
	playbackMS float64
	// openResponses counts responses the server has created and not finished.
	// While it is above zero the agent still owes this turn something, so
	// silence is work rather than completion.
	openResponses      int
	playbackFinishedAt time.Time
	failure            string
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
	recorder.playbackFinishedAt = time.Now()
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
//
// Quiet is measured from the later of playback ending and the last event,
// which matters more than it sounds. Arming a timer only when an event arrives
// means a session that produces nothing after playback never arms it at all,
// and every task in that cell fails with a timeout - which looks like the
// system hanging rather than the harness waiting.
//
// Quiet only means finished while the agent owes nothing. This system has a
// reasoning phase that is silent by construction, so a turn that needs it is
// quiet for as long as the question is hard - and a driver that read that as
// completion would score the agent on the answers it managed before the stop
// watch, which is a measurement of the harness. An open response is the
// protocol saying work is still owed, so quiet is not the test while one is
// open; workingFor bounds that separately, because a server that opens a
// response and never finishes it must still fail rather than hang.
func (recorder *recorder) collect(
	ctx context.Context, client *realtimeclient.Client, config SessionConfig,
) Transcript {
	const quietFor = 3 * time.Second
	workingFor := config.WorkingTimeout
	if workingFor <= 0 {
		workingFor = 30 * time.Second
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	lastEvent := time.Now()

	for {
		select {
		case <-ctx.Done():
			return recorder.snapshot()
		case <-ticker.C:
			recorder.mu.Lock()
			finishedAt := recorder.playbackFinishedAt
			recorder.mu.Unlock()
			if finishedAt.IsZero() {
				continue
			}
			since := finishedAt
			if lastEvent.After(since) {
				since = lastEvent
			}
			recorder.mu.Lock()
			working := recorder.openResponses > 0
			recorder.mu.Unlock()
			limit := quietFor
			if working {
				limit = workingFor
			}
			if time.Since(since) >= limit {
				return recorder.snapshot()
			}
		case event, open := <-client.Events():
			if !open {
				return recorder.snapshot()
			}
			lastEvent = time.Now()
			recorder.handle(ctx, client, config, event)
			recorder.mu.Lock()
			failed := recorder.failure != ""
			recorder.mu.Unlock()
			if failed {
				return recorder.snapshot()
			}
		}
	}
}

func (recorder *recorder) handle(
	ctx context.Context, client *realtimeclient.Client,
	config SessionConfig, event realtimeclient.Event,
) {
	switch event.Type {
	case "response.created":
		recorder.mu.Lock()
		recorder.openResponses++
		recorder.mu.Unlock()
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
		recorder.mu.Lock()
		if recorder.openResponses > 0 {
			recorder.openResponses--
		}
		recorder.mu.Unlock()
		recorder.add(Moment{Kind: MomentResponseDone})
	case "response.function_call_arguments.done":
		var decoded struct {
			CallID string `json:"call_id"`
			Name   string `json:"name"`
			// The protocol carries arguments as a JSON *string*, not as an
			// object. Decoding it as raw JSON and handing that to a suite
			// gives every suite a quoted blob that will never match anything
			// it compares against - which looks like a model that always gets
			// arguments wrong.
			Arguments string `json:"arguments"`
		}
		_ = event.Decode(&decoded)
		recorder.add(Moment{
			Kind: MomentToolCall, CallID: decoded.CallID, Name: decoded.Name,
			Arguments: decoded.Arguments,
		})
		arguments := json.RawMessage(decoded.Arguments)
		if !json.Valid(arguments) {
			arguments = json.RawMessage(`{}`)
		}
		recorder.answer(ctx, client, config, decoded.CallID, decoded.Name, arguments)
	case openrealtime.EventObservationAdded:
		var decoded struct {
			Observer string `json:"observer"`
			Source   string `json:"source"`
			Text     string `json:"text"`
		}
		_ = event.Decode(&decoded)
		recorder.add(Moment{
			Kind: MomentObservation, Observer: decoded.Observer,
			Source: decoded.Source, Text: decoded.Text,
		})
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
	if config.HandleTool != nil {
		produced, err := config.HandleTool(ctx, ToolRequest{
			CallID: callID, Name: name, Arguments: arguments, Received: time.Now(),
		})
		if err != nil {
			output = json.RawMessage(fmt.Sprintf("{%q:%q}", "error", err.Error()))
		} else if len(produced) > 0 {
			output = produced
		}
	} else if config.Respond != nil {
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
	recorder.add(Moment{
		Kind: MomentToolResult, CallID: callID, Name: name, Text: string(output),
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
	return pcm24k(decoded)
}

// DecodePCM24k decodes an in-memory WAV into the format the Realtime protocol
// uses. Benchmark suites embed their audio so an installed binary owns every
// task asset and does not depend on a source checkout at runtime.
func DecodePCM24k(raw []byte) ([]int16, error) {
	decoded, err := audio.Decode(raw)
	if err != nil {
		return nil, err
	}
	return pcm24k(decoded)
}

func pcm24k(decoded audio.Decoded) ([]int16, error) {
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
