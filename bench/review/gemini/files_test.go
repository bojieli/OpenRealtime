package gemini

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/bench/review"
)

func oversizedFilesRequest(
	t testing.TB,
) (review.Request, review.PreparedRequest, [][]byte) {
	t.Helper()
	request, _, payloads := preparedMultimodalRequest(t)
	target := maximumInlineMediaBytes - len(payloads[1]) - len(payloads[2]) + 2
	wav := sizedGeminiWAV(t, target)
	path := filepath.Join(request.RootDirectory, request.Media[0].Path)
	if err := os.WriteFile(path, wav, 0o600); err != nil {
		t.Fatal(err)
	}
	request.Media[0].SHA256 = digest(wav)
	prepared, err := review.Prepare(request)
	if err != nil {
		t.Fatal(err)
	}
	payloads[0] = wav
	if !useFilesTransport(prepared) {
		t.Fatal("oversized Files fixture remained inline")
	}
	return request, prepared, payloads
}

func sizedGeminiWAV(t testing.TB, size int) []byte {
	t.Helper()
	if size < 44 || (size-44)%2 != 0 {
		t.Fatalf("invalid WAV fixture size %d", size)
	}
	payload := make([]byte, size)
	copy(payload[0:4], "RIFF")
	binary.LittleEndian.PutUint32(payload[4:8], uint32(size-8))
	copy(payload[8:12], "WAVE")
	copy(payload[12:16], "fmt ")
	binary.LittleEndian.PutUint32(payload[16:20], 16)
	binary.LittleEndian.PutUint16(payload[20:22], 1)
	binary.LittleEndian.PutUint16(payload[22:24], 1)
	binary.LittleEndian.PutUint32(payload[24:28], 24_000)
	binary.LittleEndian.PutUint32(payload[28:32], 48_000)
	binary.LittleEndian.PutUint16(payload[32:34], 2)
	binary.LittleEndian.PutUint16(payload[34:36], 16)
	copy(payload[36:40], "data")
	binary.LittleEndian.PutUint32(payload[40:44], uint32(size-44))
	return payload
}

type filesFixtureTransport struct {
	mu sync.Mutex

	prepared          review.PreparedRequest
	interactionRaw    []byte
	uploadURL         func(int) string
	mutateFile        func(int, *filesAPIFile)
	processingOrdinal int
	cleanupStatus     int
	cleanupType       string
	cleanupBody       []byte
	cancelInteraction context.CancelFunc

	starts       int
	finalizes    int
	interactions int
	readiness    int
	deletes      int
	interaction  []byte
}

func newFilesFixtureTransport(
	prepared review.PreparedRequest, interactionRaw []byte,
) *filesFixtureTransport {
	return &filesFixtureTransport{
		prepared: prepared, interactionRaw: interactionRaw,
		uploadURL: func(ordinal int) string {
			return filesUploadURL + "?upload_id=fixture-" + strconv.Itoa(ordinal) +
				"&upload_protocol=resumable"
		},
	}
}

func (transport *filesFixtureTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if request == nil || request.URL == nil {
		return nil, errors.New("fixture received nil request")
	}
	if strings.Contains(request.URL.String(), testAPIKey) {
		return nil, errors.New("credential crossed into a request URL")
	}
	path := request.URL.EscapedPath()
	command := request.Header.Get("X-Goog-Upload-Command")
	switch {
	case request.Method == http.MethodPost && path == "/upload/v1beta/files" && command == "start":
		return transport.start(request)
	case request.Method == http.MethodPost && path == "/upload/v1beta/files" && command == "upload, finalize":
		return transport.finalize(request)
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/v1beta/files/file-"):
		return transport.poll(request)
	case request.Method == http.MethodPost && path == "/v1beta/interactions":
		return transport.interact(request)
	case request.Method == http.MethodDelete && strings.HasPrefix(path, "/v1beta/files/file-"):
		return transport.delete(request)
	default:
		return nil, fmt.Errorf("unexpected Gemini fixture request %s %s command=%q",
			request.Method, request.URL.String(), command)
	}
}

func (transport *filesFixtureTransport) start(request *http.Request) (*http.Response, error) {
	transport.starts++
	ordinal := transport.starts
	if ordinal > len(transport.prepared.Media) {
		return nil, errors.New("too many upload starts")
	}
	if request.Header.Get("x-goog-api-key") != testAPIKey ||
		request.Header.Get("X-Goog-Upload-Protocol") != "resumable" ||
		request.Header.Get("X-Goog-Upload-Header-Content-Length") !=
			strconv.Itoa(len(transport.prepared.Media[ordinal-1].Bytes)) ||
		request.Header.Get("X-Goog-Upload-Header-Content-Type") !=
			transport.prepared.Media[ordinal-1].MediaType {
		return nil, errors.New("upload start headers drifted")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	expected, err := filesStartBody(transport.prepared, ordinal-1)
	if err != nil || !bytes.Equal(body, expected) {
		return nil, errors.New("upload start body drifted")
	}
	header := http.Header{}
	header.Add("X-Goog-Upload-URL", transport.uploadURL(ordinal))
	return &http.Response{
		StatusCode: http.StatusOK, Header: header,
		Body: io.NopCloser(bytes.NewReader(nil)),
	}, nil
}

func (transport *filesFixtureTransport) finalize(request *http.Request) (*http.Response, error) {
	transport.finalizes++
	ordinal, err := fixtureUploadOrdinal(request.URL)
	if err != nil || ordinal > len(transport.prepared.Media) {
		return nil, errors.New("upload finalize URL drifted")
	}
	media := transport.prepared.Media[ordinal-1]
	if request.Header.Get("x-goog-api-key") != "" || request.Header.Get("X-Goog-Upload-Offset") != "0" ||
		request.Header.Get("Content-Type") != media.MediaType || request.ContentLength != int64(len(media.Bytes)) {
		return nil, errors.New("upload finalize headers drifted")
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, request.Body)
	if err != nil || written != int64(len(media.Bytes)) ||
		fmt.Sprintf("sha256:%x", hasher.Sum(nil)) != media.SHA256 {
		return nil, errors.New("upload finalize bytes drifted")
	}
	file := fixtureFile(transport.prepared, ordinal, "ACTIVE")
	if transport.processingOrdinal == ordinal {
		file.State = "PROCESSING"
	}
	if transport.mutateFile != nil {
		transport.mutateFile(ordinal, &file)
	}
	payload, err := json.Marshal(filesAPIEnvelope{File: file})
	if err != nil {
		return nil, err
	}
	return jsonResponse(http.StatusOK, payload), nil
}

func (transport *filesFixtureTransport) poll(request *http.Request) (*http.Response, error) {
	if request.Header.Get("x-goog-api-key") != testAPIKey {
		return nil, errors.New("readiness credential header drifted")
	}
	ordinal, err := fixtureFileOrdinal(request.URL.EscapedPath())
	if err != nil {
		return nil, err
	}
	transport.readiness++
	payload, err := json.Marshal(fixtureFile(transport.prepared, ordinal, "ACTIVE"))
	if err != nil {
		return nil, err
	}
	return jsonResponse(http.StatusOK, payload), nil
}

func (transport *filesFixtureTransport) interact(request *http.Request) (*http.Response, error) {
	transport.interactions++
	if request.Header.Get("x-goog-api-key") != testAPIKey || request.Header.Get("Accept") != "application/json" ||
		request.Header.Get("Content-Type") != "application/json" {
		return nil, errors.New("interaction headers drifted")
	}
	body, err := io.ReadAll(request.Body)
	if err != nil {
		return nil, err
	}
	transport.interaction = bytes.Clone(body)
	var wire interactionRequest
	if err := json.Unmarshal(body, &wire); err != nil || len(wire.Input) != len(transport.prepared.Media)+2 {
		return nil, errors.New("interaction body drifted")
	}
	for index, block := range wire.Input[2:] {
		file := fixtureFile(transport.prepared, index+1, "ACTIVE")
		if block.Type != transport.prepared.Media[index].Kind || block.MediaType != file.MIMEType ||
			block.URI != file.URI || block.Data != "" {
			return nil, errors.New("interaction file block drifted")
		}
	}
	if transport.cancelInteraction != nil {
		transport.cancelInteraction()
		return nil, context.Canceled
	}
	return jsonResponse(http.StatusOK, transport.interactionRaw), nil
}

func (transport *filesFixtureTransport) delete(request *http.Request) (*http.Response, error) {
	if request.Header.Get("x-goog-api-key") != testAPIKey {
		return nil, errors.New("cleanup credential header drifted")
	}
	if _, err := fixtureFileOrdinal(request.URL.EscapedPath()); err != nil {
		return nil, err
	}
	transport.deletes++
	status := transport.cleanupStatus
	if status == 0 {
		status = http.StatusOK
	}
	body := []byte(`{}`)
	if status != http.StatusOK {
		body = []byte(`{"error":"cleanup failed"}`)
	}
	contentType := "application/json"
	if transport.cleanupBody != nil {
		body = bytes.Clone(transport.cleanupBody)
		contentType = transport.cleanupType
	}
	response := jsonResponse(status, body)
	response.Header.Set("Content-Type", contentType)
	return response, nil
}

func fixtureUploadOrdinal(value *url.URL) (int, error) {
	if value == nil || value.Query().Get("upload_protocol") != "resumable" {
		return 0, errors.New("invalid upload URL")
	}
	encoded := strings.TrimPrefix(value.Query().Get("upload_id"), "fixture-")
	ordinal, err := strconv.Atoi(encoded)
	if err != nil || ordinal < 1 {
		return 0, errors.New("invalid upload ordinal")
	}
	return ordinal, nil
}

func fixtureFileOrdinal(path string) (int, error) {
	encoded := strings.TrimPrefix(path, "/v1beta/files/file-")
	ordinal, err := strconv.Atoi(encoded)
	if err != nil || ordinal < 1 {
		return 0, errors.New("invalid file ordinal")
	}
	return ordinal, nil
}

func fixtureFile(prepared review.PreparedRequest, ordinal int, state string) filesAPIFile {
	media := prepared.Media[ordinal-1]
	name := "files/file-" + strconv.Itoa(ordinal)
	return filesAPIFile{
		Name: name, DisplayName: "fixture", MIMEType: media.MediaType,
		SizeBytes: strconv.Itoa(len(media.Bytes)), SHA256Hash: preparedDigestBase64(media.SHA256),
		URI: filesResourceURL + "/file-" + strconv.Itoa(ordinal), State: state, Source: "UPLOADED",
	}
}

func TestFilesSHA256HashUsesExactLiveV1BetaEncoding(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	digestValue := prepared.Media[0].SHA256
	encoded := preparedDigestBase64(digestValue)
	decoded, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 88 || len(decoded) != 64 || string(decoded) != strings.TrimPrefix(digestValue, "sha256:") ||
		canonicalFileDigest(encoded) != digestValue {
		t.Fatalf("Gemini Files digest encoding length=%d decoded=%d canonical=%q",
			len(encoded), len(decoded), canonicalFileDigest(encoded))
	}
	rawDigest, err := hex.DecodeString(strings.TrimPrefix(digestValue, "sha256:"))
	if err != nil {
		t.Fatal(err)
	}
	if canonicalFileDigest(base64.StdEncoding.EncodeToString(rawDigest)) != "" {
		t.Fatal("base64 of raw 32-byte digest was accepted as the live v1beta File encoding")
	}
}

func (transport *filesFixtureTransport) counts() (starts, finalizes, interactions, readiness, deletes int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.starts, transport.finalizes, transport.interactions, transport.readiness, transport.deletes
}

func TestFilesTransportRetainsExactOversizedMediaAndCleanupEvidence(t *testing.T) {
	request, prepared, payloads := oversizedFilesRequest(t)
	responseRaw := successfulInteraction(t, testAssessment)

	t.Run("exact evaluation bundle", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, responseRaw)
		client := &http.Client{Transport: transport}
		evaluation, err := review.Evaluate(t.Context(), openGeminiLease(t, client), request)
		if err != nil {
			t.Fatal(err)
		}
		starts, finalizes, interactions, readiness, deletes := transport.counts()
		if starts != 3 || finalizes != 3 || interactions != 1 || readiness != 0 || deletes != 3 {
			t.Fatalf("Files transport calls start=%d finalize=%d interaction=%d readiness=%d delete=%d",
				starts, finalizes, interactions, readiness, deletes)
		}
		if bytes.Contains(evaluation.ProviderRequest, []byte(testAPIKey)) ||
			bytes.Contains(evaluation.ProviderRequest, []byte("upload_id=")) ||
			bytes.Contains(transport.interaction, []byte(`"data"`)) {
			t.Fatal("retained Files transport leaked a credential/capability or inlined media")
		}
		var evidence filesTransportEvidence
		if err := decodeFilesJSON(evaluation.ProviderRequest, &evidence); err != nil {
			t.Fatal(err)
		}
		for index, cleanup := range evidence.Cleanup {
			if cleanup.Outcome != "deleted" || cleanup.Ordinal != index+1 {
				t.Fatalf("cleanup evidence %d = %+v", index, cleanup)
			}
		}

		directory := filepath.Join(t.TempDir(), "files-evaluation")
		options := review.EvaluationBundleOptions{Directory: directory}
		receipt, err := review.WriteEvaluationBundle(t.Context(), options, evaluation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := review.VerifyEvaluationBundle(t.Context(), options, receipt); err != nil {
			t.Fatal(err)
		}
		for index, expected := range payloads {
			retained, err := os.ReadFile(filepath.Join(directory, []string{
				"media-001.wav", "media-002.png", "media-003.mp4",
			}[index]))
			if err != nil || !bytes.Equal(retained, expected) {
				t.Fatalf("retained Files medium %d differs: %v", index, err)
			}
		}
	})

	t.Run("cleanup failure is evidence not evaluation failure", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, responseRaw)
		transport.cleanupStatus = http.StatusInternalServerError
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		response, err := plugin.Review(t.Context(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		var evidence filesTransportEvidence
		if err := decodeFilesJSON(response.Request, &evidence); err != nil {
			t.Fatal(err)
		}
		for _, cleanup := range evidence.Cleanup {
			if cleanup.Outcome != "status_error" || cleanup.Response.StatusCode != http.StatusInternalServerError {
				t.Fatalf("failed cleanup evidence = %+v", cleanup)
			}
		}
		mutated := evidence
		mutated.Uploads = append([]filesUploadEvidence(nil), evidence.Uploads...)
		mutated.Uploads[0].Start.RequestBody = `{"file":{"display_name":"forged"}}`
		payload, err := json.Marshal(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateRetainedFilesTransport(t.Context(), prepared, payload, response.Raw); err == nil {
			t.Fatal("tampered retained upload metadata was accepted")
		}
		if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("non-JSON cleanup failure is bounded evidence not evaluation failure", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, responseRaw)
		transport.cleanupStatus = http.StatusServiceUnavailable
		transport.cleanupType = "text/plain"
		transport.cleanupBody = []byte("temporarily unavailable")
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		response, err := plugin.Review(t.Context(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		var evidence filesTransportEvidence
		if err := decodeFilesJSON(response.Request, &evidence); err != nil {
			t.Fatal(err)
		}
		for _, cleanup := range evidence.Cleanup {
			if cleanup.Outcome != "response_discarded" ||
				cleanup.Response.StatusCode != http.StatusServiceUnavailable ||
				cleanup.Response.ContentType != "text/plain" ||
				cleanup.Response.Body != "" || cleanup.Response.BodySHA256 != digest(nil) {
				t.Fatalf("non-JSON cleanup evidence = %+v", cleanup)
			}
		}
		if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("unexpected successful cleanup body is exact invalid-response evidence", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, responseRaw)
		transport.cleanupBody = []byte(`{"unexpected":true}`)
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		response, err := plugin.Review(t.Context(), prepared)
		if err != nil {
			t.Fatal(err)
		}
		var evidence filesTransportEvidence
		if err := decodeFilesJSON(response.Request, &evidence); err != nil {
			t.Fatal(err)
		}
		for _, cleanup := range evidence.Cleanup {
			if cleanup.Outcome != "invalid_response" || cleanup.Response.StatusCode != http.StatusOK {
				t.Fatalf("invalid cleanup response evidence = %+v", cleanup)
			}
		}
		if err := plugin.VerifyResponse(t.Context(), prepared, response); err != nil {
			t.Fatal(err)
		}
		mutated := evidence
		mutated.Cleanup = append([]filesCleanupEvidence(nil), evidence.Cleanup...)
		mutated.Cleanup[0].Outcome = "status_error"
		payload, err := json.Marshal(mutated)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := validateRetainedFilesTransport(
			t.Context(), prepared, payload, response.Raw,
		); err == nil {
			t.Fatal("cleanup outcome inconsistent with HTTP status was accepted")
		}
	})

	t.Run("caller cancellation still deletes uploaded files", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		transport := newFilesFixtureTransport(prepared, responseRaw)
		transport.cancelInteraction = cancel
		plugin, err := newWithHTTPClient(testAPIKey, &http.Client{Transport: transport})
		if err != nil {
			t.Fatal(err)
		}
		defer plugin.Close()
		if _, err := plugin.Review(ctx, prepared); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Files review error = %v", err)
		}
		starts, finalizes, interactions, _, deletes := transport.counts()
		if starts != 3 || finalizes != 3 || interactions != 1 || deletes != 3 {
			t.Fatalf("canceled Files cleanup calls start=%d finalize=%d interaction=%d delete=%d",
				starts, finalizes, interactions, deletes)
		}
	})
}

func TestFilesUploadURLAndRemoteIdentityAreFailClosed(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	for _, test := range []struct {
		name   string
		values []string
	}{
		{"http", []string{"http://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=x"}},
		{"foreign host", []string{"https://example.com/upload/v1beta/files?upload_id=x"}},
		{"userinfo", []string{"https://user@generativelanguage.googleapis.com/upload/v1beta/files?upload_id=x"}},
		{"path prefix", []string{"https://generativelanguage.googleapis.com/upload/v1beta/filesevil?upload_id=x"}},
		{"credential query", []string{
			"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=" + testAPIKey,
		}},
		{"duplicate", []string{
			"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=x",
			"https://generativelanguage.googleapis.com/upload/v1beta/files?upload_id=y",
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls++
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"X-Goog-Upload-URL": append([]string(nil), test.values...)},
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			})}
			if _, _, err := uploadPreparedFile(
				t.Context(), prepared, 0, prepared.Media[0], testAPIKey, scanner, client,
			); err == nil || calls != 1 {
				t.Fatalf("unsafe upload URL error=%v calls=%d", err, calls)
			}
		})
	}

	t.Run("canonical digest mismatch is cleaned", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, successfulInteraction(t, testAssessment))
		transport.mutateFile = func(_ int, file *filesAPIFile) {
			file.SHA256Hash = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
		}
		_, _, err := uploadPreparedFile(t.Context(), prepared, 0, prepared.Media[0], testAPIKey,
			scanner, &http.Client{Transport: transport})
		if err == nil {
			t.Fatal("mismatched uploaded digest was accepted")
		}
		_, _, _, _, deletes := transport.counts()
		if deletes != 1 {
			t.Fatalf("canonical mismatched upload delete calls = %d, want 1", deletes)
		}
	})

	t.Run("uncanonical identity is never used as delete target", func(t *testing.T) {
		transport := newFilesFixtureTransport(prepared, successfulInteraction(t, testAssessment))
		transport.mutateFile = func(_ int, file *filesAPIFile) {
			file.Name = "files/../foreign"
		}
		_, _, err := uploadPreparedFile(t.Context(), prepared, 0, prepared.Media[0], testAPIKey,
			scanner, &http.Client{Transport: transport})
		if err == nil {
			t.Fatal("uncanonical uploaded identity was accepted")
		}
		_, _, _, _, deletes := transport.counts()
		if deletes != 0 {
			t.Fatalf("uncanonical upload triggered %d delete calls", deletes)
		}
	})

	t.Run("cleanup credential echo is discarded from evidence", func(t *testing.T) {
		file := fixtureFile(prepared, 1, "ACTIVE")
		client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodDelete || request.Header.Get("x-goog-api-key") != testAPIKey {
				return nil, errors.New("cleanup request drifted")
			}
			return jsonResponse(http.StatusOK, []byte(`{"echo":"`+testAPIKey+`"}`)), nil
		})}
		cleanup := (&filesExchangeState{files: []filesAPIFile{file}}).cleanup(
			t.Context(), prepared, testAPIKey, scanner, client,
		)
		if len(cleanup) != 1 || cleanup[0].Outcome != "response_discarded" ||
			cleanup[0].Response.Body != "" || cleanup[0].Response.BodySHA256 != digest(nil) {
			t.Fatalf("credential cleanup evidence = %+v", cleanup)
		}
		encoded, err := json.Marshal(cleanup)
		if err != nil || bytes.Contains(encoded, []byte(testAPIKey)) {
			t.Fatalf("credential escaped cleanup evidence: %v", err)
		}
	})
}

func TestFilesProcessingPollRetainsStableIdentity(t *testing.T) {
	_, prepared, _ := preparedMultimodalRequest(t)
	scanner, err := newJSONCredentialScanner(testAPIKey)
	if err != nil {
		t.Fatal(err)
	}
	defer scanner.wipe()
	transport := newFilesFixtureTransport(prepared, successfulInteraction(t, testAssessment))
	transport.processingOrdinal = 1
	file, evidence, err := uploadPreparedFile(t.Context(), prepared, 0, prepared.Media[0], testAPIKey,
		scanner, &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	if file.State != "ACTIVE" || len(evidence.Readiness) != 1 || evidence.Readiness[0].State != "ACTIVE" {
		t.Fatalf("processing recovery file=%+v readiness=%+v", file, evidence.Readiness)
	}
	state := &filesExchangeState{files: []filesAPIFile{file}}
	cleanup := state.cleanup(t.Context(), prepared, testAPIKey, scanner, &http.Client{Transport: transport})
	if len(cleanup) != 1 || cleanup[0].Outcome != "deleted" {
		t.Fatalf("processing recovery cleanup = %+v", cleanup)
	}
}
