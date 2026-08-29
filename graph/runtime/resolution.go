package runtime

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/bojieli/OpenRealtime/element"
	"github.com/bojieli/OpenRealtime/graph/inspect"
)

type nodeResolutionReporter struct {
	mounted *Mounted
	node    string
}

func (reporter nodeResolutionReporter) Runtime(artifactID, revision, digest string) error {
	artifact := inspect.ArtifactIdentity{ID: artifactID, Revision: revision, Digest: digest}
	if err := artifact.Validate(); err != nil {
		return fmt.Errorf("report node %s live runtime: %w", reporter.node, err)
	}
	if reporter.mounted == nil {
		return errors.New("report live runtime: nil mount")
	}
	reporter.mounted.liveMu.Lock()
	defer func() {
		reporter.mounted.liveMu.Unlock()
		reporter.mounted.recorder.signal()
	}()
	live, found := reporter.mounted.nodeLive[reporter.node]
	if !found || live.Resolution == nil {
		return fmt.Errorf("report live runtime: unknown mounted node %q", reporter.node)
	}
	resolution := live.Resolution.Clone()
	if resolution.RuntimeEvidence == inspect.EvidenceLive && resolution.Runtime != artifact {
		return fmt.Errorf("node %s live runtime identity changed from %+v to %+v",
			reporter.node, resolution.Runtime, artifact)
	}
	resolution.Runtime = artifact
	resolution.RuntimeEvidence = inspect.EvidenceLive
	live.Resolution = &resolution
	reporter.mounted.nodeLive[reporter.node] = live
	return nil
}

func (reporter nodeResolutionReporter) Capabilities(source []element.CapabilityResolution) error {
	capabilities := make([]inspect.CapabilityIdentity, 0, len(source))
	for _, capability := range source {
		converted := inspect.CapabilityIdentity{
			Name: capability.Name, Contract: capability.Contract,
			Provider: inspect.ArtifactIdentity{
				ID: capability.ProviderID, Revision: capability.ProviderRevision,
				Digest: capability.ProviderDigest,
			},
		}
		if capability.AdapterID != "" || capability.AdapterRevision != "" ||
			capability.AdapterDigest != "" {
			converted.Adapter = &inspect.ArtifactIdentity{
				ID: capability.AdapterID, Revision: capability.AdapterRevision,
				Digest: capability.AdapterDigest,
			}
		}
		capabilities = append(capabilities, converted)
	}
	canonical, err := inspect.CanonicalCapabilities(capabilities)
	if err != nil {
		return fmt.Errorf("report node %s live capabilities: %w", reporter.node, err)
	}
	if reporter.mounted == nil {
		return errors.New("report live capabilities: nil mount")
	}
	reporter.mounted.liveMu.Lock()
	defer func() {
		reporter.mounted.liveMu.Unlock()
		reporter.mounted.recorder.signal()
	}()
	live, found := reporter.mounted.nodeLive[reporter.node]
	if !found || live.Resolution == nil {
		return fmt.Errorf("report live capabilities: unknown mounted node %q", reporter.node)
	}
	resolution := live.Resolution.Clone()
	if resolution.CapabilitiesEvidence == inspect.EvidenceLive &&
		!reflect.DeepEqual(resolution.Capabilities, canonical) {
		return fmt.Errorf("node %s live capability identities changed after readiness", reporter.node)
	}
	resolution.Capabilities = canonical
	resolution.CapabilitiesEvidence = inspect.EvidenceLive
	live.Resolution = &resolution
	reporter.mounted.nodeLive[reporter.node] = live
	return nil
}

var _ element.ResolutionReporter = nodeResolutionReporter{}
