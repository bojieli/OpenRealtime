package interaction

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash"
	"strings"
)

const (
	toolCallOpen                       = "<tool_call>"
	toolCallClose                      = "</tool_call>"
	toolCallPrefix                     = "tool call:"
	toolCallUnderscore                 = "tool_call:"
	toolCallJSONFenceOpen              = "```json"
	toolCallJSONFenceEnd               = "```"
	functionTagOpen                    = "<function="
	functionTagClose                   = "</function>"
	functionCallOpen                   = "<function_call>"
	functionCallClose                  = "</function_call>"
	filterInputSliceBytes              = 4096
	maximumSerializedFunctionNameBytes = 256
)

// ControlSerializationSyntax is deliberately not an action or proposal kind.
// It names only the text wrapper whose bytes were withheld.
type ControlSerializationSyntax string

const (
	ControlSyntaxTagged             ControlSerializationSyntax = "tool_call_tag"
	ControlSyntaxPrefixed           ControlSerializationSyntax = "tool_call_prefix"
	ControlSyntaxJSONFence          ControlSerializationSyntax = "json_fence"
	ControlSyntaxPrefixedJSONFence  ControlSerializationSyntax = "tool_call_json_fence"
	ControlSyntaxBareJSON           ControlSerializationSyntax = "bare_json"
	ControlSyntaxCallableExpression ControlSerializationSyntax = "callable_expression"
	ControlSyntaxFunctionTag        ControlSerializationSyntax = "function_tag"
	ControlSyntaxFunctionCallTag    ControlSerializationSyntax = "function_call_tag"
)

type ControlSerializationDisposition string

const (
	ControlSerializationComplete  ControlSerializationDisposition = "complete"
	ControlSerializationMalformed ControlSerializationDisposition = "malformed"
	ControlSerializationTruncated ControlSerializationDisposition = "truncated"
	ControlSerializationTooLarge  ControlSerializationDisposition = "candidate_too_large"
	ControlSerializationTooMany   ControlSerializationDisposition = "too_many_blocks"
)

// quarantinedControlSpan is payload-free forensic evidence. In particular it
// never retains arguments, which can contain secrets, and it cannot be
// mistaken for a typed tool proposal by an action element.
type quarantinedControlSpan struct {
	Syntax      ControlSerializationSyntax
	Disposition ControlSerializationDisposition
	Bytes       int
	SHA256      string
	// sourceStart is the byte offset in the logical text stream. It is kept
	// internal: runtime audit exposes only the payload-free length and digest,
	// while completed-result reconstruction uses the offset to remove bytes
	// from their original PreparedOutput records without reordering text around
	// intervening tool or reasoning outputs.
	sourceStart int
}

// controlSerializationFilter removes recognized model-authored tool-control
// serialization while preserving ordinary prose. It is incremental: only a
// short possible marker or one bounded JSON candidate is retained, so normal
// speech does not wait for the complete model turn.
//
// Explicit wrappers (<tool_call> and Tool call:) become control as soon as a
// JSON object or array follows. Bare and fenced JSON become control only after
// a complete value has the strict tool-call shape. A closing wrapper is never
// searched for until the JSON value has structurally completed, which keeps a
// literal </tool_call> inside a quoted string from terminating the candidate.
type controlSerializationFilter struct {
	maxCandidateBytes  int
	maxBlocks          int
	pending            []byte
	active             *controlCandidate
	discard            *discardedControlCandidate
	lineOnlyWhitespace bool
	blocks             int
	sourceOffset       int
}

type controlCandidate struct {
	syntax        ControlSerializationSyntax
	raw           []byte
	jsonStart     int
	scan          int
	stack         []byte
	inString      bool
	escaped       bool
	jsonEnd       int
	malformed     bool
	decoded       bool
	valid         bool
	toolShape     bool
	suffixScan    int
	expressionEnd int
}

type discardedControlCandidate struct {
	syntax      ControlSerializationSyntax
	disposition ControlSerializationDisposition
	sourceStart int
	bytes       int
	digest      hash.Hash
}

type candidateDetection struct {
	start     int
	jsonStart int
	syntax    ControlSerializationSyntax
	found     bool
	hold      bool
	tooLarge  bool
}

func newControlSerializationFilter(maxCandidateBytes, maxBlocks int) controlSerializationFilter {
	return controlSerializationFilter{
		maxCandidateBytes:  maxCandidateBytes,
		maxBlocks:          maxBlocks,
		lineOnlyWhitespace: true,
	}
}

// push may be called at every possible byte boundary. The concatenation of
// its safe outputs and finish's safe output is independent of chunking.
func (filter *controlSerializationFilter) push(delta string) (string, []quarantinedControlSpan) {
	var safe strings.Builder
	var spans []quarantinedControlSpan
	for len(delta) > 0 {
		length := min(len(delta), filterInputSliceBytes)
		part := []byte(delta[:length])
		delta = delta[length:]
		if filter.discard != nil {
			filter.discard.append(part)
			continue
		}
		filter.pending = append(filter.pending, part...)
		filter.process(false, &safe, &spans)
	}
	return safe.String(), spans
}

func (filter *controlSerializationFilter) finish() (string, []quarantinedControlSpan) {
	var safe strings.Builder
	var spans []quarantinedControlSpan
	if filter.discard != nil {
		spans = append(spans, filter.discard.span())
		filter.reset()
		return "", spans
	}
	filter.process(true, &safe, &spans)
	if filter.active != nil {
		candidate := filter.active
		if explicitControlSyntax(candidate.syntax) {
			disposition := ControlSerializationTruncated
			if candidate.malformed {
				disposition = ControlSerializationMalformed
			}
			spans = append(spans, filter.spanFor(candidate.syntax, disposition, candidate.raw))
			filter.consumeSource(candidate.raw)
		} else {
			// An incomplete bare value or a generic JSON fence has not acquired
			// tool-control semantics. It remains ordinary model prose.
			safe.Write(candidate.raw)
			filter.consumeSource(candidate.raw)
		}
	}
	if len(filter.pending) != 0 {
		safe.Write(filter.pending)
		filter.consumeSource(filter.pending)
	}
	filter.reset()
	return safe.String(), spans
}

func (filter *controlSerializationFilter) process(
	final bool, safe *strings.Builder, spans *[]quarantinedControlSpan,
) {
	for filter.discard == nil {
		if filter.active != nil {
			if len(filter.pending) != 0 {
				filter.active.raw = append(filter.active.raw, filter.pending...)
				filter.pending = nil
			}
			if !filter.processCandidate(final, safe, spans) {
				return
			}
			continue
		}
		if len(filter.pending) == 0 {
			return
		}
		detection := locateControlCandidate(
			filter.pending, filter.lineOnlyWhitespace, final, filter.maxCandidateBytes,
		)
		switch {
		case detection.tooLarge:
			if detection.start > 0 {
				safe.Write(filter.pending[:detection.start])
				filter.consumeSource(filter.pending[:detection.start])
			}
			candidateBytes := append([]byte(nil), filter.pending[detection.start:]...)
			filter.pending = nil
			disposition := ControlSerializationTooLarge
			if filter.blocks >= filter.maxBlocks {
				disposition = ControlSerializationTooMany
			}
			filter.beginDiscard(detection.syntax, disposition, candidateBytes)
			return
		case detection.found:
			if detection.start > 0 {
				safe.Write(filter.pending[:detection.start])
				filter.consumeSource(filter.pending[:detection.start])
			}
			candidateBytes := append([]byte(nil), filter.pending[detection.start:]...)
			filter.pending = nil
			if filter.blocks >= filter.maxBlocks {
				filter.beginDiscard(detection.syntax, ControlSerializationTooMany, candidateBytes)
				return
			}
			filter.active = &controlCandidate{
				syntax: detection.syntax, raw: candidateBytes,
				jsonStart: detection.jsonStart - detection.start,
				scan:      detection.jsonStart - detection.start,
			}
		case detection.hold:
			if detection.start > 0 {
				safe.Write(filter.pending[:detection.start])
				filter.consumeSource(filter.pending[:detection.start])
				filter.pending = append([]byte(nil), filter.pending[detection.start:]...)
			}
			return
		default:
			safe.Write(filter.pending)
			filter.consumeSource(filter.pending)
			filter.pending = nil
			return
		}
	}
}

// processCandidate returns true after resolving a candidate and false when it
// needs more bytes. Any bytes after the resolved block are returned to the
// ordinary scanner, allowing arbitrarily many blocks in one provider delta.
func (filter *controlSerializationFilter) processCandidate(
	final bool, safe *strings.Builder, spans *[]quarantinedControlSpan,
) bool {
	candidate := filter.active
	if candidate.jsonEnd == 0 && !candidate.malformed {
		candidate.scanJSON()
	}
	if candidate.jsonEnd > filter.maxCandidateBytes ||
		candidate.jsonEnd == 0 && len(candidate.raw) > filter.maxCandidateBytes {
		filter.beginDiscard(candidate.syntax, ControlSerializationTooLarge, candidate.raw)
		filter.active = nil
		return false
	}
	if candidate.malformed {
		if candidate.syntax == ControlSyntaxCallableExpression {
			return filter.processMalformedCallable(final, safe, spans)
		}
		if explicitControlSyntax(candidate.syntax) {
			if !final {
				return false
			}
			filter.quarantineActive(len(candidate.raw), ControlSerializationMalformed, spans)
			return true
		}
		// A malformed generic or bare JSON value has no formal tool-call
		// shape. It is ordinary text, not control-by-guesswork.
		filter.releaseActive(len(candidate.raw), safe)
		return true
	}
	if candidate.jsonEnd == 0 {
		return false
	}
	if !candidate.decoded {
		value, valid := decodeCompleteJSON(candidate.raw[candidate.jsonStart:candidate.jsonEnd])
		candidate.decoded = true
		candidate.valid = valid
		candidate.toolShape = valid && toolCallJSONValue(value)
	}
	valid, recognized := candidate.valid, candidate.toolShape
	switch candidate.syntax {
	case ControlSyntaxPrefixed:
		disposition := ControlSerializationComplete
		if !valid {
			disposition = ControlSerializationMalformed
		}
		filter.quarantineActive(candidate.jsonEnd, disposition, spans)
		return true
	case ControlSyntaxTagged:
		end, examined, complete, wait := candidate.closingWrapperEnd(toolCallClose, final)
		if filter.discardActiveIfOversized(examined) {
			return false
		}
		if wait {
			return false
		}
		if !complete {
			end = candidate.jsonEnd
		}
		disposition := ControlSerializationComplete
		if !valid {
			disposition = ControlSerializationMalformed
		} else if !complete {
			disposition = ControlSerializationTruncated
		}
		filter.quarantineActive(end, disposition, spans)
		return true
	case ControlSyntaxFunctionTag, ControlSyntaxFunctionCallTag:
		closeMarker := functionTagClose
		if candidate.syntax == ControlSyntaxFunctionCallTag {
			closeMarker = functionCallClose
		}
		end, examined, complete, wait := candidate.closingWrapperEnd(closeMarker, final)
		if filter.discardActiveIfOversized(examined) {
			return false
		}
		if wait {
			return false
		}
		if !complete {
			end = candidate.jsonEnd
		}
		disposition := ControlSerializationComplete
		if !valid {
			disposition = ControlSerializationMalformed
		} else if !complete {
			disposition = ControlSerializationTruncated
		}
		filter.quarantineActive(end, disposition, spans)
		return true
	case ControlSyntaxJSONFence:
		if !recognized {
			// Once the complete JSON value proves this is an ordinary fenced
			// value, release the withheld prefix immediately. Its arbitrarily
			// long closing whitespace/fence is ordinary prose and must not grow
			// parser state.
			filter.releaseActive(candidate.jsonEnd, safe)
			return true
		}
		end, examined, complete, wait := candidate.closingWrapperEnd(toolCallJSONFenceEnd, final)
		if filter.discardActiveIfOversized(examined) {
			return false
		}
		if wait {
			return false
		}
		if !complete {
			end = candidate.jsonEnd
		}
		disposition := ControlSerializationComplete
		if !complete {
			disposition = ControlSerializationTruncated
		}
		filter.quarantineActive(end, disposition, spans)
		return true
	case ControlSyntaxPrefixedJSONFence:
		end, examined, complete, wait := candidate.closingWrapperEnd(toolCallJSONFenceEnd, final)
		if filter.discardActiveIfOversized(examined) {
			return false
		}
		if wait {
			return false
		}
		if !complete {
			end = candidate.jsonEnd
		}
		disposition := ControlSerializationComplete
		if !complete {
			disposition = ControlSerializationTruncated
		}
		filter.quarantineActive(end, disposition, spans)
		return true
	case ControlSyntaxCallableExpression:
		return filter.processCallableCandidate(final, valid, safe, spans)
	case ControlSyntaxBareJSON:
		if recognized {
			filter.quarantineActive(candidate.jsonEnd, ControlSerializationComplete, spans)
		} else {
			filter.releaseActive(candidate.jsonEnd, safe)
		}
		return true
	default:
		filter.releaseActive(len(candidate.raw), safe)
		return true
	}
}

func (filter *controlSerializationFilter) processCallableCandidate(
	final, valid bool, safe *strings.Builder, spans *[]quarantinedControlSpan,
) bool {
	candidate := filter.active
	if !valid {
		candidate.malformed = true
		return filter.processMalformedCallable(final, safe, spans)
	}
	if candidate.suffixScan < candidate.jsonEnd {
		candidate.suffixScan = candidate.jsonEnd
	}
	if candidate.expressionEnd == 0 {
		for candidate.suffixScan < len(candidate.raw) &&
			(candidate.raw[candidate.suffixScan] == ' ' || candidate.raw[candidate.suffixScan] == '\t') {
			candidate.suffixScan++
		}
		if filter.discardActiveIfOversized(candidate.suffixScan) {
			return false
		}
		if candidate.suffixScan == len(candidate.raw) {
			if !final {
				return false
			}
			filter.quarantineActive(
				len(candidate.raw), ControlSerializationTruncated, spans,
			)
			return true
		}
		if candidate.raw[candidate.suffixScan] != ')' {
			candidate.malformed = true
			return filter.processMalformedCallable(final, safe, spans)
		}
		candidate.suffixScan++
		if filter.discardActiveIfOversized(candidate.suffixScan) {
			return false
		}
		candidate.expressionEnd = candidate.suffixScan
	}
	for candidate.suffixScan < len(candidate.raw) &&
		(candidate.raw[candidate.suffixScan] == ' ' || candidate.raw[candidate.suffixScan] == '\t') {
		candidate.suffixScan++
	}
	if filter.discardActiveIfOversized(candidate.suffixScan) {
		return false
	}
	if candidate.suffixScan == len(candidate.raw) {
		if !final {
			return false
		}
		filter.quarantineActive(candidate.suffixScan, ControlSerializationComplete, spans)
		return true
	}
	if candidate.raw[candidate.suffixScan] == '\n' || candidate.raw[candidate.suffixScan] == '\r' {
		filter.quarantineActive(candidate.suffixScan, ControlSerializationComplete, spans)
		return true
	}
	// A complete expression followed by prose on the same line is an example
	// or discussion, not a whole-record control serialization.
	filter.releaseActive(candidate.expressionEnd, safe)
	return true
}

func (filter *controlSerializationFilter) discardActiveIfOversized(examined int) bool {
	if examined <= filter.maxCandidateBytes {
		return false
	}
	candidate := filter.active
	filter.beginDiscard(candidate.syntax, ControlSerializationTooLarge, candidate.raw)
	filter.active = nil
	return true
}

func (filter *controlSerializationFilter) processMalformedCallable(
	final bool, _ *strings.Builder, spans *[]quarantinedControlSpan,
) bool {
	candidate := filter.active
	if !final {
		return false
	}
	filter.quarantineActive(len(candidate.raw), ControlSerializationMalformed, spans)
	return true
}

func (candidate *controlCandidate) scanJSON() {
	for candidate.scan < len(candidate.raw) {
		character := candidate.raw[candidate.scan]
		candidate.scan++
		if candidate.inString {
			if candidate.escaped {
				candidate.escaped = false
				continue
			}
			switch character {
			case '\\':
				candidate.escaped = true
			case '"':
				candidate.inString = false
			}
			continue
		}
		switch character {
		case '"':
			candidate.inString = true
		case '{', '[':
			candidate.stack = append(candidate.stack, character)
		case '}', ']':
			if len(candidate.stack) == 0 ||
				!matchingJSONDelimiters(candidate.stack[len(candidate.stack)-1], character) {
				candidate.malformed = true
				return
			}
			candidate.stack = candidate.stack[:len(candidate.stack)-1]
			if len(candidate.stack) == 0 {
				candidate.jsonEnd = candidate.scan
				return
			}
		}
	}
}

func (filter *controlSerializationFilter) quarantineActive(
	end int, disposition ControlSerializationDisposition, spans *[]quarantinedControlSpan,
) {
	candidate := filter.active
	end = min(max(end, 0), len(candidate.raw))
	if end > filter.maxCandidateBytes {
		disposition = ControlSerializationTooLarge
	}
	withheld := candidate.raw[:end]
	*spans = append(*spans, filter.spanFor(candidate.syntax, disposition, withheld))
	filter.blocks++
	filter.consumeSource(withheld)
	tail := append([]byte(nil), candidate.raw[end:]...)
	filter.active = nil
	filter.pending = append(tail, filter.pending...)
}

func (filter *controlSerializationFilter) releaseActive(end int, safe *strings.Builder) {
	candidate := filter.active
	end = min(max(end, 0), len(candidate.raw))
	released := candidate.raw[:end]
	safe.Write(released)
	filter.consumeSource(released)
	tail := append([]byte(nil), candidate.raw[end:]...)
	filter.active = nil
	filter.pending = append(tail, filter.pending...)
}

func (filter *controlSerializationFilter) beginDiscard(
	syntax ControlSerializationSyntax, disposition ControlSerializationDisposition, data []byte,
) {
	discard := &discardedControlCandidate{
		syntax: syntax, disposition: disposition,
		sourceStart: filter.sourceOffset, digest: sha256.New(),
	}
	discard.append(data)
	filter.discard = discard
	filter.pending = nil
}

func (discard *discardedControlCandidate) append(data []byte) {
	discard.bytes += len(data)
	_, _ = discard.digest.Write(data)
}

func (discard *discardedControlCandidate) span() quarantinedControlSpan {
	return quarantinedControlSpan{
		Syntax: discard.syntax, Disposition: discard.disposition,
		Bytes: discard.bytes, SHA256: hex.EncodeToString(discard.digest.Sum(nil)),
		sourceStart: discard.sourceStart,
	}
}

func (filter *controlSerializationFilter) spanFor(
	syntax ControlSerializationSyntax, disposition ControlSerializationDisposition, data []byte,
) quarantinedControlSpan {
	span := spanFor(syntax, disposition, data)
	span.sourceStart = filter.sourceOffset
	return span
}

func spanFor(
	syntax ControlSerializationSyntax, disposition ControlSerializationDisposition, data []byte,
) quarantinedControlSpan {
	digest := sha256.Sum256(data)
	return quarantinedControlSpan{
		Syntax: syntax, Disposition: disposition,
		Bytes: len(data), SHA256: hex.EncodeToString(digest[:]),
	}
}

func (filter *controlSerializationFilter) consumeSource(data []byte) {
	filter.sourceOffset += len(data)
	for _, character := range data {
		switch character {
		case '\n', '\r':
			filter.lineOnlyWhitespace = true
		case ' ', '\t':
			// Preserve the line's current eligibility.
		default:
			filter.lineOnlyWhitespace = false
		}
	}
}

func (filter *controlSerializationFilter) reset() {
	filter.pending = nil
	filter.active = nil
	filter.discard = nil
	filter.lineOnlyWhitespace = true
	filter.blocks = 0
	filter.sourceOffset = 0
}

func locateControlCandidate(
	data []byte, lineOnlyWhitespace, final bool, maxCandidateBytes int,
) candidateDetection {
	lineEligible := lineOnlyWhitespace
	for index := 0; index < len(data); index++ {
		if lineEligible && (data[index] == '{' || data[index] == '[') {
			return candidateDetection{
				start: index, jsonStart: index,
				syntax: ControlSyntaxBareJSON, found: true,
			}
		}
		if lineEligible && identifierStart(data[index]) {
			if detection, applicable := locateCallableExpression(
				data, index, final, maxCandidateBytes,
			); applicable {
				return detection
			}
		}
		for _, original := range []struct {
			text   string
			syntax ControlSerializationSyntax
		}{
			{toolCallOpen, ControlSyntaxTagged},
			{toolCallPrefix, ControlSyntaxPrefixed},
			{toolCallUnderscore, ControlSyntaxPrefixed},
			{toolCallJSONFenceOpen, ControlSyntaxJSONFence},
			{functionCallOpen, ControlSyntaxFunctionCallTag},
		} {
			marker := original
			matched, partial := asciiFoldMarker(data[index:], marker.text)
			if partial && !final {
				return candidateDetection{start: index, hold: true}
			}
			if !matched {
				continue
			}
			markerEnd := index + len(marker.text)
			value := skipJSONWhitespace(data, markerEnd)
			if value > markerEnd && value-index > maxCandidateBytes {
				if marker.syntax == ControlSyntaxJSONFence {
					continue
				}
				return candidateDetection{
					start: index, syntax: marker.syntax, tooLarge: true,
				}
			}
			if marker.syntax == ControlSyntaxPrefixed {
				if fence, fencePartial := asciiFoldMarker(data[value:], toolCallJSONFenceOpen); fence {
					markerEnd = value + len(toolCallJSONFenceOpen)
					value = skipJSONWhitespace(data, markerEnd)
					marker.syntax = ControlSyntaxPrefixedJSONFence
					if value > markerEnd && value-index > maxCandidateBytes {
						return candidateDetection{
							start: index, syntax: marker.syntax, tooLarge: true,
						}
					}
				} else if fencePartial && !final {
					if len(data)-index > maxCandidateBytes {
						return candidateDetection{
							start: index, syntax: marker.syntax, tooLarge: true,
						}
					}
					return candidateDetection{start: index, hold: true}
				}
			}
			if value == len(data) && !final {
				return candidateDetection{start: index, hold: true}
			}
			if value < len(data) && (data[value] == '{' || data[value] == '[') {
				if value-index > maxCandidateBytes {
					if marker.syntax == ControlSyntaxJSONFence {
						continue
					}
					return candidateDetection{
						start: index, syntax: marker.syntax, tooLarge: true,
					}
				}
				return candidateDetection{
					start: index, jsonStart: value,
					syntax: marker.syntax, found: true,
				}
			}
		}
		if detection, applicable := locateFunctionTag(
			data, index, final, maxCandidateBytes,
		); applicable {
			return detection
		}
		switch data[index] {
		case '\n', '\r':
			lineEligible = true
		case ' ', '\t':
			// Preserve state.
		default:
			lineEligible = false
		}
	}
	return candidateDetection{}
}

func locateCallableExpression(
	data []byte, start int, final bool, maxCandidateBytes int,
) (candidateDetection, bool) {
	nameEnd := start + 1
	for nameEnd < len(data) && identifierContinue(data[nameEnd]) {
		nameEnd++
		if nameEnd-start > maximumSerializedFunctionNameBytes {
			return candidateDetection{}, false
		}
	}
	if nameEnd == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	open := skipHorizontalWhitespace(data, nameEnd)
	if open > nameEnd && open-start > maxCandidateBytes {
		// Until '(' appears this is only prose beginning with an identifier.
		// Release it once bounded lookahead is exhausted; later bytes may not
		// reinterpret the already-released prefix as a callable expression.
		return candidateDetection{}, false
	}
	if open == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if open >= len(data) || data[open] != '(' {
		return candidateDetection{}, false
	}
	if open+1-start > maxCandidateBytes {
		return candidateDetection{
			start: start, syntax: ControlSyntaxCallableExpression, tooLarge: true,
		}, true
	}
	value := skipJSONWhitespace(data, open+1)
	if value-start > maxCandidateBytes {
		return candidateDetection{
			start: start, syntax: ControlSyntaxCallableExpression, tooLarge: true,
		}, true
	}
	if value == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if value >= len(data) || (data[value] != '{' && data[value] != '[') {
		return candidateDetection{}, false
	}
	return candidateDetection{
		start: start, jsonStart: value,
		syntax: ControlSyntaxCallableExpression, found: true,
	}, true
}

func locateFunctionTag(
	data []byte, start int, final bool, maxCandidateBytes int,
) (candidateDetection, bool) {
	matched, partial := asciiFoldMarker(data[start:], functionTagOpen)
	if partial && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if !matched {
		return candidateDetection{}, false
	}
	nameStart := start + len(functionTagOpen)
	if nameStart == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if nameStart >= len(data) || !identifierStart(data[nameStart]) {
		return candidateDetection{}, false
	}
	nameEnd := nameStart + 1
	for nameEnd < len(data) && identifierContinue(data[nameEnd]) {
		nameEnd++
		if nameEnd-nameStart > maximumSerializedFunctionNameBytes {
			return candidateDetection{}, false
		}
	}
	if nameEnd == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if nameEnd >= len(data) || data[nameEnd] != '>' {
		return candidateDetection{}, false
	}
	markerEnd := nameEnd + 1
	value := skipJSONWhitespace(data, markerEnd)
	if value > markerEnd && value-start > maxCandidateBytes {
		return candidateDetection{
			start: start, syntax: ControlSyntaxFunctionTag, tooLarge: true,
		}, true
	}
	if value == len(data) && !final {
		return candidateDetection{start: start, hold: true}, true
	}
	if value >= len(data) || (data[value] != '{' && data[value] != '[') {
		return candidateDetection{}, false
	}
	if value-start > maxCandidateBytes {
		return candidateDetection{
			start: start, syntax: ControlSyntaxFunctionTag, tooLarge: true,
		}, true
	}
	return candidateDetection{
		start: start, jsonStart: value,
		syntax: ControlSyntaxFunctionTag, found: true,
	}, true
}

func identifierStart(character byte) bool {
	return character == '_' || character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z'
}

func identifierContinue(character byte) bool {
	return identifierStart(character) || character >= '0' && character <= '9' ||
		character == '-' || character == '.' || character == ':'
}

func asciiFoldMarker(data []byte, marker string) (matched, partial bool) {
	length := min(len(data), len(marker))
	for index := 0; index < length; index++ {
		if lowerASCII(data[index]) != lowerASCII(marker[index]) {
			return false, false
		}
	}
	if len(data) < len(marker) {
		return false, true
	}
	return true, false
}

func (candidate *controlCandidate) closingWrapperEnd(
	marker string, final bool,
) (end, examined int, complete, wait bool) {
	if candidate.suffixScan < candidate.jsonEnd {
		candidate.suffixScan = candidate.jsonEnd
	}
	for candidate.suffixScan < len(candidate.raw) {
		switch candidate.raw[candidate.suffixScan] {
		case ' ', '\t', '\r', '\n':
			candidate.suffixScan++
		default:
			goto marker
		}
	}

marker:
	matched, partial := asciiFoldMarker(candidate.raw[candidate.suffixScan:], marker)
	if matched {
		end = candidate.suffixScan + len(marker)
		return end, end, true, false
	}
	if partial && !final {
		return 0, len(candidate.raw), false, true
	}
	examined = candidate.suffixScan
	if examined < len(candidate.raw) {
		examined++
	}
	return candidate.jsonEnd, examined, false, false
}

func skipJSONWhitespace(data []byte, start int) int {
	for start < len(data) {
		switch data[start] {
		case ' ', '\t', '\r', '\n':
			start++
		default:
			return start
		}
	}
	return start
}

func skipHorizontalWhitespace(data []byte, start int) int {
	for start < len(data) && (data[start] == ' ' || data[start] == '\t') {
		start++
	}
	return start
}

func explicitControlSyntax(syntax ControlSerializationSyntax) bool {
	switch syntax {
	case ControlSyntaxTagged, ControlSyntaxPrefixed, ControlSyntaxPrefixedJSONFence,
		ControlSyntaxCallableExpression, ControlSyntaxFunctionTag, ControlSyntaxFunctionCallTag:
		return true
	default:
		return false
	}
}

func matchingJSONDelimiters(open, close byte) bool {
	return open == '{' && close == '}' || open == '[' && close == ']'
}

func decodeCompleteJSON(source []byte) (any, bool) {
	var value any
	if err := json.Unmarshal(source, &value); err != nil {
		return nil, false
	}
	return value, true
}

func toolCallJSONValue(value any) bool {
	switch typed := value.(type) {
	case []any:
		if len(typed) == 0 {
			return false
		}
		for _, item := range typed {
			if !toolCallJSONValue(item) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, key := range []string{"function", "function_call", "tool_call"} {
			if nested, ok := typed[key]; ok && toolCallJSONValue(nested) {
				return true
			}
		}
		if calls, ok := typed["tool_calls"]; ok && toolCallJSONValue(calls) {
			return true
		}
		return directToolCallJSON(typed)
	default:
		return false
	}
}

func directToolCallJSON(object map[string]any) bool {
	name, named := object["name"].(string)
	if !named || strings.TrimSpace(name) == "" {
		return false
	}
	arguments, hasArguments := object["arguments"]
	parameters, hasParameters := object["parameters"]
	if hasArguments == hasParameters {
		return false
	}
	value := arguments
	if hasParameters {
		value = parameters
	}
	switch value.(type) {
	case string, map[string]any, []any, nil:
		return true
	default:
		return false
	}
}

func lowerASCII(character byte) byte {
	if character >= 'A' && character <= 'Z' {
		return character + ('a' - 'A')
	}
	return character
}
