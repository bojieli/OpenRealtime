package graphnative

import (
	"context"
	"errors"
	"fmt"
	"slices"

	legacy "github.com/bojieli/OpenRealtime/binding"
	graphbinding "github.com/bojieli/OpenRealtime/graph/binding"
	graphconfig "github.com/bojieli/OpenRealtime/graph/config"
	graphlaunch "github.com/bojieli/OpenRealtime/graph/launch"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

// Config contains an exact generic graph launch plus the scenario slab that
// launch must support. Cases empty means the complete reviewed suite.
//
// There is no binding name, legacy implementation selector, or fallback in
// this surface. Artifacts, assembly contributions, adapter identity, and
// mount dependencies are checked by graph/launch after the scenario adapter
// guard has been installed.
type Config struct {
	Launch graphlaunch.Config
	Cases  []string
}

// Result is the prepared graph-native provider plus the immutable scenario
// contract it was checked against. Binding implements server.SessionProvider
// and can be passed directly to the composable server bundle.
type Result struct {
	Contract Contract
	Plan     *graphconfig.Plan
	Binding  *graphbinding.NativeBinding
}

// New prepares an exact graph-native session provider for a scenario slab.
// It performs no mount, adapter-factory call, secret resolution, network
// operation, or provider acquisition.
func New(ctx context.Context, config Config) (Result, error) {
	contract, err := BuildContract(config.Cases...)
	if err != nil {
		return Result{}, fmt.Errorf("prepare scenario graph launch contract: %w", err)
	}
	launchConfig, err := GuardLaunch(contract, config.Launch)
	if err != nil {
		return Result{}, fmt.Errorf("prepare scenario graph launch: %w", err)
	}
	launched, err := graphlaunch.New(ctx, launchConfig)
	if err != nil {
		return Result{}, fmt.Errorf("prepare scenario graph launch: %w", err)
	}
	return Result{
		Contract: contract.Clone(), Plan: launched.Plan, Binding: launched.Binding,
	}, nil
}

// GuardLaunch returns a launch config whose exact selected adapter is guarded
// by contract. This is the direct seam for server.NewGraphBundle: a host can
// keep graph preparation and server compilation in that generic bundle while
// adding the scenario contract as an ordinary adapter-plugin decorator.
//
// GuardLaunch does not invoke any binder or acquire any resource. It owns the
// returned adapter slice so installing the guard never mutates the caller's
// catalog.
func GuardLaunch(
	contract Contract,
	config graphlaunch.Config,
) (graphlaunch.Config, error) {
	if err := contract.Validate(); err != nil {
		return graphlaunch.Config{}, err
	}
	result := config
	result.Catalog.Adapters = slices.Clone(config.Catalog.Adapters)
	selected := -1
	for index, plugin := range result.Catalog.Adapters {
		if plugin.Reference != result.Adapter.Reference {
			continue
		}
		if selected >= 0 {
			return graphlaunch.Config{}, fmt.Errorf(
				"scenario graph launch adapter catalog is ambiguous for %q",
				result.Adapter.Reference,
			)
		}
		selected = index
	}
	if selected < 0 {
		return graphlaunch.Config{}, fmt.Errorf(
			"scenario graph launch selected adapter %q is absent from the plugin catalog",
			result.Adapter.Reference,
		)
	}
	guarded, err := GuardAdapter(contract, result.Catalog.Adapters[selected])
	if err != nil {
		return graphlaunch.Config{}, fmt.Errorf("scenario graph launch adapter: %w", err)
	}
	result.Catalog.Adapters[selected] = guarded
	return result, nil
}

// GuardAdapter decorates one resource-free adapter plugin with a scenario
// contract check. The underlying binder still derives its profile from the
// immutable selected plan; the guard then verifies exact graph boundaries and
// capability projection before graph/launch can expose the provider.
//
// This is the plugin-composition seam for a host that assembles the catalog
// itself. New uses the same function and adds exact selected-plugin checks.
func GuardAdapter(
	contract Contract,
	plugin graphlaunch.AdapterPlugin,
) (graphlaunch.AdapterPlugin, error) {
	if err := contract.Validate(); err != nil {
		return graphlaunch.AdapterPlugin{}, err
	}
	if plugin.Bind == nil {
		return graphlaunch.AdapterPlugin{}, errors.New("scenario graph adapter plugin requires a binder")
	}
	if err := (graphbinding.AdapterRegistration{
		Reference: plugin.Reference,
		Artifact:  plugin.Artifact,
		Factory:   rejectedAdapterFactory,
	}).Validate(); err != nil {
		return graphlaunch.AdapterPlugin{}, fmt.Errorf("scenario graph adapter plugin: %w", err)
	}

	result := plugin
	binder := plugin.Bind
	frozen := contract.Clone()
	result.Bind = func(
		ctx context.Context,
		plan *graphconfig.Plan,
	) (graphlaunch.BoundAdapter, error) {
		if plan == nil {
			return graphlaunch.BoundAdapter{}, errors.New("scenario graph adapter binder received a nil plan")
		}
		bound, err := binder(ctx, plan)
		if err != nil {
			return graphlaunch.BoundAdapter{}, err
		}
		if err := frozen.ValidateProfile(bound.Profile, plan.Graph()); err != nil {
			return graphlaunch.BoundAdapter{}, err
		}
		return bound, nil
	}
	return result, nil
}

func rejectedAdapterFactory(
	context.Context,
	*graphruntime.Mounted,
	legacy.Options,
	graphbinding.SessionAdapterProfile,
) (graphbinding.SessionAdapter, error) {
	return nil, errors.New("scenario adapter validation sentinel must not be called")
}
