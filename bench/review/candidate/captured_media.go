package candidate

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maximumCapturedMediaBytes    = 128 << 20
	maximumCapturedArtifactBytes = 64 << 20
)

// CapturedMedia is an owned artifact emitted by an external harness that
// controls the Realtime client itself. It is the tau2-style counterpart to
// SessionAudioCapture: bytes are copied into the candidate plug-in, while the
// source checkout path is deliberately not part of retained evidence.
type CapturedMedia struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Role      string `json:"role"`
	MediaType string `json:"media_type"`
	Bytes     []byte `json:"-"`
}

// CapturedArtifact is a non-media companion emitted by an external harness,
// such as the exact simulation trace that explains a retained recording.
// It is deliberately distinct from Attempt.Context: context stays a small
// queryable envelope, while this payload is copied into durable evidence.
type CapturedArtifact struct {
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
	ContentType string `json:"content_type"`
	Bytes       []byte `json:"-"`
}

func (media CapturedMedia) validate() error {
	if media.Name == "" || len(media.Name) > 256 || !utf8.ValidString(media.Name) ||
		filepath.IsAbs(media.Name) || filepath.VolumeName(media.Name) != "" ||
		filepath.Base(media.Name) != media.Name ||
		filepath.Clean(media.Name) != media.Name || media.Name == "." || media.Name == ".." ||
		strings.ContainsAny(media.Name, `/\`) {
		return errors.New("candidate captured media name is invalid")
	}
	for _, value := range []string{media.Name, media.Kind, media.Role, media.MediaType} {
		if value == "" || strings.TrimSpace(value) != value || len(value) > 256 ||
			!utf8.ValidString(value) {
			return errors.New("candidate captured media identity is empty, noncanonical, or oversized")
		}
		for _, symbol := range value {
			if unicode.IsControl(symbol) {
				return errors.New("candidate captured media identity contains a control character")
			}
		}
	}
	switch media.Kind + "\x00" + media.MediaType {
	case "audio\x00audio/wav", "audio\x00audio/ogg", "audio\x00audio/m4a", "audio\x00audio/opus",
		"image\x00image/png", "image\x00image/jpeg", "image\x00image/gif",
		"video\x00video/mp4", "video\x00video/mov", "video\x00video/3gpp":
	default:
		return errors.New("candidate captured media type is unsupported")
	}
	if len(media.Bytes) == 0 || len(media.Bytes) > maximumCapturedMediaBytes {
		return errors.New("candidate captured media bytes are empty or oversized")
	}
	return nil
}

func cloneCapturedMedia(source CapturedMedia) CapturedMedia {
	result := source
	result.Bytes = slices.Clone(source.Bytes)
	return result
}

func (artifact CapturedArtifact) validate() error {
	if err := validateCapturedName(artifact.Name); err != nil {
		return err
	}
	for _, value := range []string{artifact.Name, artifact.Kind, artifact.Role, artifact.ContentType} {
		if err := validateCapturedIdentity(value); err != nil {
			return err
		}
	}
	switch artifact.Kind + "\x00" + artifact.ContentType {
	case "trace\x00application/json", "labels\x00text/plain; charset=utf-8",
		"wire_media\x00application/zip":
	default:
		return errors.New("candidate captured artifact type is unsupported")
	}
	if len(artifact.Bytes) == 0 || len(artifact.Bytes) > maximumCapturedArtifactBytes {
		return errors.New("candidate captured artifact bytes are empty or oversized")
	}
	return nil
}

func cloneCapturedArtifact(source CapturedArtifact) CapturedArtifact {
	result := source
	result.Bytes = slices.Clone(source.Bytes)
	return result
}

func validateCapturedName(name string) error {
	if name == "" || len(name) > 256 || !utf8.ValidString(name) ||
		filepath.IsAbs(name) || filepath.VolumeName(name) != "" ||
		filepath.Base(name) != name || filepath.Clean(name) != name ||
		name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return errors.New("candidate captured artifact name is invalid")
	}
	return nil
}

func validateCapturedIdentity(value string) error {
	if value == "" || strings.TrimSpace(value) != value || len(value) > 256 ||
		!utf8.ValidString(value) {
		return errors.New("candidate captured artifact identity is empty, noncanonical, or oversized")
	}
	for _, symbol := range value {
		if unicode.IsControl(symbol) {
			return errors.New("candidate captured artifact identity contains a control character")
		}
	}
	return nil
}

// CapturedMediaEvidence is the optional plug-in capability used only when an
// external benchmark harness owns the media transport and emits a completed
// artifact. Session-driven suites continue using CaptureAudio/CaptureVideo.
type CapturedMediaEvidence interface {
	CaptureMedia(CapturedMedia) error
}

// CapturedArtifactEvidence is the optional companion capability for exact
// non-media artifacts emitted by an external benchmark harness.
type CapturedArtifactEvidence interface {
	CaptureArtifact(CapturedArtifact) error
}

// CaptureMedia transfers an immutable copy of one external-harness artifact.
func (attempt *ActiveAttempt) CaptureMedia(media CapturedMedia) error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	if err := media.validate(); err != nil {
		return attempt.RecordFailure("capture media", err)
	}
	owned := cloneCapturedMedia(media)
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate active attempt is terminal")
	}
	if attempt.specification.MediaSource != MediaExternalHarness {
		err := errors.New("candidate captured media requires an external-harness attempt")
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture media", err))
		return err
	}
	importer, supported := attempt.sink.(CapturedMediaEvidence)
	if !supported || nilPlugin(importer) {
		err := errors.New("candidate evidence plug-in does not support captured media")
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture media", err))
		return err
	}
	err := importer.CaptureMedia(owned)
	if err != nil {
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture media", err))
	}
	return err
}

// CaptureArtifact transfers an immutable copy of one external-harness trace
// or label artifact without placing a large payload in Attempt.Context.
func (attempt *ActiveAttempt) CaptureArtifact(artifact CapturedArtifact) error {
	if attempt == nil {
		return errors.New("candidate active attempt is nil")
	}
	if err := artifact.validate(); err != nil {
		return attempt.RecordFailure("capture artifact", err)
	}
	owned := cloneCapturedArtifact(artifact)
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.terminal {
		return errors.New("candidate active attempt is terminal")
	}
	if attempt.specification.MediaSource != MediaExternalHarness {
		err := errors.New("candidate captured artifact requires an external-harness attempt")
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture artifact", err))
		return err
	}
	importer, supported := attempt.sink.(CapturedArtifactEvidence)
	if !supported || nilPlugin(importer) {
		err := errors.New("candidate evidence plug-in does not support captured artifacts")
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture artifact", err))
		return err
	}
	err := importer.CaptureArtifact(owned)
	if err != nil {
		attempt.failures = append(attempt.failures, StageError(attempt.caseID, "capture artifact", err))
	}
	return err
}
