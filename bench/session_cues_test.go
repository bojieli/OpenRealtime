package bench

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func cueTone(ms int) []int16 {
	pcm := make([]int16, ms*24)
	for i := range pcm {
		if i%2 == 0 {
			pcm[i] = 1200
		} else {
			pcm[i] = -1200
		}
	}
	return pcm
}

func TestSpeechCueWaitsForPlayedAudioAndRecordsOnlySuccessfulSends(t *testing.T) {
	base := make([]int16, 3000*24)
	cue := SpeechCue{Name: "cue", PCM16: cueTone(125), EarliestMS: 1000, LatestMS: 2000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}
	player, err := prepareSpeechCues([]SpeechCue{cue}, base, true)
	if err != nil {
		t.Fatal(err)
	}
	recorder := newSessionAudioRecorder(base)
	recorder.beginEpisode()
	// A single prefetched packet contains silence until 1200 ms, followed by
	// speech. Its arrival cannot make the input cue precede that played speech.
	output := append(make([]int16, 1200*24), cueTone(1800)...)
	recorder.addAgent(0, output)
	clear(cue.PCM16) // The prepared stimulus owns the original PCM.
	for offset := 0; offset < 1800*24; offset += 2400 {
		frame := player.frame(offset, base[offset:offset+2400], recorder)
		if !slices.Equal(frame, make([]int16, 2400)) {
			t.Fatal("cue fired before 600 ms of played speech")
		}
		player.sent(offset, frame, recorder)
	}
	frame := player.frame(1800*24, base[1800*24:1900*24], recorder)
	if player.observed[0].Status != "partial" || player.observed[0].StartMS != 1800 || player.observed[0].ActiveMS != 600 || player.observed[0].SentSamples != 0 {
		t.Fatalf("wrong release observation: %+v", player.observed)
	}
	if !slices.Equal(recorder.snapshot().RoomPCM16, base) {
		t.Fatal("unsent stimulus appeared in capture")
	}
	player.sent(1800*24, frame, recorder)
	if player.observed[0].Status != "partial" || player.observed[0].SentSamples != 2400 {
		t.Fatal("partial send claimed completion")
	}
	frame = player.frame(1900*24, base[1900*24:2000*24], recorder)
	player.sent(1900*24, frame, recorder)
	if player.observed[0].Status != "sent" || player.observed[0].SentSamples != 3000 || player.observed[0].EndMS != 1925 ||
		!slices.Equal(recorder.snapshot().RoomPCM16[1800*24:1925*24], cueTone(125)) {
		t.Fatalf("wrong sent cue/capture: %+v", player.observed)
	}
}

func TestSpeechCueMissesSilenceAndEnforcesRecentActivityAndOrderedGap(t *testing.T) {
	for _, mode := range []string{"silence", "quiet-tail", "continuous"} {
		t.Run(mode, func(t *testing.T) {
			base := make([]int16, 3000*24)
			cue := SpeechCue{Name: "one", PCM16: cueTone(100), EarliestMS: 1000, LatestMS: 1400, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100, MinimumGapMS: 600}
			second := cue
			second.Name = "two"
			second.LatestMS = 2500
			player, err := prepareSpeechCues([]SpeechCue{cue, second}, base, true)
			if err != nil {
				t.Fatal(err)
			}
			recorder := newSessionAudioRecorder(base)
			recorder.beginEpisode()
			switch mode {
			case "quiet-tail":
				recorder.addAgent(0, cueTone(800))
			case "continuous":
				recorder.addAgent(0, cueTone(3000))
			}
			for offset := 0; offset < len(base); offset += 2400 {
				frame := player.frame(offset, base[offset:offset+2400], recorder)
				player.sent(offset, frame, recorder)
			}
			if mode == "continuous" {
				if player.observed[0].StartMS != 1000 || player.observed[1].StartMS != 1700 || player.observed[1].Status != "sent" {
					t.Fatalf("wrong ordered gap: %+v", player.observed)
				}
			} else if player.observed[0].Status != "missed" || player.observed[1].Status != "missed" || !slices.Equal(recorder.snapshot().RoomPCM16, base) {
				t.Fatalf("absent opportunity acquired speech: %+v", player.observed)
			}
		})
	}
}

func TestSpeechCueRejectsBadAuthoredInputBeforeDial(t *testing.T) {
	base := SpeechCue{Name: "cue", PCM16: cueTone(100), EarliestMS: 1000, LatestMS: 1200, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}
	for _, mutate := range []func(*SpeechCue){
		func(c *SpeechCue) { c.Name = "" }, func(c *SpeechCue) { c.LatestMS = 999 }, func(c *SpeechCue) { c.LookbackMS = 10001 },
		func(c *SpeechCue) { c.MinimumActiveMS = 0 }, func(c *SpeechCue) { c.RecentMS = 0 }, func(c *SpeechCue) { c.PCM16 = nil },
		func(c *SpeechCue) { c.LatestMS = 120001 }, func(c *SpeechCue) { c.MinimumGapMS = -1 },
	} {
		cue := base
		mutate(&cue)
		if _, err := PlaySamples(t.Context(), SessionConfig{Endpoint: "invalid", Realtime: true, SpeechCues: []SpeechCue{cue}}, make([]int16, 2000*24)); err == nil || !strings.Contains(err.Error(), "speech cue") {
			t.Fatalf("bad cue reached dial: %v", err)
		}
	}
	for _, samples := range [][]int16{cueTone(2000), make([]int16, 1000*24)} {
		if _, err := prepareSpeechCues([]SpeechCue{base}, samples, true); err == nil {
			t.Fatal("overlap/short horizon accepted")
		}
	}
	if _, err := prepareSpeechCues([]SpeechCue{base}, make([]int16, 2000*24), false); err == nil {
		t.Fatal("unpaced cue accepted")
	}
}

func TestSpeechCueWireInputMatchesRetainedAudioAndActualClock(t *testing.T) {
	var mu sync.Mutex
	var input []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer connection.Close(websocket.StatusNormalClosure, "done")
		sent := false
		for {
			_, payload, err := connection.Read(r.Context())
			if err != nil {
				return
			}
			var event struct {
				Type  string `json:"type"`
				Audio string `json:"audio"`
			}
			if json.Unmarshal(payload, &event) != nil || event.Type != "input_audio_buffer.append" {
				continue
			}
			data, err := base64.StdEncoding.DecodeString(event.Audio)
			if err != nil {
				return
			}
			mu.Lock()
			input = append(input, data...)
			mu.Unlock()
			if sent {
				continue
			}
			sent = true
			pcm := append(make([]int16, 600*24), cueTone(2400)...)
			for _, event := range []map[string]any{{"type": "response.created"}, {"type": "response.output_audio.delta", "response_id": "speech", "delta": base64.StdEncoding.EncodeToString(encodePCM(pcm))}, {"type": "response.done", "response": map[string]any{"id": "speech", "status": "completed"}}} {
				payload, _ := json.Marshal(event)
				if connection.Write(r.Context(), websocket.MessageText, payload) != nil {
					return
				}
			}
		}
	}))
	defer server.Close()
	var capture SessionAudioCapture
	transcript, err := PlaySamples(t.Context(), SessionConfig{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"), Realtime: true, Timeout: 5 * time.Second, TrailingSilence: time.Millisecond, PostPlaybackQuiet: time.Millisecond,
		SpeechCues: []SpeechCue{{Name: "cue", PCM16: cueTone(125), EarliestMS: 1000, LatestMS: 2000, LookbackMS: 1000, MinimumActiveMS: 600, RecentMS: 100}}, CaptureAudio: func(value SessionAudioCapture) error { capture = value; return nil }}, make([]int16, 2400*24))
	if err != nil || len(transcript.SpeechCues) != 1 {
		t.Fatalf("cue playback: %+v %v", transcript.SpeechCues, err)
	}
	cue := transcript.SpeechCues[0]
	// The window is the authored one, not a narrower guess at where the cue
	// lands. What proves the sent-audio clock is the equality below: the tone
	// is present at exactly StartMS in the retained room audio, which an
	// arrival or authored clock cannot arrange. A tighter bound here proved
	// nothing extra and did fail the verification gate at load average 70,
	// where the agent's audio arrived later and the cue landed at 1600 ms -
	// inside its authored window and correct.
	if cue.Status != "sent" || cue.StartMS <= 1000 || cue.StartMS > 2000 || cue.SentSamples != 3000 {
		t.Fatalf("cue outside its authored window: %+v", cue)
	}
	mu.Lock()
	wire := slices.Clone(input)
	mu.Unlock()
	if !slices.Equal(wire, encodePCM(capture.RoomPCM16)) || !slices.Equal(capture.RoomPCM16[cue.StartMS*24:cue.StartMS*24+3000], cueTone(125)) {
		t.Fatal("wire, retained input, and cue identity differ")
	}
	payload, err := json.Marshal(transcript)
	if err != nil || !strings.Contains(string(payload), `"speech_cues"`) {
		t.Fatal("cue evidence missing from transcript")
	}
}

func TestSpeechActivityBoundsExternalClockAndIgnoresSubthresholdNoise(t *testing.T) {
	for _, at := range []int{-1, math.MaxInt, 120001} {
		if active, recent := SpeechActivity([]TimedAudioChunk{{PCM16: cueTone(2000)}}, at, 1000, 100); active != 0 || recent != 0 {
			t.Fatal("invalid external clock acquired activity")
		}
	}
	noise := make([]int16, 1000*24)
	for i := range noise {
		noise[i] = 127
	}
	if active, recent := SpeechActivity([]TimedAudioChunk{{PCM16: noise}}, 1000, 1000, 100); active != 0 || recent != 0 {
		t.Fatal("subthreshold noise acquired opportunity")
	}
}
