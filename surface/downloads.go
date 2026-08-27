package surface

import (
	"encoding/base64"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const DefaultMaxDownloadBytes = 8 << 20

// Download is a file the agent made available to the person. The bytes stay
// private to the store; tool results and the page receive only bounded
// metadata and a same-origin download route.
type Download struct {
	ID        string    `json:"id"`
	Filename  string    `json:"filename"`
	MediaType string    `json:"media_type"`
	Bytes     int       `json:"bytes"`
	Version   int       `json:"version"`
	UpdatedAt time.Time `json:"updated_at"`
}

type storedDownload struct {
	Download
	content []byte
}

// DownloadStore bounds and serves generated files from memory. It never
// exposes a workspace path, so a download URL cannot become an arbitrary file
// read primitive.
type DownloadStore struct {
	maxBytes int

	mu        sync.RWMutex
	downloads map[string]*storedDownload
	order     []string
}

func NewDownloadStore(maxBytes int) *DownloadStore {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxDownloadBytes
	}
	return &DownloadStore{maxBytes: maxBytes, downloads: make(map[string]*storedDownload)}
}

func (store *DownloadStore) MaxBytes() int { return store.maxBytes }

// Put stores UTF-8 text or decoded binary content. Exactly one representation
// is required, which avoids silently preferring one when a malformed tool call
// supplies both.
func (store *DownloadStore) Put(id, filename, mediaType, text, encoded string) (*Download, error) {
	id = strings.TrimSpace(id)
	if id == "" || !validArtifactID(id) {
		return nil, fmt.Errorf("download id %q must be 1-64 characters of letters, digits, dash, or underscore", id)
	}
	filename = strings.TrimSpace(filename)
	if filename == "" || filepath.Base(filename) != filename || strings.ContainsAny(filename, "\r\n\x00") {
		return nil, errors.New("a download filename must be one safe file name without a path")
	}
	mediaType = strings.TrimSpace(mediaType)
	if mediaType == "" {
		mediaType = "application/octet-stream"
	}
	parsed, _, err := mime.ParseMediaType(mediaType)
	if err != nil || !strings.Contains(parsed, "/") {
		return nil, fmt.Errorf("invalid media type %q", mediaType)
	}
	mediaType = parsed
	if (text == "") == (encoded == "") {
		return nil, errors.New("a download requires exactly one of text or base64")
	}
	content := []byte(text)
	if encoded != "" {
		content, err = base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, errors.New("download base64 is invalid")
		}
	}
	if len(content) == 0 {
		return nil, errors.New("a download cannot be empty")
	}
	if len(content) > store.maxBytes {
		return nil, fmt.Errorf("download is %d bytes and the limit is %d", len(content), store.maxBytes)
	}

	store.mu.Lock()
	defer store.mu.Unlock()
	entry, exists := store.downloads[id]
	if !exists {
		entry = &storedDownload{Download: Download{ID: id}}
		store.downloads[id] = entry
		store.order = append(store.order, id)
		for len(store.order) > maxArtifacts {
			delete(store.downloads, store.order[0])
			store.order = store.order[1:]
		}
	}
	entry.Filename = filename
	entry.MediaType = mediaType
	entry.Bytes = len(content)
	entry.Version++
	entry.UpdatedAt = time.Now()
	entry.content = append(entry.content[:0], content...)
	snapshot := entry.Download
	return &snapshot, nil
}

func (store *DownloadStore) Get(id string) (*Download, []byte, bool) {
	store.mu.RLock()
	defer store.mu.RUnlock()
	entry, exists := store.downloads[id]
	if !exists {
		return nil, nil, false
	}
	metadata := entry.Download
	return &metadata, append([]byte(nil), entry.content...), true
}

func (store *DownloadStore) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	download, content, exists := store.Get(request.PathValue("id"))
	if !exists {
		http.Error(writer, "no such download", http.StatusNotFound)
		return
	}
	writer.Header().Set("Content-Type", download.MediaType)
	writer.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{
		"filename": download.Filename,
	}))
	writer.Header().Set("Content-Length", fmt.Sprint(len(content)))
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Cache-Control", "no-store")
	_, _ = writer.Write(content)
}
