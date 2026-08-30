package host

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var (
	benchmarkArtifact Artifact
	benchmarkDownload Download
	benchmarkStats    ResourceStoreStats
)

func BenchmarkArtifactPublish64KiBRevision(b *testing.B) {
	const bytes = 64 << 10
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: bytes, MaxTotalBytes: 4 * bytes,
	}, nil)
	input := ArtifactInput{ID: "chart", Title: "Chart", HTML: strings.Repeat("x", bytes)}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		artifact, err := store.Publish(context.Background(), input)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkArtifact = artifact
	}
}

func BenchmarkArtifactServe64KiBSandboxed(b *testing.B) {
	const bytes = 64 << 10
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: bytes, MaxTotalBytes: 4 * bytes,
	}, nil)
	if _, err := store.Publish(context.Background(), ArtifactInput{
		ID: "chart", Title: "Chart", HTML: strings.Repeat("x", bytes),
	}); err != nil {
		b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/client/v1/artifacts/chart", nil)
	request.SetPathValue("id", "chart")
	writer := &discardResourceWriter{header: make(http.Header)}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		store.serveHTTP(writer, request)
		if writer.bytes == 0 {
			b.Fatal("artifact handler wrote no response")
		}
		writer.bytes = 0
	}
}

func BenchmarkDownloadPublish64KiBRevision(b *testing.B) {
	const bytes = 64 << 10
	store := newDownloadStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: bytes, MaxTotalBytes: 4 * bytes,
	}, nil)
	input := DownloadInput{
		ID: "report", Filename: "report.bin", Content: []byte(strings.Repeat("x", bytes)),
	}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		download, err := store.Publish(context.Background(), input)
		if err != nil {
			b.Fatal(err)
		}
		benchmarkDownload = download
	}
}

func BenchmarkDownloadServe64KiBAttachment(b *testing.B) {
	const bytes = 64 << 10
	store := newDownloadStore(ResourceStoreLimits{
		MaxEntries: 4, MaxItemBytes: bytes, MaxTotalBytes: 4 * bytes,
	}, nil)
	if _, err := store.Publish(context.Background(), DownloadInput{
		ID: "report", Filename: "report.bin", Content: []byte(strings.Repeat("x", bytes)),
	}); err != nil {
		b.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/client/v1/downloads/report", nil)
	request.SetPathValue("id", "report")
	writer := &discardResourceWriter{header: make(http.Header)}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		store.serveHTTP(writer, request)
		if writer.bytes != bytes {
			b.Fatalf("download handler wrote %d bytes", writer.bytes)
		}
		writer.bytes = 0
	}
}

func BenchmarkArtifactBoundedEviction1KiB(b *testing.B) {
	const bytes = 1 << 10
	store := newArtifactStore(ResourceStoreLimits{
		MaxEntries: 32, MaxItemBytes: bytes, MaxTotalBytes: 32 * bytes,
	}, nil)
	inputs := make([]ArtifactInput, 64)
	for index := range inputs {
		inputs[index] = ArtifactInput{
			ID: fmt.Sprintf("item_%d", index), HTML: strings.Repeat("x", bytes),
		}
	}
	b.SetBytes(bytes)
	b.ReportAllocs()
	b.ResetTimer()
	for index := range b.N {
		if _, err := store.Publish(context.Background(), inputs[index%len(inputs)]); err != nil {
			b.Fatal(err)
		}
	}
	benchmarkStats = store.Stats()
}

type discardResourceWriter struct {
	header http.Header
	bytes  int
}

func (writer *discardResourceWriter) Header() http.Header { return writer.header }

func (*discardResourceWriter) WriteHeader(int) {}

func (writer *discardResourceWriter) Write(payload []byte) (int, error) {
	writer.bytes += len(payload)
	return len(payload), nil
}
