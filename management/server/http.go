package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
	"github.com/bojieli/OpenRealtime/management"
	"github.com/bojieli/OpenRealtime/plugin"
	pluginruntime "github.com/bojieli/OpenRealtime/plugin/runtime"
)

const (
	defaultDeltaLimit = 256
	maxDeltaLimit     = 4096
	maxAuthoringBody  = 16 << 20
	maxReconcileBody  = 64 << 20
)

func lookupService[T any](services pluginruntime.Services, want plugin.Contract) (T, error) {
	var zero T
	value, contract, _, _, found := services.Lookup(want.Name)
	if !found || contract != want {
		return zero, fmt.Errorf("required management service %s is unavailable", want.Name)
	}
	typed, ok := value.(T)
	if !ok || nilInterface(typed) {
		return zero, fmt.Errorf("management service %s has the wrong Go type", want.Name)
	}
	return typed, nil
}

func authorize(request *http.Request, authorizer management.Authorizer, operation management.Operation, resource string) error {
	values := request.Header.Values(management.CapabilityHeader)
	if len(values) != 1 {
		return management.ErrUnauthorized
	}
	token := values[0]
	if token == "" || token != strings.TrimSpace(token) || strings.ContainsAny(token, "\x00\r\n") {
		return management.ErrUnauthorized
	}
	return authorizer.Authorize(request.Context(), management.AuthorizationRequest{
		Capability: token, Operation: operation, Resource: resource,
	})
}

func secureJSON(writer http.ResponseWriter) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Referrer-Policy", "no-referrer")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	secureJSON(writer)
	var payload bytes.Buffer
	encoder := json.NewEncoder(&payload)
	// This is an application/json endpoint with nosniff/CSP, never an inline
	// script. Avoid six-byte HTML escapes so the lossless schema-string wire
	// representation remains within its documented two-times expansion bound.
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		writer.WriteHeader(http.StatusInternalServerError)
		return
	}
	writer.WriteHeader(status)
	_, _ = writer.Write(payload.Bytes())
}

func writeServiceError(writer http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "management request failed"
	switch {
	case errors.Is(err, management.ErrUnauthorized), errors.Is(err, management.ErrNotFound):
		// Unauthorized and absent resources are intentionally indistinguishable.
		status, message = http.StatusNotFound, "not found"
	case errors.Is(err, management.ErrInvalid):
		status, message = http.StatusBadRequest, "invalid request"
	case errors.Is(err, management.ErrConflict):
		status, message = http.StatusConflict, "conflict"
	case errors.Is(err, management.ErrUnavailable):
		status, message = http.StatusServiceUnavailable, "service unavailable"
	}
	writeJSON(writer, status, map[string]string{"error": message})
}

func decodeStrictJSON(request *http.Request, maximum int64, destination any) error {
	contentType := request.Header.Get("Content-Type")
	if mediaType := strings.TrimSpace(strings.Split(contentType, ";")[0]); mediaType != "application/json" {
		return fmt.Errorf("%w: content type must be application/json", management.ErrInvalid)
	}
	payload, err := io.ReadAll(io.LimitReader(request.Body, maximum+1))
	if err != nil {
		return fmt.Errorf("%w: bounded request body: %v", management.ErrInvalid, err)
	}
	if int64(len(payload)) > maximum {
		return fmt.Errorf("%w: request body exceeds %d bytes", management.ErrInvalid, maximum)
	}
	if err := strictjson.Validate(payload); err != nil {
		return fmt.Errorf("%w: JSON: %v", management.ErrInvalid, err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: JSON shape: %v", management.ErrInvalid, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: trailing JSON value", management.ErrInvalid)
	}
	return nil
}

func parseUint(value, name string, fallback uint64, maximum uint64) (uint64, error) {
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed > maximum {
		return 0, fmt.Errorf("%w: %s is outside its bound", management.ErrInvalid, name)
	}
	return parsed, nil
}

func validateQuery(request *http.Request, allowed ...string) error {
	permitted := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		permitted[name] = struct{}{}
	}
	for name, values := range request.URL.Query() {
		if _, ok := permitted[name]; !ok || len(values) != 1 {
			return fmt.Errorf("%w: unexpected or repeated query parameter", management.ErrInvalid)
		}
	}
	return nil
}

func canonicalDigest(value string) bool {
	return management.CanonicalDigest(value)
}
