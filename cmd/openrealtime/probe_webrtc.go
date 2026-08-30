package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// probeWebRTC drives a session the way a browser does.
//
// It exists so the claim that both transports work is checked by the same
// tool, on the same turn, rather than asserted twice. What it does is exactly
// what the browser demo does: offer, answer, one audio track, one data
// channel, and nothing else.
func probeWebRTC(ctx context.Context, endpoint string, samples []int16, realTime bool, output io.Writer) (probeResult, error) {
	connection, err := pion.NewPeerConnection(pion.Configuration{})
	if err != nil {
		return probeResult{}, fmt.Errorf("create peer connection: %w", err)
	}
	defer connection.Close()

	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{
		MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", "probe")
	if err != nil {
		return probeResult{}, err
	}
	if _, err := connection.AddTrack(track); err != nil {
		return probeResult{}, err
	}

	var collector webrtcCollector
	collector.done = make(chan struct{})
	connection.OnTrack(func(remote *pion.TrackRemote, _ *pion.RTPReceiver) {
		for {
			packet, _, err := remote.ReadRTP()
			if err != nil {
				return
			}
			collector.audio(len(packet.Payload))
		}
	})
	channel, err := connection.CreateDataChannel("oai-events", nil)
	if err != nil {
		return probeResult{}, err
	}
	channel.OnMessage(func(message pion.DataChannelMessage) {
		collector.event(message.Data, output)
	})

	offer, err := connection.CreateOffer(nil)
	if err != nil {
		return probeResult{}, err
	}
	gathered := pion.GatheringCompletePromise(connection)
	if err := connection.SetLocalDescription(offer); err != nil {
		return probeResult{}, err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return probeResult{}, ctx.Err()
	}

	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, endpoint, strings.NewReader(connection.LocalDescription().SDP))
	if err != nil {
		return probeResult{}, err
	}
	request.Header.Set("Content-Type", "application/sdp")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return probeResult{}, fmt.Errorf("offer: %w", err)
	}
	defer response.Body.Close()
	answer, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return probeResult{}, err
	}
	if response.StatusCode != http.StatusCreated {
		return probeResult{}, fmt.Errorf("the adapter refused the offer: %d %s",
			response.StatusCode, strings.TrimSpace(string(answer)))
	}
	if err := connection.SetRemoteDescription(pion.SessionDescription{
		Type: pion.SDPTypeAnswer, SDP: string(answer),
	}); err != nil {
		return probeResult{}, err
	}
	if err := waitConnected(ctx, connection); err != nil {
		return probeResult{}, err
	}

	// The track carries mu-law at 8 kHz, so the probe's 24 kHz audio is
	// downsampled and encoded here - which is what a browser's own stack would
	// be doing, just with a better resampler.
	encoded := encodeMuLaw(downsample(samples, 24_000, 8_000))
	const packetSamples = 160 // 20 ms
	started := time.Now()
	for offset := 0; offset < len(encoded); offset += packetSamples {
		end := min(offset+packetSamples, len(encoded))
		if err := track.WriteSample(media.Sample{
			Data: encoded[offset:end], Duration: 20 * time.Millisecond,
		}); err != nil {
			return probeResult{}, err
		}
		if realTime {
			elapsed := time.Duration(end) * time.Second / 8000
			if wait := elapsed - time.Since(started); wait > 0 {
				time.Sleep(wait)
			}
		}
	}

	select {
	case <-collector.done:
	case <-ctx.Done():
	}
	return collector.result(), nil
}

func waitConnected(ctx context.Context, connection *pion.PeerConnection) error {
	deadline := time.After(30 * time.Second)
	for {
		switch connection.ConnectionState() {
		case pion.PeerConnectionStateConnected:
			return nil
		case pion.PeerConnectionStateFailed, pion.PeerConnectionStateClosed:
			return errors.New("the peer connection failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return errors.New("the peer connection never connected")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// webrtcCollector accumulates what a browser would see.
type webrtcCollector struct {
	mu        sync.Mutex
	outcome   probeResult
	endpoint  time.Time
	responses int
	closed    bool
	done      chan struct{}
}

func (collector *webrtcCollector) audio(bytes int) {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	collector.outcome.audioFrames++
	collector.outcome.audioSeconds += float64(bytes) / 8000
	if collector.outcome.firstAudioReceipt == 0 && !collector.endpoint.IsZero() {
		collector.outcome.firstAudioReceipt = time.Since(collector.endpoint)
	}
}

func (collector *webrtcCollector) event(raw []byte, output io.Writer) {
	var envelope struct {
		Type       string `json:"type"`
		Transcript string `json:"transcript"`
		Delta      string `json:"delta"`
		Name       string `json:"name"`
		Error      struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	switch envelope.Type {
	case "input_audio_buffer.speech_stopped":
		collector.endpoint = time.Now()
		fmt.Fprint(output, ".")
	case "conversation.item.input_audio_transcription.completed":
		collector.outcome.transcript = appendTranscript(collector.outcome.transcript, envelope.Transcript)
	case "response.output_audio_transcript.delta":
		if strings.TrimSpace(envelope.Delta) != "" {
			collector.outcome.spoken = append(collector.outcome.spoken, envelope.Delta)
		}
	case "response.function_call_arguments.done":
		collector.outcome.toolCalls = append(collector.outcome.toolCalls, envelope.Name)
	case "response.done":
		collector.responses++
		if collector.responses >= 2 && collector.outcome.audioFrames > 0 && !collector.closed {
			collector.closed = true
			close(collector.done)
		}
	case "error":
		collector.outcome.err = errors.New(envelope.Error.Message)
		if !collector.closed {
			collector.closed = true
			close(collector.done)
		}
	}
}

func (collector *webrtcCollector) result() probeResult {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	return collector.outcome
}

// downsample reduces the probe's audio to the track's rate.
//
// It averages the input samples that map to each output sample rather than
// picking one of them. Dropping two samples in three folds everything above
// the new Nyquist frequency back into the audible band, and the result sounds
// like a bad connection - which would make this probe measure its own
// resampler rather than the server. A browser's own stack does this properly;
// averaging is the cheapest thing that is not actively wrong.
func downsample(samples []int16, sourceRate, targetRate int) []int16 {
	if sourceRate == targetRate || len(samples) == 0 {
		return samples
	}
	count := len(samples) * targetRate / sourceRate
	if count == 0 {
		return nil
	}
	result := make([]int16, count)
	for index := range result {
		start := index * sourceRate / targetRate
		end := (index + 1) * sourceRate / targetRate
		if end > len(samples) {
			end = len(samples)
		}
		if end <= start {
			end = start + 1
		}
		total := 0
		for position := start; position < end && position < len(samples); position++ {
			total += int(samples[position])
		}
		result[index] = int16(total / (end - start))
	}
	return result
}

// encodeMuLaw is the G.711 encoding the track carries.
func encodeMuLaw(samples []int16) []byte {
	encoded := make([]byte, len(samples))
	for index, sample := range samples {
		value := int(sample)
		sign := byte(0)
		if value < 0 {
			sign = 0x80
			value = -value
			if value > 32767 {
				value = 32767
			}
		}
		if value > 32635 {
			value = 32635
		}
		value += 0x84
		exponent := byte(7)
		for mask := 0x4000; exponent > 0 && value&mask == 0; mask >>= 1 {
			exponent--
		}
		mantissa := byte(value >> (exponent + 3) & 0x0f)
		encoded[index] = ^(sign | exponent<<4 | mantissa)
	}
	return encoded
}
