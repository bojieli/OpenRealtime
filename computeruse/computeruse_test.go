package computeruse_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type fakeSurface struct {
	mu      sync.Mutex
	actions []string
}

func (surface *fakeSurface) Name() string { return "fake" }

func (surface *fakeSurface) note(text string) error {
	surface.mu.Lock()
	defer surface.mu.Unlock()
	surface.actions = append(surface.actions, text)
	return nil
}

func (surface *fakeSurface) Click(_ context.Context, x, y int, button string) error {
	return surface.note("click")
}
func (surface *fakeSurface) DoubleClick(context.Context, int, int) error {
	return surface.note("double")
}
func (surface *fakeSurface) Move(context.Context, int, int) error { return surface.note("move") }
func (surface *fakeSurface) Drag(context.Context, int, int, int, int) error {
	return surface.note("drag")
}
func (surface *fakeSurface) Type(context.Context, string) error  { return surface.note("type") }
func (surface *fakeSurface) Key(context.Context, []string) error { return surface.note("key") }
func (surface *fakeSurface) Scroll(context.Context, int, int, int, int) error {
	return surface.note("scroll")
}
func (surface *fakeSurface) Screenshot(context.Context) error { return surface.note("screenshot") }

func (surface *fakeSurface) performed() []string {
	surface.mu.Lock()
	defer surface.mu.Unlock()
	return append([]string(nil), surface.actions...)
}

func newDispatcher(t *testing.T) (*computeruse.Dispatcher, *fakeSurface) {
	t.Helper()
	surface := &fakeSurface{}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target: computeruse.Target{
			Name: "browser-1", Sources: []string{"screen"}, Width: 1280, Height: 720,
		},
		Surface: surface,
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	return dispatcher, surface
}

func call(name, arguments string) trajectory.ToolCall {
	return trajectory.ToolCall{CallID: "c1", Name: name, Arguments: json.RawMessage(arguments)}
}

func TestVocabularyIsNineActionsWithStrictSchemas(t *testing.T) {
	definitions := computeruse.Definitions()
	if len(definitions) != 9 || len(computeruse.Names()) != 9 {
		t.Fatalf("expected nine actions, got %d", len(definitions))
	}
	for _, definition := range definitions {
		if !computeruse.IsAction(definition.Name) {
			t.Fatalf("%q is outside the namespace", definition.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatalf("%s schema: %v", definition.Name, err)
		}
		if schema["additionalProperties"] != false {
			t.Fatalf("%s must reject unknown arguments: an action with a misread argument acts on the wrong thing", definition.Name)
		}
		if _, present := schema["required"]; !present {
			t.Fatalf("%s must declare required arguments", definition.Name)
		}
		if definition.Name != computeruse.Wait {
			properties := schema["properties"].(map[string]any)
			if _, present := properties["source"]; !present {
				t.Fatalf("%s must name the coordinate space it acts on", definition.Name)
			}
		}
	}
}

func TestActionsAreGroundedInTheDeclaredSpace(t *testing.T) {
	dispatcher, surface := newDispatcher(t)
	ctx := context.Background()

	result, err := dispatcher.Dispatch(ctx, call(computeruse.Click, `{"source":"screen","x":100,"y":100}`))
	if err != nil || result.Error != "" {
		t.Fatalf("a grounded click must succeed: %v %s", err, result.Error)
	}
	if string(result.Output) != `"clicked"` {
		t.Fatalf("the output stays text, got %s", result.Output)
	}

	outside, _ := dispatcher.Dispatch(ctx, call(computeruse.Click, `{"source":"screen","x":5000,"y":100}`))
	if !strings.Contains(outside.Error, "outside") {
		t.Fatalf("a click outside the space the model saw must be refused, got %q", outside.Error)
	}
	foreign, _ := dispatcher.Dispatch(ctx, call(computeruse.Click, `{"source":"desktop","x":10,"y":10}`))
	if !strings.Contains(foreign.Error, "does not own") {
		t.Fatalf("blast radius is bounded by the declared target, got %q", foreign.Error)
	}
	if performed := surface.performed(); len(performed) != 1 {
		t.Fatalf("only the grounded click may reach the surface, got %v", performed)
	}
}

func TestFailedActionsReturnEvidenceRatherThanAnError(t *testing.T) {
	dispatcher, _ := newDispatcher(t)
	result, err := dispatcher.Dispatch(context.Background(), call(computeruse.Type, `{"source":"screen","text":""}`))
	if err != nil {
		t.Fatalf("a refused action is an outcome, not a transport failure: %v", err)
	}
	if result.Error == "" {
		t.Fatal("the model must see that the action did not work")
	}
	if result.CallID != "c1" || result.Name != computeruse.Type {
		t.Fatal("a result must keep its call identity")
	}
}

func TestEveryActionIsAudited(t *testing.T) {
	var seen []computeruse.Record
	surface := &fakeSurface{}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target:  computeruse.Target{Name: "browser-1", Sources: []string{"screen"}, Width: 800, Height: 600},
		Surface: surface,
		Audit:   func(record computeruse.Record) { seen = append(seen, record) },
	})
	if err != nil {
		t.Fatalf("new dispatcher: %v", err)
	}
	ctx := context.Background()
	_, _ = dispatcher.Dispatch(ctx, call(computeruse.Click, `{"source":"screen","x":1,"y":1}`))
	_, _ = dispatcher.Dispatch(ctx, call(computeruse.Click, `{"source":"screen","x":9000,"y":1}`))
	if len(seen) != 2 || len(dispatcher.Records()) != 2 {
		t.Fatalf("both the performed and the refused action must be audited, got %d", len(seen))
	}
	if seen[0].Target != "browser-1" || seen[0].Source != "screen" {
		t.Fatalf("the audit record must name what was acted on: %+v", seen[0])
	}
	if seen[1].Error == "" {
		t.Fatal("a refusal must be recorded as one")
	}
}

func TestSpecsCarryTargetsAndConfirmationDefaults(t *testing.T) {
	dispatcher, _ := newDispatcher(t)
	target := computeruse.Target{Name: "browser-1", Sources: []string{"screen"}, Width: 1280, Height: 720}
	specs, err := computeruse.Specs(target, dispatcher, nil)
	if err != nil {
		t.Fatalf("specs: %v", err)
	}
	if len(specs) != 9 {
		t.Fatalf("expected the whole vocabulary, got %d", len(specs))
	}
	byName := make(map[string]action.ToolSpec, len(specs))
	for _, spec := range specs {
		if spec.Target != "browser-1" || spec.Dispatcher == nil {
			t.Fatalf("%s must be bound to its target and dispatcher", spec.Name)
		}
		byName[spec.Name] = spec
	}
	if byName[computeruse.Click].Confirm != action.ConfirmPolicy {
		t.Fatal("clicking defaults to asking")
	}
	if byName[computeruse.Screenshot].Confirm != action.ConfirmNever {
		t.Fatal("observing does not need authorization")
	}

	overridden, err := computeruse.Specs(target, dispatcher, map[string]action.Confirm{
		computeruse.Click: action.ConfirmAlways,
	})
	if err != nil {
		t.Fatalf("override: %v", err)
	}
	for _, spec := range overridden {
		if spec.Name == computeruse.Click && spec.Confirm != action.ConfirmAlways {
			t.Fatal("a deployment's declaration must win over the default")
		}
	}
}

func TestTargetsMustDeclareASpace(t *testing.T) {
	dispatcher, _ := newDispatcher(t)
	if _, err := computeruse.Specs(computeruse.Target{Name: "x"}, dispatcher, nil); err == nil {
		t.Fatal("a target with no sources must be rejected")
	}
	if _, err := computeruse.Specs(computeruse.Target{
		Name: "x", Sources: []string{"screen"},
	}, dispatcher, nil); err == nil {
		t.Fatal("a target with no coordinate space must be rejected")
	}
	if _, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target: computeruse.Target{Name: "x", Sources: []string{"s"}, Width: 1, Height: 1},
	}); err == nil {
		t.Fatal("a dispatcher with no surface must be rejected")
	}
}

func TestWaitIsBounded(t *testing.T) {
	dispatcher, _ := newDispatcher(t)
	result, err := dispatcher.Dispatch(context.Background(), call(computeruse.Wait, `{"duration_ms":1}`))
	if err != nil || result.Error != "" {
		t.Fatalf("wait: %v %s", err, result.Error)
	}
	if string(result.Output) != `"waited"` {
		t.Fatalf("unexpected output %s", result.Output)
	}
}

// Every action in the namespace that changes anything declares "policy" by
// default. If the policy denies, the agent can move the pointer and take
// screenshots and can never press anything - which is not a safe deployment,
// it is a broken one. So the fence has to be the declared target, and it has
// to actually admit work inside it.
func TestTargetPolicyAdmitsActionsInsideTheDeclaredTarget(t *testing.T) {
	target := computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 800,
	}
	policy := computeruse.TargetPolicy(target)

	for _, admitted := range []trajectory.ToolCall{
		{CallID: "c1", Name: computeruse.Click, Arguments: json.RawMessage(`{"source":"screen","x":10,"y":10}`)},
		{CallID: "c2", Name: computeruse.Type, Arguments: json.RawMessage(`{"source":"screen","text":"hello"}`)},
		{CallID: "c3", Name: computeruse.Wait, Arguments: json.RawMessage(`{"duration_ms":100}`)},
	} {
		if !policy(admitted) {
			t.Fatalf("an action inside the declared target must be admitted: %s", admitted.Name)
		}
	}

	for _, refused := range []trajectory.ToolCall{
		{CallID: "d1", Name: computeruse.Click, Arguments: json.RawMessage(`{"source":"camera","x":10,"y":10}`)},
		{CallID: "d2", Name: computeruse.Click, Arguments: json.RawMessage(`{"x":10,"y":10}`)},
		{CallID: "d3", Name: "transfer_funds", Arguments: json.RawMessage(`{"source":"screen"}`)},
		{CallID: "d4", Name: computeruse.Click, Arguments: json.RawMessage(`not json`)},
	} {
		if policy(refused) {
			t.Fatalf("an action outside the declared target must be refused: %s", refused.Name)
		}
	}
}

// The whole point of a policy is that it is narrower than "yes". A tool that
// declares "always" is a different requirement and this must not answer it.
func TestTargetPolicyDoesNotAnswerAnAlwaysRequirement(t *testing.T) {
	target := computeruse.Target{
		Name: "browser", Sources: []string{"screen"}, Width: 1280, Height: 800,
	}
	dispatcher, _ := newDispatcher(t)
	specs, err := computeruse.Specs(target, dispatcher, map[string]action.Confirm{
		computeruse.Click: action.ConfirmAlways,
	})
	if err != nil {
		t.Fatalf("specs: %v", err)
	}
	for _, spec := range specs {
		if spec.Name == computeruse.Click && spec.Confirm != action.ConfirmAlways {
			t.Fatalf("an explicit override must survive, got %q", spec.Confirm)
		}
	}
}
