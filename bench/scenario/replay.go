package scenario

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strings"

	"github.com/bojieli/OpenRealtime/bench"
)

const (
	ReplayVersion        = 1
	maximumReplayMS      = 600_000
	maximumReplaySamples = maximumReplayMS * 24
)

// ReplayEvidence retains the non-score inputs needed to reproduce a completed
// attempt. PCM stays in the independently sealed stereo recording. Hearing
// text is an external recognizer observation, not the runtime's spoken cursor.
// Replaying it checks the deterministic count decision, not recognizer accuracy.
type ReplayEvidence struct {
	Version          int             `json:"version"`
	ScenarioSHA256   string          `json:"scenario_sha256"`
	Inputs           []ReplayInput   `json:"inputs"`
	TrailingSamples  int             `json:"trailing_samples"`
	HearingAvailable bool            `json:"hearing_available"`
	Hearings         []ReplayHearing `json:"hearings,omitempty"`
}

type ReplayInput struct {
	Samples     int    `json:"samples"`
	PCM16SHA256 string `json:"pcm16_sha256"`
}

type ReplayHearing struct {
	FromMS      int    `json:"from_ms"`
	ToMS        int    `json:"to_ms"`
	PCM16SHA256 string `json:"pcm16_sha256"`
	Text        string `json:"text,omitempty"`
	Error       string `json:"error,omitempty"`
}

type recordingVoice struct {
	voice  Voice
	inputs []ReplayInput
}

func (voice *recordingVoice) Speak(ctx context.Context, speaker, text string) ([]int16, error) {
	samples, err := voice.voice.Speak(ctx, speaker, text)
	if err == nil {
		voice.inputs = append(voice.inputs, ReplayInput{Samples: len(samples), PCM16SHA256: replayPCMDigest(samples)})
	}
	return samples, err
}

func replayPCMDigest(samples []int16) string {
	digest := sha256.New()
	var buffer [4096]byte
	for len(samples) > 0 {
		count := min(len(samples), len(buffer)/2)
		for index, value := range samples[:count] {
			binary.LittleEndian.PutUint16(buffer[index*2:], uint16(value))
		}
		_, _ = digest.Write(buffer[:count*2])
		samples = samples[count:]
	}
	return fmt.Sprintf("sha256:%x", digest.Sum(nil))
}

func replayScenarioDigest(item Scenario) (string, error) {
	var menu *Menu
	if item.Menu != nil {
		menu = item.Menu()
	}
	images := make([]string, len(item.Sees))
	for index, sight := range item.Sees {
		data, err := os.ReadFile(sight.Path)
		if err != nil {
			return "", fmt.Errorf("read authored replay visual input: %w", err)
		}
		images[index] = fmt.Sprintf("sha256:%x", sha256.Sum256(data))
	}
	payload, err := json.Marshal(struct {
		Name, Note, Instructions string
		Script                   []Line
		Checks                   []Check
		Tools                    []Tool
		Sees                     []Sight
		TrailingMS               int
		Menu                     *Menu
		ImageSHA256              []string
	}{item.Name, item.Note, item.Instructions, item.Script, item.Checks, item.Tools, item.Sees, item.TrailingMS, menu, images})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("sha256:%x", sha256.Sum256(payload)), nil
}

func recordHearing(listen heard, capture bench.SessionAudioCapture, evidence *ReplayEvidence) heard {
	evidence.HearingAvailable = listen != nil
	if listen == nil {
		return nil
	}
	return func(from, to int) (string, error) {
		text, err := listen(from, to)
		entry := ReplayHearing{FromMS: from, ToMS: to,
			PCM16SHA256: replayPCMDigest(agentAudioBetween(capture, from, to)), Text: text}
		if err != nil {
			entry.Error = err.Error()
		}
		evidence.Hearings = append(evidence.Hearings, entry)
		return text, err
	}
}

type replayVoice struct {
	inputs [][]int16
	next   int
}

func (voice *replayVoice) Speak(context.Context, string, string) ([]int16, error) {
	if voice.next >= len(voice.inputs) {
		return nil, errors.New("replay input inventory exhausted")
	}
	samples := voice.inputs[voice.next]
	voice.next++
	return samples, nil
}

// ReplayRecorded recomputes one current-scorer result using the canonical
// authored case, exact retained WAV, transcript, and observed hearing windows.
// It refuses older/missing evidence instead of silently rescoring history.
// Missing/partly sent cues are retained diagnostics but cannot be reconstructed
// from the room channel alone and therefore do not pass this verifier.
func ReplayRecorded(ctx context.Context, item Scenario, retained Result, wav []byte) (Result, error) {
	if ctx == nil || ctx.Err() != nil {
		return Result{}, errors.New("scenario replay requires an active context")
	}
	if retained.ScorerVersion != ScorerVersion || retained.Replay == nil || retained.Replay.Version != ReplayVersion {
		return Result{}, errors.New("scenario result has no current deterministic replay evidence")
	}
	evidence := retained.Replay
	digest, err := replayScenarioDigest(item)
	if err != nil || digest != evidence.ScenarioSHA256 || retained.Scenario != item.Name || len(evidence.Inputs) != len(item.Script) || len(item.Script) > 64 || len(evidence.Hearings) > 128 {
		return Result{}, errors.New("scenario replay authored case or input inventory differs")
	}
	if err := validateScenarioChecks(item); err != nil {
		return Result{}, err
	}
	room, agent, err := replayWAV(wav)
	if err != nil {
		return Result{}, err
	}
	capture, err := replayCapture(retained.Transcript, room, agent)
	if err != nil {
		return Result{}, err
	}
	inputs := make([][]int16, len(item.Script))
	totalSamples := 0
	boundMS := item.TrailingMS
	if boundMS < 0 || boundMS > maximumReplayMS {
		return Result{}, errors.New("scenario replay trailing interval exceeds bounds")
	}
	for _, sight := range item.Sees {
		if sight.AtMS < 0 || sight.AtMS > maximumReplayMS {
			return Result{}, errors.New("scenario replay visual interval exceeds bounds")
		}
		boundMS = max(boundMS, sight.AtMS)
	}
	for index, input := range evidence.Inputs {
		line := item.Script[index]
		if input.Samples < 0 || input.Samples > maximumReplaySamples-totalSamples || line.AtMS < 0 || line.AtMS > maximumReplayMS {
			return Result{}, errors.New("scenario replay input exceeds bounds")
		}
		totalSamples += input.Samples
		boundMS = max(boundMS, line.AtMS)
		if line.AfterSpeech != nil {
			if line.AfterSpeech.LatestMS < 0 || line.AfterSpeech.LatestMS > maximumReplayMS {
				return Result{}, errors.New("scenario replay cue interval exceeds bounds")
			}
			boundMS = max(boundMS, line.AfterSpeech.LatestMS)
		}
	}
	// Conservative upper bound before Compose allocates, including source
	// duration, every inter-line breath, and trailing silence.
	if boundMS+(totalSamples+23)/24+len(inputs)*breathMS+item.TrailingMS > maximumReplayMS {
		return Result{}, errors.New("scenario replay composed input exceeds bounds")
	}
	for index, input := range evidence.Inputs {
		inputs[index] = make([]int16, input.Samples)
	}
	// First derive the authored positions from sample counts. Then recover
	// source PCM at those positions and let Compose reconstruct the real input.
	timeline, err := Compose(ctx, &replayVoice{inputs: inputs}, item)
	if err != nil || timeline.TotalMS < 0 || timeline.TotalMS > maximumReplayMS {
		return Result{}, errors.New("scenario replay timeline exceeds bounds")
	}
	if evidence.TrailingSamples < 0 || evidence.TrailingSamples > maximumReplaySamples-len(timeline.Samples) {
		return Result{}, errors.New("scenario replay trailing input exceeds bounds")
	}
	playbackSamples := len(timeline.Samples) + evidence.TrailingSamples
	if retained.Transcript.PlaybackMS != float64(playbackSamples)/24 || len(room) < playbackSamples || retained.Transcript.Failure != "" {
		return Result{}, errors.New("scenario replay lacks a complete authored playback")
	}
	cueIndex := 0
	for index, line := range item.Script {
		start := timeline.Spans[index].StartMS
		if line.AfterSpeech != nil {
			if cueIndex >= len(retained.Transcript.SpeechCues) {
				return Result{}, errors.New("scenario replay lacks sent cue evidence")
			}
			cue := retained.Transcript.SpeechCues[cueIndex]
			cueIndex++
			if cue.Status != "sent" || cue.SentSamples != evidence.Inputs[index].Samples {
				return Result{}, errors.New("scenario replay needs the complete sent cue PCM")
			}
			start = cue.StartMS
		}
		if start < 0 || start > maximumReplayMS || evidence.Inputs[index].Samples > len(room)-start*24 {
			return Result{}, errors.New("scenario replay input lies outside the recording")
		}
		inputs[index] = room[start*24 : start*24+evidence.Inputs[index].Samples]
		if replayPCMDigest(inputs[index]) != evidence.Inputs[index].PCM16SHA256 {
			return Result{}, fmt.Errorf("scenario replay input PCM differs on line %d", index)
		}
	}
	timeline, err = Compose(ctx, &replayVoice{inputs: inputs}, item)
	if err != nil {
		return Result{}, err
	}
	// Everything outside the authored lines/cues must be input silence.
	expectedRoom := make([]int16, len(room))
	copy(expectedRoom, timeline.Samples)
	cueIndex = 0
	for index, line := range item.Script {
		if line.AfterSpeech != nil {
			start := retained.Transcript.SpeechCues[cueIndex].StartMS * 24
			copy(expectedRoom[start:], inputs[index])
			cueIndex++
		}
	}
	if !slices.Equal(expectedRoom, room) {
		return Result{}, errors.New("scenario replay room PCM contains undeclared input")
	}
	menu, err := replayMenu(item, retained.Transcript)
	if err != nil {
		return Result{}, err
	}
	hearingIndex := 0
	var hearingErr error
	var listen heard
	if evidence.HearingAvailable {
		listen = func(from, to int) (string, error) {
			if hearingIndex >= len(evidence.Hearings) || from < 0 || to < from || to > maximumReplayMS {
				hearingErr = errors.New("scenario replay lacks bounded hearing evidence")
				return "", hearingErr
			}
			entry := evidence.Hearings[hearingIndex]
			hearingIndex++
			if entry.FromMS != from || entry.ToMS != to || len(entry.Text)+len(entry.Error) > 64<<10 ||
				entry.PCM16SHA256 != replayPCMDigest(agentAudioBetween(capture, from, to)) {
				hearingErr = errors.New("scenario replay hearing window or audio differs")
				return "", hearingErr
			}
			if entry.Error != "" {
				return entry.Text, errors.New(entry.Error)
			}
			return entry.Text, nil
		}
	}
	replayed := score(item, timeline, retained.Transcript, menu, listen, &capture)
	if hearingErr != nil || hearingIndex != len(evidence.Hearings) {
		return Result{}, errors.New("scenario replay hearing inventory differs from the scorer requests")
	}
	actual, _ := resolveSpeechCues(item, timeline, retained.Transcript, &capture)
	replayed.Latencies = latencies(item, actual, retained.Transcript)
	replayed.Replay = evidence
	want, err := json.Marshal(retained)
	if err != nil {
		return Result{}, err
	}
	got, err := json.Marshal(replayed)
	if err != nil || !bytes.Equal(want, got) {
		return Result{}, errors.New("scenario replay outcome or metrics differ from the retained result")
	}
	return replayed, nil
}

func replayWAV(wav []byte) ([]int16, []int16, error) {
	if len(wav) < 44 || len(wav) > 44+maximumReplaySamples*4 || (len(wav)-44)%4 != 0 ||
		string(wav[:4]) != "RIFF" || int(binary.LittleEndian.Uint32(wav[4:8])) != len(wav)-8 ||
		string(wav[8:16]) != "WAVEfmt " || binary.LittleEndian.Uint32(wav[16:20]) != 16 ||
		binary.LittleEndian.Uint16(wav[20:22]) != 1 || binary.LittleEndian.Uint16(wav[22:24]) != 2 ||
		binary.LittleEndian.Uint32(wav[24:28]) != 24000 || binary.LittleEndian.Uint32(wav[28:32]) != 96000 ||
		binary.LittleEndian.Uint16(wav[32:34]) != 4 || binary.LittleEndian.Uint16(wav[34:36]) != 16 ||
		string(wav[36:40]) != "data" || int(binary.LittleEndian.Uint32(wav[40:44])) != len(wav)-44 {
		return nil, nil, errors.New("scenario replay requires a bounded canonical 24 kHz stereo PCM16 WAV")
	}
	room, agent := make([]int16, (len(wav)-44)/4), make([]int16, (len(wav)-44)/4)
	for index := range room {
		room[index] = int16(binary.LittleEndian.Uint16(wav[44+index*4:]))
		agent[index] = int16(binary.LittleEndian.Uint16(wav[46+index*4:]))
	}
	return room, agent, nil
}

func replayCapture(transcript bench.Transcript, room, agent []int16) (bench.SessionAudioCapture, error) {
	capture := bench.SessionAudioCapture{SampleRateHz: 24000, RoomPCM16: room}
	end := 0
	if len(transcript.Moments) > 100_000 {
		return capture, errors.New("scenario replay has too many moments")
	}
	for _, moment := range transcript.Moments {
		if !finiteReplayMS(moment.AtMS) {
			return capture, errors.New("scenario replay has an invalid event time")
		}
		if moment.Kind != bench.MomentAgentAudio {
			continue
		}
		if !finiteReplayMS(moment.PlayoutAtMS) || !finiteReplayMS(moment.AudioMS) || moment.AudioMS <= 0 {
			return capture, errors.New("scenario replay has an invalid audio interval")
		}
		start, count := int(math.Round(moment.PlayoutAtMS*24)), int(math.Round(moment.AudioMS*24))
		if start < end || count <= 0 || start > len(agent) || count > len(agent)-start || math.Abs(float64(count)-moment.AudioMS*24) > 0.000001 {
			return capture, errors.New("scenario replay audio intervals overlap or exceed the recording")
		}
		for _, sample := range agent[end:start] {
			if sample != 0 {
				return capture, errors.New("scenario replay has unattributed agent PCM")
			}
		}
		capture.Agent = append(capture.Agent, bench.TimedAudioChunk{AtMS: moment.PlayoutAtMS, PCM16: agent[start : start+count]})
		end = start + count
	}
	for _, sample := range agent[end:] {
		if sample != 0 {
			return capture, errors.New("scenario replay has unattributed trailing agent PCM")
		}
	}
	return capture, nil
}

func finiteReplayMS(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && value >= 0 && value <= maximumReplayMS
}

func replayMenu(item Scenario, transcript bench.Transcript) (*Menu, error) {
	if item.Menu == nil {
		return nil, nil
	}
	menu := item.Menu()
	pending := map[string]string{}
	seen := map[string]bool{}
	for _, moment := range transcript.Moments {
		switch moment.Kind {
		case bench.MomentToolCall:
			if moment.CallID == "" || seen[moment.CallID] {
				return nil, errors.New("scenario replay menu call identity is missing or duplicated")
			}
			seen[moment.CallID] = true
			arguments := json.RawMessage(moment.Arguments)
			if !json.Valid(arguments) {
				arguments = json.RawMessage(`{}`)
			}
			output, err := menu.Respond(moment.Name, arguments)
			if err != nil {
				return nil, err
			}
			pending[moment.CallID] = moment.Name + "\x00" + string(output)
		case bench.MomentToolResult:
			if pending[moment.CallID] != moment.Name+"\x00"+moment.Text {
				return nil, errors.New("scenario replay menu result differs from the authored state machine")
			}
			delete(pending, moment.CallID)
		}
	}
	if len(pending) != 0 {
		return nil, errors.New("scenario replay menu calls lack result evidence")
	}
	return menu, nil
}

// TaskOutcome is the shared scenario-to-benchmark projection used by both
// campaign production and independent replay.
func TaskOutcome(id string, result Result, runErr error) bench.TaskOutcome {
	outcome := bench.TaskOutcome{ID: id, Completed: runErr == nil, Passed: result.Passed}
	outcome.AttachExecution(result.Transcript)
	if runErr != nil {
		outcome.Error = runErr.Error()
	}
	var heard []float64
	missed, negative := 0, 0
	for _, latency := range result.Latencies {
		switch {
		case !latency.Heard:
			missed++
		case latency.MS < 0:
			negative++
		default:
			heard = append(heard, latency.MS)
		}
	}
	outcome.Metrics = map[string]float64{"missed_reaction_triggers": float64(missed),
		"overlap_before_trigger": float64(negative), "failed_checks": float64(len(result.Failures))}
	if len(heard) > 0 {
		distribution := bench.Summarise(heard)
		outcome.Metrics["reaction_latency_p50_ms"] = distribution.P50
		outcome.Metrics["reaction_latency_p90_ms"] = distribution.P90
	}
	if len(result.Failures) > 0 {
		outcome.Notes = map[string]string{"failures": strings.Join(result.Failures, " | ")}
	}
	return outcome
}
