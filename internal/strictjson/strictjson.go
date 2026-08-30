// Package strictjson performs bounded, duplicate-safe RFC 8259 structural
// validation before callers decode JSON into typed values.
package strictjson

import (
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// Limits bounds both the accepted JSON shape and the work needed to validate
// it. A zero field selects that field's value from DefaultLimits. Negative
// values are invalid.
//
// MaxDepth counts nested arrays and objects. MaxTokens counts every JSON value
// and every object key. Object-member and array-element limits apply to each
// individual container. Byte limits count decoded UTF-8 key bytes.
// MaxWorkBytes charges input bytes, tokens, decoded-key validation, and bounded
// duplicate-detection work.
type Limits struct {
	MaxInputBytes    int
	MaxDepth         int
	MaxTokens        int
	MaxObjectMembers int
	MaxArrayElements int
	MaxKeyBytes      int
	MaxTotalKeyBytes int64
	MaxWorkBytes     int64
}

const (
	defaultMaxInputBytes    = 256 << 20
	defaultMaxDepth         = 256
	defaultMaxTokens        = 16_000_000
	defaultMaxObjectMembers = 100_000
	defaultMaxArrayElements = 1_000_000
	defaultMaxKeyBytes      = 64 << 10
	defaultMaxTotalKeyBytes = 128 << 20
	defaultMaxWorkBytes     = 1 << 30
)

// DefaultLimits returns conservative process-level bounds. The array and
// token defaults intentionally admit manifests containing at least 100,000
// frame records while keeping hostile inputs finite.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes:    defaultMaxInputBytes,
		MaxDepth:         defaultMaxDepth,
		MaxTokens:        defaultMaxTokens,
		MaxObjectMembers: defaultMaxObjectMembers,
		MaxArrayElements: defaultMaxArrayElements,
		MaxKeyBytes:      defaultMaxKeyBytes,
		MaxTotalKeyBytes: defaultMaxTotalKeyBytes,
		MaxWorkBytes:     defaultMaxWorkBytes,
	}
}

// Validate applies DefaultLimits and rejects malformed JSON, duplicate decoded
// object keys, trailing data, and inputs that exceed a structural bound.
func Validate(source []byte) error {
	return ValidateWithLimits(source, Limits{})
}

// ValidateWithLimits applies caller-provided structural limits. Zero-valued
// fields inherit their defaults, which makes Limits{} equivalent to Validate.
func ValidateWithLimits(source []byte, configured Limits) error {
	limits, err := resolveLimits(configured)
	if err != nil {
		return err
	}
	if len(source) > limits.MaxInputBytes {
		return fmt.Errorf("strict JSON limit: input bytes %d exceed maximum %d", len(source), limits.MaxInputBytes)
	}
	p := parser{
		source: source,
		limits: limits,
		stack:  make([]frame, 0, min(limits.MaxDepth, 8)),
	}
	return p.validate()
}

func resolveLimits(limits Limits) (Limits, error) {
	if err := validateNonNegative(limits); err != nil {
		return Limits{}, err
	}
	defaults := DefaultLimits()
	if limits.MaxInputBytes == 0 {
		limits.MaxInputBytes = defaults.MaxInputBytes
	}
	if limits.MaxDepth == 0 {
		limits.MaxDepth = defaults.MaxDepth
	}
	if limits.MaxTokens == 0 {
		limits.MaxTokens = defaults.MaxTokens
	}
	if limits.MaxObjectMembers == 0 {
		limits.MaxObjectMembers = defaults.MaxObjectMembers
	}
	if limits.MaxArrayElements == 0 {
		limits.MaxArrayElements = defaults.MaxArrayElements
	}
	if limits.MaxKeyBytes == 0 {
		limits.MaxKeyBytes = defaults.MaxKeyBytes
	}
	if limits.MaxTotalKeyBytes == 0 {
		limits.MaxTotalKeyBytes = defaults.MaxTotalKeyBytes
	}
	if limits.MaxWorkBytes == 0 {
		limits.MaxWorkBytes = defaults.MaxWorkBytes
	}
	return limits, nil
}

func validateNonNegative(limits Limits) error {
	switch {
	case limits.MaxInputBytes < 0:
		return fmt.Errorf("strictjson: MaxInputBytes must not be negative")
	case limits.MaxDepth < 0:
		return fmt.Errorf("strictjson: MaxDepth must not be negative")
	case limits.MaxTokens < 0:
		return fmt.Errorf("strictjson: MaxTokens must not be negative")
	case limits.MaxObjectMembers < 0:
		return fmt.Errorf("strictjson: MaxObjectMembers must not be negative")
	case limits.MaxArrayElements < 0:
		return fmt.Errorf("strictjson: MaxArrayElements must not be negative")
	case limits.MaxKeyBytes < 0:
		return fmt.Errorf("strictjson: MaxKeyBytes must not be negative")
	case limits.MaxTotalKeyBytes < 0:
		return fmt.Errorf("strictjson: MaxTotalKeyBytes must not be negative")
	case limits.MaxWorkBytes < 0:
		return fmt.Errorf("strictjson: MaxWorkBytes must not be negative")
	default:
		return nil
	}
}

type containerState uint8

const (
	objectKeyOrEnd containerState = iota + 1
	objectKeyRequired
	objectColon
	objectValue
	objectNext
	arrayValueOrEnd
	arrayValueRequired
	arrayNext
)

type containerKind uint8

const (
	objectContainer containerKind = iota + 1
	arrayContainer
)

type frame struct {
	kind     containerKind
	state    containerState
	count    int
	firstKey string
	seen     map[string]struct{}
}

type parser struct {
	source        []byte
	limits        Limits
	offset        int
	work          int64
	tokens        int
	totalKeyBytes int64
	stack         []frame
}

func (p *parser) validate() error {
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.offset == len(p.source) {
		return p.errorAt(p.offset, "expected a JSON value")
	}
	if err := p.parseValue(); err != nil {
		return err
	}
	for len(p.stack) > 0 {
		if err := p.stepContainer(); err != nil {
			return err
		}
	}
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.offset != len(p.source) {
		return p.errorAt(p.offset, "trailing JSON token")
	}
	return nil
}

func (p *parser) stepContainer() error {
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	frameIndex := len(p.stack) - 1
	current := &p.stack[frameIndex]
	switch current.state {
	case objectKeyOrEnd:
		if p.peek('}') {
			return p.closeContainer()
		}
		return p.parseObjectKey(frameIndex)
	case objectKeyRequired:
		return p.parseObjectKey(frameIndex)
	case objectColon:
		if !p.peek(':') {
			return p.expected("':' after object key")
		}
		if err := p.advance(1); err != nil {
			return err
		}
		p.stack[frameIndex].state = objectValue
		return nil
	case objectValue:
		p.stack[frameIndex].state = objectNext
		return p.parseValue()
	case objectNext:
		switch {
		case p.peek('}'):
			return p.closeContainer()
		case p.peek(','):
			if err := p.advance(1); err != nil {
				return err
			}
			p.stack[frameIndex].state = objectKeyRequired
			return nil
		default:
			return p.expected("',' or '}' after object value")
		}
	case arrayValueOrEnd:
		if p.peek(']') {
			return p.closeContainer()
		}
		return p.parseArrayValue(frameIndex)
	case arrayValueRequired:
		return p.parseArrayValue(frameIndex)
	case arrayNext:
		switch {
		case p.peek(']'):
			return p.closeContainer()
		case p.peek(','):
			if err := p.advance(1); err != nil {
				return err
			}
			p.stack[frameIndex].state = arrayValueRequired
			return nil
		default:
			return p.expected("',' or ']' after array value")
		}
	default:
		return p.errorAt(p.offset, "invalid internal container state")
	}
}

func (p *parser) parseObjectKey(frameIndex int) error {
	current := &p.stack[frameIndex]
	if current.count >= p.limits.MaxObjectMembers {
		return p.limitAt(p.offset, "object members", int64(current.count+1), int64(p.limits.MaxObjectMembers))
	}
	if !p.peek('"') {
		return p.expected("an object key string")
	}
	if err := p.addToken(); err != nil {
		return err
	}
	keyOffset := p.offset
	decoded, err := p.parseString(true)
	if err != nil {
		return err
	}
	// Conversion plus exact duplicate lookup/insertion each perform bounded work
	// proportional to the decoded key. Charge conservatively before either.
	if err := p.chargeExtra(3 * int64(len(decoded))); err != nil {
		return err
	}
	p.totalKeyBytes += int64(len(decoded))
	key := string(decoded)

	current = &p.stack[frameIndex]
	current.count++
	if current.count == 1 {
		current.firstKey = key
	} else {
		if current.seen == nil {
			if key == current.firstKey {
				return p.errorAt(keyOffset, "duplicate JSON key (decoded length %d)", len(key))
			}
			current.seen = make(map[string]struct{}, min(p.limits.MaxObjectMembers, 8))
			current.seen[current.firstKey] = struct{}{}
			current.firstKey = ""
		}
		if _, duplicate := current.seen[key]; duplicate {
			return p.errorAt(keyOffset, "duplicate JSON key (decoded length %d)", len(key))
		}
		current.seen[key] = struct{}{}
	}
	current.state = objectColon
	return nil
}

func (p *parser) parseArrayValue(frameIndex int) error {
	current := &p.stack[frameIndex]
	if current.count >= p.limits.MaxArrayElements {
		return p.limitAt(p.offset, "array elements", int64(current.count+1), int64(p.limits.MaxArrayElements))
	}
	current.count++
	current.state = arrayNext
	return p.parseValue()
}

func (p *parser) parseValue() error {
	if err := p.skipWhitespace(); err != nil {
		return err
	}
	if p.offset == len(p.source) {
		return p.errorAt(p.offset, "expected a JSON value")
	}
	if err := p.addToken(); err != nil {
		return err
	}
	switch p.source[p.offset] {
	case '{':
		if err := p.advance(1); err != nil {
			return err
		}
		return p.pushContainer(objectContainer, objectKeyOrEnd)
	case '[':
		if err := p.advance(1); err != nil {
			return err
		}
		return p.pushContainer(arrayContainer, arrayValueOrEnd)
	case '"':
		_, err := p.parseString(false)
		return err
	case 't':
		return p.parseLiteral("true")
	case 'f':
		return p.parseLiteral("false")
	case 'n':
		return p.parseLiteral("null")
	case '-':
		return p.parseNumber()
	default:
		if isDigit(p.source[p.offset]) {
			return p.parseNumber()
		}
		return p.expected("a JSON value")
	}
}

func (p *parser) pushContainer(kind containerKind, state containerState) error {
	depth := len(p.stack) + 1
	if depth > p.limits.MaxDepth {
		return p.limitAt(p.offset-1, "container depth", int64(depth), int64(p.limits.MaxDepth))
	}
	p.stack = append(p.stack, frame{kind: kind, state: state})
	return nil
}

func (p *parser) closeContainer() error {
	if err := p.advance(1); err != nil {
		return err
	}
	last := len(p.stack) - 1
	p.stack[last] = frame{}
	p.stack = p.stack[:last]
	return nil
}

func (p *parser) parseString(decodeKey bool) ([]byte, error) {
	if err := p.advance(1); err != nil { // opening quote
		return nil, err
	}
	var decoded []byte
	for p.offset < len(p.source) {
		b := p.source[p.offset]
		switch {
		case b == '"':
			if err := p.advance(1); err != nil {
				return nil, err
			}
			return decoded, nil
		case b < 0x20:
			return nil, p.errorAt(p.offset, "unescaped control byte in string")
		case b == '\\':
			appended, appendedSize, consumed, err := p.parseEscape()
			if err != nil {
				return nil, err
			}
			if decodeKey {
				if err := p.checkDecodedKeyGrowth(len(decoded), appendedSize); err != nil {
					return nil, err
				}
				if err := p.chargeExtra(int64(appendedSize)); err != nil {
					return nil, err
				}
				decoded = append(decoded, appended[:appendedSize]...)
			}
			if err := p.advance(consumed); err != nil {
				return nil, err
			}
		case b < utf8.RuneSelf:
			if decodeKey {
				if err := p.checkDecodedKeyGrowth(len(decoded), 1); err != nil {
					return nil, err
				}
				if err := p.chargeExtra(1); err != nil {
					return nil, err
				}
				decoded = append(decoded, b)
			}
			if err := p.advance(1); err != nil {
				return nil, err
			}
		default:
			_, size := utf8.DecodeRune(p.source[p.offset:])
			if size == 1 {
				return nil, p.errorAt(p.offset, "malformed UTF-8 in string")
			}
			if decodeKey {
				if err := p.checkDecodedKeyGrowth(len(decoded), size); err != nil {
					return nil, err
				}
				if err := p.chargeExtra(int64(size)); err != nil {
					return nil, err
				}
				decoded = append(decoded, p.source[p.offset:p.offset+size]...)
			}
			if err := p.advance(size); err != nil {
				return nil, err
			}
		}
	}
	return nil, p.errorAt(p.offset, "unterminated string")
}

func (p *parser) parseEscape() ([utf8.UTFMax]byte, int, int, error) {
	var encoded [utf8.UTFMax]byte
	escapeOffset := p.offset
	if p.offset+1 >= len(p.source) {
		return encoded, 0, 0, p.errorAt(escapeOffset, "unterminated string escape")
	}
	switch p.source[p.offset+1] {
	case '"', '\\', '/':
		encoded[0] = p.source[p.offset+1]
		return encoded, 1, 2, nil
	case 'b':
		encoded[0] = '\b'
		return encoded, 1, 2, nil
	case 'f':
		encoded[0] = '\f'
		return encoded, 1, 2, nil
	case 'n':
		encoded[0] = '\n'
		return encoded, 1, 2, nil
	case 'r':
		encoded[0] = '\r'
		return encoded, 1, 2, nil
	case 't':
		encoded[0] = '\t'
		return encoded, 1, 2, nil
	case 'u':
		first, ok := p.hexCodeUnit(p.offset + 2)
		if !ok {
			return encoded, 0, 0, p.errorAt(escapeOffset, "invalid Unicode escape")
		}
		consumed := 6
		var r rune
		switch {
		case first >= 0xD800 && first <= 0xDBFF:
			if p.offset+12 > len(p.source) || p.source[p.offset+6] != '\\' || p.source[p.offset+7] != 'u' {
				return encoded, 0, 0, p.errorAt(escapeOffset, "lone high UTF-16 surrogate")
			}
			second, valid := p.hexCodeUnit(p.offset + 8)
			if !valid || second < 0xDC00 || second > 0xDFFF {
				return encoded, 0, 0, p.errorAt(escapeOffset, "invalid UTF-16 surrogate pair")
			}
			r = utf16.DecodeRune(rune(first), rune(second))
			consumed = 12
		case first >= 0xDC00 && first <= 0xDFFF:
			return encoded, 0, 0, p.errorAt(escapeOffset, "lone low UTF-16 surrogate")
		default:
			r = rune(first)
		}
		size := utf8.EncodeRune(encoded[:], r)
		return encoded, size, consumed, nil
	default:
		return encoded, 0, 0, p.errorAt(escapeOffset, "invalid string escape")
	}
}

func (p *parser) hexCodeUnit(offset int) (uint16, bool) {
	if offset < 0 || offset+4 > len(p.source) {
		return 0, false
	}
	var value uint16
	for index := 0; index < 4; index++ {
		digit, ok := hexValue(p.source[offset+index])
		if !ok {
			return 0, false
		}
		value = value<<4 | uint16(digit)
	}
	return value, true
}

func (p *parser) checkDecodedKeyGrowth(current, additional int) error {
	if additional > p.limits.MaxKeyBytes-current {
		return p.limitAt(p.offset, "decoded key bytes", int64(current)+int64(additional), int64(p.limits.MaxKeyBytes))
	}
	prospective := p.totalKeyBytes + int64(current) + int64(additional)
	if prospective > p.limits.MaxTotalKeyBytes {
		return p.limitAt(p.offset, "total decoded key bytes", prospective, p.limits.MaxTotalKeyBytes)
	}
	return nil
}

func (p *parser) parseLiteral(literal string) error {
	if len(p.source)-p.offset < len(literal) {
		return p.errorAt(p.offset, "incomplete JSON literal")
	}
	for index := range len(literal) {
		if p.source[p.offset+index] != literal[index] {
			return p.errorAt(p.offset, "invalid JSON literal")
		}
	}
	return p.advance(len(literal))
}

func (p *parser) parseNumber() error {
	if p.peek('-') {
		if err := p.advance(1); err != nil {
			return err
		}
		if p.offset == len(p.source) {
			return p.errorAt(p.offset, "incomplete JSON number")
		}
	}
	switch {
	case p.peek('0'):
		if err := p.advance(1); err != nil {
			return err
		}
	case p.offset < len(p.source) && p.source[p.offset] >= '1' && p.source[p.offset] <= '9':
		if err := p.consumeDigits(); err != nil {
			return err
		}
	default:
		return p.errorAt(p.offset, "invalid JSON number")
	}
	if p.peek('.') {
		if err := p.advance(1); err != nil {
			return err
		}
		if p.offset == len(p.source) || !isDigit(p.source[p.offset]) {
			return p.errorAt(p.offset, "fraction requires a digit")
		}
		if err := p.consumeDigits(); err != nil {
			return err
		}
	}
	if p.peek('e') || p.peek('E') {
		if err := p.advance(1); err != nil {
			return err
		}
		if p.peek('+') || p.peek('-') {
			if err := p.advance(1); err != nil {
				return err
			}
		}
		if p.offset == len(p.source) || !isDigit(p.source[p.offset]) {
			return p.errorAt(p.offset, "exponent requires a digit")
		}
		return p.consumeDigits()
	}
	return nil
}

func (p *parser) consumeDigits() error {
	for p.offset < len(p.source) && isDigit(p.source[p.offset]) {
		if err := p.advance(1); err != nil {
			return err
		}
	}
	return nil
}

func (p *parser) skipWhitespace() error {
	for p.offset < len(p.source) {
		switch p.source[p.offset] {
		case ' ', '\t', '\n', '\r':
			if err := p.advance(1); err != nil {
				return err
			}
		default:
			return nil
		}
	}
	return nil
}

func (p *parser) addToken() error {
	if p.tokens >= p.limits.MaxTokens {
		return p.limitAt(p.offset, "tokens", int64(p.tokens+1), int64(p.limits.MaxTokens))
	}
	if err := p.chargeExtra(1); err != nil {
		return err
	}
	p.tokens++
	return nil
}

func (p *parser) advance(count int) error {
	if count < 0 || count > len(p.source)-p.offset {
		return p.errorAt(p.offset, "unexpected end of input")
	}
	if err := p.chargeExtra(int64(count)); err != nil {
		return err
	}
	p.offset += count
	return nil
}

func (p *parser) chargeExtra(count int64) error {
	if count < 0 || count > p.limits.MaxWorkBytes-p.work {
		observed := p.work
		if count > 0 && count <= int64(^uint64(0)>>1)-observed {
			observed += count
		}
		return p.limitAt(p.offset, "structural work bytes", observed, p.limits.MaxWorkBytes)
	}
	p.work += count
	return nil
}

func (p *parser) peek(expected byte) bool {
	return p.offset < len(p.source) && p.source[p.offset] == expected
}

func (p *parser) expected(description string) error {
	if p.offset == len(p.source) {
		return p.errorAt(p.offset, "expected %s; reached end of input", description)
	}
	return p.errorAt(p.offset, "expected %s; found byte 0x%02x", description, p.source[p.offset])
}

func (p *parser) limitAt(offset int, name string, observed, maximum int64) error {
	return p.errorAt(offset, "%s %d exceed maximum %d", name, observed, maximum)
}

func (p *parser) errorAt(offset int, format string, args ...any) error {
	return fmt.Errorf("strict JSON at byte %d: %s", offset, fmt.Sprintf(format, args...))
}

func isDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func hexValue(value byte) (byte, bool) {
	switch {
	case value >= '0' && value <= '9':
		return value - '0', true
	case value >= 'a' && value <= 'f':
		return value - 'a' + 10, true
	case value >= 'A' && value <= 'F':
		return value - 'A' + 10, true
	default:
		return 0, false
	}
}
