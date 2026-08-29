package flow

import (
	"context"
	"errors"

	"github.com/bojieli/OpenRealtime/element"
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
	return relay(input, output, false), nil
}

type eventMuxFactory struct{}

func (eventMuxFactory) Descriptor() element.Descriptor { return EventMuxDescriptor() }
func (eventMuxFactory) Mount(_ context.Context, mount element.MountContext) (element.Runnable, error) {
	input, err := mount.Ports.Input("in")
	if err != nil {
		return nil, err
	}
	output, err := mount.Ports.Output("out")
	if err != nil {
		return nil, err
	}
	return element.RunnableFunc(func(ctx context.Context) error {
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
	return relay(input, output, true), nil
}

func relay(input element.InputPort, output element.OutputPort, replaceType bool) element.Runnable {
	return element.RunnableFunc(func(ctx context.Context) error {
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

func terminal(ctx context.Context, err error) bool {
	return err != nil && (ctx.Err() != nil || errors.Is(err, graphruntime.ErrChannelClosed))
}
