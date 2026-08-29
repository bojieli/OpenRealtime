// Package ingress normalizes authenticated external content into typed graph
// observations and explicit retained-media requests. It is a gateway boundary,
// not a wire-protocol parser: transports translate their events before these
// ports and cannot choose provenance by setting an authority field.
package ingress

import (
	"errors"
	"fmt"
	"mime"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	"github.com/bojieli/OpenRealtime/perception"
)

const (
	userContentRuntimeID     = "builtin://openrealtime/elements/ingress.UserContent"
	ingressImplementationRev = "implementation:1"
	maximumIngressBytes      = 1 << 30
	maximumIdentifierBytes   = 256
	maximumReasonBytes       = 1024
)

var (
	userTextType       = element.Trigger(element.Named("ingress.UserText"))
	userImageType      = element.Trigger(element.Named("ingress.UserImage"))
	userFileType       = element.Trigger(element.Named("ingress.UserFile"))
	userAttachmentType = element.Trigger(element.Named("ingress.UserAttachment"))
	contentCancelType  = element.Interrupt(element.Named("ingress.ContentID"))
	observationType    = element.Revisions(
		element.Named("perception.Observation"), element.Named("perception.RevisionID"),
	)
	attachmentHandleType = element.Event(element.Named("media.AttachmentHandle"))
	ingressOutcomeType   = element.Event(element.Named("ingress.Outcome"))
)

func UserTextType() element.Type         { return userTextType.Clone() }
func UserImageType() element.Type        { return userImageType.Clone() }
func UserFileType() element.Type         { return userFileType.Clone() }
func UserAttachmentType() element.Type   { return userAttachmentType.Clone() }
func ContentCancelType() element.Type    { return contentCancelType.Clone() }
func ObservationType() element.Type      { return observationType.Clone() }
func AttachmentHandleType() element.Type { return attachmentHandleType.Clone() }
func OutcomeType() element.Type          { return ingressOutcomeType.Clone() }

// UserText is authenticated participant text. Authority is intentionally not
// a payload field: only a gateway edge selected by topology may feed this type.
type UserText struct {
	ContentID  string `json:"content_id,omitempty"`
	StreamID   string `json:"stream_id,omitempty"`
	Text       string `json:"text"`
	StableText string `json:"stable_text,omitempty"`
	Language   string `json:"language,omitempty"`
	Revision   uint64 `json:"revision"`
	Supersedes uint64 `json:"supersedes,omitempty"`
	Final      bool   `json:"final,omitempty"`
}

type UserImage struct {
	ContentID      string `json:"content_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	Caption        string `json:"caption,omitempty"`
	MIMEType       string `json:"mime_type"`
	Content        []byte `json:"-"`
	SHA256         string `json:"sha256,omitempty"`
	Width          int    `json:"width"`
	Height         int    `json:"height"`
	SourceRevision uint64 `json:"source_revision"`
	CapturedNS     uint64 `json:"captured_ns,omitempty"`
}

type UserFile struct {
	ContentID      string `json:"content_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	Name           string `json:"name"`
	Caption        string `json:"caption,omitempty"`
	MIMEType       string `json:"mime_type"`
	Content        []byte `json:"-"`
	SHA256         string `json:"sha256,omitempty"`
	SourceRevision uint64 `json:"source_revision"`
	CapturedNS     uint64 `json:"captured_ns,omitempty"`
}

// UserAttachment covers non-image, non-file content without weakening those
// two contracts. Kind remains media.AttachmentOther after retention.
type UserAttachment struct {
	ContentID      string `json:"content_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	Name           string `json:"name,omitempty"`
	Caption        string `json:"caption,omitempty"`
	MIMEType       string `json:"mime_type"`
	Content        []byte `json:"-"`
	SHA256         string `json:"sha256,omitempty"`
	SourceRevision uint64 `json:"source_revision"`
	CapturedNS     uint64 `json:"captured_ns,omitempty"`
}

type ContentCancel struct {
	ContentID string `json:"content_id,omitempty"`
	StreamID  string `json:"stream_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type OutcomeKind string

const (
	OutcomeSucceeded OutcomeKind = "succeeded"
	OutcomeCanceled  OutcomeKind = "canceled"
	OutcomeRefused   OutcomeKind = "refused"
	OutcomeFailed    OutcomeKind = "failed"
	OutcomeIgnored   OutcomeKind = "ignored"
)

type Outcome struct {
	Kind           OutcomeKind `json:"kind"`
	Operation      string      `json:"operation"`
	ContentID      string      `json:"content_id,omitempty"`
	StreamID       string      `json:"stream_id,omitempty"`
	SourceRevision uint64      `json:"source_revision,omitempty"`
	Handle         string      `json:"handle,omitempty"`
	Code           string      `json:"code,omitempty"`
	Message        string      `json:"message,omitempty"`
	FinishedNS     uint64      `json:"finished_ns,omitempty"`
}

type normalizedAttachment struct {
	contentID      string
	streamID       string
	name           string
	caption        string
	mimeType       string
	content        []byte
	sha256         string
	kind           mediaelements.AttachmentKind
	sourceRevision uint64
	capturedNS     uint64
	width          int
	height         int
}

func cloneUserImage(value UserImage) UserImage {
	return value
}

func cloneUserFile(value UserFile) UserFile {
	return value
}

func cloneUserAttachment(value UserAttachment) UserAttachment {
	return value
}

func observationPayload(payload any) (perception.Observation, bool) {
	switch value := payload.(type) {
	case perception.Observation:
		value.Media = slices.Clone(value.Media)
		return value, true
	case *perception.Observation:
		if value != nil {
			copy := *value
			copy.Media = slices.Clone(value.Media)
			return copy, true
		}
	}
	return perception.Observation{}, false
}

func canonicalID(value, field string) (string, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return "", fmt.Errorf("%s is required", field)
	}
	if value != trimmed {
		return "", fmt.Errorf("%s must not have surrounding whitespace", field)
	}
	if len(value) > maximumIdentifierBytes {
		return "", fmt.Errorf("%s exceeds %d bytes", field, maximumIdentifierBytes)
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("%s is not valid UTF-8", field)
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return "", fmt.Errorf("%s contains whitespace or control characters", field)
		}
	}
	return value, nil
}

func boundedReason(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	if len(value) <= maximumReasonBytes {
		return value
	}
	const suffix = "…"
	value = value[:maximumReasonBytes-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func canonicalMIMEType(value string) (string, error) {
	value = strings.TrimSpace(value)
	mediaType, parameters, err := mime.ParseMediaType(value)
	if err != nil || !strings.Contains(mediaType, "/") {
		if err == nil {
			err = errors.New("MIME type requires a type and subtype")
		}
		return "", fmt.Errorf("invalid MIME type %q: %w", value, err)
	}
	canonical := mime.FormatMediaType(strings.ToLower(mediaType), parameters)
	if canonical == "" {
		return "", fmt.Errorf("invalid MIME type %q", value)
	}
	return canonical, nil
}

func reportLiveResolution(reporter element.ResolutionReporter) error {
	artifact := liveidentity.Artifact{ID: userContentRuntimeID, Revision: ingressImplementationRev}
	// Typed ports are graph contracts, not provider capabilities. This pure
	// built-in therefore attests its exact artifact with an explicitly empty
	// provider-capability set.
	return liveidentity.Report(reporter, artifact, []element.CapabilityResolution{})
}
