package scenario

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (voice SpeechVoice) Speak(ctx context.Context, speaker, text string) ([]int16, error) {
	name := voice.Default
	if chosen, ok := voice.Voices[speaker]; ok && chosen != "" {
		name = chosen
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
	return decodeWAV(payload)
}

// decodeWAV pulls 16-bit mono samples out of a RIFF file.
//
// It walks the chunk list rather than assuming the data starts at byte 44,
// because synthesis backends emit LIST and fact chunks and a fixed offset
// reads those as audio - which sounds like a click and measures like speech.
func decodeWAV(payload []byte) ([]int16, error) {
	if len(payload) < 12 || string(payload[0:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return nil, fmt.Errorf("speech endpoint did not return a WAV")
	}
	offset := 12
	for offset+8 <= len(payload) {
		id := string(payload[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(payload[offset+4 : offset+8]))
		start := offset + 8
		if size < 0 || start+size > len(payload) {
			size = len(payload) - start
		}
		if id == "data" {
			samples := make([]int16, size/2)
			for index := range samples {
				samples[index] = int16(binary.LittleEndian.Uint16(payload[start+index*2:]))
			}
			return samples, nil
		}
		offset = start + size
		if size%2 == 1 {
			offset++
		}
	}
	return nil, fmt.Errorf("WAV carried no data chunk")
}

func truncate(payload []byte) string {
	if len(payload) > 200 {
		return string(payload[:200])
	}
	return string(payload)
}
