package flow

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/elements/internal/liveidentity"
	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
)

type teeFactory struct{}

func (teeFactory) Descriptor() element.Descriptor { return TeeDescriptor() }
func (teeFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	return relay(input, output, false, mount.Resolution, TeeDescriptor()), nil
}

type muxFactory struct{ descriptor element.Descriptor }

func (factory muxFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }
func (factory muxFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		if err := reportFlowResolution(mount.Resolution, factory.descriptor); err != nil {
			return err
		}
		for {
			message, _, err := input.ReceiveAny(ctx)
			if terminal(ctx, err) {
				return nil
			}
			if err != nil {
				return err
			}
			if _, err := output.Broadcast(ctx, message); terminal(ctx, err) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}), nil
}

type dropFactory struct{ descriptor element.Descriptor }

func (factory dropFactory) Descriptor() element.Descriptor { return factory.descriptor.Clone() }
func (factory dropFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
		if err := reportFlowResolution(mount.Resolution, factory.descriptor); err != nil {
			return err
		}
		for {
			if _, err := input.Receive(ctx); terminal(ctx, err) {
				return nil
			} else if err != nil {
				return err
			}
		}
	}), nil
}

type latestFactory struct{}

func (latestFactory) Descriptor() element.Descriptor { return LatestDescriptor() }
func (latestFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("state")
	if err != nil {
		return nil, err
	}
	return relay(input, output, true, mount.Resolution, LatestDescriptor()), nil
}

func relay(
	input element.InputPort, output element.OutputPort, replaceType bool,
	reporter element.ResolutionReporter, descriptor element.Descriptor,
) element.Runnable {
	return element.RunnableFunc(func(ctx context.Context) error {
		if err := reportFlowResolution(reporter, descriptor); err != nil {
			return err
		}
		for {
			message, err := input.Receive(ctx)
			if terminal(ctx, err) {
				return nil
			}
			if err != nil {
				return err
			}
			if replaceType {
				message.Type = output.Type()
			}
			if _, err := output.Broadcast(ctx, message); terminal(ctx, err) {
				return nil
			} else if err != nil {
				return err
			}
		}
	})
}

func reportFlowResolution(
	reporter element.ResolutionReporter, descriptor element.Descriptor,
) error {
	return liveidentity.Report(reporter, liveidentity.Artifact{
		ID: flowRuntimeID(descriptor), Revision: flowImplementationRevision,
	}, nil)
}

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}
