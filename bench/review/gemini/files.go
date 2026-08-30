package gemini

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/bojieli/OpenRealtime/bench/review"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	filesUploadURL                = "https://generativelanguage.googleapis.com/upload/v1beta/files"
	filesResourceURL              = "https://generativelanguage.googleapis.com/v1beta/files"
	filesTransportEvidenceFormat  = "openrealtime.gemini-files-transport.v1"
	filesTransportEvidenceVersion = 1
	maximumFilesResponseBytes     = 2 << 20
	maximumFilesMetadataBytes     = 2 << 20
	maximumFilePolls              = 300
	filePollInterval              = time.Second
	fileCleanupTimeout            = 30 * time.Second
	transportModeInline           = "inline"
	transportModeFiles            = "files"
)

type filesAPIEnvelope struct {
	File filesAPIFile `json:"file"`
}

// filesAPIFile lists every field in the v1beta File resource documented for
// the pinned API revision. Only the immutable identity fields are retained in
// review evidence; timestamps and generated metadata are admitted here so the
// strict decoder does not turn ordinary Google responses into schema drift.
type filesAPIFile struct {
	Name           string          `json:"name"`
	DisplayName    string          `json:"displayName,omitempty"`
	MIMEType       string          `json:"mimeType"`
	SizeBytes      string          `json:"sizeBytes"`
	CreateTime     string          `json:"createTime,omitempty"`
	UpdateTime     string          `json:"updateTime,omitempty"`
	ExpirationTime string          `json:"expirationTime,omitempty"`
	SHA256Hash     string          `json:"sha256Hash"`
	URI            string          `json:"uri"`
	DownloadURI    string          `json:"downloadUri,omitempty"`
	State          string          `json:"state"`
	Source         string          `json:"source,omitempty"`
	Error          json.RawMessage `json:"error,omitempty"`
	VideoMetadata  json.RawMessage `json:"videoMetadata,omitempty"`
}

type retainedFileIdentity struct {
	Name      string `json:"name"`
	URI       string `json:"uri"`
	MIMEType  string `json:"mime_type"`
	SizeBytes int64  `json:"size_bytes"`
	SHA256    string `json:"sha256"`
	State     string `json:"state"`
	Source    string `json:"source"`
}

type filesWireResponse struct {
	StatusCode  int    `json:"status_code"`
	ContentType string `json:"content_type"`
	Body        string `json:"body"`
	BodySHA256  string `json:"body_sha256"`
}

type filesUploadStartEvidence struct {
	Method              string            `json:"method"`
	URL                 string            `json:"url"`
	Protocol            string            `json:"upload_protocol"`
	Command             string            `json:"upload_command"`
	HeaderContentLength int64             `json:"header_content_length"`
	HeaderContentType   string            `json:"header_content_type"`
	ContentType         string            `json:"content_type"`
	RequestBody         string            `json:"request_body"`
	Response            filesWireResponse `json:"response"`
	UploadURLSHA256     string            `json:"upload_url_sha256"`
}

type filesUploadFinalizeEvidence struct {
	Method          string            `json:"method"`
	UploadURLSHA256 string            `json:"upload_url_sha256"`
	Offset          int64             `json:"upload_offset"`
	Command         string            `json:"upload_command"`
	ContentLength   int64             `json:"content_length"`
	ContentType     string            `json:"content_type"`
	BodySHA256      string            `json:"body_sha256"`
	Response        filesWireResponse `json:"response"`
}

type filesReadinessEvidence struct {
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Response filesWireResponse `json:"response"`
	State    string            `json:"state"`
}

type filesUploadEvidence struct {
	Ordinal   int                         `json:"ordinal"`
	Kind      string                      `json:"kind"`
	MediaType string                      `json:"media_type"`
	SizeBytes int64                       `json:"size_bytes"`
	SHA256    string                      `json:"sha256"`
	Start     filesUploadStartEvidence    `json:"start"`
	Finalize  filesUploadFinalizeEvidence `json:"finalize"`
	Readiness []filesReadinessEvidence    `json:"readiness"`
	File      retainedFileIdentity        `json:"file"`
}

type filesInteractionEvidence struct {
	Method               string            `json:"method"`
	URL                  string            `json:"url"`
	Accept               string            `json:"accept"`
	ContentType          string            `json:"content_type"`
	UserAgent            string            `json:"user_agent"`
	RequestBodySizeBytes int64             `json:"request_body_size_bytes"`
	RequestBodySHA256    string            `json:"request_body_sha256"`
	Response             filesWireResponse `json:"response"`
}

type filesCleanupEvidence struct {
	Ordinal  int               `json:"ordinal"`
	Name     string            `json:"name"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Outcome  string            `json:"outcome"`
	Response filesWireResponse `json:"response"`
}

type filesTransportEvidence struct {
	Format      string                   `json:"format"`
	Version     int                      `json:"version"`
	Mode        string                   `json:"mode"`
	Uploads     []filesUploadEvidence    `json:"uploads"`
	Interaction filesInteractionEvidence `json:"interaction"`
	Cleanup     []filesCleanupEvidence   `json:"cleanup"`
}

type filesExchangeState struct {
	uploads []filesUploadEvidence
	files   []filesAPIFile
}

func beginFilesTransport(
	ctx context.Context,
	prepared review.PreparedRequest,
	credential string,
	credentialScan *jsonCredentialScanner,
	httpClient *http.Client,
) (*filesExchangeState, []byte, error) {
	state, err := uploadPreparedFiles(ctx, prepared, credential, credentialScan, httpClient)
	if err != nil {
		return nil, nil, err
	}
	blocks := make([]contentBlock, len(state.files))
	for index, file := range state.files {
		blocks[index] = contentBlock{
			Type: prepared.Media[index].Kind, URI: file.URI,
			MediaType: prepared.Media[index].MediaType,
		}
	}
	body, err := marshalInteractionBlocksContext(ctx, prepared, blocks)
	if err != nil {
		state.cleanup(context.WithoutCancel(ctx), prepared, credential, credentialScan, httpClient)
		return nil, nil, err
	}
	return state, body, nil
}

func uploadPreparedFiles(
	ctx context.Context,
	prepared review.PreparedRequest,
	credential string,
	credentialScan *jsonCredentialScanner,
	httpClient *http.Client,
) (state *filesExchangeState, resultErr error) {
	state = &filesExchangeState{
		uploads: make([]filesUploadEvidence, 0, len(prepared.Media)),
		files:   make([]filesAPIFile, 0, len(prepared.Media)),
	}
	owned := state
	committed := false
	defer func() {
		if !committed && len(owned.files) > 0 {
			owned.cleanup(context.WithoutCancel(ctx), prepared, credential, credentialScan, httpClient)
		}
	}()
	for index, media := range prepared.Media {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		file, evidence, err := uploadPreparedFile(
			ctx, prepared, index, media, credential, credentialScan, httpClient,
		)
		if err != nil {
			return nil, err
		}
		state.files = append(state.files, file)
		state.uploads = append(state.uploads, evidence)
		if filesUploadsMetadataBytes(state.uploads) > maximumFilesMetadataBytes {
			return nil, errors.New("Gemini Files upload metadata exceeds its retained evidence bound")
		}
	}
	committed = true
	return state, nil
}

func uploadPreparedFile(
	ctx context.Context,
	prepared review.PreparedRequest,
	index int,
	media review.PreparedMedia,
	credential string,
	credentialScan *jsonCredentialScanner,
	httpClient *http.Client,
) (filesAPIFile, filesUploadEvidence, error) {
	startBody, err := filesStartBody(prepared, index)
	if err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("encode Gemini Files upload metadata")
	}
	if err := rejectFilesOutgoing(ctx, prepared, credentialScan, startBody); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	startRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, filesUploadURL, bytes.NewReader(startBody),
	)
	if err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("construct Gemini Files upload start request")
	}
	startRequest.Header.Set("Content-Type", "application/json")
	startRequest.Header.Set("x-goog-api-key", credential)
	startRequest.Header.Set("X-Goog-Upload-Protocol", "resumable")
	startRequest.Header.Set("X-Goog-Upload-Command", "start")
	startRequest.Header.Set("X-Goog-Upload-Header-Content-Length", strconv.FormatInt(int64(len(media.Bytes)), 10))
	startRequest.Header.Set("X-Goog-Upload-Header-Content-Type", media.MediaType)
	startRequest.Header.Set("User-Agent", "OpenRealtime-benchmark-review/1")
	startResponse, err := httpClient.Do(startRequest)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return filesAPIFile{}, filesUploadEvidence{}, cause
		}
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("call Gemini Files upload start failed")
	}
	startRaw, readErr := readAndCloseBounded(ctx, startResponse, maximumFilesResponseBytes, true)
	if readErr != nil {
		return filesAPIFile{}, filesUploadEvidence{}, readErr
	}
	if err := rejectFilesIncoming(ctx, prepared, credential, credentialScan, startRaw); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	if startResponse.StatusCode != http.StatusOK {
		return filesAPIFile{}, filesUploadEvidence{}, filesHTTPError("upload start", startResponse.StatusCode)
	}
	uploadURL, err := exactUploadURL(startResponse.Header)
	if err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	if err := rejectFilesOutgoing(ctx, prepared, credentialScan, []byte(uploadURL)); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	uploadURLSHA256 := digest([]byte(uploadURL))
	evidence := filesUploadEvidence{
		Ordinal: index + 1, Kind: media.Kind, MediaType: media.MediaType,
		SizeBytes: int64(len(media.Bytes)), SHA256: media.SHA256,
		Start: filesUploadStartEvidence{
			Method: http.MethodPost, URL: filesUploadURL,
			Protocol: "resumable", Command: "start",
			HeaderContentLength: int64(len(media.Bytes)), HeaderContentType: media.MediaType,
			ContentType: "application/json", RequestBody: string(startBody),
			Response:        filesResponseEvidence(startResponse, startRaw),
			UploadURLSHA256: uploadURLSHA256,
		},
	}

	finalizeRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, uploadURL, bytes.NewReader(media.Bytes),
	)
	if err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("construct Gemini Files upload finalize request")
	}
	finalizeRequest.Header.Set("Content-Type", media.MediaType)
	finalizeRequest.Header.Set("X-Goog-Upload-Offset", "0")
	finalizeRequest.Header.Set("X-Goog-Upload-Command", "upload, finalize")
	finalizeRequest.Header.Set("User-Agent", "OpenRealtime-benchmark-review/1")
	finalizeResponse, err := httpClient.Do(finalizeRequest)
	if err != nil {
		if cause := ctx.Err(); cause != nil {
			return filesAPIFile{}, filesUploadEvidence{}, cause
		}
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("call Gemini Files upload finalize failed")
	}
	finalizeRaw, readErr := readAndCloseBounded(ctx, finalizeResponse, maximumFilesResponseBytes, false)
	if readErr != nil {
		return filesAPIFile{}, filesUploadEvidence{}, readErr
	}
	if err := rejectFilesIncoming(ctx, prepared, credential, credentialScan, finalizeRaw); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	evidence.Finalize = filesUploadFinalizeEvidence{
		Method: http.MethodPost, UploadURLSHA256: uploadURLSHA256,
		Offset: 0, Command: "upload, finalize", ContentLength: int64(len(media.Bytes)),
		ContentType: media.MediaType, BodySHA256: media.SHA256,
		Response: filesResponseEvidence(finalizeResponse, finalizeRaw),
	}
	metadataBytes := int64(len(startRaw) + len(finalizeRaw))
	if metadataBytes > maximumFilesMetadataBytes {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New(
			"Gemini Files upload metadata exceeds its retained evidence bound")
	}
	if finalizeResponse.StatusCode != http.StatusOK {
		return filesAPIFile{}, filesUploadEvidence{}, filesHTTPError("upload finalize", finalizeResponse.StatusCode)
	}
	if err := requireJSONResponse(finalizeResponse, finalizeRaw, "upload finalize"); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	var envelope filesAPIEnvelope
	if err := decodeFilesJSON(finalizeRaw, &envelope); err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("decode Gemini Files upload response")
	}
	// Once Google has returned a canonical resource identity, this call owns a
	// remote file even when a later digest/state/readiness check fails. Keep that
	// identity only long enough to make a bounded best-effort DELETE. It is not
	// admitted to retained evidence until every exact-media check below passes.
	ownedFile := envelope.File
	cleanupOwned := validFileName(ownedFile.Name) && validFileURI(ownedFile.Name, ownedFile.URI)
	defer func() {
		if cleanupOwned {
			state := &filesExchangeState{files: []filesAPIFile{ownedFile}}
			state.cleanup(
				context.WithoutCancel(ctx), prepared, credential, credentialScan, httpClient,
			)
		}
	}()
	file, err := validateUploadedFile(ownedFile, media)
	if err != nil {
		return filesAPIFile{}, filesUploadEvidence{}, err
	}
	for file.State == "PROCESSING" {
		if len(evidence.Readiness) >= maximumFilePolls {
			return filesAPIFile{}, filesUploadEvidence{}, errors.New("Gemini Files processing exceeded its bounded poll budget")
		}
		timer := time.NewTimer(filePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return filesAPIFile{}, filesUploadEvidence{}, ctx.Err()
		case <-timer.C:
		}
		resourceURL := filesResourceURL + "/" + strings.TrimPrefix(file.Name, "files/")
		getRequest, requestErr := http.NewRequestWithContext(ctx, http.MethodGet, resourceURL, nil)
		if requestErr != nil {
			return filesAPIFile{}, filesUploadEvidence{}, errors.New("construct Gemini Files readiness request")
		}
		getRequest.Header.Set("x-goog-api-key", credential)
		getRequest.Header.Set("Accept", "application/json")
		getRequest.Header.Set("User-Agent", "OpenRealtime-benchmark-review/1")
		getResponse, requestErr := httpClient.Do(getRequest)
		if requestErr != nil {
			if cause := ctx.Err(); cause != nil {
				return filesAPIFile{}, filesUploadEvidence{}, cause
			}
			return filesAPIFile{}, filesUploadEvidence{}, errors.New("call Gemini Files readiness failed")
		}
		getRaw, requestErr := readAndCloseBounded(ctx, getResponse, maximumFilesResponseBytes, false)
		if requestErr != nil {
			return filesAPIFile{}, filesUploadEvidence{}, requestErr
		}
		if err := rejectFilesIncoming(ctx, prepared, credential, credentialScan, getRaw); err != nil {
			return filesAPIFile{}, filesUploadEvidence{}, err
		}
		if int64(len(getRaw)) > maximumFilesMetadataBytes-metadataBytes {
			return filesAPIFile{}, filesUploadEvidence{}, errors.New(
				"Gemini Files readiness metadata exceeds its retained evidence bound")
		}
		metadataBytes += int64(len(getRaw))
		poll := filesReadinessEvidence{
			Method: http.MethodGet, URL: resourceURL,
			Response: filesResponseEvidence(getResponse, getRaw),
		}
		if getResponse.StatusCode != http.StatusOK {
			return filesAPIFile{}, filesUploadEvidence{}, filesHTTPError("readiness", getResponse.StatusCode)
		}
		if err := requireJSONResponse(getResponse, getRaw, "readiness"); err != nil {
			return filesAPIFile{}, filesUploadEvidence{}, err
		}
		var refreshed filesAPIFile
		if err := decodeFilesJSON(getRaw, &refreshed); err != nil {
			return filesAPIFile{}, filesUploadEvidence{}, errors.New("decode Gemini Files readiness response")
		}
		refreshed, err = validateUploadedFile(refreshed, media)
		if err != nil || refreshed.Name != file.Name || refreshed.URI != file.URI {
			return filesAPIFile{}, filesUploadEvidence{}, errors.New("Gemini Files readiness identity drifted")
		}
		poll.State = refreshed.State
		evidence.Readiness = append(evidence.Readiness, poll)
		file = refreshed
	}
	if file.State != "ACTIVE" {
		return filesAPIFile{}, filesUploadEvidence{}, errors.New("Gemini Files upload did not become active")
	}
	evidence.File = retainFileIdentity(file)
	cleanupOwned = false
	return file, evidence, nil
}

func filesStartBody(prepared review.PreparedRequest, index int) ([]byte, error) {
	fingerprint := strings.TrimPrefix(prepared.RequestFingerprint, "sha256:")
	if len(fingerprint) < 16 || index < 0 || index >= len(prepared.Media) {
		return nil, errors.New("encode Gemini Files upload metadata: invalid prepared identity")
	}
	displayName := fmt.Sprintf("openrealtime-review-%s-%03d", fingerprint[:16], index+1)
	body, err := json.Marshal(map[string]any{
		"file": map[string]any{"display_name": displayName},
	})
	if err != nil {
		return nil, errors.New("encode Gemini Files upload metadata")
	}
	return body, nil
}

func filesUploadsMetadataBytes(uploads []filesUploadEvidence) int64 {
	total := int64(0)
	for _, upload := range uploads {
		total += int64(len(upload.Start.Response.Body) + len(upload.Finalize.Response.Body))
		for _, readiness := range upload.Readiness {
			total += int64(len(readiness.Response.Body))
		}
	}
	return total
}

func (state *filesExchangeState) cleanup(
	parent context.Context,
	prepared review.PreparedRequest,
	credential string,
	credentialScan *jsonCredentialScanner,
	httpClient *http.Client,
) []filesCleanupEvidence {
	if state == nil || len(state.files) == 0 {
		return nil
	}
	base := parent
	if base == nil {
		base = context.Background()
	}
	ctx, cancel := context.WithTimeout(base, fileCleanupTimeout)
	defer cancel()
	result := make([]filesCleanupEvidence, 0, len(state.files))
	for index, file := range state.files {
		resourceURL := filesResourceURL + "/" + strings.TrimPrefix(file.Name, "files/")
		item := filesCleanupEvidence{
			Ordinal: index + 1, Name: file.Name, Method: http.MethodDelete,
			URL: resourceURL, Outcome: "transport_error",
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodDelete, resourceURL, nil)
		if err != nil {
			result = append(result, item)
			continue
		}
		request.Header.Set("x-goog-api-key", credential)
		request.Header.Set("Accept", "application/json")
		request.Header.Set("User-Agent", "OpenRealtime-benchmark-review/1")
		response, err := httpClient.Do(request)
		if err != nil {
			result = append(result, item)
			continue
		}
		raw, err := readAndCloseBounded(ctx, response, maximumFilesResponseBytes, true)
		if err != nil {
			result = append(result, item)
			continue
		}
		item.Response = filesResponseEvidence(response, raw)
		if rejectFilesIncoming(ctx, prepared, credential, credentialScan, raw) != nil {
			item.Outcome = "response_discarded"
			item.Response.Body = ""
			item.Response.BodySHA256 = digest(nil)
			result = append(result, item)
			continue
		}
		if response.StatusCode != http.StatusOK {
			item.Outcome = "status_error"
			result = append(result, item)
			continue
		}
		if requireJSONResponse(response, raw, "cleanup") != nil || !bytes.Equal(bytes.TrimSpace(raw), []byte("{}")) {
			item.Outcome = "invalid_response"
			result = append(result, item)
			continue
		}
		item.Outcome = "deleted"
		result = append(result, item)
	}
	return result
}

func (state *filesExchangeState) retainedEvidence(
	interactionBody, interactionRaw []byte,
	interactionStatus int,
	interactionContentType string,
	cleanup []filesCleanupEvidence,
) ([]byte, error) {
	if state == nil {
		return nil, errors.New("retain Gemini Files transport: nil state")
	}
	evidence := filesTransportEvidence{
		Format: filesTransportEvidenceFormat, Version: filesTransportEvidenceVersion,
		Mode: transportModeFiles, Uploads: state.uploads, Cleanup: cleanup,
		Interaction: filesInteractionEvidence{
			Method: http.MethodPost, URL: interactionsURL, Accept: "application/json",
			ContentType: "application/json", UserAgent: "OpenRealtime-benchmark-review/1",
			RequestBodySizeBytes: int64(len(interactionBody)), RequestBodySHA256: digest(interactionBody),
			Response: filesWireResponse{
				StatusCode: interactionStatus, ContentType: interactionContentType,
				Body: "", BodySHA256: digest(interactionRaw),
			},
		},
	}
	payload, err := json.Marshal(evidence)
	if err != nil {
		return nil, errors.New("encode Gemini Files transport evidence")
	}
	if len(payload) == 0 || len(payload) > maximumInlineRequestBytes {
		return nil, errors.New("Gemini Files transport evidence is oversized")
	}
	return payload, nil
}

func validateRetainedFilesTransport(
	ctx context.Context,
	prepared review.PreparedRequest,
	payload []byte,
	interactionRaw []byte,
) ([]byte, error) {
	var evidence filesTransportEvidence
	if err := decodeFilesTransportJSON(payload, &evidence); err != nil {
		return nil, errors.New("Gemini retained Files transport evidence is invalid")
	}
	if evidence.Format != filesTransportEvidenceFormat ||
		evidence.Version != filesTransportEvidenceVersion || evidence.Mode != transportModeFiles ||
		len(evidence.Uploads) != len(prepared.Media) || len(evidence.Cleanup) != len(prepared.Media) ||
		filesUploadsMetadataBytes(evidence.Uploads) > maximumFilesMetadataBytes {
		return nil, errors.New("Gemini retained Files transport evidence has invalid identity or cardinality")
	}
	files := make([]filesAPIFile, len(evidence.Uploads))
	for index, item := range evidence.Uploads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		media := prepared.Media[index]
		expectedStartBody, err := filesStartBody(prepared, index)
		if err != nil || item.Start.RequestBody != string(expectedStartBody) {
			return nil, errors.New("Gemini retained Files upload metadata differs from prepared media")
		}
		if item.Ordinal != index+1 || item.Kind != media.Kind || item.MediaType != media.MediaType ||
			item.SizeBytes != int64(len(media.Bytes)) || item.SHA256 != media.SHA256 ||
			item.Start.Method != http.MethodPost || item.Start.URL != filesUploadURL ||
			item.Start.Protocol != "resumable" || item.Start.Command != "start" ||
			item.Start.HeaderContentLength != int64(len(media.Bytes)) ||
			item.Start.HeaderContentType != media.MediaType || item.Start.ContentType != "application/json" ||
			item.Start.Response.StatusCode != http.StatusOK || !validDigest(item.Start.UploadURLSHA256) ||
			item.Finalize.Method != http.MethodPost ||
			item.Finalize.UploadURLSHA256 != item.Start.UploadURLSHA256 ||
			item.Finalize.Offset != 0 || item.Finalize.Command != "upload, finalize" ||
			item.Finalize.ContentLength != int64(len(media.Bytes)) ||
			item.Finalize.ContentType != media.MediaType || item.Finalize.BodySHA256 != media.SHA256 ||
			item.Finalize.Response.StatusCode != http.StatusOK {
			return nil, errors.New("Gemini retained Files upload evidence differs from prepared media")
		}
		if !validWireResponse(item.Start.Response, true) || !validWireResponse(item.Finalize.Response, false) {
			return nil, errors.New("Gemini retained Files upload response evidence is invalid")
		}
		var finalized filesAPIEnvelope
		if decodeFilesJSON([]byte(item.Finalize.Response.Body), &finalized) != nil {
			return nil, errors.New("Gemini retained Files finalize response is invalid")
		}
		file, err := validateUploadedFile(finalized.File, media)
		if err != nil {
			return nil, err
		}
		for pollIndex, poll := range item.Readiness {
			if pollIndex >= maximumFilePolls || poll.Method != http.MethodGet ||
				poll.URL != filesResourceURL+"/"+strings.TrimPrefix(file.Name, "files/") ||
				poll.Response.StatusCode != http.StatusOK || !validWireResponse(poll.Response, false) {
				return nil, errors.New("Gemini retained Files readiness evidence is invalid")
			}
			var refreshed filesAPIFile
			if decodeFilesJSON([]byte(poll.Response.Body), &refreshed) != nil {
				return nil, errors.New("Gemini retained Files readiness response is invalid")
			}
			refreshed, err = validateUploadedFile(refreshed, media)
			if err != nil || refreshed.Name != file.Name || refreshed.URI != file.URI || refreshed.State != poll.State {
				return nil, errors.New("Gemini retained Files readiness identity drifted")
			}
			file = refreshed
		}
		if file.State != "ACTIVE" || item.File != retainFileIdentity(file) {
			return nil, errors.New("Gemini retained Files active identity is invalid")
		}
		files[index] = file
		cleanup := evidence.Cleanup[index]
		if cleanup.Ordinal != index+1 || cleanup.Name != file.Name || cleanup.Method != http.MethodDelete ||
			cleanup.URL != filesResourceURL+"/"+strings.TrimPrefix(file.Name, "files/") ||
			!validCleanupOutcome(cleanup) {
			return nil, errors.New("Gemini retained Files cleanup evidence is invalid")
		}
	}
	interaction := evidence.Interaction
	if interaction.Method != http.MethodPost || interaction.URL != interactionsURL ||
		interaction.Accept != "application/json" || interaction.ContentType != "application/json" ||
		interaction.UserAgent != "OpenRealtime-benchmark-review/1" ||
		interaction.RequestBodySizeBytes <= 0 || !validDigest(interaction.RequestBodySHA256) ||
		interaction.Response.StatusCode != http.StatusOK ||
		interaction.Response.ContentType != "application/json" ||
		interaction.Response.Body != "" || interaction.Response.BodySHA256 != digest(interactionRaw) {
		return nil, errors.New("Gemini retained Files interaction evidence is invalid")
	}
	blocks := make([]contentBlock, len(files))
	for index, file := range files {
		blocks[index] = contentBlock{
			Type: prepared.Media[index].Kind, URI: file.URI,
			MediaType: prepared.Media[index].MediaType,
		}
	}
	expected, err := marshalInteractionBlocksContext(ctx, prepared, blocks)
	if err != nil || interaction.RequestBodySizeBytes != int64(len(expected)) ||
		interaction.RequestBodySHA256 != digest(expected) {
		return nil, errors.New("Gemini retained Files interaction request differs from prepared media")
	}
	return expected, nil
}

func validCleanupOutcome(item filesCleanupEvidence) bool {
	switch item.Outcome {
	case "deleted":
		return item.Response.StatusCode == http.StatusOK && validWireResponse(item.Response, false) &&
			bytes.Equal(bytes.TrimSpace([]byte(item.Response.Body)), []byte("{}"))
	case "status_error":
		return item.Response.StatusCode != http.StatusOK && validBoundedWireResponse(item.Response)
	case "invalid_response":
		return item.Response.StatusCode == http.StatusOK && validBoundedWireResponse(item.Response) &&
			(item.Response.ContentType != "application/json" ||
				!bytes.Equal(bytes.TrimSpace([]byte(item.Response.Body)), []byte("{}")))
	case "transport_error":
		return item.Response == (filesWireResponse{})
	case "response_discarded":
		return item.Response.StatusCode > 0 && item.Response.StatusCode <= 599 &&
			item.Response.Body == "" && item.Response.BodySHA256 == digest(nil) &&
			len(item.Response.ContentType) <= 256
	default:
		return false
	}
}

func validBoundedWireResponse(response filesWireResponse) bool {
	return response.StatusCode > 0 && response.StatusCode <= 599 &&
		len(response.ContentType) <= 256 &&
		len(response.Body) <= maximumFilesResponseBytes &&
		response.BodySHA256 == digest([]byte(response.Body))
}

func validWireResponse(response filesWireResponse, allowEmpty bool) bool {
	if !validBoundedWireResponse(response) {
		return false
	}
	if response.Body == "" {
		return allowEmpty && response.ContentType == ""
	}
	return response.ContentType == "application/json" && json.Valid([]byte(response.Body))
}

func exactUploadURL(header http.Header) (string, error) {
	values := header.Values("X-Goog-Upload-URL")
	if len(values) != 1 || strings.TrimSpace(values[0]) != values[0] {
		return "", errors.New("Gemini Files upload start returned no exact upload URL")
	}
	candidate := values[0]
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "generativelanguage.googleapis.com" ||
		parsed.User != nil || parsed.Fragment != "" || parsed.Opaque != "" || parsed.RawPath != "" ||
		parsed.EscapedPath() != "/upload/v1beta/files" {
		return "", errors.New("Gemini Files upload start returned an invalid upload URL")
	}
	return candidate, nil
}

func validateUploadedFile(source filesAPIFile, media review.PreparedMedia) (filesAPIFile, error) {
	if !validFileName(source.Name) || !validFileURI(source.Name, source.URI) ||
		source.MIMEType != media.MediaType || source.SizeBytes != strconv.FormatInt(int64(len(media.Bytes)), 10) ||
		source.SHA256Hash != preparedDigestBase64(media.SHA256) ||
		(source.State != "ACTIVE" && source.State != "PROCESSING" && source.State != "FAILED") ||
		(source.Source != "" && source.Source != "UPLOADED") {
		return filesAPIFile{}, errors.New("Gemini Files upload response differs from exact prepared media")
	}
	if source.State == "FAILED" || len(source.Error) > 0 {
		return filesAPIFile{}, errors.New("Gemini Files upload processing failed")
	}
	return source, nil
}

func validFileName(name string) bool {
	if !strings.HasPrefix(name, "files/") {
		return false
	}
	id := strings.TrimPrefix(name, "files/")
	if len(id) == 0 || len(id) > 40 || id[0] == '-' || id[len(id)-1] == '-' {
		return false
	}
	for _, character := range []byte(id) {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func validFileURI(name, value string) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host != "generativelanguage.googleapis.com" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	return parsed.EscapedPath() == "/v1beta/files/"+strings.TrimPrefix(name, "files/")
}

func retainFileIdentity(file filesAPIFile) retainedFileIdentity {
	size, _ := strconv.ParseInt(file.SizeBytes, 10, 64)
	return retainedFileIdentity{
		Name: file.Name, URI: file.URI, MIMEType: file.MIMEType, SizeBytes: size,
		SHA256: "sha256:" + hex.EncodeToString(mustDecodeBase64(file.SHA256Hash)),
		State:  file.State, Source: file.Source,
	}
}

func preparedDigestBase64(value string) string {
	raw, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	if err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func mustDecodeBase64(value string) []byte {
	raw, _ := base64.StdEncoding.DecodeString(value)
	return raw
}

func readAndCloseBounded(
	ctx context.Context, response *http.Response, maximum int64, allowEmpty bool,
) ([]byte, error) {
	if response == nil || response.Body == nil {
		return nil, errors.New("Gemini Files API returned no response body")
	}
	raw, err := readFilesBounded(ctx, response.Body, maximum, allowEmpty)
	closeErr := response.Body.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, errors.New("close Gemini Files API response")
	}
	return raw, nil
}

func readFilesBounded(
	ctx context.Context, reader io.Reader, maximum int64, allowEmpty bool,
) ([]byte, error) {
	if ctx == nil || reader == nil || maximum <= 0 {
		return nil, errors.New("read Gemini Files response: invalid bounds")
	}
	var output bytes.Buffer
	chunk := make([]byte, 64<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		count, err := reader.Read(chunk)
		if cause := ctx.Err(); cause != nil {
			return nil, cause
		}
		if count > 0 {
			if int64(output.Len()) > maximum-int64(count) {
				return nil, fmt.Errorf("Gemini Files response must be 0..%d bytes", maximum)
			}
			_, _ = output.Write(chunk[:count])
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("read Gemini Files response")
		}
	}
	if output.Len() == 0 && !allowEmpty {
		return nil, fmt.Errorf("Gemini Files response must be 1..%d bytes", maximum)
	}
	return output.Bytes(), nil
}

func filesResponseEvidence(response *http.Response, raw []byte) filesWireResponse {
	contentType := ""
	if response != nil && len(raw) > 0 {
		contentType, _, _ = mime.ParseMediaType(response.Header.Get("Content-Type"))
	}
	status := 0
	if response != nil {
		status = response.StatusCode
	}
	return filesWireResponse{
		StatusCode: status, ContentType: contentType, Body: string(raw), BodySHA256: digest(raw),
	}
}

func requireJSONResponse(response *http.Response, raw []byte, operation string) error {
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" || len(raw) == 0 || !json.Valid(raw) {
		return fmt.Errorf("Gemini Files %s returned an invalid JSON response", operation)
	}
	return nil
}

func decodeFilesJSON(source []byte, destination any) error {
	return decodeFilesJSONWithMaximum(source, destination, maximumFilesResponseBytes)
}

func decodeFilesTransportJSON(source []byte, destination any) error {
	return decodeFilesJSONWithMaximum(source, destination, maximumInlineRequestBytes)
}

func decodeFilesJSONWithMaximum(source []byte, destination any, maximum int) error {
	work := int64(8 << 20)
	if maximum > maximumFilesResponseBytes {
		work = 64 << 20
	}
	if strictjson.ValidateWithLimits(source, strictjson.Limits{
		MaxInputBytes: maximum, MaxDepth: 32, MaxTokens: 2_000_000,
		MaxObjectMembers: 100_000, MaxArrayElements: 100_000, MaxKeyBytes: 4096,
		MaxTotalKeyBytes: 8 << 20, MaxWorkBytes: work,
	}) != nil {
		return errors.New("invalid strict JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(source))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}

func rejectFilesOutgoing(
	ctx context.Context,
	prepared review.PreparedRequest,
	credentialScan *jsonCredentialScanner,
	payload []byte,
) error {
	contains, err := prepared.ContainsDeclaredSensitiveLiteralContext(ctx, payload)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("Gemini Files request contains declared sensitive material and was discarded")
	}
	contains, err = credentialScan.containsLiteralContext(ctx, payload)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("Gemini Files request contains credential material and was discarded")
	}
	return nil
}

func rejectFilesIncoming(
	ctx context.Context,
	prepared review.PreparedRequest,
	credential string,
	credentialScan *jsonCredentialScanner,
	payload []byte,
) error {
	if len(payload) == 0 {
		return nil
	}
	contains, err := prepared.ContainsDeclaredSensitiveValueContext(ctx, payload)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("Gemini Files response contains declared sensitive material and was discarded")
	}
	if bytes.Contains(payload, []byte(credential)) {
		return errors.New("Gemini Files response contains credential material and was discarded")
	}
	contains, err = credentialScan.containsJSONContext(ctx, payload)
	if err != nil {
		return err
	}
	if contains {
		return errors.New("Gemini Files response contains credential material and was discarded")
	}
	return nil
}

func filesHTTPError(operation string, status int) error {
	return fmt.Errorf("Gemini Files %s returned HTTP %d", operation, status)
}
