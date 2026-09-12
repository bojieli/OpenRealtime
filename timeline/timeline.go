// Package timeline reduces the runtime's debug stream to the handful of
// events that tell the story of a turn, in five lanes: what the recogniser
// heard, what the interaction policy chose, what the language model was asked
// and said, what was synthesised and played, and what the background passes
// (standing-instruction extraction and its follow-up questions) concluded.
//
// It exists because the debug stream is complete and therefore unreadable:
// a few minutes of conversation is twenty thousand events, most of them the
// graph's own bookkeeping. Reading a turn back - why did it speak there, why
// did it not count that animal - means finding the twenty events that matter
// among them, and the vocabulary for those twenty is defined once, here, so
// the log file on disk and the timeline drawn in the browser show the same
// thing.
package timeline

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bojieli/OpenRealtime/binding"
)

// Lane is one row of the timeline.
type Lane string

const (
	// LaneASR is the recogniser: speech activity and each transcript revision.
	LaneASR Lane = "asr"
	// LanePolicy is the interaction policy: one choice per transcript event.
	LanePolicy Lane = "policy"
	// LaneModel is the language model behind the voice: request, text, outcome.
	LaneModel Lane = "model"
	// LaneTTS is synthesis and playback of what the model said.
	LaneTTS Lane = "tts"
	// LaneBackground is every model question asked off the critical path:
	// standing-instruction extraction, grounding, and the readings of a pin.
	LaneBackground Lane = "background"
)

// Lanes lists every lane in display order.
func Lanes() []Lane {
	return []Lane{LaneASR, LanePolicy, LaneModel, LaneTTS, LaneBackground}
}

// Event is one mark on the timeline.
type Event struct {
	Lane Lane `json:"lane"`
	// Kind names what happened within the lane: partial, final, speak, result,
	// synthesis, playback, extract, ground...
	Kind string `json:"kind"`
	// Phase is "start", "end", or "point". A start and an end with the same
	// Span draw as one bar.
	Phase string `json:"phase"`
	// Span joins a start with its end: an utterance id, a generation id, a
	// speech stream id.
	Span string `json:"span,omitempty"`
	// Detail is metadata - revision numbers, confidences, durations, codes.
	// It never carries conversation content, so it can be shown without the
	// payload opt-in.
	Detail string `json:"detail,omitempty"`
	// Text is conversation content: a transcript, what the model said, what a
	// background question answered. It is a debug payload and is withheld from
	// a client that did not ask for payloads.
	Text string `json:"text,omitempty"`
	// DurationMS is how long the thing took, when the event reports it.
	DurationMS float64 `json:"duration_ms,omitempty"`
}

// Names lists the debug event names that project onto the timeline, so a
// caller can skip the JSON round trip for everything else.
var names = map[string]struct{}{
	"vad.speech_started": {}, "vad.speech_stopped": {}, "asr.transcript": {},
	"semantic_decision": {}, "semantic_admission_outcome": {},
	"invocation_outcome": {}, "model_result": {}, "model_outcome": {},
	"tts.utterance_started": {}, "tts.first_audio": {}, "tts.utterance_completed": {},
	"tts_status": {}, "playback_status": {}, "speech_cancel_request": {},
	// The stages a tool call passes through on its way to being executed.
	"provenance_outcome": {}, "admission_action_outcome": {}, "lookup_outcome": {},
	"argument_normalization_outcome": {}, "confirmation_outcome": {}, "target_fence_outcome": {},
	"canonical_call_outcome": {}, "ledger_outcome": {}, "dispatch_outcome": {},
	"result_commit_outcome": {}, "client_tool_result_join_outcome": {},
	"model_commit_outcome": {},
}

// Projects reports whether a debug event of this name has a place on the
// timeline.
func Projects(name string) bool {
	_, ok := names[name]
	return ok
}

// Project reduces one debug event to its timeline events. Most debug events
// project to nothing; a policy decision that also ran the standing-instruction
// pass projects to several, one per background question.
//
// The debug payload holds whatever Go value the element emitted. Rather than
// know every element's types, the value is read through its JSON form, which
// is also exactly what the log and the wire carry.
func Project(entry binding.DebugEvent) []Event {
	if !Projects(entry.Name) {
		return nil
	}
	value := payloadValue(entry)
	switch entry.Name {
	case "vad.speech_started":
		return []Event{{Lane: LaneASR, Kind: "speech", Phase: "start", Span: entry.CorrelationID}}
	case "vad.speech_stopped":
		detail := ""
		if end, ok := number(entry.Attributes["audio_end_ms"]); ok {
			start, _ := number(entry.Attributes["audio_start_ms"])
			detail = formatMS(end - start)
		}
		return []Event{{Lane: LaneASR, Kind: "speech", Phase: "end", Span: entry.CorrelationID, Detail: detail}}
	case "asr.transcript":
		// The gateway reports every revision the binding delivered, whichever
		// binding it was; the graph's own transcript_events output would say
		// the same thing a second time.
		kind := "partial"
		if boolean(entry.Attributes["final"]) {
			kind = "final"
		}
		detail := ""
		if revision, ok := number(entry.Attributes["revision"]); ok && revision > 0 {
			detail = "rev " + strconv.FormatInt(int64(revision), 10)
		}
		return []Event{{Lane: LaneASR, Kind: kind, Phase: "point", Detail: detail, Text: text(entry.Payload["text"])}}
	case "semantic_decision":
		return projectDecision(value)
	case "semantic_admission_outcome":
		kind := text(value["kind"])
		if kind != "refused" && kind != "failed" && kind != "canceled" {
			return nil
		}
		return []Event{{
			Lane: LanePolicy, Kind: kind, Phase: "point",
			Detail: strings.TrimSpace(text(value["code"]) + " " + text(value["message"])),
		}}
	case "provenance_outcome", "admission_action_outcome", "lookup_outcome", "argument_normalization_outcome",
		"confirmation_outcome", "target_fence_outcome", "canonical_call_outcome", "ledger_outcome",
		"dispatch_outcome", "result_commit_outcome", "client_tool_result_join_outcome":
		// One mark per stage a tool call passes; a refusal says why, and
		// that is the whole reason these are here.
		kind := text(value["kind"])
		// Every trajectory reply reaches every commit element, and each one
		// that was not waiting for it says so; that is plumbing, not the
		// turn's story.
		if kind == "ignored" && strings.HasPrefix(text(value["code"]), "unknown_") {
			return nil
		}
		detail := strings.TrimSpace(text(value["stage"]) + "/" + text(value["operation"]) + " " + kind)
		if code := text(value["code"]); code != "" {
			detail += " " + code
		}
		if message := strings.TrimSpace(text(value["message"])); message != "" {
			detail += " " + truncate(message, 160)
		}
		return []Event{{
			Lane: LaneModel, Kind: "action", Phase: "point", Span: text(value["call_id"]),
			Detail: detail,
		}}
	case "model_commit_outcome":
		// What the model produced reaching the trajectory, or failing to: a
		// tool proposal that never became canonical is a key never pressed,
		// and it waits in silence otherwise.
		kind := text(value["kind"])
		if kind == "ignored" && strings.HasPrefix(text(value["code"]), "unknown_") {
			return nil
		}
		detail := kind
		if code := text(value["code"]); code != "" {
			detail += " " + code
		}
		if message := strings.TrimSpace(text(value["message"])); message != "" {
			detail += " " + truncate(message, 160)
		}
		if items := stringList(value["item_ids"]); len(items) > 0 {
			detail += " " + strconv.Itoa(len(items)) + " items"
		}
		return []Event{{Lane: LaneModel, Kind: "commit", Phase: "point", Span: text(value["run_id"]), Detail: detail}}
	case "invocation_outcome":
		kind := text(value["kind"])
		if kind == "updated" {
			// The invocation being (re)configured is session bookkeeping, not
			// a request to the model.
			return nil
		}
		span := text(value["generation_id"])
		detail := ""
		if version, ok := number(value["context_version"]); ok {
			detail = "context v" + strconv.FormatInt(int64(version), 10)
		}
		if choice := text(value["choice"]); choice != "" {
			detail = strings.TrimSpace(detail + " " + choice)
		}
		if kind == "emitted" {
			return []Event{{Lane: LaneModel, Kind: "request", Phase: "start", Span: span, Detail: detail}}
		}
		if code := text(value["code"]); code != "" {
			detail = strings.TrimSpace(detail + " " + code)
		}
		return []Event{{Lane: LaneModel, Kind: kind, Phase: "point", Span: span, Detail: detail}}
	case "model_result":
		detail := ""
		if descriptor, ok := value["descriptor"].(map[string]any); ok {
			detail = text(descriptor["model"])
		}
		return []Event{{
			Lane: LaneModel, Kind: "result", Phase: "point", Span: text(value["run_id"]),
			Detail: detail, Text: text(value["assistant_text"]),
		}}
	case "model_outcome":
		duration := 0.0
		if nanoseconds, ok := number(value["duration_ns"]); ok {
			duration = nanoseconds / 1e6
		}
		detail := strings.TrimSpace(text(value["code"]) + " " + formatMS(duration))
		if message := strings.TrimSpace(text(value["message"])); message != "" {
			detail += " " + truncate(message, 160)
		}
		return []Event{{
			Lane: LaneModel, Kind: text(value["kind"]), Phase: "end", Span: text(value["run_id"]),
			Detail: detail, DurationMS: duration,
		}}
	case "tts.utterance_started":
		return []Event{{
			Lane: LaneTTS, Kind: "speaking", Phase: "start", Span: entry.CorrelationID,
			Text: text(entry.Payload["text"]),
		}}
	case "tts.first_audio":
		detail := ""
		if bytes, ok := number(entry.Attributes["encoded_bytes"]); ok {
			detail = strconv.FormatInt(int64(bytes), 10) + " bytes"
		}
		return []Event{{Lane: LaneTTS, Kind: "first_audio", Phase: "point", Span: entry.CorrelationID, Detail: detail}}
	case "tts.utterance_completed":
		detail := "sent"
		if !boolean(entry.Attributes["completed"]) {
			detail = "cut"
		}
		if played, ok := number(entry.Attributes["played_ms"]); ok {
			detail += " " + formatMS(played)
		}
		return []Event{{Lane: LaneTTS, Kind: "audio_sent", Phase: "point", Span: entry.CorrelationID, Detail: detail}}
	case "tts_status", "playback_status":
		return projectTransition(value)
	case "speech_cancel_request":
		span := text(entry.Payload["value"])
		if span == "" {
			span = text(value["utterance_id"])
		}
		return []Event{{Lane: LaneTTS, Kind: "cancel", Phase: "point", Span: span}}
	}
	return nil
}

// projectDecision renders one policy choice, and the background pass that ran
// alongside it when it did.
func projectDecision(value map[string]any) []Event {
	choice := text(value["choice"])
	if choice == "" {
		return nil
	}
	parts := []string{}
	if event := text(value["event"]); event != "" {
		parts = append(parts, "on "+event)
	}
	stage := text(value["decision_stage"])
	switch stage {
	case "state":
		parts = append(parts, "no model asked")
	case "clock":
		parts = append(parts, "quiet clock")
	default:
		if confidence, ok := number(value["confidence"]); ok && confidence > 0 {
			parts = append(parts, "conf "+strconv.FormatFloat(confidence, 'f', 2, 64))
		}
	}
	duration := 0.0
	if started, ok := number(value["started_ns"]); ok {
		if finished, ok := number(value["finished_ns"]); ok && finished > started {
			duration = (finished - started) / 1e6
			if stage != "state" && stage != "clock" {
				parts = append(parts, formatMS(duration))
			}
		}
	}
	for _, field := range []string{"standing_pinned", "standing_revoked"} {
		if count, ok := number(value[field]); ok && count > 0 {
			parts = append(parts, strings.TrimPrefix(field, "standing_")+" "+strconv.FormatInt(int64(count), 10))
		}
	}
	if count, ok := number(value["standing_after"]); ok {
		parts = append(parts, "pins "+strconv.FormatInt(int64(count), 10))
	}
	if failure := text(value["failure"]); failure != "" {
		parts = append(parts, failure)
	}
	if questions, ok := value["questions"].([]any); ok && len(questions) > 0 {
		answers := make([]string, 0, len(questions))
		for _, raw := range questions {
			if question, ok := raw.(map[string]any); ok {
				answers = append(answers, text(question["question"])+"="+text(question["answer"]))
			}
		}
		parts = append(parts, strings.Join(answers, " "))
	}
	events := []Event{{
		Lane: LanePolicy, Kind: choice, Phase: "point",
		Detail: strings.Join(parts, " "), Text: text(value["evidence"]), DurationMS: duration,
	}}
	standing, ok := value["standing"].(map[string]any)
	if !ok {
		return events
	}
	if calls, ok := standing["calls"].([]any); ok {
		for _, raw := range calls {
			call, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			duration, _ := number(call["duration_ms"])
			events = append(events, Event{
				Lane: LaneBackground, Kind: text(call["question"]), Phase: "point",
				Detail: formatMS(duration), Text: text(call["answer"]), DurationMS: duration,
			})
		}
	}
	summary := []string{}
	for _, field := range []string{"pinned", "revoked", "dropped"} {
		if lines := stringList(standing[field]); len(lines) > 0 {
			summary = append(summary, field+": "+strings.Join(lines, "; "))
		}
	}
	if failure := text(standing["failure"]); failure != "" {
		summary = append(summary, "failed: "+failure)
	}
	if len(summary) == 0 {
		summary = append(summary, "nothing set")
	}
	events = append(events, Event{
		Lane: LaneBackground, Kind: "standing", Phase: "point",
		Detail: "after \"" + truncate(text(standing["utterance"]), 60) + "\"",
		Text:   strings.Join(summary, " | "),
	})
	return events
}

// projectTransition renders a synthesis or playback state change.
func projectTransition(value map[string]any) []Event {
	stage, state := text(value["stage"]), text(value["state"])
	span := text(value["utterance_id"])
	detail := ""
	if reason := text(value["reason"]); reason != "" {
		detail = reason
	}
	if played, ok := number(value["played_ns"]); ok && played > 0 {
		detail = strings.TrimSpace(detail + " played " + formatMS(played/1e6))
	}
	switch stage + "/" + state {
	case "synthesis/generating":
		return []Event{{Lane: LaneTTS, Kind: "synthesis", Phase: "start", Span: span}}
	case "synthesis/generated":
		return []Event{{Lane: LaneTTS, Kind: "synthesis", Phase: "end", Span: span, Detail: detail}}
	case "synthesis/cancelled", "synthesis/failed", "synthesis/refused":
		return []Event{{Lane: LaneTTS, Kind: "synthesis", Phase: "end", Span: span, Detail: strings.TrimSpace(state + " " + detail)}}
	case "playback/queued":
		return []Event{{Lane: LaneTTS, Kind: "queued", Phase: "point", Span: span}}
	case "playback/emitting":
		return []Event{{Lane: LaneTTS, Kind: "playback", Phase: "start", Span: span}}
	case "playback/played":
		return []Event{{Lane: LaneTTS, Kind: "playback", Phase: "end", Span: span, Detail: detail}}
	case "playback/cancelled", "playback/failed", "playback/refused":
		return []Event{{Lane: LaneTTS, Kind: "playback", Phase: "end", Span: span, Detail: strings.TrimSpace(state + " " + detail)}}
	}
	return nil
}

// Line renders an event as one line of the timeline log.
func (event Event) Line(at time.Time, session string) string {
	var line strings.Builder
	line.WriteString(at.UTC().Format("2006-01-02T15:04:05.000Z"))
	line.WriteString(" ")
	line.WriteString(session)
	line.WriteString(" ")
	line.WriteString(fmt.Sprintf("%-10s", strings.ToUpper(string(event.Lane))))
	line.WriteString(" ")
	kind := event.Kind
	switch event.Phase {
	case "start":
		kind += " start"
	case "end":
		kind += " end"
	}
	line.WriteString(fmt.Sprintf("%-16s", kind))
	if event.Detail != "" {
		line.WriteString(" " + event.Detail)
	}
	if event.Text != "" {
		line.WriteString(" " + strconv.Quote(event.Text))
	}
	if event.Span != "" {
		line.WriteString(" [" + shortSpan(event.Span) + "]")
	}
	return line.String()
}

// Writer appends timeline lines to one destination from every session, in
// arrival order.
type Writer struct {
	mu  sync.Mutex
	out io.Writer
}

// NewWriter wraps a destination. A nil destination yields a writer that
// discards, so callers need not test for one.
func NewWriter(out io.Writer) *Writer {
	return &Writer{out: out}
}

// Write appends one line per event.
func (writer *Writer) Write(at time.Time, session string, events []Event) error {
	if writer == nil || writer.out == nil || len(events) == 0 {
		return nil
	}
	var block strings.Builder
	for _, event := range events {
		block.WriteString(event.Line(at, session))
		block.WriteByte('\n')
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	_, err := io.WriteString(writer.out, block.String())
	return err
}

// payloadValue reads the element value out of a debug payload through its
// JSON form. Elements emit their own Go types; their wire and log form is the
// JSON, and that is the one shape every emitter shares.
func payloadValue(entry binding.DebugEvent) map[string]any {
	raw, ok := entry.Payload["value"]
	if !ok {
		return nil
	}
	if value, ok := raw.(map[string]any); ok {
		return value
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil
	}
	return value
}

func text(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	case nil:
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	var asString string
	if json.Unmarshal(encoded, &asString) == nil {
		return asString
	}
	return ""
}

func number(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		return parsed, err == nil
	}
	return 0, false
}

func boolean(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func stringList(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(items))
	for _, item := range items {
		if line := text(item); line != "" {
			result = append(result, line)
		}
	}
	return result
}

func formatMS(milliseconds float64) string {
	if milliseconds <= 0 {
		return ""
	}
	if milliseconds < 10 {
		return strconv.FormatFloat(milliseconds, 'f', 1, 64) + " ms"
	}
	return strconv.FormatInt(int64(milliseconds+0.5), 10) + " ms"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// shortSpan keeps a span identifiable without printing a whole digest.
func shortSpan(span string) string {
	if len(span) <= 24 {
		return span
	}
	return span[:8] + "…" + span[len(span)-12:]
}

// Sorted returns the lane names in display order, for callers that render a
// legend from the vocabulary rather than hard-coding it.
func Sorted(lanes map[Lane]struct{}) []Lane {
	result := make([]Lane, 0, len(lanes))
	for lane := range lanes {
		result = append(result, lane)
	}
	order := map[Lane]int{}
	for index, lane := range Lanes() {
		order[lane] = index
	}
	sort.Slice(result, func(i, j int) bool { return order[result[i]] < order[result[j]] })
	return result
}
