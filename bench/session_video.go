package bench

import (
	"fmt"
	"net/http"
	"sync"
)

// SessionVideoCapture is one encoded frame successfully sent by the benchmark
// session driver. Data is an owned copy and may be retained or mutated by the
// callback without changing the protocol payload.
//
// WireTimestamp is the Unix millisecond timestamp placed in the frame's
// timestamp_ms wire field. EpisodeAtMS is measured from the environment-ready
// episode clock immediately after the successful wire send. MediaType is
// sniffed from Data and is currently restricted to image/jpeg or image/png.
type SessionVideoCapture struct {
	Source        string
	Width         int
	Height        int
	MediaType     string
	WireTimestamp int64
	EpisodeAtMS   float64
	Data          []byte
}

// sessionVideoCaptureSink serializes callbacks across all streams in one
// session. A capture plugin can therefore write one ordered artifact stream
// without acquiring a second lock merely because screen and camera are both
// enabled. EpisodeAtMS remains the authoritative cross-media ordering field.
type sessionVideoCaptureSink struct {
	mu      sync.Mutex
	capture func(SessionVideoCapture) error
}

func newSessionVideoCaptureSink(capture func(SessionVideoCapture) error) *sessionVideoCaptureSink {
	if capture == nil {
		return nil
	}
	return &sessionVideoCaptureSink{capture: capture}
}

func (sink *sessionVideoCaptureSink) emit(capture SessionVideoCapture) error {
	if sink == nil || sink.capture == nil {
		return nil
	}
	capture.Data = append([]byte(nil), capture.Data...)
	sink.mu.Lock()
	defer sink.mu.Unlock()
	return sink.capture(capture)
}

func sessionVideoMediaType(frame []byte) (string, error) {
	mediaType := http.DetectContentType(frame)
	switch mediaType {
	case "image/jpeg", "image/png":
		return mediaType, nil
	default:
		return "", fmt.Errorf("unsupported video frame media type %q; want image/jpeg or image/png", mediaType)
	}
}
