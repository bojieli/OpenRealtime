package gptlive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/httpclient"
)

// A sideband is a second socket onto a running session: it receives the
// session's events, copies of its audio with timestamps, and may send every
// command except audio. The vendor built it for a server that must watch and
// steer a conversation whose media goes elsewhere - a browser talking to the
// endpoint directly over WebRTC, or a telephone call the endpoint answered.
//
// For OpenRealtime that is the topology in which this process is not on the
// audio path. Everything the upstream binding does with a primary connection
// it can do with a sideband: mirror transcripts into the trajectory, run the
// reasoner when the voice delegates, hand answers back as commentary, push
// evidence as thinking. What it cannot do is hear the audio itself or fill
// the frame clock, and on a sideband it must not: the primary connection owns
// both.

// Attach opens a sideband onto a running session.
//
// The session is already running, so there is no handshake: nothing is held,
// nothing is started, and the first events arrive at once. Audio sent through
// this client is refused rather than forwarded, because the endpoint forbids
// it and the caller is better told than silently ignored.
func Attach(ctx context.Context, config Config, sessionID string) (*Client, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("attaching a sideband needs the session id to attach to")
	}
	if config.URL == "" {
		config.URL = DefaultURL
	}
	// The frame clock is the primary connection's job, and a sideband that
	// ran one would be sending audio it is forbidden to send.
	config.FrameInterval = -1
	config.URL = attachURL(config.URL, sessionID)
	client, err := prepare(config)
	if err != nil {
		return nil, err
	}
	client.sideband = true
	connection, err := client.dial(ctx, client.dialURL)
	if err != nil {
		return nil, err
	}
	client.writeMu.Lock()
	client.connection = connection
	client.sessionID = sessionID
	client.startSent, client.started = true, true
	client.writeMu.Unlock()
	go client.read(ctx)
	return client, nil
}

// attachURL is where a running session's sideband is opened.
func attachURL(base, sessionID string) string {
	return strings.TrimRight(base, "/") + "/" + sessionID + "/attach"
}

// Recording downloads a stored session's audio once the vendor has finalised
// it: stereo WAV, the caller in the left channel and the voice in the right.
//
// It needs a session opened with Store and closed, and a data policy that
// permits persistence; under Zero Data Retention there is nothing to download
// and the endpoint says so. The address is derived from the WebSocket one,
// because the vendor serves both from the same path.
func Recording(ctx context.Context, config Config, sessionID string) ([]byte, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, errors.New("downloading a recording needs the session id")
	}
	if config.URL == "" {
		config.URL = DefaultURL
	}
	target, err := contentURL(config.URL, sessionID)
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	for name, values := range config.Header {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(config.APIKey))
	httpClient := config.HTTPClient
	if httpClient == nil {
		// The shared client, not the default one: the default inherits a
		// two-connection idle pool per host, and this project bounds every
		// live HTTP path through one place.
		httpClient = httpclient.Shared()
	}
	response, err := httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("download recording %s: %w", sessionID, err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, fmt.Errorf("read recording %s: %w", sessionID, err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download recording %s: %s: %s", sessionID, response.Status,
			strings.TrimSpace(string(payload)))
	}
	if len(payload) < 12 || string(payload[:4]) != "RIFF" || string(payload[8:12]) != "WAVE" {
		return nil, fmt.Errorf("recording %s is not a WAV file (%d bytes)", sessionID, len(payload))
	}
	return payload, nil
}

// contentURL is the HTTP address of a session's recording, derived from the
// WebSocket address of the sessions endpoint.
func contentURL(base, sessionID string) (string, error) {
	parsed, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "wss":
		parsed.Scheme = "https"
	case "ws":
		parsed.Scheme = "http"
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + sessionID + "/content"
	parsed.RawQuery = ""
	return parsed.String(), nil
}
