package providers_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/providers"
	"github.com/coder/websocket"
)

// An endpoint that ends a turn without speaking is reported with the reason.
//
// The probe exists to say what a real endpoint actually did, and reading only
// the text it produced would report this one as reachable and silent - which
// is the finding, minus the single fact that explains it. The reason travels
// on response.done, which the probe was ending on without looking inside.
func TestTheProbeSaysWhyATurnProducedNothing(t *testing.T) {
	endpoint := fakeRealtimeEndpoint(t, func(send func(map[string]any)) {
		send(map[string]any{"type": "response.created"})
		send(map[string]any{"type": "response.done", "response": map[string]any{
			"status":         "incomplete",
			"status_details": map[string]any{"type": "incomplete", "reason": "max_output_tokens"},
		}})
	})
	t.Setenv("OPENAI_API_KEY", "test-key")

	result := providers.ProbeUpstream(context.Background(), providers.UpstreamRequest{
		Provider: "openai", URL: endpoint,
	}, 10*time.Second)

	if !result.Connected {
		t.Fatalf("the probe never connected: %v", result.Failure)
	}
	if result.Spoken != "" {
		t.Fatalf("this endpoint said nothing, got %q", result.Spoken)
	}
	if !strings.Contains(result.Failure, "without speaking") {
		t.Fatalf("a silent turn must be reported as one, got %q", result.Failure)
	}
	if !strings.Contains(result.Failure, "max_output_tokens") {
		t.Fatalf("the report must carry the reason the endpoint gave, got %q", result.Failure)
	}
}

// An endpoint that answers is not reported as failing, which is the property
// that stops this from turning every probe into a complaint.
func TestTheProbeReportsNoFailureWhenTheEndpointSpeaks(t *testing.T) {
	endpoint := fakeRealtimeEndpoint(t, func(send func(map[string]any)) {
		send(map[string]any{
			"type": "response.output_audio_transcript.delta", "delta": "probe ok.",
		})
		send(map[string]any{"type": "response.done", "response": map[string]any{
			"status": "completed",
		}})
	})
	t.Setenv("OPENAI_API_KEY", "test-key")

	result := providers.ProbeUpstream(context.Background(), providers.UpstreamRequest{
		Provider: "openai", URL: endpoint,
	}, 10*time.Second)

	if result.Spoken != "probe ok." {
		t.Fatalf("spoken = %q", result.Spoken)
	}
	if result.Failure != "" {
		t.Fatalf("an endpoint that answered must not be reported as failing: %q", result.Failure)
	}
}

// fakeRealtimeEndpoint accepts the probe's turn and then runs the script.
func fakeRealtimeEndpoint(t *testing.T, script func(send func(map[string]any))) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			connection, err := websocket.Accept(writer, request, &websocket.AcceptOptions{
				CompressionMode: websocket.CompressionDisabled,
			})
			if err != nil {
				return
			}
			defer connection.CloseNow()
			ctx := request.Context()
			send := func(message map[string]any) {
				encoded, _ := json.Marshal(message)
				_ = connection.Write(ctx, websocket.MessageText, encoded)
			}
			// The probe sends session.update, the prompt, then response.create.
			// The script runs once it has asked for a response, which is the
			// only ordering that matters here.
			for {
				_, raw, err := connection.Read(ctx)
				if err != nil {
					return
				}
				var decoded map[string]any
				if json.Unmarshal(raw, &decoded) != nil {
					continue
				}
				if decoded["type"] == "response.create" {
					script(send)
					return
				}
			}
		}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}
