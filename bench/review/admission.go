package review

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"math"
	"path/filepath"
	"strconv"
	"unicode/utf8"
)

const (
	maximumPromptBytes                 = 8 << 20
	maximumSchemaBytes                 = 1 << 20
	maximumRootDirectoryBytes          = 4096
	maximumProviderImplementationBytes = 1 << 20
	maximumProviderConfigurationBytes  = 1 << 20
	maximumProviderRequestBytes        = 128 << 20
	maximumPreparedPublicBytes         = 16 << 20
	maximumNormalizedOutputBytes       = maximumResponseBytes
	maximumRecordBytes                 = maximumResponseBytes
	maximumSensitiveAggregateBytes     = 64 << 10
)

// preparedValidationSeal is an opaque capability issued only after PrepareContext
// has fully decoded and validated every retained medium. Exported copies retain
// the seal, but any mutation of public metadata or bytes changes the recomputed
// digest and therefore fails without repeating expensive media decoders.
type preparedValidationSeal struct {
	digest string
}

func preflightRequest(request Request) error {
	if len(request.AttemptID) == 0 || len(request.AttemptID) > 1024 ||
		len(request.Suite) == 0 || len(request.Suite) > 1024 ||
		len(request.Case) == 0 || len(request.Case) > 1024 {
		return errors.New("review request identity is empty or oversized")
	}
	if request.Trial <= 0 {
		return errors.New("review trial must be positive")
	}
	if len(request.RootDirectory) == 0 || len(request.RootDirectory) > maximumRootDirectoryBytes ||
		!utf8.ValidString(request.RootDirectory) || containsControl(request.RootDirectory) ||
		!filepath.IsAbs(request.RootDirectory) || filepath.Clean(request.RootDirectory) != request.RootDirectory {
		return errors.New("review root directory must be bounded canonical UTF-8 and a clean absolute path")
	}
	if len(request.Context) == 0 || len(request.Context) > maximumContextBytes {
		return fmt.Errorf("review context must be 1..%d bytes", maximumContextBytes)
	}
	if len(request.Media) == 0 || len(request.Media) > maximumMediaCount {
		return fmt.Errorf("review request needs 1..%d media artifacts", maximumMediaCount)
	}
	if len(request.SensitiveValues) > maximumSensitiveValues {
		return fmt.Errorf("review sensitive values exceed the %d-value limit", maximumSensitiveValues)
	}
	seen := make(map[string]struct{}, len(request.Media))
	seenDigests := make(map[string]struct{}, len(request.Media))
	for index, media := range request.Media {
		if media.SizeBytes != 0 {
			return fmt.Errorf("review media %d size is assigned only after verified loading", index)
		}
		if err := validateMediaIdentity(media); err != nil {
			return fmt.Errorf("review media %d: %w", index, err)
		}
		if !supportedReviewMedia(media.Kind, media.MediaType) {
			return fmt.Errorf("review media %d type %q has no bounded payload validator", index, media.MediaType)
		}
		if _, duplicate := seen[media.Path]; duplicate {
			return fmt.Errorf("review media repeats path %q", media.Path)
		}
		seen[media.Path] = struct{}{}
		if _, duplicate := seenDigests[media.SHA256]; duplicate {
			return fmt.Errorf("review media repeats content digest %q", media.SHA256)
		}
		seenDigests[media.SHA256] = struct{}{}
	}
	return nil
}

func supportedReviewMedia(kind, mediaType string) bool {
	switch kind + "\x00" + mediaType {
	case "audio\x00audio/wav", "audio\x00audio/ogg", "audio\x00audio/m4a", "audio\x00audio/opus",
		"image\x00image/png", "image\x00image/jpeg", "image\x00image/gif",
		"video\x00video/mp4", "video\x00video/mov", "video\x00video/3gpp":
		return true
	default:
		return false
	}
}

func preflightPrepared(prepared PreparedRequest, requireSeal bool) error {
	if len(prepared.AttemptID) == 0 || len(prepared.AttemptID) > 1024 ||
		len(prepared.Suite) == 0 || len(prepared.Suite) > 1024 ||
		len(prepared.Case) == 0 || len(prepared.Case) > 1024 {
		return errors.New("prepared review identity is empty or oversized")
	}
	if prepared.Trial <= 0 || len(prepared.Prompt) == 0 || len(prepared.Prompt) > maximumPromptBytes ||
		len(prepared.Schema) == 0 || len(prepared.Schema) > maximumSchemaBytes ||
		len(prepared.Context) == 0 || len(prepared.Context) > maximumContextBytes ||
		len(prepared.Media) == 0 || len(prepared.Media) > maximumMediaCount {
		return errors.New("prepared review scalar, media, prompt, schema, or context bounds are invalid")
	}
	if prepared.SensitiveValueCount < 0 || prepared.SensitiveValueCount > maximumSensitiveValues ||
		prepared.sensitiveGuard == nil || prepared.sensitiveGuard.count != prepared.SensitiveValueCount {
		return errors.New("prepared review sensitive-value capability is invalid")
	}
	if requireSeal && (prepared.validationSeal == nil || prepared.validationSeal.digest == "") {
		return errors.New("prepared review validation seal is missing")
	}
	if len(prepared.RequestFingerprint) != len("sha256:")+sha256.Size*2 {
		return errors.New("prepared review request fingerprint has an invalid size")
	}
	total := int64(0)
	seen := make(map[string]struct{}, len(prepared.Media))
	seenDigests := make(map[string]struct{}, len(prepared.Media))
	for index, item := range prepared.Media {
		if err := validateMediaIdentity(item.Media); err != nil {
			return fmt.Errorf("prepared review media %d: %w", index, err)
		}
		if !supportedReviewMedia(item.Kind, item.MediaType) {
			return fmt.Errorf("prepared review media %d type %q has no bounded payload validator", index, item.MediaType)
		}
		if item.Validation != MediaValidationVersion {
			return fmt.Errorf("prepared review media %d has invalid validation provenance", index)
		}
		if _, duplicate := seen[item.Path]; duplicate {
			return fmt.Errorf("prepared review repeats media path %q", item.Path)
		}
		seen[item.Path] = struct{}{}
		if _, duplicate := seenDigests[item.SHA256]; duplicate {
			return fmt.Errorf("prepared review repeats media content digest %q", item.SHA256)
		}
		seenDigests[item.SHA256] = struct{}{}
		if len(item.Bytes) == 0 || len(item.Bytes) > maximumMediaBytes {
			return fmt.Errorf("prepared review media %d byte size is invalid", index)
		}
		if item.SizeBytes != int64(len(item.Bytes)) {
			return fmt.Errorf("prepared review media %d retained size differs from its bytes", index)
		}
		if total > maximumMediaBytes-int64(len(item.Bytes)) {
			return fmt.Errorf("prepared review media exceeds %d total bytes", maximumMediaBytes)
		}
		total += int64(len(item.Bytes))
	}
	return nil
}

func preflightProviderResponse(response ProviderResponse) error {
	if len(response.Raw) == 0 || len(response.Raw) > maximumResponseBytes {
		return fmt.Errorf("review provider raw response must be 1..%d bytes", maximumResponseBytes)
	}
	if len(response.Output) == 0 || len(response.Output) > maximumResponseBytes {
		return fmt.Errorf("review provider output must be 1..%d bytes", maximumResponseBytes)
	}
	if len(response.Request) == 0 || len(response.Request) > maximumProviderRequestBytes {
		return fmt.Errorf("review provider request must be 1..%d bytes", maximumProviderRequestBytes)
	}
	if len(response.ReportedModel) == 0 || len(response.ReportedModel) > 256 ||
		len(response.RequestID) > 4096 || len(response.RequestIDState) > 16 {
		return errors.New("review provider response metadata is empty or oversized")
	}
	return nil
}

func sealPrepared(prepared PreparedRequest) (*preparedValidationSeal, error) {
	return sealPreparedContext(context.Background(), prepared)
}

func sealPreparedContext(
	ctx context.Context, prepared PreparedRequest,
) (*preparedValidationSeal, error) {
	if err := preflightPrepared(prepared, false); err != nil {
		return nil, err
	}
	digest, err := preparedSealDigestContext(ctx, prepared)
	if err != nil {
		return nil, err
	}
	return &preparedValidationSeal{digest: digest}, nil
}

func validatePreparedSeal(prepared PreparedRequest) error {
	return validatePreparedSealContext(context.Background(), prepared)
}

func validatePreparedSealContext(ctx context.Context, prepared PreparedRequest) error {
	if prepared.validationSeal == nil || prepared.validationSeal.digest == "" {
		return errors.New("prepared review validation seal is missing")
	}
	digest, err := preparedSealDigestContext(ctx, prepared)
	if err != nil {
		return err
	}
	if digest != prepared.validationSeal.digest {
		return errors.New("prepared review validation seal differs from its public inputs")
	}
	return nil
}

func preparedSealDigest(prepared PreparedRequest) (string, error) {
	return preparedSealDigestContext(context.Background(), prepared)
}

func preparedSealDigestContext(
	ctx context.Context, prepared PreparedRequest,
) (string, error) {
	if ctx == nil {
		return "", errors.New("prepared review seal requires a context")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	hasher := sha256.New()
	if err := writeSealStringContext(
		ctx, hasher, "openrealtime.prepared-review-validation.v2",
	); err != nil {
		return "", err
	}
	for _, value := range []string{
		prepared.AttemptID, prepared.Suite, prepared.Case,
		prepared.PromptVersion, prepared.Prompt, prepared.SchemaVersion,
		string(prepared.Schema), string(prepared.Context), prepared.RequestFingerprint,
		prepared.Sanitization,
	} {
		if err := writeSealStringContext(ctx, hasher, value); err != nil {
			return "", err
		}
	}
	writeSealInt(hasher, int64(prepared.Trial))
	writeSealInt(hasher, int64(prepared.SensitiveValueCount))
	writeSealInt(hasher, int64(len(prepared.Media)))
	for _, item := range prepared.Media {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		for _, value := range []string{
			item.Kind, item.Role, item.Path, item.SHA256, item.MediaType, item.Validation,
		} {
			if err := writeSealStringContext(ctx, hasher, value); err != nil {
				return "", err
			}
		}
		writeSealInt(hasher, int64(len(item.Bytes)))
		writeSealInt(hasher, item.SizeBytes)
		itemDigest, err := digestContext(ctx, item.Bytes)
		if err != nil {
			return "", err
		}
		if err := writeSealStringContext(ctx, hasher, itemDigest); err != nil {
			return "", err
		}
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), nil
}

func writeSealString(hasher hash.Hash, value string) {
	writeSealInt(hasher, int64(len(value)))
	_, _ = hasher.Write([]byte(value))
}

func writeSealStringContext(ctx context.Context, hasher hash.Hash, value string) error {
	writeSealInt(hasher, int64(len(value)))
	for offset := 0; offset < len(value); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(offset+(64<<10), len(value))
		_, _ = hasher.Write([]byte(value[offset:end]))
		offset = end
	}
	return ctx.Err()
}

func writeSealInt(hasher hash.Hash, value int64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], uint64(value))
	_, _ = hasher.Write(encoded[:])
}

func marshalCanonicalCompact(value any, maximum int) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	payload := bytes.TrimSuffix(output.Bytes(), []byte{'\n'})
	if len(payload) == 0 || len(payload) > maximum {
		return nil, fmt.Errorf("canonical JSON must be 1..%d bytes", maximum)
	}
	return append([]byte(nil), payload...), nil
}

func marshalCanonicalIndented(value any, maximum int) ([]byte, error) {
	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	if output.Len() == 0 || output.Len() > maximum {
		return nil, fmt.Errorf("canonical JSON must be 1..%d bytes", maximum)
	}
	return append([]byte(nil), output.Bytes()...), nil
}

func exactBase64EncodedLength(size int) (int, error) {
	if size < 0 || size > (math.MaxInt-2)/4*3 {
		return 0, errors.New("base64 input size overflows")
	}
	return ((size + 2) / 3) * 4, nil
}

func canonicalInteger(value int) string { return strconv.Itoa(value) }
