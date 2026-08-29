package computeruse_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/action"
	"github.com/bojieli/OpenRealtime/computeruse"
	"github.com/bojieli/OpenRealtime/trajectory"
)

type fakeSurface struct {
	mu      sync.Mutex
	actions []string
	lastX   int
	lastY   int
}

func (surface *fakeSurface) Name() string { return "fake" }

func (surface *fakeSurface) note(text string) error {
	surface.mu.Lock()
	defer surface.mu.Unlock()
	surface.actions = append(surface.actions, text)
	return nil
}

func (surface *fakeSurface) Click(_ context.Context, x, y int, button string) error {
	surface.mu.Lock()
	defer surface.mu.Unlock()
	surface.lastX, surface.lastY = x, y
	surface.actions = append(surface.actions, "click")
	return nil
}
func (surface *fakeSurface) ClickElement(context.Context, string) error {
	return surface.note("click-element")
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

func TestVocabularyHasStrictSchemas(t *testing.T) {
	definitions := computeruse.Definitions()
	if len(definitions) != 11 || len(computeruse.Names()) != 11 {
		t.Fatalf("expected eleven actions, got %d", len(definitions))
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

func TestNormalizedClickMapsExplicitlyIntoTheTargetPixelSpace(t *testing.T) {
	dispatcher, surface := newDispatcher(t)
	result, err := dispatcher.Dispatch(context.Background(), call(
		computeruse.ClickNormalized, `{"source":"screen","x":500,"y":500}`))
	if err != nil || result.Error != "" {
		t.Fatalf("normalized click: %v %s", err, result.Error)
	}
	surface.mu.Lock()
	x, y := surface.lastX, surface.lastY
	surface.mu.Unlock()
	if x != 640 || y != 360 {
		t.Fatalf("normalized midpoint mapped to (%d,%d), want (640,360)", x, y)
	}
	outside, _ := dispatcher.Dispatch(context.Background(), call(
		computeruse.ClickNormalized, `{"source":"screen","x":500,"y":1001}`))
	if !strings.Contains(outside.Error, "0..1000") {
		t.Fatalf("out-of-range normalized click was not refused: %q", outside.Error)
	}
}

func TestReflexActionsExcludeObservationControl(t *testing.T) {
	for _, name := range computeruse.Names() {
		want := name != computeruse.Screenshot && name != computeruse.Wait
		if got := computeruse.IsReflexAction(name); got != want {
			t.Fatalf("IsReflexAction(%q) = %t, want %t", name, got, want)
		}
	}
	for _, name := range []string{"computer.exfiltrate", "transfer_funds", ""} {
		if computeruse.IsReflexAction(name) {
			t.Fatalf("non-standard action %q entered the reflex vocabulary", name)
		}
	}
}

func TestSetOfMarkActionsResolveOnlyOnElementSurfaces(t *testing.T) {
	dispatcher, surface := newDispatcher(t)
	result, err := dispatcher.Dispatch(context.Background(), call(
		computeruse.ClickElement, `{"source":"screen","element_id":"7"}`))
	if err != nil || result.Error != "" {
		t.Fatalf("marked click: %v %s", err, result.Error)
	}
	if performed := surface.performed(); len(performed) != 1 || performed[0] != "click-element" {
		t.Fatalf("the mark must resolve through the element surface, got %v", performed)
	}

	plain := &pixelOnlySurface{}
	unsupported, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target:  computeruse.Target{Name: "display", Sources: []string{"screen"}, Width: 100, Height: 100},
		Surface: plain,
	})
	if err != nil {
		t.Fatal(err)
	}
	refused, _ := unsupported.Dispatch(context.Background(), call(
		computeruse.ClickElement, `{"source":"screen","element_id":"1"}`))
	if !strings.Contains(refused.Error, "does not support set-of-mark") {
		t.Fatalf("a pixel-only target must refuse element grounding, got %q", refused.Error)
	}
}

type blockingElementSurface struct {
	fakeSurface
	entered chan struct{}
	release chan struct{}
}

func (surface *blockingElementSurface) ClickElement(ctx context.Context, _ string) error {
	select {
	case surface.entered <- struct{}{}:
	case <-ctx.Done():
		return context.Cause(ctx)
	}
	select {
	case <-surface.release:
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

func TestConcurrentPhysicalActionsAreSerialized(t *testing.T) {
	surface := &blockingElementSurface{
		entered: make(chan struct{}), release: make(chan struct{}),
	}
	dispatcher, err := computeruse.NewDispatcher(computeruse.DispatcherConfig{
		Target: computeruse.Target{
			Name: "browser-1", Sources: []string{"screen"}, Width: 1280, Height: 720,
		},
		Surface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 2)
	dispatch := func(id string) {
		result, dispatchErr := dispatcher.Dispatch(ctx, trajectory.ToolCall{
			CallID: id, Name: computeruse.ClickElement,
			Arguments: json.RawMessage(`{"source":"screen","element_id":"1"}`),
		})
		if dispatchErr == nil && result.Error != "" {
			dispatchErr = context.Canceled
		}
		done <- dispatchErr
	}
	go dispatch("first")
	select {
	case <-surface.entered:
	case <-ctx.Done():
		t.Fatal("first physical action did not enter the surface")
	}
	go dispatch("second")
	select {
	case <-surface.entered:
		t.Fatal("the second physical action interleaved with the first")
	case <-time.After(50 * time.Millisecond):
	}
	surface.release <- struct{}{}
	select {
	case <-surface.entered:
	case <-ctx.Done():
		t.Fatal("the second physical action did not run after the first completed")
	}
	surface.release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("serialized action failed: %v", err)
		}
	}
}

type pixelOnlySurface struct{}

func (*pixelOnlySurface) Name() string                                     { return "pixels" }
func (*pixelOnlySurface) Click(context.Context, int, int, string) error    { return nil }
func (*pixelOnlySurface) DoubleClick(context.Context, int, int) error      { return nil }
func (*pixelOnlySurface) Move(context.Context, int, int) error             { return nil }
func (*pixelOnlySurface) Drag(context.Context, int, int, int, int) error   { return nil }
func (*pixelOnlySurface) Type(context.Context, string) error               { return nil }
func (*pixelOnlySurface) Key(context.Context, []string) error              { return nil }
func (*pixelOnlySurface) Scroll(context.Context, int, int, int, int) error { return nil }
func (*pixelOnlySurface) Screenshot(context.Context) error                 { return nil }

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
	if len(specs) != 11 {
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

// The model is told which sources exist, rather than asked to recover the name
// from prose.
//
// A live run produced source "video browser" - the observer name and the
// source name run together, taken from an observation that had honestly
// reported both. That looks like a model failure and is a schema failure:
// nothing in the vocabulary ever said what the sources were called.
func TestTheSourceFieldNamesTheSourcesTheTargetOwns(t *testing.T) {
	target := computeruse.Target{
		Name: "surface-browser", Sources: []string{"browser"}, Width: 1024, Height: 768,
	}
	declared, err := computeruse.DefinitionsFor(target)
	if err != nil {
		t.Fatalf("definitions: %v", err)
	}
	if len(declared) != len(computeruse.Names()) {
		t.Fatalf("narrowing must not drop an action, got %d of %d",
			len(declared), len(computeruse.Names()))
	}
	for _, definition := range declared {
		if definition.Name == computeruse.Wait {
			// The one action that touches no screen takes no source.
			if strings.Contains(string(definition.Parameters), `"source"`) {
				t.Fatalf("computer.wait must not take a source, got %s", definition.Parameters)
			}
			continue
		}
		var schema struct {
			Properties struct {
				Source struct {
					Enum []string `json:"enum"`
				} `json:"source"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatalf("%s: %v", definition.Name, err)
		}
		if len(schema.Properties.Source.Enum) != 1 || schema.Properties.Source.Enum[0] != "browser" {
			t.Fatalf("%s must name the declared source, got %v",
				definition.Name, schema.Properties.Source.Enum)
		}
	}

	// The target-free form stays target-free: it is what gets published with
	// the specification, and a schema naming one deployment's sources would be
	// the wrong thing to publish.
	for _, definition := range computeruse.Definitions() {
		if strings.Contains(string(definition.Parameters), `"enum":["browser"]`) {
			t.Fatalf("Definitions must not carry a target's sources, got %s", definition.Parameters)
		}
	}

	if _, err := computeruse.DefinitionsFor(computeruse.Target{Name: "x"}); err == nil {
		t.Fatal("a target with no sources cannot narrow anything and must be refused")
	}
}

func TestTargetSchemasNameTheExactCoordinateSpace(t *testing.T) {
	target := computeruse.Target{
		Name: "surface-browser", Sources: []string{"browser"}, Width: 1280, Height: 577,
	}
	declared, err := computeruse.DefinitionsFor(target)
	if err != nil {
		t.Fatalf("definitions: %v", err)
	}
	want := map[string]map[string]float64{
		computeruse.Click:           {"x": 1279, "y": 576},
		computeruse.ClickNormalized: {"x": 1000, "y": 1000},
		computeruse.DoubleClick:     {"x": 1279, "y": 576},
		computeruse.Move:            {"x": 1279, "y": 576},
		computeruse.Drag: {
			"from_x": 1279, "from_y": 576, "to_x": 1279, "to_y": 576,
		},
		computeruse.Scroll: {"x": 1279, "y": 576},
	}
	for _, definition := range declared {
		coordinates, relevant := want[definition.Name]
		if !relevant {
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Minimum *float64 `json:"minimum"`
				Maximum *float64 `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatalf("%s: %v", definition.Name, err)
		}
		for name, maximum := range coordinates {
			property := schema.Properties[name]
			if property.Minimum == nil || *property.Minimum != 0 ||
				property.Maximum == nil || *property.Maximum != maximum {
				t.Errorf("%s.%s bounds = [%v,%v], want [0,%v]",
					definition.Name, name, property.Minimum, property.Maximum, maximum)
			}
		}
	}

	// The portable vocabulary does not pretend to know a deployment's
	// dimensions. Only DefinitionsFor may publish target-specific maxima.
	for _, definition := range computeruse.Definitions() {
		if definition.Name == computeruse.ClickNormalized {
			// Its coordinate space is definitionally fixed, not guessed from a
			// deployment target.
			continue
		}
		var schema struct {
			Properties map[string]struct {
				Maximum *float64 `json:"maximum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(definition.Parameters, &schema); err != nil {
			t.Fatalf("%s: %v", definition.Name, err)
		}
		for _, name := range []string{"x", "y", "from_x", "from_y", "to_x", "to_y"} {
			if property, present := schema.Properties[name]; present && property.Maximum != nil {
				t.Errorf("portable %s.%s unexpectedly has maximum %v",
					definition.Name, name, *property.Maximum)
			}
		}
	}
}
