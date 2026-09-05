package livekit

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"golang.org/x/image/draw"
	"golang.org/x/image/vp8"
)

// A stock room client publishes a screen share as a video track, and this
// agent used to be audio-only to it: video reached the engine only when a
// custom client sent protocol frames as data packets, which is not something
// a meeting client does. This bridges the track.
//
// It stays a protocol client. The bridge emits exactly the source declaration
// and frame events any client would send, only after the server has answered
// that it enabled video input, and inside the limits that answer declared.
// Nothing here can express anything a plain WebSocket client cannot.
//
// It bridges key frames, and only key frames, of a VP8 track. There is no
// pure-Go decoder for VP8 inter frames, and a cgo dependency on libvpx would
// cost this module the static binary it exists to keep; the key-frame decoder
// in golang.org/x/image builds everywhere. The engine's video observer samples
// at a few hertz and gates on pixel change, so a key frame at the negotiated
// cadence is the observation it wanted, and the inter frames in between are
// the ones it would have discarded. The agent asks the publisher for a key
// frame at that cadence with a picture-loss indication, which is what the
// room's own subscribers do.
//
// The protocol shapes are written here rather than imported for the same
// reason the client is (see protocol.go): an integration that ships separately
// should not depend on the server's internals, and two JSON objects are less
// coupling than a module edge.

// DefaultVideoKeyframeInterval is how often the agent asks a publisher for a
// key frame. One a second is a screen share's cadence and inside the shipped
// three-frame cap.
const DefaultVideoKeyframeInterval = time.Second

// videoJPEGQualities are tried in order until a frame fits the negotiated
// byte limit. A frame that cannot fit at the last is dropped rather than sent
// as something the server would refuse.
var videoJPEGQualities = []int{85, 70, 50, 30}

// videoLimits are the server's declared bounds on video input.
type videoLimits struct {
	Format        string `json:"format"`
	FPSCap        int    `json:"fps_cap"`
	MaxDimension  int    `json:"max_dimension"`
	MaxFrameBytes int    `json:"max_frame_bytes,omitempty"`
}

// defaultVideoLimits mirror the shipped server bounds and are what the agent
// conforms to until a session answer says otherwise.
func defaultVideoLimits() videoLimits {
	return videoLimits{Format: "jpeg", FPSCap: 3, MaxDimension: 1280, MaxFrameBytes: 4 << 20}
}

// videoNegotiation is what the server answered about video for this session.
type videoNegotiation struct {
	enabled bool
	limits  videoLimits
}

// readVideoNegotiation reads the extension answer out of a session event. A
// server that does not implement the extension never echoes the key, so its
// sessions read as video off, which is also what its client sees.
func readVideoNegotiation(raw []byte) (videoNegotiation, bool) {
	var envelope struct {
		Type    string `json:"type"`
		Session struct {
			Extension *struct {
				Enabled []string     `json:"enabled"`
				Video   *videoLimits `json:"video"`
			} `json:"openrealtime"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return videoNegotiation{}, false
	}
	if envelope.Type != "session.created" && envelope.Type != "session.updated" {
		return videoNegotiation{}, false
	}
	result := videoNegotiation{limits: defaultVideoLimits()}
	if envelope.Session.Extension == nil {
		return result, true
	}
	for _, feature := range envelope.Session.Extension.Enabled {
		if feature == "video.input" {
			result.enabled = true
		}
	}
	if envelope.Session.Extension.Video != nil {
		result.limits = *envelope.Session.Extension.Video
	}
	return result, true
}

// keyframeBridge turns decoded VP8 key frames into protocol video events. It
// knows nothing about RTP or rooms so that the decode, scale, encode, rate and
// declaration rules can be tested against bytes.
type keyframeBridge struct {
	source string
	emit   func(event any) error
	now    func() time.Time

	mu       sync.Mutex
	limits   videoLimits
	declared bool
	width    int
	height   int
	lastSent time.Time
}

func newKeyframeBridge(source string, limits videoLimits, emit func(any) error) *keyframeBridge {
	defaults := defaultVideoLimits()
	if limits.FPSCap <= 0 {
		limits.FPSCap = defaults.FPSCap
	}
	if limits.MaxDimension <= 0 {
		limits.MaxDimension = defaults.MaxDimension
	}
	return &keyframeBridge{source: source, emit: emit, now: time.Now, limits: limits}
}

// errNotKeyframe says the frame was an inter frame, which the bridge cannot
// decode and the observer would not have wanted.
var errNotKeyframe = errors.New("not a key frame")

// HandleFrame decodes one complete VP8 frame and emits it as protocol events
// when it is a key frame that fits the negotiated cadence, dimension and byte
// limit. It reports whether a frame event was emitted.
func (bridge *keyframeBridge) HandleFrame(frame []byte) (bool, error) {
	if len(frame) == 0 {
		return false, errors.New("empty frame")
	}
	if frame[0]&1 != 0 {
		return false, errNotKeyframe
	}
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	at := bridge.now()
	if interval := time.Second / time.Duration(bridge.limits.FPSCap); !bridge.lastSent.IsZero() &&
		at.Sub(bridge.lastSent) < interval {
		return false, nil
	}
	decoder := vp8.NewDecoder()
	decoder.Init(bytes.NewReader(frame), len(frame))
	header, err := decoder.DecodeFrameHeader()
	if err != nil {
		return false, fmt.Errorf("decode VP8 frame header: %w", err)
	}
	if !header.KeyFrame {
		return false, errNotKeyframe
	}
	decoded, err := decoder.DecodeFrame()
	if err != nil {
		return false, fmt.Errorf("decode VP8 key frame: %w", err)
	}
	picture := fitToDimension(decoded, bridge.limits.MaxDimension)
	encoded, err := encodeWithinLimit(picture, bridge.limits.MaxFrameBytes)
	if err != nil {
		return false, err
	}
	bounds := picture.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if !bridge.declared || width != bridge.width || height != bridge.height {
		if err := bridge.emit(map[string]any{
			"type": "openrealtime.input_video_source.update", "source": bridge.source,
			"state": "active", "width": width, "height": height,
		}); err != nil {
			return false, err
		}
		bridge.declared, bridge.width, bridge.height = true, width, height
	}
	if err := bridge.emit(map[string]any{
		"type": "openrealtime.input_video_frame.append", "source": bridge.source,
		"frame": base64.StdEncoding.EncodeToString(encoded), "timestamp_ms": at.UnixMilli(),
	}); err != nil {
		return false, err
	}
	bridge.lastSent = at
	return true, nil
}

// Close declares the source closed if it was ever declared, so the engine does
// not keep waiting on a participant who left.
func (bridge *keyframeBridge) Close() {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.declared {
		return
	}
	_ = bridge.emit(map[string]any{
		"type":   "openrealtime.input_video_source.update",
		"source": bridge.source, "state": "closed",
	})
	bridge.declared = false
}

// fitToDimension scales a frame whose long edge exceeds the negotiated
// maximum. A frame already inside the limit is returned as it is.
func fitToDimension(picture image.Image, maxDimension int) image.Image {
	bounds := picture.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	longest := max(width, height)
	if maxDimension <= 0 || longest <= maxDimension {
		return picture
	}
	scale := float64(maxDimension) / float64(longest)
	target := image.Rect(0, 0, max(1, int(float64(width)*scale+0.5)), max(1, int(float64(height)*scale+0.5)))
	scaled := image.NewRGBA(target)
	draw.ApproxBiLinear.Scale(scaled, target, picture, bounds, draw.Src, nil)
	return scaled
}

// encodeWithinLimit encodes a JPEG under the negotiated byte limit, lowering
// quality before giving up: a frame the server would refuse is not sent.
func encodeWithinLimit(picture image.Image, maxBytes int) ([]byte, error) {
	var buffer bytes.Buffer
	for _, quality := range videoJPEGQualities {
		buffer.Reset()
		if err := jpeg.Encode(&buffer, picture, &jpeg.Options{Quality: quality}); err != nil {
			return nil, fmt.Errorf("encode video frame: %w", err)
		}
		if maxBytes <= 0 || buffer.Len() <= maxBytes {
			return append([]byte(nil), buffer.Bytes()...), nil
		}
	}
	return nil, fmt.Errorf("video frame of %d bytes exceeds the negotiated %d byte limit at the lowest quality",
		buffer.Len(), maxBytes)
}

// videoSourceName names the protocol source a participant's track becomes.
// The publisher's identity is what a person reading an observation needs.
func videoSourceName(identity string, remote *pion.TrackRemote) string {
	for _, candidate := range []string{identity, remote.StreamID(), remote.ID()} {
		if trimmed := strings.TrimSpace(candidate); trimmed != "" {
			return "livekit:" + trimmed
		}
	}
	return "livekit:video"
}

// vp8MimeType is what the depacketizer here understands.
const vp8MimeType = "video/vp8"

// keyframeRequester asks a publisher for a key frame. The room's remote
// participant provides it; a test provides its own.
type keyframeRequester interface{ WritePLI(ssrc pion.SSRC) }

// pumpVideo bridges one subscribed video track for as long as it lasts.
func (agent *Agent) pumpVideo(
	ctx context.Context, remote *pion.TrackRemote, publisher keyframeRequester, identity string,
) {
	mime := strings.ToLower(remote.Codec().MimeType)
	if mime != vp8MimeType {
		agent.config.Logf(
			"video track from %s is %s; only VP8 key frames are bridged, so it is drained and ignored",
			identity, remote.Codec().MimeType)
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
		}
	}
	agent.config.Logf("bridging %s from %s as key frames", remote.Codec().MimeType, identity)
	bridge := newKeyframeBridge(videoSourceName(identity, remote), defaultVideoLimits(),
		func(event any) error { return agent.client.Send(ctx, event) })
	defer bridge.Close()

	builder := samplebuilder.New(64, &codecs.VP8Packet{}, remote.Codec().ClockRate)
	interval := agent.config.VideoKeyframeInterval
	if interval <= 0 {
		interval = DefaultVideoKeyframeInterval
	}
	stop := make(chan struct{})
	defer close(stop)
	go agent.requestKeyframes(remote, publisher, interval, stop)

	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if agent.closed.Load() || ctx.Err() != nil {
			return
		}
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			negotiation, known := agent.currentVideo()
			if !known || !negotiation.enabled {
				continue
			}
			bridge.mu.Lock()
			bridge.limits = negotiation.limits
			bridge.mu.Unlock()
			if _, err := bridge.HandleFrame(sample.Data); err != nil && !errors.Is(err, errNotKeyframe) {
				agent.config.Logf("bridge video frame: %v", err)
			}
		}
	}
}

// requestKeyframes asks the publisher for a key frame at the bridging cadence,
// but only while the session has video negotiated: a publisher asked for key
// frames nobody will decode is paying for nothing.
func (agent *Agent) requestKeyframes(
	remote *pion.TrackRemote, publisher keyframeRequester, interval time.Duration, stop <-chan struct{},
) {
	if publisher == nil {
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-agent.done:
			return
		case <-ticker.C:
		}
		if negotiation, known := agent.currentVideo(); !known || !negotiation.enabled {
			continue
		}
		publisher.WritePLI(remote.SSRC())
	}
}

// currentVideo reports what the server last answered about video.
func (agent *Agent) currentVideo() (videoNegotiation, bool) {
	agent.videoMu.Lock()
	defer agent.videoMu.Unlock()
	return agent.video, agent.videoKnown
}

// observeVideoNegotiation records the answer carried by a session event.
func (agent *Agent) observeVideoNegotiation(raw []byte) {
	negotiation, ok := readVideoNegotiation(raw)
	if !ok {
		return
	}
	agent.videoMu.Lock()
	agent.video, agent.videoKnown = negotiation, true
	agent.videoMu.Unlock()
}
