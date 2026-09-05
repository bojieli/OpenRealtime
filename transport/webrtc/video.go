package webrtc

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

	"github.com/pion/rtcp"
	"github.com/pion/rtp/codecs"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media/samplebuilder"
	"golang.org/x/image/draw"
	"golang.org/x/image/vp8"

	"github.com/bojieli/OpenRealtime/protocol/openrealtime"
)

// Video enters the engine as protocol events and nothing else: the
// descriptor-locked browser client encodes JPEG frames and sends them over the
// data channel, and a client that only publishes a video track used to be
// audio-only to this adapter. This bridges that track.
//
// It bridges key frames, and only key frames, of a VP8 track. There is no
// pure-Go decoder for VP8 inter frames, and taking a cgo dependency on libvpx
// would cost the static binary the same way libopus would (see codec.go); the
// key-frame decoder in golang.org/x/image is the one this project can build
// everywhere. The engine's own video observer samples at a few hertz and
// gates on pixel change, so a stream of key frames at the negotiated cap is
// the observation it wanted in the first place - the inter frames it cannot
// decode are the ones it would have discarded. The adapter asks the sender
// for a key frame at that cadence with an RTCP picture-loss indication, which
// every WebRTC sender honours.
//
// Nothing here can express anything a plain WebSocket client cannot: the
// bridge emits exactly the source declaration and frame events a client
// would, only after the client itself negotiated video input, and within the
// limits the server answered with. A client that never negotiated video gets
// an ordinary voice session and the track is drained and ignored.

// DefaultVideoKeyframeInterval is how often the adapter asks the sender for
// a key frame when the client did not configure it. One a second is a screen
// share's cadence and well inside the shipped three-frame cap.
const DefaultVideoKeyframeInterval = time.Second

// videoJPEGQualities are tried in order until a frame fits the negotiated
// byte limit. The last is the floor: a frame that cannot fit at it is dropped
// rather than sent as something the server would refuse.
var videoJPEGQualities = []int{85, 70, 50, 30}

// videoNegotiation is what the server answered about video for this session.
type videoNegotiation struct {
	enabled bool
	limits  openrealtime.Limits
}

// readVideoNegotiation reads the extension response out of a session.created
// or session.updated event. Absent, malformed, or base-protocol answers all
// mean video is not enabled, which is also what the client sees.
func readVideoNegotiation(raw []byte) (videoNegotiation, bool) {
	var envelope struct {
		Type    string `json:"type"`
		Session struct {
			Extension *openrealtime.Response `json:"openrealtime"`
		} `json:"session"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return videoNegotiation{}, false
	}
	if envelope.Type != "session.created" && envelope.Type != "session.updated" {
		return videoNegotiation{}, false
	}
	result := videoNegotiation{limits: openrealtime.DefaultLimits()}
	if envelope.Session.Extension == nil {
		return result, true
	}
	for _, feature := range envelope.Session.Extension.Enabled {
		if feature == openrealtime.FeatureVideoInput {
			result.enabled = true
		}
	}
	if envelope.Session.Extension.Video != nil {
		result.limits = *envelope.Session.Extension.Video
	}
	return result, true
}

// keyframeBridge turns decoded VP8 key frames into protocol video events. It
// knows nothing about RTP or peer connections so that the decode, scale,
// encode, rate, and declaration rules can be tested against bytes.
type keyframeBridge struct {
	source string
	emit   func(event any) error
	now    func() time.Time

	mu       sync.Mutex
	limits   openrealtime.Limits
	declared bool
	width    int
	height   int
	lastSent time.Time
}

func newKeyframeBridge(source string, limits openrealtime.Limits, emit func(any) error) *keyframeBridge {
	if limits.FPSCap <= 0 || limits.MaxDimension <= 0 {
		defaults := openrealtime.DefaultLimits()
		if limits.FPSCap <= 0 {
			limits.FPSCap = defaults.FPSCap
		}
		if limits.MaxDimension <= 0 {
			limits.MaxDimension = defaults.MaxDimension
		}
	}
	return &keyframeBridge{source: source, emit: emit, now: time.Now, limits: limits}
}

// errNotKeyframe says the frame was an inter frame, which the bridge cannot
// decode and the observer would not have wanted.
var errNotKeyframe = errors.New("not a key frame")

// HandleFrame decodes one complete VP8 frame and emits it as protocol events
// when it is a key frame that fits the negotiated cadence, dimension, and
// byte limit. It reports whether a frame event was emitted.
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
		if err := bridge.emit(openrealtime.VideoSourceUpdate{
			Type: openrealtime.EventVideoSourceUpdate, Source: bridge.source,
			State: openrealtime.SourceActive, Width: width, Height: height,
		}); err != nil {
			return false, err
		}
		bridge.declared, bridge.width, bridge.height = true, width, height
	}
	if err := bridge.emit(openrealtime.VideoFrameAppend{
		Type: openrealtime.EventVideoFrameAppend, Source: bridge.source,
		Frame: base64.StdEncoding.EncodeToString(encoded), TimestampMS: at.UnixMilli(),
	}); err != nil {
		return false, err
	}
	bridge.lastSent = at
	return true, nil
}

// Close declares the source closed if it was ever declared, so the engine
// does not keep waiting on a track that went away.
func (bridge *keyframeBridge) Close() {
	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	if !bridge.declared {
		return
	}
	_ = bridge.emit(openrealtime.VideoSourceUpdate{
		Type: openrealtime.EventVideoSourceUpdate, Source: bridge.source, State: openrealtime.SourceClosed,
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

// videoSourceName names the protocol source a track becomes. The stream the
// browser attached it to is the most meaningful name it carries.
func videoSourceName(remote *webrtc.TrackRemote) string {
	if name := strings.TrimSpace(remote.StreamID()); name != "" {
		return "webrtc:" + name
	}
	return "webrtc:video"
}

// vp8MimeType is what the depacketizer here understands.
const vp8MimeType = "video/vp8"

// pumpVideo bridges one inbound video track for as long as it lasts.
func (session *session) pumpVideo(remote *webrtc.TrackRemote) {
	mime := strings.ToLower(remote.Codec().MimeType)
	if mime != vp8MimeType {
		session.adapter.config.Logf(
			"video track %s is %s; only VP8 key frames are bridged, so it is drained and ignored",
			remote.ID(), remote.Codec().MimeType)
		for {
			if _, _, err := remote.ReadRTP(); err != nil {
				return
			}
		}
	}
	session.adapter.config.Logf("receiving %s as key frames", remote.Codec().MimeType)
	bridge := newKeyframeBridge(videoSourceName(remote), openrealtime.DefaultLimits(), session.emitProtocolEvent)
	defer bridge.Close()

	builder := samplebuilder.New(64, &codecs.VP8Packet{}, remote.Codec().ClockRate)
	interval := session.adapter.config.VideoKeyframeInterval
	if interval <= 0 {
		interval = DefaultVideoKeyframeInterval
	}
	stop := make(chan struct{})
	defer close(stop)
	go session.requestKeyframes(remote, interval, stop)

	for {
		packet, _, err := remote.ReadRTP()
		if err != nil {
			return
		}
		if session.closed.Load() {
			continue
		}
		builder.Push(packet)
		for sample := builder.Pop(); sample != nil; sample = builder.Pop() {
			negotiation, ok := session.currentVideo()
			if !ok || !negotiation.enabled {
				continue
			}
			bridge.mu.Lock()
			bridge.limits = negotiation.limits
			bridge.mu.Unlock()
			if _, err := bridge.HandleFrame(sample.Data); err != nil && !errors.Is(err, errNotKeyframe) {
				session.adapter.config.Logf("bridge video frame: %v", err)
			}
		}
	}
}

// requestKeyframes asks the sender for a key frame at the bridging cadence,
// but only while the session has video negotiated: a sender asked for key
// frames nobody will decode is paying for nothing.
func (session *session) requestKeyframes(remote *webrtc.TrackRemote, interval time.Duration, stop <-chan struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-session.done:
			return
		case <-ticker.C:
		}
		if negotiation, ok := session.currentVideo(); !ok || !negotiation.enabled {
			continue
		}
		if err := session.connection.WriteRTCP([]rtcp.Packet{
			&rtcp.PictureLossIndication{MediaSSRC: uint32(remote.SSRC())},
		}); err != nil {
			return
		}
	}
}

// emitProtocolEvent sends one bridge-produced event to the endpoint.
func (session *session) emitProtocolEvent(event any) error {
	if session.closed.Load() || session.client == nil {
		return errors.New("session is closed")
	}
	return session.client.Send(context.Background(), event)
}

// currentVideo reports what the server last answered about video.
func (session *session) currentVideo() (videoNegotiation, bool) {
	session.videoMu.Lock()
	defer session.videoMu.Unlock()
	return session.video, session.videoKnown
}

// observeVideoNegotiation records the answer carried by a session event.
func (session *session) observeVideoNegotiation(raw []byte) {
	negotiation, ok := readVideoNegotiation(raw)
	if !ok {
		return
	}
	session.videoMu.Lock()
	session.video, session.videoKnown = negotiation, true
	session.videoMu.Unlock()
}
