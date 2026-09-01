package server

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/management"
)

func TestSourceReadingAPIRequiresIndependentRootScopedAuthority(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "graphs"), 0o700); err != nil {
		t.Fatal(err)
	}
	source := "graph server_read_β {\n}\n"
	if err := os.WriteFile(filepath.Join(root, "graphs", "agent-β.ortg"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	reading, err := management.NewRootedSourcePublisher(management.RootedSourcePublisherOptions{
		Root: root, RootIdentity: serverSourceRootIdentity,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reading.Close() })
	authority := management.NewCapabilityRegistry()
	readAccess, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.ReadSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	writeAccess, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.CreateSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authority, SourceReading: reading})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	input := management.SourceReadRequest{
		FormatVersion: management.SourceReadFormatVersion,
		RootIdentity:  serverSourceRootIdentity,
		Path:          "graphs/agent-β.ortg",
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	path := management.APIPrefix + "/authoring/read"
	for name, token := range map[string]string{"absent": "", "write-only": writeAccess.Token} {
		response := request(t, handler, http.MethodPost, path, token, body)
		if response.Code != http.StatusNotFound {
			t.Errorf("%s source read status = %d body=%s", name, response.Code, response.Body.String())
		}
	}
	response := request(t, handler, http.MethodPost, path, readAccess.Token, body)
	if response.Code != http.StatusOK {
		t.Fatalf("authorized source read = %d %s", response.Code, response.Body.String())
	}
	var result management.SourceReadResult
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil ||
		management.ValidateSourceReadResult(input, result) != nil || result.Source != source {
		t.Fatalf("source read result = %+v, %v", result, err)
	}
	if queried := request(t, handler, http.MethodPost, path+"?path=leak", readAccess.Token, body); queried.Code != http.StatusBadRequest {
		t.Fatalf("query-bearing source read = %d", queried.Code)
	}
	duplicate := []byte(strings.Replace(string(body), `"path":`, `"path":"other.ortg","path":`, 1))
	if malformed := request(t, handler, http.MethodPost, path, readAccess.Token, duplicate); malformed.Code != http.StatusBadRequest {
		t.Fatalf("duplicate-key source read = %d %s", malformed.Code, malformed.Body.String())
	}
	if wrongMethod := request(t, handler, http.MethodGet, path, readAccess.Token, nil); wrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("wrong source-read method = %d", wrongMethod.Code)
	}
	if write := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/write", readAccess.Token, body); write.Code != http.StatusNotFound {
		t.Fatalf("read-only bundle exposed source publication = %d", write.Code)
	}
}

type forgedSourceReader struct{}

func (forgedSourceReader) Read(
	_ context.Context, request management.SourceReadRequest,
) (management.SourceReadResult, error) {
	result, err := management.NewSourceReadResult(request, "graph forged {\n}\n")
	if err == nil {
		result.SourceDigest = "sha256:" + strings.Repeat("4", 64)
	}
	return result, err
}

func TestSourceReadingAPIRejectsForgedProviderResult(t *testing.T) {
	authority := management.NewCapabilityRegistry()
	access, err := authority.Issue(time.Minute, []management.Grant{{
		Operation: management.ReadSource, Resource: serverSourceRootIdentity,
	}})
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := NewBundle(BundleConfig{Authorizer: authority, SourceReading: forgedSourceReader{}})
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := bundle.Mount(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mounted.Close(context.Background()) })
	handler, err := HTTPHandler(mounted, "http")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(management.SourceReadRequest{
		FormatVersion: 1, RootIdentity: serverSourceRootIdentity, Path: "forged.ortg",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := request(t, handler, http.MethodPost,
		management.APIPrefix+"/authoring/read", access.Token, body)
	if response.Code != http.StatusConflict {
		t.Fatalf("forged source reader = %d %s", response.Code, response.Body.String())
	}
}

var _ management.SourceReading = forgedSourceReader{}
