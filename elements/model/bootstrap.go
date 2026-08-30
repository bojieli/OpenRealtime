package model

import (
	"errors"
	"fmt"
	"strings"

	graphruntime "github.com/bojieli/OpenRealtime/graph/runtime"
	"github.com/bojieli/OpenRealtime/sidecar"
)

// Bootstrap is the deployment-facing owner of external-model dialers and the
// default strict payload codec. It keeps transport credentials/configuration
// in services rather than graph values.
type Bootstrap struct {
	Deployments *DeploymentRegistry
	Codec       *JSONCodec
}

func NewBootstrap() *Bootstrap {
	return &Bootstrap{Deployments: NewDeploymentRegistry(), Codec: NewStandardJSONCodec()}
}

func (bootstrap *Bootstrap) RegisterDialer(reference string, dialer Dialer) error {
	if bootstrap == nil || bootstrap.Deployments == nil {
		return errors.New("register external model dialer: nil bootstrap")
	}
	return bootstrap.Deployments.Register(reference, dialer)
}

// RegisterClient installs the concrete child-process/socket sidecar dialer.
// Remote provider adapters use RegisterDialer with their own Session.
func (bootstrap *Bootstrap) RegisterClient(reference string, config sidecar.Config) error {
	hasCommand := len(config.Command) != 0
	hasAddress := strings.TrimSpace(config.Address) != ""
	if hasCommand == hasAddress {
		return errors.New("register external model client: configure exactly one command or address")
	}
	if hasCommand && strings.TrimSpace(config.Command[0]) == "" {
		return errors.New("register external model client: sidecar command is empty")
	}
	return bootstrap.RegisterDialer(reference, ClientDialer(config))
}

// Install publishes both required services together. Existing registrations
// are refused so bootstrap cannot silently replace live deployment authority.
func (bootstrap *Bootstrap) Install(services *graphruntime.ServiceSet) error {
	if bootstrap == nil || bootstrap.Deployments == nil || bootstrap.Codec == nil {
		return errors.New("install external model services: incomplete bootstrap")
	}
	if services == nil {
		return errors.New("install external model services: nil service set")
	}
	if _, err := services.InstallIfAbsent(map[string]any{
		DeploymentRegistryService: bootstrap.Deployments,
		PayloadCodecService:       bootstrap.Codec,
	}); err != nil {
		return fmt.Errorf("install external model services: %w", err)
	}
	return nil
}
