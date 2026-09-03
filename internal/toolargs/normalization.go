// Package toolargs implements the closed, deterministic tool-argument
// transformations shared by action authority and canonical trajectory
// validation. Keeping the byte-level algorithm below both boundaries lets the
// trajectory reject self-described derivations rather than trusting digests
// supplied by their author.
package toolargs

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const (
	// CompactASCIIAlphanumericV1 removes the bounded set of separators that
	// speech recognition commonly inserts into a dictated compact identifier.
	// Every remaining byte must be an ASCII letter or digit.
	CompactASCIIAlphanumericV1 = "compact-ascii-alphanumeric-v1"
	// CoordinatePairXYV1 recognizes one narrowly scoped provider defect: the
	// model placed the normalized x/y coordinates in a two-integer array under
	// x and omitted y. It derives separate x and y members. A canonical scalar
	// x/y call is unchanged; no other array shape is accepted.
	CoordinatePairXYV1 = "coordinate-pair-x-y-v1"

	// MaximumJSONBytes bounds both validation and byte-preserving rewrite work.
	MaximumJSONBytes = 1 << 20
)

// Rule identifies one direct top-level object member and its closed,
// versioned transformation. A transformation may derive another fixed member
// when that behavior is part of its versioned identity. Rules must be strictly
// ordered by Argument.
type Rule struct {
	Argument   string
	Normalizer string
}

// Supported reports whether identity names one closed transformation that can
// be replayed at both the action and trajectory boundaries.
func Supported(identity string) bool {
	switch identity {
	case CompactASCIIAlphanumericV1, CoordinatePairXYV1:
		return true
	default:
		return false
	}
}

// Apply validates source as one bounded strict JSON object and applies rules
// without re-marshalling the object. It returns the exact transformed bytes
// and the subset of rules that changed a value. Unnamed values retain their
// original order, whitespace, escapes, and number spellings.
func Apply(source json.RawMessage, rules []Rule) (json.RawMessage, []Rule, error) {
	if len(source) > MaximumJSONBytes {
		return nil, nil, fmt.Errorf("tool arguments exceed %d bytes", MaximumJSONBytes)
	}
	if len(rules) > 4096 {
		return nil, nil, errors.New("tool arguments declare more than 4096 normalization rules")
	}
	if err := strictjson.ValidateWithLimits(source, strictjson.Limits{
		MaxInputBytes: MaximumJSONBytes, MaxDepth: 32, MaxTokens: 16_384,
		MaxObjectMembers: 4_096, MaxArrayElements: 4_096, MaxKeyBytes: 4_096,
		MaxTotalKeyBytes: MaximumJSONBytes, MaxWorkBytes: 8 * MaximumJSONBytes,
	}); err != nil {
		return nil, nil, fmt.Errorf("tool arguments: %w", err)
	}
	members, err := parseObjectMembers(source)
	if err != nil {
		return nil, nil, errors.New("tool arguments must be one JSON object")
	}
	byName := make(map[string]objectMember, len(members))
	for _, member := range members {
		byName[member.name] = member
	}

	last := ""
	replacements := make([]replacement, 0, len(rules))
	changed := make([]Rule, 0, len(rules))
	for index, rule := range rules {
		if rule.Argument == "" || rule.Argument != strings.TrimSpace(rule.Argument) ||
			(index > 0 && rule.Argument <= last) {
			return nil, nil, errors.New("tool argument normalization rules must name strictly ordered unique canonical arguments")
		}
		if !Supported(rule.Normalizer) {
			return nil, nil, fmt.Errorf("tool argument %q names unsupported normalizer %q",
				rule.Argument, rule.Normalizer)
		}
		last = rule.Argument
		member, present := byName[rule.Argument]
		if !present {
			continue
		}
		rewritten, wasChanged, err := normalizeMember(source, rule, member, byName)
		if err != nil {
			return nil, nil, err
		}
		if !wasChanged {
			continue
		}
		replacements = append(replacements, rewritten)
		changed = append(changed, rule)
	}
	if len(replacements) == 0 {
		return slices.Clone(source), nil, nil
	}
	return applyReplacements(source, replacements), changed, nil
}

func normalizeMember(
	source []byte, rule Rule, member objectMember, members map[string]objectMember,
) (replacement, bool, error) {
	switch rule.Normalizer {
	case CompactASCIIAlphanumericV1:
		var value string
		if err := json.Unmarshal(source[member.valueStart:member.valueEnd], &value); err != nil {
			return replacement{}, false,
				fmt.Errorf("tool argument %q compact identifier must be a string", rule.Argument)
		}
		normalized, changed, err := compactASCIIAlphanumeric(value)
		if err != nil {
			return replacement{}, false, fmt.Errorf("tool argument %q: %w", rule.Argument, err)
		}
		if !changed {
			return replacement{}, false, nil
		}
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return replacement{}, false,
				fmt.Errorf("encode normalized tool argument %q: %w", rule.Argument, err)
		}
		return replacement{start: member.valueStart, end: member.valueEnd, value: encoded}, true, nil
	case CoordinatePairXYV1:
		return normalizeCoordinatePairXY(source, rule, member, members)
	default:
		return replacement{}, false,
			fmt.Errorf("tool argument %q names unsupported normalizer %q", rule.Argument, rule.Normalizer)
	}
}

func normalizeCoordinatePairXY(
	source []byte, rule Rule, member objectMember, members map[string]objectMember,
) (replacement, bool, error) {
	if rule.Argument != "x" {
		return replacement{}, false, fmt.Errorf(
			"tool argument %q coordinate pair normalizer must be attached to x", rule.Argument,
		)
	}
	raw := source[member.valueStart:member.valueEnd]
	if len(raw) == 0 || raw[0] != '[' {
		// Correct calls already carry scalar x and y members. This rule does not
		// rewrite or reinterpret any other scalar/object/string shape.
		if _, present := members["y"]; !present {
			return replacement{}, false, errors.New(
				"tool coordinate arguments require y unless x is exactly a two-integer pair",
			)
		}
		return replacement{}, false, nil
	}
	if _, present := members["y"]; present {
		return replacement{}, false, errors.New(
			"tool coordinate pair cannot be normalized when y is already present",
		)
	}
	var pair []json.RawMessage
	if err := json.Unmarshal(raw, &pair); err != nil || len(pair) != 2 {
		return replacement{}, false, errors.New(
			"tool coordinate pair x must be exactly a two-element integer array",
		)
	}
	for _, coordinate := range pair {
		value := strings.TrimSpace(string(coordinate))
		if value == "" {
			return replacement{}, false, errors.New(
				"tool coordinate pair x must contain exactly two integers",
			)
		}
		if _, err := strconv.ParseInt(value, 10, 64); err != nil {
			return replacement{}, false, errors.New(
				"tool coordinate pair x must contain exactly two integers",
			)
		}
	}
	value := make([]byte, 0, len(pair[0])+len(pair[1])+6)
	value = append(value, pair[0]...)
	value = append(value, ',', '"', 'y', '"', ':')
	value = append(value, pair[1]...)
	return replacement{start: member.valueStart, end: member.valueEnd, value: value}, true, nil
}

type objectMember struct {
	name                 string
	valueStart, valueEnd int
}

type replacement struct {
	start, end int
	value      []byte
}

func parseObjectMembers(source []byte) ([]objectMember, error) {
	offset := skipWhitespace(source, 0)
	if offset >= len(source) || source[offset] != '{' {
		return nil, errors.New("arguments are not an object")
	}
	offset++
	var members []objectMember
	for {
		offset = skipWhitespace(source, offset)
		if offset >= len(source) {
			return nil, errors.New("unterminated argument object")
		}
		if source[offset] == '}' {
			offset = skipWhitespace(source, offset+1)
			if offset != len(source) {
				return nil, errors.New("trailing argument data")
			}
			return members, nil
		}
		if source[offset] != '"' {
			return nil, errors.New("argument key is not a string")
		}
		keyEnd, err := scanStringEnd(source, offset)
		if err != nil {
			return nil, err
		}
		var name string
		if err := json.Unmarshal(source[offset:keyEnd], &name); err != nil {
			return nil, err
		}
		offset = skipWhitespace(source, keyEnd)
		if offset >= len(source) || source[offset] != ':' {
			return nil, errors.New("argument key has no value")
		}
		valueStart := skipWhitespace(source, offset+1)
		valueEnd, err := scanValueEnd(source, valueStart)
		if err != nil {
			return nil, err
		}
		members = append(members, objectMember{name: name, valueStart: valueStart, valueEnd: valueEnd})
		offset = skipWhitespace(source, valueEnd)
		if offset >= len(source) {
			return nil, errors.New("unterminated argument object")
		}
		switch source[offset] {
		case ',':
			offset++
		case '}':
		default:
			return nil, errors.New("argument members are not comma separated")
		}
	}
}

func scanStringEnd(source []byte, start int) (int, error) {
	if start >= len(source) || source[start] != '"' {
		return 0, errors.New("expected JSON string")
	}
	escaped := false
	for offset := start + 1; offset < len(source); offset++ {
		if escaped {
			escaped = false
			continue
		}
		switch source[offset] {
		case '\\':
			escaped = true
		case '"':
			return offset + 1, nil
		}
	}
	return 0, errors.New("unterminated JSON string")
}

func scanValueEnd(source []byte, start int) (int, error) {
	if start >= len(source) {
		return 0, errors.New("missing JSON value")
	}
	if source[start] == '"' {
		return scanStringEnd(source, start)
	}
	depth := 0
	inString := false
	escaped := false
	for offset := start; offset < len(source); offset++ {
		character := source[offset]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if character == '\\' {
				escaped = true
			} else if character == '"' {
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 0 {
				return offset, nil
			}
			depth--
			if depth == 0 && (source[start] == '{' || source[start] == '[') {
				return offset + 1, nil
			}
		case ',':
			if depth == 0 {
				return offset, nil
			}
		case ' ', '\t', '\r', '\n':
			if depth == 0 {
				return offset, nil
			}
		}
	}
	return len(source), nil
}

func skipWhitespace(source []byte, offset int) int {
	for offset < len(source) {
		switch source[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func applyReplacements(source []byte, replacements []replacement) []byte {
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	result := make([]byte, 0, len(source))
	start := 0
	for _, item := range replacements {
		result = append(result, source[start:item.start]...)
		result = append(result, item.value...)
		start = item.end
	}
	return append(result, source[start:]...)
}

func compactASCIIAlphanumeric(source string) (string, bool, error) {
	if source == "" {
		return "", false, errors.New("compact identifier is empty")
	}
	var result strings.Builder
	result.Grow(len(source))
	changed := false
	for index := 0; index < len(source); index++ {
		character := source[index]
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9':
			result.WriteByte(character)
		case character == ' ', character == '\t', character == '\r', character == '\n',
			character == '-', character == '_', character == '.', character == ',':
			changed = true
		default:
			return "", false, fmt.Errorf("compact identifier contains unsupported byte 0x%02x", character)
		}
	}
	if result.Len() == 0 {
		return "", false, errors.New("compact identifier contains no ASCII letters or digits")
	}
	return result.String(), changed, nil
}
