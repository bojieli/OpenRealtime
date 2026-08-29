package video

import (
	"context"
	"fmt"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
)

type frameIngressFactory struct{}

func (frameIngressFactory) Descriptor() element.Descriptor { return FrameIngressDescriptor() }

func (frameIngressFactory) Mount(
	_ context.Context, mount element.MountContext,
) (element.Runnable, error) {
	frameInput, err := mount.Ports.Input("frame_in")
	if err != nil {
		return nil, err
	}
	referenceInput, err := mount.Ports.Input("reference_in")
	if err != nil {
		return nil, err
	}
	frames, err := mount.Ports.Output("frames")
	if err != nil {
		return nil, err
	}
	references, err := mount.Ports.Output("references")
	if err != nil {
		return nil, err
	}
	return &frameIngressRunner{
		resolution: mount.Resolution, frameInput: frameInput,
		referenceInput: referenceInput, frames: frames, references: references,
	}, nil
}

type frameIngressRunner struct {
	resolution                 element.ResolutionReporter
	frameInput, referenceInput element.InputPort
	frames, references         element.OutputPort
}

func (runner *frameIngressRunner) Run(parent context.Context) error {
	if err := reportLiveResolution(runner.resolution, frameIngressRuntimeID); err != nil {
		return err
	}
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	failures := make(chan error, 2)
	var wait sync.WaitGroup
	for _, relay := range []struct {
		name   string
		input  element.InputPort
		output element.OutputPort
	}{
		{name: "frame", input: runner.frameInput, output: runner.frames},
		{name: "reference", input: runner.referenceInput, output: runner.references},
	} {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				envelope, err := relay.input.Receive(ctx)
				if terminal(ctx, err) {
					return
				}
				if err != nil {
					select {
					case failures <- fmt.Errorf("receive video %s ingress: %w", relay.name, err):
					case <-ctx.Done():
					}
					return
				}
				if _, err := relay.output.Broadcast(ctx, envelope); err != nil {
					if terminal(ctx, err) {
						return
					}
					select {
					case failures <- fmt.Errorf("relay video %s ingress: %w", relay.name, err):
					case <-ctx.Done():
					}
					return
				}
			}
		}()
	}
	defer func() {
		cancel(nil)
		wait.Wait()
	}()
	select {
	case <-ctx.Done():
		return nil
	case err := <-failures:
		return err
	}
}
