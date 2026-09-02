package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/ir"
)

func TestStateLifecycleRestorationRegistrationAndSeal(t *testing.T) {
	restored := json.RawMessage(`{"a":1}`)
	transfer := &element.StateTransferCapabilities{Snapshot: true, Restore: true, Quiesce: true}
	state := newStateLifecycle("stateful", "schema://state/v1", transfer, restored)
	got, available, err := state.Restored()
	if err != nil || !available || string(got) != string(restored) {
		t.Fatalf("Restored() = %s, %t, %v", got, available, err)
	}
	got[5] = '9'
	if string(state.restored) != string(restored) {
		t.Fatal("Restored returned an aliased byte slice")
	}
	if _, _, err := state.Restored(); err == nil || !strings.Contains(err.Error(), "already consumed") {
		t.Fatalf("second Restored() error = %v", err)
	}

	snapshot := element.StateSnapshotter(func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(`{"a":1}`), nil
	})
	quiesce := element.StateQuiescer(func(context.Context) (element.StateResumer, error) {
		return func(context.Context) error { return nil }, nil
	})
	if err := state.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := state.Quiesce(quiesce); err != nil {
		t.Fatal(err)
	}
	sealedSnapshot, sealedQuiesce, err := state.seal()
	if err != nil || sealedSnapshot == nil || sealedQuiesce == nil {
		t.Fatalf("seal() = (%v, %v, %v)", sealedSnapshot, sealedQuiesce, err)
	}
	if _, _, err := state.Restored(); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("post-seal Restored() error = %v", err)
	}
	if err := state.Snapshot(snapshot); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("post-seal Snapshot() error = %v", err)
	}
	if err := state.Quiesce(quiesce); err == nil || !strings.Contains(err.Error(), "sealed") {
		t.Fatalf("post-seal Quiesce() error = %v", err)
	}
	if _, _, err := state.seal(); err == nil || !strings.Contains(err.Error(), "already sealed") {
		t.Fatalf("second seal() error = %v", err)
	}
}

func TestStateLifecycleValidationAndCompatibility(t *testing.T) {
	all := &element.StateTransferCapabilities{Snapshot: true, Restore: true, Quiesce: true}
	state := newStateLifecycle("stateful", "schema://state/v1", all, nil)
	if err := state.Snapshot(nil); err == nil || !strings.Contains(err.Error(), "requires a callback") {
		t.Fatalf("nil snapshot error = %v", err)
	}
	if err := state.Quiesce(nil); err == nil || !strings.Contains(err.Error(), "requires a callback") {
		t.Fatalf("nil quiescer error = %v", err)
	}
	snapshot := func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }
	if err := state.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if err := state.Snapshot(snapshot); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("duplicate snapshot error = %v", err)
	}
	quiesce := func(context.Context) (element.StateResumer, error) {
		return func(context.Context) error { return nil }, nil
	}
	if err := state.Quiesce(quiesce); err != nil {
		t.Fatal(err)
	}
	if err := state.Quiesce(quiesce); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("duplicate quiescer error = %v", err)
	}

	// StateSchema alone does not grant a live-transfer operation.
	withoutCallbacks := newStateLifecycle("legacy", "schema://state/v1", nil, nil)
	if _, _, err := withoutCallbacks.Restored(); err == nil || !strings.Contains(err.Error(), "does not declare state restore") {
		t.Fatalf("undeclared restore error = %v", err)
	}
	if err := withoutCallbacks.Snapshot(snapshot); err == nil || !strings.Contains(err.Error(), "does not declare state snapshot") {
		t.Fatalf("undeclared snapshot error = %v", err)
	}
	if err := withoutCallbacks.Quiesce(quiesce); err == nil || !strings.Contains(err.Error(), "does not declare state quiesce") {
		t.Fatalf("undeclared quiesce error = %v", err)
	}
	gotSnapshot, gotQuiesce, err := withoutCallbacks.seal()
	if err != nil || gotSnapshot != nil || gotQuiesce != nil {
		t.Fatalf("compatible seal = (%v, %v, %v)", gotSnapshot, gotQuiesce, err)
	}

	restore := &element.StateTransferCapabilities{Restore: true}
	unconsumed := newStateLifecycle("unconsumed", "schema://state/v1", restore, json.RawMessage(`{}`))
	if _, _, err := unconsumed.seal(); err == nil || !strings.Contains(err.Error(), "did not consume") {
		t.Fatalf("unconsumed restored state error = %v", err)
	}
	restoreOnly := newStateLifecycle("restore-only", "schema://state/v1", restore, json.RawMessage(`{}`))
	if _, available, err := restoreOnly.Restored(); err != nil || !available {
		t.Fatalf("restore-only consumption = available %t, error %v", available, err)
	}
	if snapshot, quiesce, err := restoreOnly.seal(); err != nil || snapshot != nil || quiesce != nil {
		t.Fatalf("restore-only seal = (%v, %v, %v)", snapshot, quiesce, err)
	}

	snapshotAndQuiesce := &element.StateTransferCapabilities{Snapshot: true, Quiesce: true}
	missingSnapshot := newStateLifecycle("missing-snapshot", "schema://state/v1", snapshotAndQuiesce, nil)
	if err := missingSnapshot.Quiesce(quiesce); err != nil {
		t.Fatal(err)
	}
	if _, _, err := missingSnapshot.seal(); err == nil || !strings.Contains(err.Error(), "without registering a callback") {
		t.Fatalf("missing snapshot callback error = %v", err)
	}

	missingQuiesce := newStateLifecycle("missing-quiesce", "schema://state/v1", snapshotAndQuiesce, nil)
	if err := missingQuiesce.Snapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, _, err := missingQuiesce.seal(); err == nil || !strings.Contains(err.Error(), "quiesce capability without registering") {
		t.Fatalf("missing quiesce callback error = %v", err)
	}
}

func TestCanonicalStateSnapshotBoundsAndStrictness(t *testing.T) {
	canonical, digest, err := canonicalStateSnapshot(json.RawMessage(` { "z": 2, "a": 1.0 } `))
	if err != nil || string(canonical) != `{"a":1,"z":2}` || !strings.HasPrefix(digest, "sha256:") {
		t.Fatalf("canonical snapshot = %s, %q, %v", canonical, digest, err)
	}
	exact := json.RawMessage(`{"x":"` + strings.Repeat("a", MaximumStateSnapshotBytes-8) + `"}`)
	if len(exact) != MaximumStateSnapshotBytes {
		t.Fatalf("exact fixture length = %d", len(exact))
	}
	if got, _, err := canonicalStateSnapshot(exact); err != nil || len(got) != MaximumStateSnapshotBytes {
		t.Fatalf("exact-limit snapshot length = %d, error %v", len(got), err)
	}

	tests := []struct {
		name string
		raw  json.RawMessage
		want string
	}{
		{name: "empty", want: "empty"},
		{name: "malformed", raw: json.RawMessage(`{"a":`), want: "strict JSON object"},
		{name: "scalar", raw: json.RawMessage(`true`), want: "JSON object"},
		{name: "array", raw: json.RawMessage(`[]`), want: "JSON object"},
		{name: "null", raw: json.RawMessage(`null`), want: "JSON object"},
		{name: "trailing", raw: json.RawMessage(`{} {}`), want: "strict JSON object"},
		{name: "duplicate", raw: json.RawMessage(`{"a":1,"a":2}`), want: "strict JSON object"},
		{name: "pre-limit", raw: append(json.RawMessage(`{}`), []byte(strings.Repeat(" ", MaximumStateSnapshotBytes))...), want: "limit"},
		{name: "canonical-limit", raw: json.RawMessage(`{"x":"` + strings.Repeat("<", MaximumStateSnapshotBytes/2) + `"}`), want: "canonical state snapshot"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := canonicalStateSnapshot(test.raw); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("canonicalStateSnapshot() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestMountedStateQuiesceCaptureAndResumeOrder(t *testing.T) {
	var mu sync.Mutex
	var order []string
	quiesced := make(map[string]bool)
	nodes := make([]mountedNode, 0, 3)
	for _, name := range []string{"alpha", "beta", "gamma"} {
		name := name
		nodes = append(nodes, mountedNode{
			id: name, identity: element.Identity{Name: "test.State", Revision: 1, Digest: "sha256:" + strings.Repeat("a", 64)},
			implementation: "impl://" + name, stateSchema: "schema://state/v1",
			stateTransfer: testStateTransfer(true, false, true),
			quiesce: func(context.Context) (element.StateResumer, error) {
				mu.Lock()
				order = append(order, "quiesce:"+name)
				quiesced[name] = true
				mu.Unlock()
				return func(context.Context) error {
					mu.Lock()
					defer mu.Unlock()
					order = append(order, "resume:"+name)
					quiesced[name] = false
					return nil
				}, nil
			},
			snapshot: func(context.Context) (json.RawMessage, error) {
				mu.Lock()
				defer mu.Unlock()
				if !quiesced[name] {
					return nil, errors.New("snapshot ran before quiescence")
				}
				return json.RawMessage(`{"node":"` + name + `"}`), nil
			},
		})
	}
	mounted := &Mounted{nodes: nodes, timeout: time.Second}
	selected := map[string]struct{}{"alpha": {}, "beta": {}, "gamma": {}}
	barriers, err := mounted.quiesceState(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	captures, err := mounted.captureState(context.Background(), selected)
	if err != nil {
		t.Fatal(err)
	}
	if len(captures) != 3 || captures[0].node != "alpha" ||
		string(captures[0].snapshot) != `{"node":"alpha"}` || captures[0].digest == "" ||
		captures[0].implementation != "impl://alpha" {
		t.Fatalf("captures = %+v", captures)
	}
	if err := mounted.resumeState(barriers); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"quiesce:gamma", "quiesce:beta", "quiesce:alpha",
		"resume:alpha", "resume:beta", "resume:gamma",
	}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("state barrier order = %v, want %v", order, want)
	}
}

func TestMountedStateSelectionRejectsUnknownNodesDeterministically(t *testing.T) {
	mounted := &Mounted{nodes: []mountedNode{{id: "known"}}, timeout: time.Second}
	err := mounted.requireTransferableState(map[string]struct{}{
		"zeta":  {},
		"alpha": {},
	})
	if err == nil || !strings.Contains(err.Error(), "unknown nodes: [alpha zeta]") {
		t.Fatalf("unknown state selection error = %v", err)
	}
}

func TestMountedStateFailureContainment(t *testing.T) {
	t.Run("quiesce failure resumes admitted nodes", func(t *testing.T) {
		var order []string
		failure := errors.New("cannot drain")
		mounted := &Mounted{timeout: time.Second, nodes: []mountedNode{
			{id: "alpha", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true), quiesce: func(context.Context) (element.StateResumer, error) {
				order = append(order, "quiesce:alpha")
				return func(context.Context) error { order = append(order, "resume:alpha"); return nil }, failure
			}},
			{id: "beta", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true), quiesce: func(context.Context) (element.StateResumer, error) {
				order = append(order, "quiesce:beta")
				return func(context.Context) error { order = append(order, "resume:beta"); return nil }, nil
			}},
		}}
		_, err := mounted.quiesceState(context.Background(), map[string]struct{}{"alpha": {}, "beta": {}})
		if !errors.Is(err, failure) {
			t.Fatalf("quiesce error = %v", err)
		}
		want := []string{"quiesce:beta", "quiesce:alpha", "resume:alpha", "resume:beta"}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("failure recovery order = %v, want %v", order, want)
		}
	})

	t.Run("capture failure resumes all quiesced nodes", func(t *testing.T) {
		var order []string
		failure := errors.New("snapshot failed")
		mounted := &Mounted{timeout: time.Second, nodes: []mountedNode{
			{
				id: "alpha", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true),
				quiesce: func(context.Context) (element.StateResumer, error) {
					order = append(order, "quiesce:alpha")
					return func(context.Context) error { order = append(order, "resume:alpha"); return nil }, nil
				},
				snapshot: func(context.Context) (json.RawMessage, error) {
					order = append(order, "snapshot:alpha")
					return nil, failure
				},
			},
			{
				id: "beta", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true),
				quiesce: func(context.Context) (element.StateResumer, error) {
					order = append(order, "quiesce:beta")
					return func(context.Context) error { order = append(order, "resume:beta"); return nil }, nil
				},
				snapshot: func(context.Context) (json.RawMessage, error) {
					order = append(order, "snapshot:beta")
					return json.RawMessage(`{}`), nil
				},
			},
		}}
		captures, barriers, err := mounted.captureStateAtBarrier(
			context.Background(), map[string]struct{}{"alpha": {}, "beta": {}},
		)
		if !errors.Is(err, failure) || captures != nil || barriers != nil {
			t.Fatalf("barrier capture = captures %+v barriers %+v error %v", captures, barriers, err)
		}
		want := []string{
			"quiesce:beta", "quiesce:alpha", "snapshot:alpha",
			"resume:alpha", "resume:beta",
		}
		if !reflect.DeepEqual(order, want) {
			t.Fatalf("capture recovery order = %v, want %v", order, want)
		}
	})

	tests := []struct {
		name string
		node mountedNode
		call func(*Mounted) error
		want string
	}{
		{
			name: "quiesce panic",
			node: mountedNode{id: "node", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true), snapshot: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, quiesce: func(context.Context) (element.StateResumer, error) { panic("quiesce panic") }},
			call: func(m *Mounted) error {
				_, err := m.quiesceState(context.Background(), map[string]struct{}{"node": {}})
				return err
			},
			want: "quiescer panicked",
		},
		{
			name: "quiesce timeout",
			node: mountedNode{id: "node", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, true), snapshot: func(context.Context) (json.RawMessage, error) { return json.RawMessage(`{}`), nil }, quiesce: func(ctx context.Context) (element.StateResumer, error) { <-ctx.Done(); return nil, context.Cause(ctx) }},
			call: func(m *Mounted) error {
				_, err := m.quiesceState(context.Background(), map[string]struct{}{"node": {}})
				return err
			},
			want: "deadline exceeded",
		},
		{
			name: "snapshot panic",
			node: mountedNode{id: "node", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, false), snapshot: func(context.Context) (json.RawMessage, error) { panic("snapshot panic") }},
			call: func(m *Mounted) error {
				_, err := m.captureState(context.Background(), map[string]struct{}{"node": {}})
				return err
			},
			want: "snapshotter panicked",
		},
		{
			name: "snapshot timeout",
			node: mountedNode{id: "node", stateSchema: "schema://state/v1", stateTransfer: testStateTransfer(true, false, false), snapshot: func(ctx context.Context) (json.RawMessage, error) { <-ctx.Done(); return nil, context.Cause(ctx) }},
			call: func(m *Mounted) error {
				_, err := m.captureState(context.Background(), map[string]struct{}{"node": {}})
				return err
			},
			want: "deadline exceeded",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mounted := &Mounted{nodes: []mountedNode{test.node}, timeout: 5 * time.Millisecond}
			if err := test.call(mounted); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("contained callback error = %v, want %q", err, test.want)
			}
		})
	}

	mounted := &Mounted{timeout: time.Second}
	resumeFailure := errors.New("resume failed")
	err := mounted.resumeState([]stateQuiescence{
		{node: "panic", resume: func(context.Context) error { panic("resume panic") }},
		{node: "failure", resume: func(context.Context) error { return resumeFailure }},
	})
	if !errors.Is(err, resumeFailure) || !strings.Contains(err.Error(), "resumer panicked") {
		t.Fatalf("resume containment error = %v", err)
	}

	timeoutMounted := &Mounted{timeout: 5 * time.Millisecond}
	err = timeoutMounted.resumeState([]stateQuiescence{{
		node: "timeout", resume: func(ctx context.Context) error {
			<-ctx.Done()
			return context.Cause(ctx)
		},
	}})
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("resume timeout error = %v", err)
	}
}

func TestMountStateLifecycleAndPrivateRestoration(t *testing.T) {
	descriptor := stateTestDescriptor("schema://state/v1")
	descriptor.StateTransfer = testStateTransfer(true, true, false)
	var captured element.StateLifecycle
	factory := &stateTestFactory{descriptor: descriptor, mount: func(mount element.MountContext) error {
		captured = mount.State
		restored, available, err := mount.State.Restored()
		if err != nil || !available || string(restored) != `{"a":1,"z":2}` {
			return errors.New("restored state was not canonical and available")
		}
		return mount.State.Snapshot(func(context.Context) (json.RawMessage, error) {
			return json.RawMessage(`{"a":1,"z":2}`), nil
		})
	}}
	registry := NewRegistry()
	if err := registry.Register("", factory); err != nil {
		t.Fatal(err)
	}
	mounted, err := mount(context.Background(), Config{
		Graph: stateTestGraph(t, descriptor), Registry: registry,
	}, map[string]json.RawMessage{"node": json.RawMessage(`{"z":2,"a":1.0}`)})
	if err != nil {
		t.Fatal(err)
	}
	if mounted.nodes[0].snapshot == nil || mounted.nodes[0].stateSchema != descriptor.StateSchema {
		t.Fatalf("mounted state callbacks = %+v", mounted.nodes[0])
	}
	if err := captured.Snapshot(func(context.Context) (json.RawMessage, error) { return nil, nil }); err == nil ||
		!strings.Contains(err.Error(), "sealed") {
		t.Fatalf("late state registration error = %v", err)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMountStateCompatibilityAndRestorationRefusal(t *testing.T) {
	stateful := stateTestDescriptor("schema://state/v1")
	legacy := &stateTestFactory{descriptor: stateful}
	registry := NewRegistry()
	if err := registry.Register("", legacy); err != nil {
		t.Fatal(err)
	}
	mounted, err := Mount(context.Background(), Config{Graph: stateTestGraph(t, stateful), Registry: registry})
	if err != nil {
		t.Fatalf("ordinary schema-only mount broke compatibility: %v", err)
	}
	if err := mounted.requireTransferableState(map[string]struct{}{"node": {}}); err == nil ||
		!strings.Contains(err.Error(), "does not declare state snapshot") {
		t.Fatalf("non-transferable state error = %v", err)
	}
	if err := mounted.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	var schemaOnlyMounts atomic.Int32
	schemaOnlyRegistry := NewRegistry()
	if err := schemaOnlyRegistry.Register("", &stateTestFactory{
		descriptor: stateful, mounts: &schemaOnlyMounts,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := mount(context.Background(), Config{
		Graph: stateTestGraph(t, stateful), Registry: schemaOnlyRegistry,
	}, map[string]json.RawMessage{"node": json.RawMessage(`{}`)}); err == nil ||
		!strings.Contains(err.Error(), "requires declared restore capability") {
		t.Fatalf("schema-only restoration error = %v", err)
	}
	if schemaOnlyMounts.Load() != 0 {
		t.Fatalf("schema-only restoration acquired resources through %d mounts", schemaOnlyMounts.Load())
	}

	transferable := stateTestDescriptor("schema://state/v1")
	transferable.StateTransfer = testStateTransfer(true, true, false)
	var mounts atomic.Int32
	unconsumed := &stateTestFactory{descriptor: transferable, mounts: &mounts}
	unconsumedRegistry := NewRegistry()
	if err := unconsumedRegistry.Register("", unconsumed); err != nil {
		t.Fatal(err)
	}
	if _, err := mount(context.Background(), Config{
		Graph: stateTestGraph(t, transferable), Registry: unconsumedRegistry,
	}, map[string]json.RawMessage{"node": json.RawMessage(`{}`)}); err == nil ||
		!strings.Contains(err.Error(), "did not consume restored state") {
		t.Fatalf("unconsumed mount error = %v", err)
	}
	if mounts.Load() != 1 {
		t.Fatalf("unconsumed factory mounts = %d", mounts.Load())
	}

	missingSnapshot := stateTestDescriptor("schema://state/v1")
	missingSnapshot.StateTransfer = testStateTransfer(true, false, false)
	var missingSnapshotMounts atomic.Int32
	missingSnapshotRegistry := NewRegistry()
	if err := missingSnapshotRegistry.Register("", &stateTestFactory{
		descriptor: missingSnapshot, mounts: &missingSnapshotMounts,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := Mount(context.Background(), Config{
		Graph: stateTestGraph(t, missingSnapshot), Registry: missingSnapshotRegistry,
	}); err == nil || !strings.Contains(err.Error(), "snapshot capability without registering") {
		t.Fatalf("missing declared snapshot callback error = %v", err)
	}
	if missingSnapshotMounts.Load() != 1 {
		t.Fatalf("missing-snapshot factory mounts = %d", missingSnapshotMounts.Load())
	}

	stateless := stateTestDescriptor("")
	var statelessMounts atomic.Int32
	statelessRegistry := NewRegistry()
	if err := statelessRegistry.Register("", &stateTestFactory{descriptor: stateless, mounts: &statelessMounts}); err != nil {
		t.Fatal(err)
	}
	invalid := []map[string]json.RawMessage{
		{"missing": json.RawMessage(`{}`)},
		{"node": json.RawMessage(`{}`)},
		{"node": json.RawMessage(`{"a":1,"a":2}`)},
		{"node": json.RawMessage(strings.Repeat(" ", MaximumStateSnapshotBytes+1))},
	}
	for _, restored := range invalid {
		if _, err := mount(context.Background(), Config{
			Graph: stateTestGraph(t, stateless), Registry: statelessRegistry,
		}, restored); err == nil {
			t.Fatalf("invalid restored state was accepted: keys=%v", reflect.ValueOf(restored).MapKeys())
		}
	}
	if statelessMounts.Load() != 0 {
		t.Fatalf("invalid restoration acquired resources through %d mounts", statelessMounts.Load())
	}
}

type stateTestFactory struct {
	descriptor element.Descriptor
	mount      func(element.MountContext) error
	mounts     *atomic.Int32
}

func (factory *stateTestFactory) Descriptor() element.Descriptor {
	return factory.descriptor.Clone()
}

func (factory *stateTestFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	if factory.mounts != nil {
		factory.mounts.Add(1)
	}
	if (factory.descriptor.StateTransfer == nil) != (mount.State == nil) {
		return nil, errors.New("state lifecycle availability differs from transfer contract")
	}
	if factory.mount != nil {
		if err := factory.mount(mount); err != nil {
			return nil, err
		}
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		<-ctx.Done()
		return nil
	}), nil
}

func stateTestDescriptor(schema string) element.Descriptor {
	typeValue := element.Event(element.Named("test.Value"))
	return element.Descriptor{
		FormatVersion: element.DescriptorFormatVersion, Name: "test.State", Revision: 1,
		Ports: []element.Port{
			{Name: "in", Direction: element.Input, Type: typeValue, Cardinality: element.One, Required: true, DefaultDepth: 1},
			{Name: "out", Direction: element.Output, Type: typeValue, Cardinality: element.One, Required: true, DefaultDepth: 1},
		},
		Reaction:    element.Reaction{Triggers: []string{"in"}, Outcomes: []string{"out"}, MaxConcurrency: 1},
		StateSchema: schema,
	}
}

func stateTestGraph(t *testing.T, descriptor element.Descriptor) ir.Graph {
	t.Helper()
	identity, err := descriptor.Identity()
	if err != nil {
		t.Fatal(err)
	}
	typeValue := element.Event(element.Named("test.Value"))
	graph, err := ir.Freeze(ir.Graph{
		FormatVersion: ir.FormatVersion, ID: "state-test", Revision: 1,
		Nodes: []ir.Node{{
			ID: "node", Element: identity,
			Ports: []ir.Port{
				{Name: "in", Direction: element.Input, Type: typeValue, Cardinality: element.One, Required: true, DefaultDepth: 1},
				{Name: "out", Direction: element.Output, Type: typeValue, Cardinality: element.One, Required: true, DefaultDepth: 1},
			},
			Reaction: descriptor.Reaction, StateSchema: descriptor.StateSchema,
			StateTransfer: descriptor.StateTransfer.Clone(),
		}},
		Boundaries: []ir.Boundary{
			{Name: "input", Direction: ir.InputBoundary, Endpoint: ir.Endpoint{Node: "node", Port: "in"}, Type: typeValue},
			{Name: "output", Direction: ir.OutputBoundary, Endpoint: ir.Endpoint{Node: "node", Port: "out"}, Type: typeValue},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return graph
}

func testStateTransfer(snapshot, restore, quiesce bool) *element.StateTransferCapabilities {
	return &element.StateTransferCapabilities{
		Snapshot: snapshot, Restore: restore, Quiesce: quiesce,
	}
}
