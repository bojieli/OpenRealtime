package scenarioconversation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/bojieli/OpenRealtime/action"
	legacy "github.com/bojieli/OpenRealtime/binding"
	"github.com/bojieli/OpenRealtime/element"
	acousticelements "github.com/bojieli/OpenRealtime/elements/acoustic"
	actionelements "github.com/bojieli/OpenRealtime/elements/action"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	interactionelements "github.com/bojieli/OpenRealtime/elements/interaction"
	mediaelements "github.com/bojieli/OpenRealtime/elements/media"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	speechelements "github.com/bojieli/OpenRealtime/elements/speech"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/perception"
	"github.com/bojieli/OpenRealtime/trajectory"
)

const (
	graphID = "scenario_conversation"

	invocationUpdateBoundary             = "invocation_update"
	responseCreateBoundary               = "response_create"
	generationCancelBoundary             = "generation_cancel"
	toolResultBoundary                   = "tool_result"
	audioBoundary                        = "audio"
	textBoundary                         = "text"
	imageBoundary                        = "image"
	contentCancelBoundary                = "content_cancel"
	mediaResolveBoundary                 = "media_resolve"
	mediaReturnLeaseBoundary             = "media_return_lease"
	modelCancelBoundary                  = "model_cancel"
	actionCancelBoundary                 = "action_cancel"
	segmentationCancelBoundary           = "segmentation_cancel"
	ttsCancelBoundary                    = "tts_cancel"
	playbackCancelBoundary               = "playback_cancel"
	playbackStateAppendBoundary          = "playback_state_append"
	acousticActivityBoundary             = "acoustic_activity"
	admissionStateBoundary               = "admission_state"
	admissionOutcomeBoundary             = "admission_outcome"
	transcriptBoundary                   = "transcript_events"
	observationBoundary                  = "observation_events"
	ingressHandlesBoundary               = "ingress_handles"
	ingressOutcomeBoundary               = "ingress_outcome"
	mediaResolvedBoundary                = "media_resolved"
	mediaLeaseReturnedBoundary           = "media_lease_returned"
	messageCommitBoundary                = "message_commit_outcome"
	semanticDecisionBoundary             = "semantic_decision"
	semanticAdmissionStateBoundary       = "semantic_admission_state"
	semanticAdmissionOutcomeBoundary     = "semantic_admission_outcome"
	semanticPolicyResolutionBoundary     = "semantic_policy_resolution"
	invocationOutcomeBoundary            = "invocation_outcome"
	dispatchCommitBoundary               = "dispatch_commit"
	canonicalResultBoundary              = "canonical_result"
	modelResultBoundary                  = "model_result"
	modelOutcomeBoundary                 = "model_outcome"
	segmentationOutcomeBoundary          = "segmentation_outcome"
	trajectorySnapshotBoundary           = "trajectory_snapshot"
	trajectoryCommitBoundary             = "trajectory_commit"
	trajectoryRejectionBoundary          = "trajectory_rejection"
	provenanceOutcomeBoundary            = "provenance_outcome"
	actionAdmissionBoundary              = "admission_action_outcome"
	lookupOutcomeBoundary                = "lookup_outcome"
	argumentNormalizationOutcomeBoundary = "argument_normalization_outcome"
	confirmationOutcomeBoundary          = "confirmation_outcome"
	targetFenceOutcomeBoundary           = "target_fence_outcome"
	canonicalCallOutcomeBoundary         = "canonical_call_outcome"
	ledgerOutcomeBoundary                = "ledger_outcome"
	dispatchOutcomeBoundary              = "dispatch_outcome"
	resultCommitOutcomeBoundary          = "result_commit_outcome"
	clientToolResultOutcomeBoundary      = "client_tool_result_outcome"
	clientToolResultJoinOutcomeBoundary  = "client_tool_result_join_outcome"
	gatewayTurnBeginBoundary             = "gateway_turn_begin"
	gatewayTurnEndBoundary               = "gateway_turn_end"
	gatewaySpeechBeginBoundary           = "gateway_speech_begin"
	gatewaySpeechTextBoundary            = "gateway_speech_text"
	gatewaySpeechAudioBoundary           = "gateway_speech_audio"
	gatewaySpeechEndBoundary             = "gateway_speech_end"

	maximumAdapterTextBytes   = 1 << 20
	maximumAdapterImageBytes  = 32 << 20
	maximumAdapterAudioBytes  = 1 << 20
	maximumAdapterSampleRate  = 384_000
	maximumAdapterReasonBytes = 1024
	maximumAdapterMemory      = 512
)

type adapterBoundaries struct {
	update, create, generationCancel, toolResult, audio, text, image, contentCancel element.OutputPort
	mediaResolve, mediaReturnLease, modelCancel, actionCancel                       element.OutputPort
	segmentationCancel, ttsCancel, playbackCancel                                   element.OutputPort
	playbackStateAppend                                                             element.OutputPort
	outputs                                                                         map[string]element.InputPort
}

type boundAdapter struct {
	profile      graphbinding.SessionAdapterProfile
	registration graphbinding.AdapterRegistration
}

func (adapter boundAdapter) Profile() graphbinding.SessionAdapterProfile {
	return adapter.profile.Clone()
}

func (adapter boundAdapter) Registration() graphbinding.AdapterRegistration {
	return adapter.registration
}

func bind(plan *graphconfig.Plan, plugin *Plugin) (boundAdapter, error) {
	if plan == nil || plugin == nil {
		return boundAdapter{}, errors.New("create scenario conversation adapter: plan and plugin are required")
	}
	if err := plan.Validate(); err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter plan: %w", err)
	}
	graph := plan.Graph()
	if graph.ID != graphID {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter: graph ID %q, want %q", graph.ID, graphID)
	}
	selected, err := validateAdapterBoundaryTypes(graph)
	if err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter: %w", err)
	}
	if err := validatePlanReferences(plan, plugin.config); err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter: %w", err)
	}
	profile, err := graphbinding.FreezeSessionAdapterProfile(graphbinding.SessionAdapterProfile{
		FormatVersion: graphbinding.SessionAdapterProfileFormatVersion,
		Name:          ProfileName, Revision: ProfileRevision, GraphFingerprint: graph.Fingerprint,
		Ownership: legacy.Ownership{
			Perception: legacy.OwnerEngine, FastCognition: legacy.OwnerEngine,
			SlowCognition: legacy.OwnerEngine, Action: legacy.OwnerEngine,
			Interaction: legacy.OwnerEngine, Floor: legacy.OwnerEngine,
		},
		Capabilities: legacy.Capabilities{
			Observations:    true,
			Video:           plugin.config.Model.Descriptor.Vision,
			Voice:           legacy.VoiceControl{InForce: plugin.config.TTS.Voice},
			MaxOutputTokens: plugin.config.MaxOutputTokens,
			Stack: legacy.StackCapabilities{
				AudioInput: true, AudioOutput: true,
				VisualInput:   plugin.config.Model.Descriptor.Vision,
				Transcription: true, TurnGeneration: true,
				ConcurrentIO: true, TextInjection: true,
			},
		},
		Boundaries: []graphbinding.AdapterBoundary{
			{Operation: graphbinding.AdapterInputUpdate, Boundary: invocationUpdateBoundary, Direction: ir.InputBoundary, Type: selected[invocationUpdateBoundary].Type},
			{Operation: graphbinding.AdapterInputAudio, Boundary: audioBoundary, Direction: ir.InputBoundary, Type: selected[audioBoundary].Type},
			{Operation: graphbinding.AdapterInputVideo, Boundary: imageBoundary, Direction: ir.InputBoundary, Type: selected[imageBoundary].Type},
			{Operation: graphbinding.AdapterInputText, Boundary: textBoundary, Direction: ir.InputBoundary, Type: selected[textBoundary].Type},
			{Operation: graphbinding.AdapterInputToolResult, Boundary: toolResultBoundary, Direction: ir.InputBoundary, Type: selected[toolResultBoundary].Type},
			{Operation: graphbinding.AdapterInputCreateResponse, Boundary: responseCreateBoundary, Direction: ir.InputBoundary, Type: selected[responseCreateBoundary].Type},
			{Operation: graphbinding.AdapterInputCancel, Boundary: generationCancelBoundary, Direction: ir.InputBoundary, Type: selected[generationCancelBoundary].Type},
			{Operation: graphbinding.AdapterOutputTurnBegin, Boundary: gatewayTurnBeginBoundary, Direction: ir.OutputBoundary, Type: selected[gatewayTurnBeginBoundary].Type},
			{Operation: graphbinding.AdapterOutputTurnEnd, Boundary: gatewayTurnEndBoundary, Direction: ir.OutputBoundary, Type: selected[gatewayTurnEndBoundary].Type},
			{Operation: graphbinding.AdapterOutputActivity, Boundary: acousticActivityBoundary, Direction: ir.OutputBoundary, Type: selected[acousticActivityBoundary].Type},
			{Operation: graphbinding.AdapterOutputTranscript, Boundary: transcriptBoundary, Direction: ir.OutputBoundary, Type: selected[transcriptBoundary].Type},
			{Operation: graphbinding.AdapterOutputObservation, Boundary: observationBoundary, Direction: ir.OutputBoundary, Type: selected[observationBoundary].Type},
			{Operation: graphbinding.AdapterOutputSpeechBegin, Boundary: gatewaySpeechBeginBoundary, Direction: ir.OutputBoundary, Type: selected[gatewaySpeechBeginBoundary].Type},
			{Operation: graphbinding.AdapterOutputSpeechText, Boundary: gatewaySpeechTextBoundary, Direction: ir.OutputBoundary, Type: selected[gatewaySpeechTextBoundary].Type},
			{Operation: graphbinding.AdapterOutputSpeechAudio, Boundary: gatewaySpeechAudioBoundary, Direction: ir.OutputBoundary, Type: selected[gatewaySpeechAudioBoundary].Type},
			{Operation: graphbinding.AdapterOutputSpeechEnd, Boundary: gatewaySpeechEndBoundary, Direction: ir.OutputBoundary, Type: selected[gatewaySpeechEndBoundary].Type},
			{Operation: graphbinding.AdapterOutputToolCalls, Boundary: dispatchCommitBoundary, Direction: ir.OutputBoundary, Type: selected[dispatchCommitBoundary].Type},
			{Operation: graphbinding.AdapterOutputFailed, Boundary: modelOutcomeBoundary, Direction: ir.OutputBoundary, Type: selected[modelOutcomeBoundary].Type},
		},
	})
	if err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter profile: %w", err)
	}
	if err := plugin.config.Architecture.ValidateRuntime(
		profile.Ownership, profile.Capabilities.Stack,
	); err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter architecture: %w", err)
	}
	frozenProfile := profile.Clone()
	registration := graphbinding.AdapterRegistration{
		Reference: AdapterReference, Artifact: plugin.config.RuntimeArtifact,
		Factory: func(
			ctx context.Context, mounted *graphruntime.Mounted, options legacy.Options,
			requested graphbinding.SessionAdapterProfile,
		) (graphbinding.SessionAdapter, error) {
			if requested.Fingerprint != frozenProfile.Fingerprint {
				return nil, errors.New("scenario conversation adapter received a different frozen profile")
			}
			if mounted == nil || mounted.Graph().Fingerprint != frozenProfile.GraphFingerprint {
				return nil, errors.New("scenario conversation adapter received a different mounted graph")
			}
			bundle, takeErr := plugin.coordinator.take(options.SessionID)
			if takeErr != nil {
				return nil, takeErr
			}
			session, sessionErr := newSession(mounted, options, plugin.config, bundle)
			if sessionErr != nil {
				_ = bundle.Close(sessionErr)
				return nil, sessionErr
			}
			return session, nil
		},
	}
	if err := registration.Validate(); err != nil {
		return boundAdapter{}, fmt.Errorf("create scenario conversation adapter registration: %w", err)
	}
	return boundAdapter{profile: profile, registration: registration}, nil
}

func validateAdapterBoundaryTypes(graph ir.Graph) (map[string]ir.Boundary, error) {
	wanted := map[string]struct {
		direction ir.BoundaryDirection
		typeOf    element.Type
	}{
		invocationUpdateBoundary:    {ir.InputBoundary, policyelements.SessionInvocationUpdateType()},
		responseCreateBoundary:      {ir.InputBoundary, policyelements.ResponseCreateType()},
		generationCancelBoundary:    {ir.InputBoundary, policyelements.GenerationCancelType()},
		toolResultBoundary:          {ir.InputBoundary, actionelements.ClientToolResultType()},
		audioBoundary:               {ir.InputBoundary, descriptorPortType(acousticelements.EnergyAdmissionDescriptor(), "audio")},
		textBoundary:                {ir.InputBoundary, ingresselements.UserTextType()},
		imageBoundary:               {ir.InputBoundary, ingresselements.UserImageType()},
		contentCancelBoundary:       {ir.InputBoundary, ingresselements.ContentCancelType()},
		mediaResolveBoundary:        {ir.InputBoundary, mediaelements.ResolveRequestType()},
		mediaReturnLeaseBoundary:    {ir.InputBoundary, mediaelements.ReturnLeaseRequestType()},
		modelCancelBoundary:         {ir.InputBoundary, cognitionelements.CancelType()},
		actionCancelBoundary:        {ir.InputBoundary, actionelements.InterruptType()},
		segmentationCancelBoundary:  {ir.InputBoundary, descriptorPortTypeByName("interaction.SegmentPreparedText", "cancel")},
		ttsCancelBoundary:           {ir.InputBoundary, descriptorPortTypeByName("speech.TTS", "cancel")},
		playbackCancelBoundary:      {ir.InputBoundary, descriptorPortTypeByName("speech.Playback", "cancel")},
		playbackStateAppendBoundary: {ir.InputBoundary, stateelements.AppendType()},

		acousticActivityBoundary:             {ir.OutputBoundary, descriptorPortType(acousticelements.EnergyAdmissionDescriptor(), "activity")},
		admissionStateBoundary:               {ir.OutputBoundary, descriptorPortType(acousticelements.EnergyAdmissionDescriptor(), "state")},
		admissionOutcomeBoundary:             {ir.OutputBoundary, descriptorPortType(acousticelements.EnergyAdmissionDescriptor(), "outcome")},
		transcriptBoundary:                   {ir.OutputBoundary, ingresselements.ObservationType()},
		observationBoundary:                  {ir.OutputBoundary, ingresselements.ObservationType()},
		ingressHandlesBoundary:               {ir.OutputBoundary, ingresselements.AttachmentHandleType()},
		ingressOutcomeBoundary:               {ir.OutputBoundary, ingresselements.OutcomeType()},
		mediaResolvedBoundary:                {ir.OutputBoundary, mediaelements.ResolvedAttachmentType()},
		mediaLeaseReturnedBoundary:           {ir.OutputBoundary, mediaelements.ReturnLeaseResultType()},
		messageCommitBoundary:                {ir.OutputBoundary, stateelements.ObservationCommitOutcomeType()},
		semanticDecisionBoundary:             {ir.OutputBoundary, policyelements.SemanticDecisionType()},
		semanticAdmissionStateBoundary:       {ir.OutputBoundary, policyelements.SemanticAdmissionStateType()},
		semanticAdmissionOutcomeBoundary:     {ir.OutputBoundary, policyelements.SemanticAdmissionOutcomeType()},
		semanticPolicyResolutionBoundary:     {ir.OutputBoundary, policyelements.SemanticDeciderResolutionType()},
		invocationOutcomeBoundary:            {ir.OutputBoundary, policyelements.SessionInvocationOutcomeType()},
		dispatchCommitBoundary:               {ir.OutputBoundary, actionelements.CommittedType()},
		canonicalResultBoundary:              {ir.OutputBoundary, actionelements.CanonicalResultType()},
		modelResultBoundary:                  {ir.OutputBoundary, interactionelements.SafeModelResultType()},
		modelOutcomeBoundary:                 {ir.OutputBoundary, cognitionelements.OutcomeType()},
		segmentationOutcomeBoundary:          {ir.OutputBoundary, interactionelements.SegmentationOutcomeType()},
		trajectorySnapshotBoundary:           {ir.OutputBoundary, stateelements.SnapshotType()},
		trajectoryCommitBoundary:             {ir.OutputBoundary, stateelements.CommitType()},
		trajectoryRejectionBoundary:          {ir.OutputBoundary, stateelements.RejectionType()},
		provenanceOutcomeBoundary:            {ir.OutputBoundary, actionelements.OutcomeType()},
		actionAdmissionBoundary:              {ir.OutputBoundary, actionelements.OutcomeType()},
		lookupOutcomeBoundary:                {ir.OutputBoundary, actionelements.OutcomeType()},
		argumentNormalizationOutcomeBoundary: {ir.OutputBoundary, actionelements.OutcomeType()},
		confirmationOutcomeBoundary:          {ir.OutputBoundary, actionelements.OutcomeType()},
		targetFenceOutcomeBoundary:           {ir.OutputBoundary, actionelements.OutcomeType()},
		canonicalCallOutcomeBoundary:         {ir.OutputBoundary, actionelements.OutcomeType()},
		ledgerOutcomeBoundary:                {ir.OutputBoundary, actionelements.OutcomeType()},
		dispatchOutcomeBoundary:              {ir.OutputBoundary, actionelements.OutcomeType()},
		resultCommitOutcomeBoundary:          {ir.OutputBoundary, actionelements.OutcomeType()},
		clientToolResultOutcomeBoundary:      {ir.OutputBoundary, actionelements.OutcomeType()},
		clientToolResultJoinOutcomeBoundary:  {ir.OutputBoundary, actionelements.OutcomeType()},
		gatewayTurnBeginBoundary:             {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
		gatewayTurnEndBoundary:               {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
		gatewaySpeechBeginBoundary:           {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
		gatewaySpeechTextBoundary:            {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
		gatewaySpeechAudioBoundary:           {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
		gatewaySpeechEndBoundary:             {ir.OutputBoundary, speechelements.PlaybackReceiptType()},
	}
	boundaries := make(map[string]ir.Boundary, len(graph.Boundaries))
	for _, boundary := range graph.Boundaries {
		boundaries[boundary.Name] = boundary
	}
	selected := make(map[string]ir.Boundary, len(wanted))
	for name, requirement := range wanted {
		boundary, found := boundaries[name]
		if !found {
			return nil, fmt.Errorf("graph has no boundary %q", name)
		}
		if requirement.typeOf.Name == "" && requirement.typeOf.Variable == "" {
			return nil, fmt.Errorf("adapter expected type for boundary %q is unavailable", name)
		}
		if boundary.Direction != requirement.direction || !boundary.Type.Equal(requirement.typeOf) {
			return nil, fmt.Errorf("graph boundary %q has %s %s, want %s %s", name,
				boundary.Direction, boundary.Type.String(), requirement.direction, requirement.typeOf.String())
		}
		selected[name] = boundary
	}
	return selected, nil
}

func descriptorPortType(descriptor element.Descriptor, name string) element.Type {
	port, found := descriptor.Port(name)
	if !found {
		return element.Type{}
	}
	return port.Type.Clone()
}

func descriptorPortTypeByName(descriptorName, portName string) element.Type {
	// These descriptors are already pinned by the graph lock. Looking them up
	// through their public constructors here would introduce a package cycle;
	// the exact concrete spelling is still checked against frozen Graph IR.
	for _, candidate := range []struct {
		descriptor string
		port       string
		typeOf     element.Type
	}{
		{"interaction.SegmentPreparedText", "cancel", element.Interrupt(element.Named("flow.RunID"))},
		{"speech.TTS", "cancel", element.Interrupt(element.Named("speech.UtteranceID"))},
		{"speech.Playback", "cancel", element.Interrupt(element.Named("speech.UtteranceID"))},
	} {
		if candidate.descriptor == descriptorName && candidate.port == portName {
			return candidate.typeOf
		}
	}
	return element.Type{}
}

func validatePlanReferences(plan *graphconfig.Plan, config PluginConfig) error {
	for _, selected := range []struct {
		name     string
		artifact inspect.ArtifactIdentity
	}{
		{cognitionelements.MediaResolverService, config.DependencyArtifact},
		{stateelements.TrajectoryStoreService, config.DependencyArtifact},
		{policyelements.SemanticDeciderRegistryService, config.Policy.Artifact},
	} {
		if err := validateSelectedScenarioDependency(plan, selected.name, selected.artifact); err != nil {
			return err
		}
	}
	wanted := map[string]map[string]string{
		"asr":                       {"provider": ASRReference},
		"semantic_admission":        {"decider": PolicyReference},
		"overlap_barge_in":          {"decider": PolicyReference},
		"voice_model":               {"provider": ModelReference},
		"silent_model":              {"provider": SilentModelReference},
		"tool_lookup":               {"registry": ToolReference},
		"confirmation":              {"provider": ConfirmationReference},
		"target_fence":              {"target": TargetReference},
		"ledger_commit":             {"ledger": LedgerReference},
		"dispatch":                  {"registry": ToolReference, "ledger": LedgerReference},
		"tts":                       {"provider": TTSReference},
		"playback":                  {"sink": PlaybackReference},
		"voice_session_invocation":  {"role": "foreground"},
		"silent_session_invocation": {"role": "silent"},
	}
	values := plan.Values()
	for node, fields := range wanted {
		var decoded map[string]any
		if err := json.Unmarshal(values[node], &decoded); err != nil {
			return fmt.Errorf("decode values for %s: %w", node, err)
		}
		for field, expected := range fields {
			actual, _ := decoded[field].(string)
			if actual != expected {
				return fmt.Errorf("node %s %s reference %q, want %q", node, field, actual, expected)
			}
		}
	}
	var endpoint struct {
		Mode string `json:"mode"`
	}
	if err := json.Unmarshal(values["endpoint_policy"], &endpoint); err != nil || endpoint.Mode != "automatic" {
		return errors.New("scenario conversation endpoint policy must be exact automatic mode")
	}
	var semanticAdmission struct {
		DirectVisualInput           bool    `json:"direct_visual_input"`
		StandingExtraction          bool    `json:"standing_extraction"`
		VerifyVoiceActivation       bool    `json:"verify_voice_activation"`
		VerifySilentAction          bool    `json:"verify_silent_action"`
		MinimumActivationConfidence float64 `json:"minimum_activation_confidence"`
		StandingMemory              int     `json:"standing_memory"`
	}
	if err := json.Unmarshal(values["semantic_admission"], &semanticAdmission); err != nil {
		return fmt.Errorf("decode scenario conversation semantic admission values: %w", err)
	}
	directVisual := false
	if evidence := config.Architecture.Interaction.EvidenceCapabilities; evidence != nil {
		directVisual = evidence.DirectVisualInput
	}
	if semanticAdmission.DirectVisualInput != directVisual {
		return errors.New("scenario conversation semantic direct-visual selection drifted from architecture")
	}
	if semanticAdmission.StandingExtraction != config.SemanticAdmission.StandingExtraction ||
		semanticAdmission.VerifyVoiceActivation != config.SemanticAdmission.VerifyVoiceActivation ||
		semanticAdmission.VerifySilentAction != config.SemanticAdmission.VerifySilentAction ||
		semanticAdmission.MinimumActivationConfidence != config.SemanticAdmission.MinimumActivationConfidence ||
		semanticAdmission.StandingMemory != config.SemanticAdmission.StandingMemory {
		return errors.New("scenario conversation semantic admission values drifted from the application selection")
	}
	var postCommitSilence struct {
		DelayMS int `json:"delay_ms"`
	}
	if err := json.Unmarshal(values["post_commit_silence"], &postCommitSilence); err != nil ||
		postCommitSilence.DelayMS != 15_000 {
		return errors.New("scenario conversation post-commit silence must be exactly 15000ms")
	}
	var admission struct {
		Threshold         float64 `json:"threshold"`
		PrefixPaddingMS   int     `json:"prefix_padding_ms"`
		SilenceDurationMS int     `json:"silence_duration_ms"`
		SpeechDurationMS  int     `json:"speech_duration_ms"`
	}
	if err := json.Unmarshal(values["admission"], &admission); err != nil {
		return fmt.Errorf("decode scenario conversation admission values: %w", err)
	}
	if admission.Threshold != config.Gate.Threshold ||
		admission.PrefixPaddingMS != config.Gate.PrefixPaddingMS ||
		admission.SilenceDurationMS != config.Gate.SilenceDurationMS ||
		admission.SpeechDurationMS != config.Gate.SpeechDurationMS {
		return errors.New("scenario conversation graph acoustic gate drifted from application selection")
	}
	var content struct {
		MaxInputBytes   int `json:"max_input_bytes"`
		MaxPending      int `json:"max_pending"`
		MaxPendingBytes int `json:"max_pending_bytes"`
	}
	if err := json.Unmarshal(values["content"], &content); err != nil {
		return fmt.Errorf("decode scenario conversation content values: %w", err)
	}
	var retention struct {
		MaxItems        int `json:"max_items"`
		MaxBytes        int `json:"max_bytes"`
		MaxItemBytes    int `json:"max_item_bytes"`
		MaxActiveLeases int `json:"max_active_leases"`
	}
	if err := json.Unmarshal(values["retention"], &retention); err != nil {
		return fmt.Errorf("decode scenario conversation retention values: %w", err)
	}
	var resolver struct {
		MaxPending int `json:"max_pending"`
		MaxBytes   int `json:"max_bytes"`
	}
	if err := json.Unmarshal(values["media_resolver"], &resolver); err != nil {
		return fmt.Errorf("decode scenario conversation media resolver values: %w", err)
	}
	media := config.Media
	if content.MaxInputBytes != media.MaxItemBytes || content.MaxPending != media.MaxPending ||
		content.MaxPendingBytes != media.MaxBytes || retention.MaxItems != media.MaxItems ||
		retention.MaxBytes != media.MaxBytes || retention.MaxItemBytes != media.MaxItemBytes ||
		retention.MaxActiveLeases != media.MaxActiveLeases || resolver.MaxPending != media.MaxPending ||
		resolver.MaxBytes != media.MaxBytes {
		return errors.New("scenario conversation graph retained-media bounds drifted from application selection")
	}
	return nil
}

func validateSelectedScenarioDependency(
	plan *graphconfig.Plan, name string, artifact inspect.ArtifactIdentity,
) error {
	matches := 0
	for _, dependency := range plan.Resolution().Dependencies {
		if dependency.Name != name {
			continue
		}
		matches++
		if dependency.Artifact != artifact || dependency.Scope != graphconfig.DependencyScopeMount {
			return fmt.Errorf(
				"scenario conversation dependency %q selection drifted from exact mount artifact", name,
			)
		}
	}
	if matches != 1 {
		return fmt.Errorf(
			"scenario conversation requires exactly one selected dependency %q; got %d", name, matches,
		)
	}
	return nil
}

type operationAck struct {
	generationID string
	err          error
}

type pendingOperation struct {
	requestID  string
	operation  string
	revision   uint64
	digest     string
	generation string
	callID     string
	name       string
	result     chan operationAck
	completed  bool
	abandoned  bool
	playback   *playbackStateAppend
}

type pendingContent struct {
	requestID      string
	streamID       string
	result         chan error
	requiresHandle bool
	handleReady    bool
	commitReady    bool
	commitVersion  uint64
	commitContext  stateelements.CommittedContext
	trajectoryItem string
	snapshotReady  bool
	completed      bool
}

type activeClientCall struct {
	call         trajectory.ToolCall
	runID        string
	commitmentID string
}

type pendingAudio struct {
	itemID     string
	streamID   string
	result     chan error
	awaitClose bool
	completed  bool
}

type playbackReceiptState struct {
	runID          string
	utterance      action.Utterance
	sequence       uint64
	sourceSequence uint64
	kind           speechelements.PlaybackReceiptKind
	outcome        action.Outcome
	active         bool
	terminal       bool
}

type session struct {
	ports     adapterBoundaries
	sink      legacy.Sink
	sessionID string
	config    PluginConfig
	bundle    *sessionBundle

	sequence      atomic.Uint64
	videoMu       sync.Mutex
	videoCaptured map[string]uint64

	updateMu         sync.Mutex
	settingsMu       sync.RWMutex
	settingsRevision uint64
	settings         legacy.Settings
	operationMu      sync.Mutex
	pendingOps       map[string]*pendingOperation

	contentMu          sync.Mutex
	seenContent        map[string]struct{}
	seenContentOrder   []string
	terminalContent    map[string]struct{}
	terminalContentIDs []string
	terminalRequests   map[string]struct{}
	terminalRequestIDs []string
	contentAcks        map[string]*pendingContent
	snapshotVersion    uint64
	snapshotItemID     string
	snapshotChanged    chan struct{}

	audioSendMu    sync.Mutex
	audioMu        sync.Mutex
	audioCaptured  uint64
	audioStream    uint64
	audioReady     chan struct{}
	audioReadyOnce sync.Once
	audioOps       map[string]*pendingAudio
	closedAudio    map[string]struct{}

	activityMu      sync.Mutex
	active          map[string]struct{}
	calls           map[string]activeClientCall
	terminalCalls   map[string]struct{}
	terminalCallIDs []string
	utterances      map[string]struct{}
	playback        map[string]playbackReceiptState
	playbackOrder   []string
	pendingSpeech   map[string]struct{}
	speechlessRuns  map[string]struct{}
	speechlessIDs   []string
	segmentedRuns   map[string]struct{}
	segmentedRunIDs []string
	speechRuns      map[string]int
	speechRunOrder  []string
	failures        map[string]struct{}
	failureIDs      []string
	terminalRuns    map[string]struct{}
	terminalRunIDs  []string

	closeOnce sync.Once
	closeErr  error
}

func newSession(
	mounted *graphruntime.Mounted, options legacy.Options, config PluginConfig,
	bundle *sessionBundle,
) (*session, error) {
	if mounted == nil || options.Sink == nil || !canonicalIdentity(options.SessionID) ||
		bundle == nil || bundle.bridge == nil || bundle.media == nil || bundle.store == nil ||
		bundle.presentation == nil || bundle.playback == nil {
		return nil, errors.New("scenario conversation session requires mounted graph, sink, identity, and dependency bundle")
	}
	if err := validateInitialSettings(options.Settings, config); err != nil {
		return nil, err
	}
	ingress := func(name string) (element.OutputPort, error) { return mounted.Ingress(name) }
	ports := adapterBoundaries{outputs: make(map[string]element.InputPort)}
	inputs := []struct {
		name string
		set  *element.OutputPort
	}{
		{invocationUpdateBoundary, &ports.update}, {responseCreateBoundary, &ports.create},
		{generationCancelBoundary, &ports.generationCancel}, {toolResultBoundary, &ports.toolResult},
		{audioBoundary, &ports.audio},
		{textBoundary, &ports.text}, {imageBoundary, &ports.image},
		{contentCancelBoundary, &ports.contentCancel}, {mediaResolveBoundary, &ports.mediaResolve},
		{mediaReturnLeaseBoundary, &ports.mediaReturnLease}, {modelCancelBoundary, &ports.modelCancel},
		{actionCancelBoundary, &ports.actionCancel}, {segmentationCancelBoundary, &ports.segmentationCancel},
		{ttsCancelBoundary, &ports.ttsCancel}, {playbackCancelBoundary, &ports.playbackCancel},
		{playbackStateAppendBoundary, &ports.playbackStateAppend},
	}
	for _, input := range inputs {
		port, err := ingress(input.name)
		if err != nil {
			return nil, err
		}
		*input.set = port
	}
	for _, boundary := range mounted.Graph().Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		port, err := mounted.Egress(boundary.Name)
		if err != nil {
			return nil, err
		}
		ports.outputs[boundary.Name] = port
	}
	if err := bundle.media.Bind(ports.mediaResolve, ports.mediaReturnLease); err != nil {
		return nil, err
	}
	return &session{
		ports: ports, sink: options.Sink, sessionID: options.SessionID,
		config: clonePluginConfig(config), bundle: bundle,
		settings: legacy.CloneSettings(options.Settings), pendingOps: make(map[string]*pendingOperation),
		seenContent: make(map[string]struct{}), terminalContent: make(map[string]struct{}),
		terminalRequests: make(map[string]struct{}),
		contentAcks:      make(map[string]*pendingContent), snapshotChanged: make(chan struct{}),
		audioStream: 1,
		audioReady:  make(chan struct{}), audioOps: make(map[string]*pendingAudio),
		closedAudio: make(map[string]struct{}),
		active:      make(map[string]struct{}), calls: make(map[string]activeClientCall),
		terminalCalls: make(map[string]struct{}),
		utterances:    make(map[string]struct{}), playback: make(map[string]playbackReceiptState),
		pendingSpeech: make(map[string]struct{}), speechlessRuns: make(map[string]struct{}),
		segmentedRuns: make(map[string]struct{}), speechRuns: make(map[string]int),
		failures:     make(map[string]struct{}),
		terminalRuns: make(map[string]struct{}),
	}, nil
}

func validateInitialSettings(settings legacy.Settings, config PluginConfig) error {
	if settings.ManualTurns {
		return errors.New("scenario conversation automatic endpoint profile does not support manual turns")
	}
	if settings.Voice != "" && settings.Voice != config.TTS.Voice {
		return fmt.Errorf("scenario conversation voice %q, want exact %q", settings.Voice, config.TTS.Voice)
	}
	if !reflect.DeepEqual(settings.Gate, config.Gate) {
		return errors.New("scenario conversation session gate drifted from the exact application profile")
	}
	if err := validateModalities(settings.Modalities); err != nil {
		return err
	}
	if len(settings.Observers) != 0 {
		return errors.New("scenario conversation profile does not select an external observer")
	}
	return nil
}

func validateModalities(modalities []string) error {
	if len(modalities) != 1 || (modalities[0] != "audio" && modalities[0] != "text") {
		return errors.New("scenario conversation output modalities must contain exactly audio or text")
	}
	return nil
}

func (session *session) Run(ctx context.Context) error {
	if ctx == nil {
		return errors.New("run scenario conversation adapter: nil context")
	}
	if err := session.bundle.media.Start(ctx); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	results := make(chan error, len(session.ports.outputs))
	var wait sync.WaitGroup
	for name, port := range session.ports.outputs {
		name, port := name, port
		wait.Add(1)
		go func() {
			defer wait.Done()
			results <- session.runOutput(runCtx, name, port)
		}()
	}
	first := <-results
	cancel(first)
	wait.Wait()
	if terminalAdapterError(ctx, first) {
		return nil
	}
	if first == nil {
		return errors.New("scenario conversation output drainer stopped without cancellation")
	}
	return first
}

func (session *session) runOutput(ctx context.Context, name string, port element.InputPort) error {
	for {
		envelope, err := port.Receive(ctx)
		if terminalAdapterError(ctx, err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("receive scenario conversation output %s: %w", name, err)
		}
		if !envelope.Type.Equal(port.Type()) {
			return fmt.Errorf("scenario conversation output %s has type %s, want %s",
				name, envelope.Type.String(), port.Type().String())
		}
		switch name {
		case acousticActivityBoundary:
			err = session.publishActivity(ctx, envelope)
		case admissionStateBoundary:
			err = session.acceptAdmissionState(envelope)
		case admissionOutcomeBoundary:
			err = session.acceptAdmissionOutcome(envelope)
		case transcriptBoundary:
			err = session.publishTranscript(ctx, envelope)
		case observationBoundary:
			err = session.publishObservation(ctx, envelope)
		case ingressHandlesBoundary:
			err = session.acceptIngressHandle(envelope)
		case ingressOutcomeBoundary:
			err = session.acceptIngressOutcome(envelope)
		case mediaResolvedBoundary:
			err = session.bundle.media.AcceptResolved(envelope)
		case mediaLeaseReturnedBoundary:
			err = session.bundle.media.AcceptLeaseReturned(envelope)
		case messageCommitBoundary:
			err = session.acceptMessageCommit(envelope)
		case semanticAdmissionOutcomeBoundary:
			err = session.acceptSemanticAdmissionOutcome(ctx, envelope)
		case invocationOutcomeBoundary:
			err = session.acceptInvocationOutcome(envelope)
		case dispatchCommitBoundary:
			err = session.publishCall(ctx, envelope)
		case canonicalResultBoundary:
			err = session.acceptCanonicalResult(envelope)
		case modelResultBoundary:
			err = session.acceptModelResult(envelope)
		case modelOutcomeBoundary:
			err = session.acceptModelOutcome(ctx, envelope)
		case segmentationOutcomeBoundary:
			err = session.acceptSegmentationOutcome(envelope)
			if err == nil {
				err = session.publishDebug(ctx, name, envelope)
			}
		case trajectorySnapshotBoundary:
			err = session.acceptTrajectorySnapshot(envelope)
		case trajectoryCommitBoundary, trajectoryRejectionBoundary:
			err = session.acceptPlaybackStateReply(name, envelope)
		case provenanceOutcomeBoundary, actionAdmissionBoundary, lookupOutcomeBoundary,
			argumentNormalizationOutcomeBoundary,
			confirmationOutcomeBoundary, targetFenceOutcomeBoundary,
			canonicalCallOutcomeBoundary, ledgerOutcomeBoundary, dispatchOutcomeBoundary,
			resultCommitOutcomeBoundary, clientToolResultJoinOutcomeBoundary:
			err = session.acceptActionOutcome(ctx, name, envelope)
		case clientToolResultOutcomeBoundary:
			err = session.acceptClientToolResultOutcome(envelope)
		case gatewayTurnBeginBoundary, gatewayTurnEndBoundary,
			gatewaySpeechBeginBoundary, gatewaySpeechTextBoundary,
			gatewaySpeechAudioBoundary, gatewaySpeechEndBoundary:
			err = session.acceptPlaybackReceipt(ctx, name, envelope)
			if err == nil {
				err = session.publishDebug(ctx, name, envelope)
			}
		default:
			err = session.publishDebug(ctx, name, envelope)
		}
		if err != nil {
			return err
		}
	}
}

func terminalAdapterError(ctx context.Context, err error) bool {
	return context.Cause(ctx) != nil || errors.Is(err, context.Canceled) ||
		errors.Is(err, graphruntime.ErrChannelClosed)
}

func usableContext(ctx context.Context, operation string) error {
	if ctx == nil {
		return fmt.Errorf("%s: nil context", operation)
	}
	if err := context.Cause(ctx); err != nil {
		return err
	}
	return nil
}

func (session *session) nextItemID(kind string) string {
	sequence := session.sequence.Add(1)
	return fmt.Sprintf("sc_%s_%d", kind, sequence)
}

func sendExact(
	ctx context.Context, port element.OutputPort, envelope element.Envelope, operation string,
) error {
	delivery, err := port.Broadcast(ctx, envelope)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if delivery.Delivered != 1 || delivery.Dropped != 0 {
		return fmt.Errorf("%s delivered %d and dropped %d lanes", operation, delivery.Delivered, delivery.Dropped)
	}
	return nil
}

func boundedAdapterReason(reason string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return "client canceled response"
	}
	if !utf8.ValidString(reason) {
		return "client canceled response"
	}
	if len(reason) > maximumAdapterReasonBytes {
		reason = reason[:maximumAdapterReasonBytes]
		for !utf8.ValidString(reason) {
			reason = reason[:len(reason)-1]
		}
	}
	return reason
}

func cloneFrame(frame perception.Frame) perception.Frame {
	frame.PCM16LE = slices.Clone(frame.PCM16LE)
	frame.Image = slices.Clone(frame.Image)
	return frame
}

func cloneTextInput(input legacy.TextInput) legacy.TextInput {
	input.Images = slices.Clone(input.Images)
	for index := range input.Images {
		input.Images[index].Bytes = slices.Clone(input.Images[index].Bytes)
	}
	return input
}
