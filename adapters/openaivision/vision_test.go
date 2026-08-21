package openaivision_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/adapters/openaivision"
	"github.com/bojieli/OpenRealtime/perception"
)

func TestDescribeSendsPromptAndInlineImages(t *testing.T) {
	var captured map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		_ = json.Unmarshal(body, &captured)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"  A dialog is open.  "}}]}`))
	}))
	defer server.Close()

	client, err := openaivision.New(openaivision.Config{BaseURL: server.URL, Model: "vlm"})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	text, err := client.Describe(context.Background(), []perception.Image{
		{Bytes: []byte("jpegdata"), MIMEType: "image/jpeg", Width: 320, Height: 240},
	}, "describe this")
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if text != "A dialog is open." {
		t.Fatalf("unexpected narration %q", text)
	}
	messages := captured["messages"].([]any)
	parts := messages[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("expected a prompt and one image, got %d parts", len(parts))
	}
	if parts[0].(map[string]any)["text"] != "describe this" {
		t.Fatal("the prompt must be sent")
	}
	url := parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/jpeg;base64,") {
		t.Fatalf("the image must be inlined, got %q", url)
	}
}

func TestDescribeReportsProviderFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte(`{"error":{"message":"slow down"}}`))
	}))
	defer server.Close()
	client, _ := openaivision.New(openaivision.Config{BaseURL: server.URL, Model: "vlm"})
	_, err := client.Describe(context.Background(), []perception.Image{
		{Bytes: []byte("x"), MIMEType: "image/jpeg"},
	}, "describe")
	if err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected the provider status to be reported, got %v", err)
	}
}

func TestDescribeRejectsMalformedInput(t *testing.T) {
	client, _ := openaivision.New(openaivision.Config{BaseURL: "http://127.0.0.1:1", Model: "vlm"})
	if _, err := client.Describe(context.Background(), []perception.Image{{MIMEType: "image/jpeg"}}, "p"); err == nil {
		t.Fatal("an image with no bytes must be rejected before a request")
	}
	if _, err := client.Describe(context.Background(), []perception.Image{{Bytes: []byte("x"), MIMEType: "image/jpeg"}}, " "); err == nil {
		t.Fatal("a narration request needs a prompt")
	}
	text, err := client.Describe(context.Background(), nil, "p")
	if err != nil || text != "" {
		t.Fatalf("no images is not an error: %q %v", text, err)
	}
}

func TestNewRequiresAModel(t *testing.T) {
	if _, err := openaivision.New(openaivision.Config{}); err == nil {
		t.Fatal("expected a missing model to be rejected")
	}
}
