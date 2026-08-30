package acoustic

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	perceptionelements "github.com/bojieli/OpenRealtime/elements/perception"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

type boundedSet struct {
	limit int
	order []string
	set   map[string]struct{}
}

func newBoundedSet(limit int) boundedSet {
	return boundedSet{limit: limit, set: make(map[string]struct{}, limit)}
}

func (memory *boundedSet) Add(value string) {
	if value == "" || memory.limit <= 0 {
		return
	}
	if _, found := memory.set[value]; found {
		return
	}
	if len(memory.order) == memory.limit {
		delete(memory.set, memory.order[0])
		copy(memory.order, memory.order[1:])
		memory.order[len(memory.order)-1] = value
	} else {
		memory.order = append(memory.order, value)
	}
	memory.set[value] = struct{}{}
}

func (memory *boundedSet) Has(value string) bool {
	_, found := memory.set[value]
	return found
}

func (memory *boundedSet) Len() int { return len(memory.order) }

type receivedInput struct {
	kind     string
	envelope element.Envelope
}

func receiveInputs(
	ctx context.Context, kind string, input element.InputPort,
	destination chan<- receivedInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalReceive(ctx, err) {
			return
		}
		if err != nil {
			sendFailure(ctx, failures, err)
			return
		}
		select {
		case destination <- receivedInput{kind: kind, envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func receiveInterrupts(
	ctx context.Context, input element.InputPort, destination chan<- element.Envelope,
	failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		envelope, err := input.Receive(ctx)
		if terminalReceive(ctx, err) {
			return
		}
		if err != nil {
			sendFailure(ctx, failures, err)
			return
		}
		select {
		case destination <- envelope:
		case <-ctx.Done():
			return
		}
	}
}

func receivePermittedAudio(
	ctx context.Context, input element.InputPort, permits <-chan struct{},
	destination chan<- receivedInput, failures chan<- error, wait *sync.WaitGroup,
) {
	defer wait.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-permits:
		}
		envelope, err := input.Receive(ctx)
		if terminalReceive(ctx, err) {
			return
		}
		if err != nil {
			sendFailure(ctx, failures, err)
			return
		}
		select {
		case destination <- receivedInput{kind: "audio", envelope: envelope}:
		case <-ctx.Done():
			return
		}
	}
}

func terminalReceive(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}

func sendFailure(ctx context.Context, failures chan<- error, err error) {
	select {
	case failures <- err:
	case <-ctx.Done():
	}
}

func reportBuiltIn(reporter element.ResolutionReporter, runtimeID, revision string) error {
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: runtimeID, Revision: revision,
	}, nil)
}

func derivedEnvelope(cause element.Envelope, typ element.Type, suffix string, payload any) element.Envelope {
	result := cause.Clone()
	result.Type = typ
	result.ItemID = cause.ItemID + suffix
	result.CausalParents = appendUnique(result.CausalParents, cause.ItemID)
	result.Payload = payload
	return result
}

func generatedEnvelope(instance string, sequence uint64, typ element.Type, label string, payload any) element.Envelope {
	return element.Envelope{
		Type: typ, ItemID: fmt.Sprintf("%s:%s:%d", instance, label, sequence), Payload: payload,
	}
}

func appendUnique(values []string, value string) []string {
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func saturatingIncrement(value *uint64) uint64 {
	if *value < math.MaxUint64 {
		*value++
	}
	return *value
}

func saturatingAdd(left, right uint64) uint64 {
	if math.MaxUint64-left < right {
		return math.MaxUint64
	}
	return left + right
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func envelopeScope(envelope element.Envelope) string {
	return firstNonempty(envelope.CancellationScope, envelope.RunID, envelope.SourceID)
}

func cancelPayload(payload any) (perceptionelements.Cancel, bool) {
	switch typed := payload.(type) {
	case nil:
		return perceptionelements.Cancel{}, true
	case perceptionelements.Cancel:
		return typed, true
	case *perceptionelements.Cancel:
		if typed == nil {
			return perceptionelements.Cancel{}, false
		}
		return *typed, true
	default:
		return perceptionelements.Cancel{}, false
	}
}

func inputFramePayload(payload any) (InputFrame, bool) {
	switch typed := payload.(type) {
	case InputFrame:
		return typed, true
	case *InputFrame:
		if typed == nil {
			return InputFrame{}, false
		}
		return *typed, true
	default:
		return InputFrame{}, false
	}
}

func validateIdentifier(label, value string, required bool) error {
	trimmed := strings.TrimSpace(value)
	if required && trimmed == "" {
		return fmt.Errorf("%s is required", label)
	}
	if value != trimmed {
		return fmt.Errorf("%s must not have surrounding whitespace", label)
	}
	if len(value) > maximumIdentifierLength {
		return fmt.Errorf("%s exceeds %d bytes", label, maximumIdentifierLength)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", label)
	}
	for _, character := range value {
		if unicode.IsSpace(character) || unicode.IsControl(character) {
			return fmt.Errorf("%s contains whitespace or control characters", label)
		}
	}
	return nil
}

func candidateIdentifier(instance, stream string, sequence uint64) string {
	digest := sha256.Sum256([]byte(instance + "\x00" + stream + "\x00" +
		strconv.FormatUint(sequence, 10)))
	return fmt.Sprintf("endpoint:sha256:%x", digest)
}

func boundedReason(value string) string {
	if !utf8.ValidString(value) {
		return ""
	}
	const maximum = 1024
	if len(value) <= maximum {
		return value
	}
	const suffix = "…"
	value = value[:maximum-len(suffix)]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value + suffix
}

func policyStatePayload(payload any) (EndpointPolicyState, bool) {
	switch typed := payload.(type) {
	case EndpointPolicyState:
		return typed, true
	case *EndpointPolicyState:
		if typed == nil {
			return EndpointPolicyState{}, false
		}
		return *typed, true
	default:
		return EndpointPolicyState{}, false
	}
}

func gateCommandPayload(payload any) (GateCommand, bool) {
	switch typed := payload.(type) {
	case GateCommand:
		return typed, true
	case *GateCommand:
		if typed == nil {
			return GateCommand{}, false
		}
		return *typed, true
	default:
		return GateCommand{}, false
	}
}

func candidatePayload(payload any) (EndpointCandidate, bool) {
	switch typed := payload.(type) {
	case EndpointCandidate:
		return typed, true
	case *EndpointCandidate:
		if typed == nil {
			return EndpointCandidate{}, false
		}
		return *typed, true
	default:
		return EndpointCandidate{}, false
	}
}

func tickPayload(payload any) (TimingTick, bool) {
	switch typed := payload.(type) {
	case TimingTick:
		return typed, true
	case *TimingTick:
		if typed == nil {
			return TimingTick{}, false
		}
		return *typed, true
	default:
		return TimingTick{}, false
	}
}

func commitPayload(payload any) (AudioCommit, bool) {
	switch typed := payload.(type) {
	case nil:
		return AudioCommit{}, true
	case AudioCommit:
		return typed, true
	case *AudioCommit:
		if typed == nil {
			return AudioCommit{}, false
		}
		return *typed, true
	default:
		return AudioCommit{}, false
	}
}

func verdictPayload(payload any) (EndpointVerdict, bool) {
	switch typed := payload.(type) {
	case EndpointVerdict:
		return typed, true
	case *EndpointVerdict:
		if typed == nil {
			return EndpointVerdict{}, false
		}
		return *typed, true
	default:
		return EndpointVerdict{}, false
	}
}
