package authority

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	effectReceiptVersion       = 1
	effectReceiptPrefix        = "ore1."
	effectReceiptKeyBytes      = 32
	effectReceiptNonceBytes    = 12
	defaultEffectReceiptTTL    = 30 * time.Second
	maximumEffectReceiptTTL    = 5 * time.Minute
	maximumEffectReceiptBytes  = 4096
	maximumEffectIdentityBytes = 256
	maximumEffectJSONDepth     = 256
)

var (
	// ErrEffectReceiptClosed means the deployment-owned key lifecycle ended.
	ErrEffectReceiptClosed = errors.New("client-effect receipt provider is closed")
	// ErrEffectReceiptInvalid deliberately covers syntax, key drift, and
	// cryptographic tampering without exposing which property of an opaque
	// receipt was useful to an attacker.
	ErrEffectReceiptInvalid = errors.New("client-effect receipt is invalid")
	// ErrEffectReceiptExpired is distinct so a caller can report an actionable
	// retry without retaining the receipt or its claims in evidence.
	ErrEffectReceiptExpired = errors.New("client-effect receipt expired")
	// ErrEffectReceiptMismatch means a valid receipt was replayed for a
	// different session, call, declaration, argument object, or target.
	ErrEffectReceiptMismatch = errors.New("client-effect receipt does not authorize this exact call")
)

// EffectReceiptClaims are the complete server-owned authority boundary for
// one client effect. They contain no model or client assertion of authority:
// only an EffectReceiptIssuer can seal them.
type EffectReceiptClaims struct {
	SessionID         string `json:"session_id"`
	CallID            string `json:"call_id"`
	Name              string `json:"name"`
	ArgumentsDigest   string `json:"arguments_digest"`
	DeclarationDigest string `json:"declaration_digest"`
	Target            string `json:"target"`
}

// EffectReceiptIssuer is the gateway seam. A deployment may replace the
// in-process sealed implementation with an HSM or remote authority service.
type EffectReceiptIssuer interface {
	IssueEffectReceipt(context.Context, EffectReceiptClaims) (string, error)
}

// EffectReceiptIssuerProvider is the lifecycle-aware gateway selection. A
// gateway offers client.effects only while Available reports true, so key
// revocation or provider loss cannot leave negotiation promising authority it
// can no longer issue.
type EffectReceiptIssuerProvider interface {
	EffectReceiptIssuer
	Available() bool
}

// EffectReceiptVerifier is the effect-host seam. Verification accepts the
// expected claims rather than returning ambient authority for a caller to
// interpret loosely.
type EffectReceiptVerifier interface {
	VerifyEffectReceipt(context.Context, string, EffectReceiptClaims) error
}

// EffectReceiptVerifierProvider is a lifecycle-aware verifier selected by a
// descriptor-locked host plugin. Identity is non-secret and must remain
// stable for the provider's lifetime; Close revokes all outstanding receipts.
type EffectReceiptVerifierProvider interface {
	EffectReceiptVerifier
	Identity() string
	Close() error
}

// EffectReceiptProvider is the common in-process deployment shape. Issuer and
// verifier remain separate interfaces so they can be deployed independently.
type EffectReceiptProvider interface {
	EffectReceiptIssuerProvider
	EffectReceiptVerifierProvider
}

// EffectReceiptOptions configures a sealed provider. Key material is always
// generated internally and is never returned. Clock and Random exist for
// deterministic conformance tests and replaceable platform entropy sources.
type EffectReceiptOptions struct {
	TTL    time.Duration
	Clock  func() time.Time
	Random io.Reader
}

// SealedEffectReceipts issues compact AES-256-GCM receipts. It stores only the
// raw key so Close can overwrite it; cipher schedules are created for one
// operation and discarded. No receipt or claim is retained after a call.
type SealedEffectReceipts struct {
	mu           sync.RWMutex
	key          [effectReceiptKeyBytes]byte
	noncePrefix  [4]byte
	nonceCounter atomic.Uint64
	identity     string
	ttl          time.Duration
	clock        func() time.Time
	closed       bool
}

type sealedEffectReceipt struct {
	Version     int                 `json:"version"`
	Issuer      string              `json:"issuer"`
	IssuedAtNS  int64               `json:"issued_at_ns"`
	ExpiresAtNS int64               `json:"expires_at_ns"`
	Claims      EffectReceiptClaims `json:"claims"`
}

var effectReceiptAdditionalData = []byte("openrealtime.client_effect.receipt/v1")

// NewSealedEffectReceipts creates one issuer/verifier key domain. The caller
// must close it; Close is idempotent and synchronizes with in-flight crypto.
func NewSealedEffectReceipts(options EffectReceiptOptions) (*SealedEffectReceipts, error) {
	ttl := options.TTL
	if ttl == 0 {
		ttl = defaultEffectReceiptTTL
	}
	if ttl <= 0 || ttl > maximumEffectReceiptTTL {
		return nil, fmt.Errorf("client-effect receipt TTL must be positive and at most %s", maximumEffectReceiptTTL)
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	random := options.Random
	if random == nil {
		random = rand.Reader
	}
	provider := &SealedEffectReceipts{ttl: ttl, clock: clock}
	if _, err := io.ReadFull(random, provider.key[:]); err != nil {
		zeroEffectReceiptBytes(provider.key[:])
		return nil, fmt.Errorf("generate client-effect receipt key: %w", err)
	}
	var identity [16]byte
	if _, err := io.ReadFull(random, identity[:]); err != nil {
		zeroEffectReceiptBytes(provider.key[:])
		zeroEffectReceiptBytes(identity[:])
		return nil, fmt.Errorf("generate client-effect receipt identity: %w", err)
	}
	provider.identity = "effect-receipts:" + hex.EncodeToString(identity[:])
	zeroEffectReceiptBytes(identity[:])
	if _, err := io.ReadFull(random, provider.noncePrefix[:]); err != nil {
		zeroEffectReceiptBytes(provider.key[:])
		zeroEffectReceiptBytes(provider.noncePrefix[:])
		return nil, fmt.Errorf("generate client-effect receipt nonce domain: %w", err)
	}
	return provider, nil
}

// Identity is a stable, non-secret provider identity suitable for live
// dependency drift checks. It is not a key fingerprint.
func (provider *SealedEffectReceipts) Identity() string {
	if provider == nil {
		return ""
	}
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return provider.identity
}

// Available reports whether this key domain can still issue and verify. It
// carries no receipt, identity, or key material.
func (provider *SealedEffectReceipts) Available() bool {
	if provider == nil {
		return false
	}
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	return !provider.closed
}

// IssueEffectReceipt seals one exact, short-lived call authorization.
func (provider *SealedEffectReceipts) IssueEffectReceipt(
	ctx context.Context, claims EffectReceiptClaims,
) (string, error) {
	if err := validateEffectReceiptClaims(claims); err != nil {
		return "", err
	}
	if err := contextCause(ctx); err != nil {
		return "", err
	}
	if provider == nil {
		return "", ErrEffectReceiptClosed
	}
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	if provider.closed {
		return "", ErrEffectReceiptClosed
	}
	now := provider.clock().UTC()
	expires := now.Add(provider.ttl)
	if expires.Before(now) {
		return "", errors.New("client-effect receipt expiry overflow")
	}
	payload, err := json.Marshal(sealedEffectReceipt{
		Version: effectReceiptVersion, Issuer: provider.identity,
		IssuedAtNS: now.UnixNano(), ExpiresAtNS: expires.UnixNano(), Claims: claims,
	})
	if err != nil {
		return "", fmt.Errorf("encode client-effect receipt: %w", err)
	}
	defer zeroEffectReceiptBytes(payload)
	aead, err := provider.aeadLocked()
	if err != nil {
		return "", err
	}
	nonce := make([]byte, effectReceiptNonceBytes)
	copy(nonce, provider.noncePrefix[:])
	counter, ok := provider.nextNonceCounter()
	if !ok {
		return "", errors.New("client-effect receipt nonce domain exhausted")
	}
	binary.BigEndian.PutUint64(nonce[len(provider.noncePrefix):], counter)
	sealed := make([]byte, 0, len(nonce)+len(payload)+aead.Overhead())
	sealed = append(sealed, nonce...)
	sealed = aead.Seal(sealed, nonce, payload, effectReceiptAdditionalData)
	runtime.KeepAlive(provider)
	if err := contextCause(ctx); err != nil {
		zeroEffectReceiptBytes(sealed)
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(sealed)
	zeroEffectReceiptBytes(sealed)
	return effectReceiptPrefix + encoded, nil
}

func (provider *SealedEffectReceipts) nextNonceCounter() (uint64, bool) {
	for {
		current := provider.nonceCounter.Load()
		if current == ^uint64(0) {
			return 0, false
		}
		if provider.nonceCounter.CompareAndSwap(current, current+1) {
			return current + 1, true
		}
	}
}

// VerifyEffectReceipt authenticates, expires, and exact-matches one opaque
// receipt. A receipt for the same call may be retransmitted; irreversible
// duplicate suppression remains the effect host's idempotency responsibility.
func (provider *SealedEffectReceipts) VerifyEffectReceipt(
	ctx context.Context, receipt string, expected EffectReceiptClaims,
) error {
	if err := validateEffectReceiptClaims(expected); err != nil {
		return err
	}
	if err := contextCause(ctx); err != nil {
		return err
	}
	if provider == nil {
		return ErrEffectReceiptClosed
	}
	if len(receipt) <= len(effectReceiptPrefix) || len(receipt) > maximumEffectReceiptBytes ||
		!strings.HasPrefix(receipt, effectReceiptPrefix) {
		return ErrEffectReceiptInvalid
	}
	encoded := strings.TrimPrefix(receipt, effectReceiptPrefix)
	sealed, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(sealed) != encoded ||
		len(sealed) <= effectReceiptNonceBytes {
		return ErrEffectReceiptInvalid
	}
	defer zeroEffectReceiptBytes(sealed)
	provider.mu.RLock()
	defer provider.mu.RUnlock()
	if provider.closed {
		return ErrEffectReceiptClosed
	}
	aead, err := provider.aeadLocked()
	if err != nil {
		return ErrEffectReceiptInvalid
	}
	nonce := sealed[:effectReceiptNonceBytes]
	payload, err := aead.Open(nil, nonce, sealed[effectReceiptNonceBytes:], effectReceiptAdditionalData)
	if err != nil {
		return ErrEffectReceiptInvalid
	}
	defer zeroEffectReceiptBytes(payload)
	var envelope sealedEffectReceipt
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return ErrEffectReceiptInvalid
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrEffectReceiptInvalid
	}
	if envelope.Version != effectReceiptVersion || envelope.Issuer != provider.identity ||
		envelope.ExpiresAtNS <= envelope.IssuedAtNS ||
		envelope.ExpiresAtNS-envelope.IssuedAtNS != provider.ttl.Nanoseconds() ||
		validateEffectReceiptClaims(envelope.Claims) != nil {
		return ErrEffectReceiptInvalid
	}
	now := provider.clock().UTC().UnixNano()
	if now < envelope.IssuedAtNS {
		return ErrEffectReceiptInvalid
	}
	if now >= envelope.ExpiresAtNS {
		return ErrEffectReceiptExpired
	}
	if envelope.Claims != expected {
		return ErrEffectReceiptMismatch
	}
	runtime.KeepAlive(provider)
	return contextCause(ctx)
}

func (provider *SealedEffectReceipts) aeadLocked() (cipher.AEAD, error) {
	block, err := aes.NewCipher(provider.key[:])
	if err != nil {
		return nil, fmt.Errorf("create client-effect receipt cipher: %w", err)
	}
	aead, err := cipher.NewGCMWithNonceSize(block, effectReceiptNonceBytes)
	if err != nil {
		return nil, fmt.Errorf("create client-effect receipt sealer: %w", err)
	}
	return aead, nil
}

// Close revokes every receipt in this key domain and overwrites retained key
// bytes. It waits for active issue/verify operations before zeroizing.
func (provider *SealedEffectReceipts) Close() error {
	if provider == nil {
		return nil
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.closed {
		return nil
	}
	provider.closed = true
	zeroEffectReceiptBytes(provider.key[:])
	zeroEffectReceiptBytes(provider.noncePrefix[:])
	provider.nonceCounter.Store(0)
	provider.clock = nil
	runtime.KeepAlive(provider.key)
	return nil
}

// CanonicalEffectArguments normalizes one strict JSON object and returns the
// exact sha256 digest used on both the gateway and host sides of a receipt.
func CanonicalEffectArguments(source []byte) (json.RawMessage, string, error) {
	if err := validateEffectJSONDepth(source); err != nil {
		return nil, "", err
	}
	if err := strictjson.Validate(source); err != nil {
		return nil, "", fmt.Errorf("canonical client-effect arguments: %w", err)
	}
	// Decode through binary64 deliberately: browser effect clients parse JSON
	// numbers as ECMAScript Number before stable encoding. encoding/json's
	// float formatting follows the same shortest-round-trip representation,
	// so 1.0, exponent spellings, and integers outside the safe range cannot
	// produce different receipts on the gateway and host sides.
	decoder := json.NewDecoder(bytes.NewReader(source))
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, "", fmt.Errorf("canonical client-effect arguments: %w", err)
	}
	if _, object := value.(map[string]any); !object {
		return nil, "", errors.New("canonical client-effect arguments must be one JSON object")
	}
	normalizeEffectJSONNumbers(value)
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, "", fmt.Errorf("canonical client-effect arguments: %w", err)
	}
	digest := sha256.Sum256(canonical)
	return json.RawMessage(canonical), "sha256:" + hex.EncodeToString(digest[:]), nil
}

// validateEffectJSONDepth is a bounded lexical preflight before strictjson's
// recursive duplicate-key walk. It ignores delimiters inside JSON strings and
// prevents an adversarial deeply nested value from reaching that recursion.
// The limit matches the browser strict parser exactly.
func validateEffectJSONDepth(source []byte) error {
	depth := 0
	inString := false
	escaped := false
	for _, character := range source {
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maximumEffectJSONDepth {
				return fmt.Errorf("canonical client-effect arguments exceed JSON depth %d", maximumEffectJSONDepth)
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return nil
}

// ECMAScript JSON.stringify renders negative zero as zero. Browser effect
// forwarding necessarily crosses that representation, so normalize it before
// hashing on both Go boundaries. Other binary64 values already use the same
// shortest-round-trip and exponent thresholds in encoding/json.
func normalizeEffectJSONNumbers(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if number, ok := child.(float64); ok && number == 0 {
				typed[key] = float64(0)
				continue
			}
			normalizeEffectJSONNumbers(child)
		}
	case []any:
		for index, child := range typed {
			if number, ok := child.(float64); ok && number == 0 {
				typed[index] = float64(0)
				continue
			}
			normalizeEffectJSONNumbers(child)
		}
	}
}

func validateEffectReceiptClaims(claims EffectReceiptClaims) error {
	if err := validateEffectReceiptIdentity("session_id", claims.SessionID, false); err != nil {
		return err
	}
	if err := validateEffectReceiptIdentity("call_id", claims.CallID, false); err != nil {
		return err
	}
	if err := validateEffectReceiptToolName(claims.Name); err != nil {
		return err
	}
	if !validEffectReceiptDigest(claims.ArgumentsDigest) {
		return errors.New("client-effect receipt requires a canonical arguments digest")
	}
	if !validEffectReceiptDigest(claims.DeclarationDigest) {
		return errors.New("client-effect receipt requires a canonical declaration digest")
	}
	if err := validateEffectReceiptIdentity("target", claims.Target, true); err != nil {
		return err
	}
	return nil
}

func validateEffectReceiptIdentity(field, value string, empty bool) error {
	if value == "" && empty {
		return nil
	}
	if value == "" || len(value) > maximumEffectIdentityBytes || value != strings.TrimSpace(value) ||
		!utf8.ValidString(value) {
		return fmt.Errorf("client-effect receipt %s is not canonical", field)
	}
	for _, character := range value {
		if character < 0x21 || character == 0x7f {
			return fmt.Errorf("client-effect receipt %s contains whitespace or control characters", field)
		}
	}
	return nil
}

func validateEffectReceiptToolName(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("client-effect receipt tool name is not canonical")
	}
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(index > 0 && character >= '0' && character <= '9') ||
			(index > 0 && (character == '_' || character == '-' || character == '.')) {
			continue
		}
		return errors.New("client-effect receipt tool name is not canonical")
	}
	return nil
}

func validEffectReceiptDigest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, character := range value[len("sha256:"):] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func contextCause(ctx context.Context) error {
	if ctx == nil {
		return errors.New("client-effect receipt operation requires a context")
	}
	if err := ctx.Err(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	return nil
}

func zeroEffectReceiptBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
	runtime.KeepAlive(value)
}

var _ EffectReceiptProvider = (*SealedEffectReceipts)(nil)
