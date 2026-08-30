package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

type meetingHostDeploymentDependencies struct {
	lookup   func(string) (string, bool)
	verifier func() (meetingDeploymentVerifier, error)
}

func newProductionServeMeetingRegistration(
	ctx context.Context, executable inspect.ArtifactIdentity,
) (*serveMeetingRegistration, error) {
	return newProductionServeMeetingRegistrationWithDependencies(
		ctx, executable, meetingHostDeploymentDependencies{
			lookup:   os.LookupEnv,
			verifier: newLocalMeetingDeploymentVerifier,
		},
	)
}

func newProductionServeMeetingRegistrationWithDependencies(
	ctx context.Context, executable inspect.ArtifactIdentity,
	dependencies meetingHostDeploymentDependencies,
) (*serveMeetingRegistration, error) {
	if ctx == nil || dependencies.lookup == nil || dependencies.verifier == nil {
		return nil, errors.New("compose Meeting host deployment: incomplete dependencies")
	}
	if cause := context.Cause(ctx); cause != nil {
		return nil, cause
	}
	value, present := dependencies.lookup(meetingLocalDeploymentEnvironment)
	if !present || strings.TrimSpace(value) == "" {
		return nil, nil
	}
	if value != "1" {
		return nil, errors.New("Meeting local deployment opt-in must be exactly 1")
	}
	verifier, err := dependencies.verifier()
	if err != nil {
		return nil, fmt.Errorf("construct Meeting deployment verifier: %w", err)
	}
	if nilMeetingDeploymentInterface(verifier) {
		return nil, errors.New("construct Meeting deployment verifier: nil verifier")
	}
	deployments, err := verifier.Resolve(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve Meeting live deployments: %w", err)
	}
	registration, err := newServeMeetingRegistration(ctx, executable, deployments, verifier)
	if err != nil {
		return nil, fmt.Errorf("register Meeting application: %w", err)
	}
	return &registration, nil
}
