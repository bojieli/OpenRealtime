package v1_test

import (
	"context"

	stable "github.com/bojieli/OpenRealtime/api/v1"
)

type externalPerception struct{}

func (*externalPerception) Descriptor() stable.Descriptor {
	return stable.Descriptor{Name: "external.test", Version: "1", Capabilities: stable.Capabilities{}}
}

func (*externalPerception) PushFrame(context.Context, stable.AudioFrame) ([]stable.PerceptionRevision, error) {
	return nil, nil
}

func (*externalPerception) Finalize(context.Context, uint64) (stable.PerceptionRevision, error) {
	return stable.PerceptionRevision{}, nil
}

var _ stable.PerceptionProvider = (*externalPerception)(nil)
