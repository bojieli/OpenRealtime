package providers

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/binding/upstream"
	"github.com/bojieli/OpenRealtime/realtimeclient"
)

// UpstreamProbe is what one realtime endpoint did when actually contacted.
type UpstreamProbe struct {
	Provider string
	Dialect  Dialect
	URL      string
	Model    string
	// Connected reports that the socket opened and the endpoint accepted the
	// credential. It is the first thing that can go wrong and the first thing
	// worth reporting separately.
	Connected bool
	// Events counts each server event the endpoint sent, by name. This is the
	// evidence that the catalogue's spellings match reality: an endpoint that
	// sends response.audio.delta where the entry expects the current name
	// shows up here as an unaliased event.
	Events map[string]int
	// Spoken is what the endpoint said, if it got that far.
	Spoken string
	// Failure is why the probe stopped, or empty on success.
	Failure string
}

// probePrompt is deliberately trivial. The probe is checking that a socket
// opens, a credential is accepted, and events come back under the names the
// catalogue expects - not that the model is any good.
const probePrompt = "Reply with exactly: probe ok."

// ProbeUpstream contacts a realtime endpoint for real.
//
// It exists because a fake server built from a vendor's documentation proves
// only that this code does what the documentation was read to say. It cannot
// catch a document that is wrong, a spelling that changed, or a field a vendor
// quietly requires. So the same dial path the binding uses is pointed at the
// real endpoint and asked for one short turn, and what comes back is reported
// by name.
func ProbeUpstream(ctx context.Context, request UpstreamRequest, patience time.Duration) UpstreamProbe {
	result := UpstreamProbe{Provider: request.Provider, Events: map[string]int{}}
	settings, err := ResolveUpstream(request)
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	result.Dialect, result.URL, result.Model = settings.Dialect, settings.URL, settings.Model
	if patience <= 0 {
		patience = 45 * time.Second
	}
	probeContext, cancel := context.WithTimeout(ctx, patience)
	defer cancel()

	config := upstream.Config{
		URL: settings.URL, Token: settings.Token, Model: settings.Model,
		Header: settings.Header, EventAliases: settings.EventAliases,
	}
	var connection upstream.RemoteConn
	if settings.Dial != nil {
		connection, err = settings.Dial(probeContext, config)
	} else {
		connection, err = realtimeclient.Dial(probeContext, realtimeclient.Config{
			URL: config.URL, Token: config.Token, Model: config.Model,
			Header: config.Header, EventAliases: config.EventAliases,
		})
	}
	if err != nil {
		result.Failure = err.Error()
		return result
	}
	defer connection.Close()
	result.Connected = true

	send := []map[string]any{
		{"type": "session.update", "session": map[string]any{
			"type": "realtime", "instructions": "You are a probe. Answer in five words or fewer.",
		}},
		{"type": "conversation.item.create", "item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": probePrompt}},
		}},
		{"type": "response.create"},
	}
	for _, message := range send {
		if err := connection.Send(probeContext, message); err != nil {
			result.Failure = err.Error()
			return result
		}
	}

	var spoken strings.Builder
	for {
		select {
		case event, open := <-connection.Events():
			if !open {
				if err := connection.Err(); err != nil {
					result.Failure = err.Error()
				}
				result.Spoken = strings.TrimSpace(spoken.String())
				return result
			}
			result.Events[event.Type]++
			switch event.Type {
			case "response.output_audio_transcript.delta":
				spoken.WriteString(decodeField(event.Raw, "delta"))
			case "response.output_text.delta", "response.text.delta":
				spoken.WriteString(decodeField(event.Raw, "delta"))
			case "error":
				result.Failure = decodeError(event.Raw)
				result.Spoken = strings.TrimSpace(spoken.String())
				return result
			case "response.done":
				result.Spoken = strings.TrimSpace(spoken.String())
				// A turn can end without having said anything, and the reason
				// travels on this event. Reading only the text would report
				// the endpoint as reachable and silent - which is what a
				// probe is for, minus the one fact that explains it.
				if reason := incompleteReason(event.Raw); reason != "" && result.Spoken == "" {
					result.Failure = "the endpoint ended the turn without speaking: " + reason
				}
				return result
			}
		case <-probeContext.Done():
			result.Spoken = strings.TrimSpace(spoken.String())
			if result.Failure == "" && len(result.Events) == 0 {
				result.Failure = "the endpoint accepted the connection but sent nothing"
			}
			return result
		}
	}
}

func decodeField(raw []byte, field string) string {
	var decoded map[string]json.RawMessage
	if json.Unmarshal(raw, &decoded) != nil {
		return ""
	}
	var value string
	if json.Unmarshal(decoded[field], &value) != nil {
		return ""
	}
	return value
}

func decodeError(raw []byte) string {
	var decoded struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &decoded) != nil {
		return string(raw)
	}
	if decoded.Error.Code != "" {
		return fmt.Sprintf("%s: %s", decoded.Error.Code, decoded.Error.Message)
	}
	return decoded.Error.Message
}

// incompleteReason reads why a response stopped short, or empty when it did
// not. The status is the claim and status_details carries the cause; an
// endpoint that reports one without the other still gets named.
func incompleteReason(raw []byte) string {
	var decoded struct {
		Response struct {
			Status        string `json:"status"`
			StatusDetails struct {
				Type   string `json:"type"`
				Reason string `json:"reason"`
			} `json:"status_details"`
		} `json:"response"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	response := decoded.Response
	if response.Status == "" || response.Status == "completed" {
		return ""
	}
	if response.StatusDetails.Reason != "" {
		return response.Status + " (" + response.StatusDetails.Reason + ")"
	}
	return response.Status
}
