package surface_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bojieli/OpenRealtime/surface"
)

func TestPublishedDownloadIsBoundedSafeAndServedAsAnAttachment(t *testing.T) {
	store := surface.NewDownloadStore(64)
	file, err := store.Put("report", "report.csv", "text/csv; charset=utf-8", "a,b\n1,2\n", "")
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if file.Bytes != 8 || file.Version != 1 || file.MediaType != "text/csv" {
		t.Fatalf("unexpected metadata: %+v", file)
	}
	local := startSurface(t, surface.Config{
		Endpoint: newEndpoint(t).url(), Downloads: store,
	})
	response, err := http.Get(local.URL + "/downloads/report")
	if err != nil {
		t.Fatalf("download: %v", err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "a,b\n1,2\n" || !strings.Contains(response.Header.Get("Content-Disposition"), "report.csv") {
		t.Fatalf("unexpected attachment: %q %q", body, response.Header.Get("Content-Disposition"))
	}
	if response.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("a generated file must not be content-sniffed")
	}

	if _, err := store.Put("escape", "../secret", "text/plain", "x", ""); err == nil {
		t.Fatal("a download filename must not be a path")
	}
	if _, err := store.Put("large", "large.txt", "text/plain", strings.Repeat("x", 65), ""); err == nil {
		t.Fatal("the declared file bound must be enforced")
	}
}

func TestDownloadToolHandsMetadataToThePage(t *testing.T) {
	store := surface.NewDownloadStore(1024)
	local := startSurface(t, surface.Config{
		Endpoint: newEndpoint(t).url(), Downloads: store,
	})
	page := dialTools(t, local)
	page.call("call-download", "publish_download", map[string]any{
		"artifact_id": "notes", "filename": "notes.md", "media_type": "text/markdown",
		"text": "# Notes\n",
	})
	result := page.receive()
	if result["error"] != nil || result["channel"] != string(surface.ChannelDownload) {
		t.Fatalf("publishing must succeed on the download channel: %v", result)
	}
	download, ok := result["download"].(map[string]any)
	if !ok || download["filename"] != "notes.md" || download["bytes"].(float64) != 8 {
		t.Fatalf("the page needs safe download metadata: %v", result)
	}
	if _, content, exists := store.Get("notes"); !exists || string(content) != "# Notes\n" {
		t.Fatalf("unexpected stored file: exists=%t content=%q", exists, content)
	}
}
