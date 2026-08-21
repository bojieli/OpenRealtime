// Package livekit joins a LiveKit room as an agent participant and proxies to
// an OpenRealtime endpoint.
//
// It is a separate module on purpose. This is an agent that joins a room and
// speaks the protocol; it is a client rather than a component, so it can ship
// on its own release cycle, be replaced by an equivalent for another RTC
// provider, or be rewritten by somebody else entirely without touching the
// server. The server does not know it exists.
//
// The rule that governs the in-process WebRTC adapter governs this one too: it
// may terminate media, and it may not express anything a plain protocol client
// could not.
package livekit

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/opus"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

// Config configures the agent.
type Config struct {
	// URL is the LiveKit server, ws:// or wss://.
	URL string
	// APIKey and APISecret authenticate the agent to LiveKit.
	APIKey    string
	APISecret string
	// Room is the room to join.
	Room string
	// Identity and Name are how the agent appears to other participants.
	Identity string
	Name     string

	// Endpoint is the OpenRealtime protocol endpoint this agent proxies to.
	Endpoint string
	// Token is the credential the agent presents to that endpoint.
	Token string
	// Model selects the endpoint's model.
	Model string

	// PacketDuration is the outbound packetisation interval. Zero selects 20 ms.
	PacketDuration time.Duration
	// Logf receives operational messages.
	Logf func(string, ...any)
}

// Agent is one room participant bridged to one protocol session.
type Agent struct {
	config Config

	room   *lksdk.Room
	client *client
	track  *pion.TrackLocalStaticSample

	mu      sync.Mutex
	started bool
	closed  atomic.Bool
	done    chan struct{}
	once    sync.Once
}

// New validates the configuration.
func New(config Config) (*Agent, error) {
	for name, value := range map[string]string{
		"URL": config.URL, "APIKey": config.APIKey, "APISecret": config.APISecret,
		"Room": config.Room, "Endpoint": config.Endpoint,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("a LiveKit agent requires %s", name)
		}
	}
	if strings.TrimSpace(config.Identity) == "" {
		config.Identity = "openrealtime-agent"
	}
	if strings.TrimSpace(config.Name) == "" {
		config.Name = "OpenRealtime"
	}
	if config.PacketDuration <= 0 {
		config.PacketDuration = 20 * time.Millisecond
	}
	if config.Logf == nil {
		config.Logf = func(string, ...any) {}
	}
	return &Agent{config: config, done: make(chan struct{})}, nil
}

// Run joins the room and proxies until the context ends or the room closes.
func (agent *Agent) Run(ctx context.Context) error {
	agent.mu.Lock()
	if agent.started {
		agent.mu.Unlock()
		return errors.New("this agent has already run")
	}
	agent.started = true
	agent.mu.Unlock()

	protocolClient, err := dial(ctx, agent.config.Endpoint, agent.config.Token, agent.config.Model)
	if err != nil {
		return fmt.Errorf("connect to the protocol endpoint: %w", err)
	}
	agent.client = protocolClient
	defer protocolClient.Close()

	// The agent owns the media format because it terminates media, exactly as
	// the in-process adapter does.
	if err := protocolClient.Send(ctx, map[string]any{
		"type": "session.update",
		"session": map[string]any{
			"type": "realtime",
			"audio": map[string]any{
				"input":  map[string]any{"format": map[string]any{"type": "audio/pcmu"}},
				"output": map[string]any{"format": map[string]any{"type": "audio/pcmu"}},
			},
		},
	}); err != nil {
		return fmt.Errorf("configure the protocol session: %w", err)
	}

	track, err := pion.NewTrackLocalStaticSample(pion.RTPCodecCapability{
		MimeType: pion.MimeTypePCMU, ClockRate: 8000, Channels: 1,
	}, "audio", agent.config.Identity)
	if err != nil {
		return fmt.Errorf("create the agent's audio track: %w", err)
	}
	agent.track = track

	callback := &lksdk.RoomCallback{
		OnDisconnected: func() { agent.close() },
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(remote *pion.TrackRemote, _ *lksdk.RemoteTrackPublication, participant *lksdk.RemoteParticipant) {
				if remote.Kind() != pion.RTPCodecTypeAudio {
					return
				}
				agent.config.Logf("subscribed to audio from %s", participant.Identity())
				go agent.pumpInbound(ctx, remote)
			},
			OnDataPacket: func(packet lksdk.DataPacket, _ lksdk.DataReceiveParams) {
				user, ok := packet.(*lksdk.UserDataPacket)
				if !ok {
					return
				}
				agent.forwardToProtocol(ctx, user.Payload)
			},
		},
	}
	room, err := lksdk.ConnectToRoom(agent.config.URL, lksdk.ConnectInfo{
		APIKey: agent.config.APIKey, APISecret: agent.config.APISecret,
		RoomName: agent.config.Room, ParticipantIdentity: agent.config.Identity,
		ParticipantName: agent.config.Name, ParticipantKind: lksdk.ParticipantAgent,
	}, callback)
	if err != nil {
		return fmt.Errorf("join room %q: %w", agent.config.Room, err)
	}
	agent.room = room
	defer room.Disconnect()

	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "agent-audio",
	}); err != nil {
		return fmt.Errorf("publish the agent's audio: %w", err)
	}
	agent.config.Logf("agent %s joined %s", agent.config.Identity, agent.config.Room)

	go agent.pumpOutbound(ctx)
	select {
	case <-ctx.Done():
		return nil
	case <-agent.done:
		return nil
	}
}

// pumpInbound decodes a participant's audio and forwards it to the protocol.
//
// Room audio is Opus; the protocol connection carries mu-law. The decode is
// pure Go, which keeps this agent a static binary - the same trade the
// in-process adapter makes, for the same reason.
func (agent *Agent) pumpInbound(ctx context.Context, remote *pion.TrackRemote) {
	decoder, err := opus.NewDecoderWithOutput(8000, 1)
	if err != nil {
		agent.config.Logf("create the Opus decoder: %v", err)
		return
	}
	samples := make([]int16, 960)
	for {
		if agent.closed.Load() || ctx.Err() != nil {
			return
		}
		packet, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if len(packet.Payload) == 0 {
			continue
		}
		count, err := decoder.DecodeToInt16(packet.Payload, samples)
		if err != nil || count == 0 {
			continue
		}
		encoded := make([]byte, count)
		for index := 0; index < count; index++ {
			encoded[index] = LinearToMuLaw(int(samples[index]))
		}
		if err := agent.client.Send(ctx, map[string]any{
			"type":  "input_audio_buffer.append",
			"audio": base64.StdEncoding.EncodeToString(encoded),
		}); err != nil {
			agent.config.Logf("forward inbound audio: %v", err)
			return
		}
	}
}

// pumpOutbound routes protocol events: audio to the published track,
// everything else to the room's data channel unchanged.
func (agent *Agent) pumpOutbound(ctx context.Context) {
	defer agent.close()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-agent.client.Events():
			if !open {
				return
			}
			if event.Type == "response.output_audio.delta" {
				agent.playAudio(event.Raw)
				continue
			}
			agent.forwardToRoom(event.Raw)
		}
	}
}

func (agent *Agent) playAudio(raw []byte) {
	var decoded struct {
		Delta string `json:"delta"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return
	}
	payload, err := base64.StdEncoding.DecodeString(decoded.Delta)
	if err != nil || len(payload) == 0 {
		return
	}
	samplesPerPacket := int(8000 * agent.config.PacketDuration / time.Second)
	if samplesPerPacket <= 0 {
		samplesPerPacket = 160
	}
	for offset := 0; offset < len(payload); offset += samplesPerPacket {
		end := offset + samplesPerPacket
		if end > len(payload) {
			end = len(payload)
		}
		chunk := payload[offset:end]
		if err := agent.track.WriteSample(media.Sample{
			Data: chunk, Duration: time.Duration(len(chunk)) * time.Second / 8000,
		}); err != nil {
			agent.config.Logf("write audio sample: %v", err)
			return
		}
	}
}

func (agent *Agent) forwardToRoom(raw []byte) {
	if agent.room == nil {
		return
	}
	if err := agent.room.LocalParticipant.PublishDataPacket(
		lksdk.UserData(raw), lksdk.WithDataPublishReliable(true),
	); err != nil {
		agent.config.Logf("forward event to the room: %v", err)
	}
}

// forwardToProtocol sends a participant's event to the endpoint.
//
// Audio format changes are dropped for the same reason the in-process adapter
// drops them: the participant's audio is on a LiveKit track, so its opinion
// about the protocol connection's format would break the media path without
// meaning anything.
func (agent *Agent) forwardToProtocol(ctx context.Context, raw []byte) {
	if agent.closed.Load() {
		return
	}
	sanitised, err := StripAudioFormat(raw)
	if err != nil {
		agent.config.Logf("participant event: %v", err)
		return
	}
	if err := agent.client.Send(ctx, json.RawMessage(sanitised)); err != nil {
		agent.config.Logf("forward participant event: %v", err)
	}
}

func (agent *Agent) close() {
	agent.once.Do(func() {
		agent.closed.Store(true)
		close(agent.done)
	})
}
