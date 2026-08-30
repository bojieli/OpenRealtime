package runtime

import (
	"time"

	"github.com/bojieli/OpenRealtime/graph/inspect"
	"github.com/bojieli/OpenRealtime/plugin"
)

const LiveFormatVersion = 1

// EntryLive is payload-free runtime evidence for one plugin row.
type EntryLive struct {
	State          string                   `json:"state"`
	Desired        bool                     `json:"desired"`
	Identity       plugin.Identity          `json:"identity"`
	Implementation string                   `json:"implementation"`
	Runtime        inspect.ArtifactIdentity `json:"runtime"`
	Workers        int                      `json:"workers"`
	Effects        int                      `json:"effects"`
	Services       []plugin.Contract        `json:"services,omitempty"`
	Error          string                   `json:"error,omitempty"`
}

// ExportLive is a payload-free view of one declared application boundary.
type ExportLive struct {
	Provider  string          `json:"provider"`
	Contract  plugin.Contract `json:"contract"`
	Available bool            `json:"available"`
	Revision  uint64          `json:"revision,omitempty"`
}

// Live identifies both the exact client/host/server plugin plan and its live
// implementation selections.
type Live struct {
	FormatVersion uint64                `json:"format_version"`
	Fingerprint   string                `json:"fingerprint"`
	Realm         plugin.Realm          `json:"realm"`
	Sequence      uint64                `json:"sequence"`
	ObservedAt    time.Time             `json:"observed_at"`
	State         string                `json:"state"`
	Entries       map[string]EntryLive  `json:"entries"`
	Exports       map[string]ExportLive `json:"exports,omitempty"`
}

// Live returns one recursively independent best-effort lifecycle snapshot.
func (mounted *Mounted) Live() Live {
	mounted.mu.Lock()
	defer mounted.mu.Unlock()
	state := "active"
	if mounted.closed {
		state = "closed"
	}
	result := Live{
		FormatVersion: LiveFormatVersion, Fingerprint: mounted.plan.Fingerprint,
		Realm: mounted.plan.Realm, Sequence: mounted.sequence.Load(),
		ObservedAt: time.Now().UTC(), State: state,
		Entries: make(map[string]EntryLive, len(mounted.entries)),
		Exports: make(map[string]ExportLive, len(mounted.plan.Exports)),
	}
	for _, entry := range mounted.entries {
		workers, effects := entry.scope.counts()
		services := make([]plugin.Contract, 0, len(entry.plan.Descriptor.Provides))
		for _, contract := range entry.plan.Descriptor.Provides {
			if _, found := mounted.store.lookup(entry.plan.Entry.ID, contract); found {
				services = append(services, contract)
			}
		}
		result.Entries[entry.plan.Entry.ID] = EntryLive{
			State: entry.state, Desired: entry.desired, Identity: entry.plan.Identity,
			Implementation: entry.implementation, Runtime: entry.artifact,
			Workers: workers, Effects: effects, Services: services, Error: entry.err,
		}
	}
	for _, boundary := range mounted.plan.Exports {
		record, available := mounted.store.lookup(boundary.Provider, boundary.Service)
		row := ExportLive{
			Provider: boundary.Provider, Contract: boundary.Service, Available: available,
		}
		if available {
			row.Revision = record.revision
		}
		result.Exports[boundary.Name] = row
	}
	return result
}
