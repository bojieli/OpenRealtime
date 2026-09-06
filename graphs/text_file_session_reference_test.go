package graphs_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/continuation"
	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements"
	cognitionelements "github.com/bojieli/OpenRealtime/elements/cognition"
	ingresselements "github.com/bojieli/OpenRealtime/elements/ingress"
	policyelements "github.com/bojieli/OpenRealtime/elements/policy"
	stateelements "github.com/bojieli/OpenRealtime/elements/state"
	"github.com/bojieli/OpenRealtime/graph/ir"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	graphvalues "github.com/bojieli/OpenRealtime/graph/values"
	"github.com/bojieli/OpenRealtime/trajectory"
)

// The consumer is a real mounted TextModel provider and the final assertions
// read the canonical store. Sending a create envelope alone does not prove that
// the graph used the updated instruction, latest file, or previous answer.
func TestTextFileSessionExplicitResponsesUseUpdatedSettingsFilesAndCanonicalHistory(t *testing.T) {
	reference := loadTextFileCognitionReference(t)
	values, err := graphvalues.ParseYAML("agent.values.yaml", readFile(t, "components/text-file-cognition/agent.values.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	values.Nodes["activation"] = json.RawMessage(`{"role":"text-file","generate_on_commit":false}`)
	reference.bound, err = graphvalues.Bind(reference.graph, values)
	if err != nil {
		t.Fatal(err)
	}
	provider := &textSessionProvider{requests: make(chan continuation.Request, 4)}
	providers := cognitionelements.NewProviderRegistry()
	if err := providers.Register("deployment.text-file", provider.Descriptor(),
		func() (continuation.Provider, error) { return provider, nil }); err != nil {
		t.Fatal(err)
	}
	store := trajectory.NewStore()
	bridge := &textFileReferenceMediaBridge{}
	services := graphruntime.NewServiceSet()
	for name, service := range map[string]any{
		cognitionelements.ProviderRegistryService: providers,
		cognitionelements.MediaResolverService:    continuation.MediaResolver(bridge.Resolve),
		stateelements.TrajectoryStoreService: &stateelements.TrajectoryStoreServiceValue{
			Store: store, SessionID: "text-file-session",
		},
	} {
		if _, err := services.Set(name, service); err != nil {
			t.Fatal(err)
		}
	}
	registry, err := elements.RuntimeRegistry()
	if err != nil {
		t.Fatal(err)
	}
	mounted, err := graphruntime.Mount(t.Context(), graphruntime.Config{
		Graph: reference.bound.Graph, Values: reference.bound.Values,
		Registry: registry, Services: services, ShutdownTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	bridge.mounted = mounted
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- mounted.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("text session shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("text session did not stop")
		}
	})
	for _, boundary := range reference.bound.Graph.Boundaries {
		if boundary.Direction != ir.OutputBoundary {
			continue
		}
		switch boundary.Name {
		case "activation_outcome", "observation_outcome", "prepared_text", "model_result", "model_outcome", "model_commit_outcome", "handles", "resolved", "lease_returned":
		default:
			textFileDrain(t, ctx, mounted, boundary.Name)
		}
	}

	for turn := 1; turn <= 2; turn++ {
		instruction := fmt.Sprint("instruction-", turn)
		textFileInstallInvocation(t, mounted, "text-file-session", uint64(turn), instruction)
		if turn == 1 {
			textFileSend(t, mounted, "text", element.Envelope{
				Type: ingresselements.UserTextType(), ItemID: "typed", SessionID: "text-file-session", SourceID: "participant",
				Payload: ingresselements.UserText{ContentID: "message", StreamID: "messages", Text: "read the report", Revision: 1, Final: true},
			})
		} else {
			textFileSend(t, mounted, "file", element.Envelope{
				Type: ingresselements.UserFileType(), ItemID: "uploaded", SessionID: "text-file-session", SourceID: "participant",
				Payload: ingresselements.UserFile{ContentID: "report", StreamID: "files", Name: "report.txt", MIMEType: "text/plain",
					Content: []byte("Ada owns the launch"), SourceRevision: 1, CapturedNS: 100},
			})
		}
		var commit stateelements.ObservationCommitOutcome
		for {
			commit = textFileReceive(t, mounted, "observation_outcome").Payload.(stateelements.ObservationCommitOutcome)
			if commit.Kind == stateelements.ObservationCommitted {
				break
			}
			if commit.Kind != stateelements.ObservationRejected || commit.Code != "unknown_commit_reply" {
				t.Fatalf("observation outcome = %+v", commit)
			}
		}
		outcome := textFileReceive(t, mounted, "activation_outcome").Payload.(policyelements.SessionInvocationOutcome)
		if outcome.Kind != policyelements.SessionInvocationIgnored || outcome.Code != "explicit_response_required" {
			t.Fatalf("manual content activated cognition: %+v", outcome)
		}
		select {
		case request := <-provider.requests:
			t.Fatalf("provider invoked before response.create: %+v", request.Invocation)
		default:
		}
		create := policyelements.ResponseCreate{ResponseID: fmt.Sprint("response-", turn),
			ExpectedContextVersion: &commit.StoreVersion, ExpectedContextItemID: commit.Context.StateItemID, CommittedContext: &commit.Context}
		textFileSend(t, mounted, "response_create", element.Envelope{
			Type: policyelements.ResponseCreateType(), ItemID: create.ResponseID, SessionID: "text-file-session", Payload: create,
		})
		outcome = textFileReceive(t, mounted, "activation_outcome").Payload.(policyelements.SessionInvocationOutcome)
		if outcome.Kind != policyelements.SessionInvocationEmitted || outcome.Operation != "create" ||
			outcome.InvocationRevision != uint64(turn) || outcome.ContextVersion != commit.StoreVersion {
			t.Fatalf("explicit response outcome = %+v", outcome)
		}
		var request continuation.Request
		select {
		case request = <-provider.requests:
		case <-time.After(3 * time.Second):
			t.Fatal("explicit response did not reach provider")
		}
		if request.Invocation.Instruction != instruction || request.Trajectory.Version != commit.StoreVersion ||
			request.Invocation.SourceRevision != 0 || len(request.Invocation.Tools) != 0 {
			t.Fatalf("provider request invocation=%+v context=%+v", request.Invocation, request.Trajectory)
		}
		prefix, err := trajectory.IdentifyPrefix(request.Trajectory, request.Trajectory.Version)
		if err != nil || prefix != commit.Context.Prefix {
			t.Fatalf("provider context prefix=%+v err=%v", prefix, err)
		}
		response := instruction
		if turn == 2 {
			response += ": Ada owns the launch"
			foundPreviousAnswer := false
			for _, item := range request.Trajectory.Items {
				if item.Kind == trajectory.KindAssistant && item.Content == "instruction-1" {
					foundPreviousAnswer = true
				}
			}
			if !foundPreviousAnswer {
				t.Fatal("second response lost the first committed answer")
			}
		}
		textFileAwaitModelCommit(t, mounted, "manual response", response)
	}
	if bridge.resolutions.Load() != 1 {
		t.Fatalf("file resolutions = %d", bridge.resolutions.Load())
	}
	var answers []string
	for _, item := range store.Snapshot().Items {
		if item.Kind == trajectory.KindAssistant {
			answers = append(answers, item.Content)
		}
	}
	if strings.Join(answers, "|") != "instruction-1|instruction-2: Ada owns the launch" {
		t.Fatalf("canonical assistant history = %q", answers)
	}
}

type textSessionProvider struct{ requests chan continuation.Request }

func (*textSessionProvider) Descriptor() continuation.Descriptor {
	return textFileReferenceDescriptor()
}

func (provider *textSessionProvider) Continue(ctx context.Context, request continuation.Request, emit continuation.Emit) (continuation.Completion, error) {
	select {
	case provider.requests <- request:
	case <-ctx.Done():
		return continuation.Completion{}, context.Cause(ctx)
	}
	response := request.Invocation.Instruction
	for handle := range continuation.LatestMediaHandles(request.Trajectory.Items) {
		media, err := request.Media(handle)
		if err != nil {
			return continuation.Completion{}, err
		}
		response += ": " + string(media.Bytes)
	}
	if err := emit(continuation.Event{Kind: continuation.EventAssistantDelta, Text: response}); err != nil {
		return continuation.Completion{}, err
	}
	return continuation.Completion{StopReason: "stop"}, nil
}
