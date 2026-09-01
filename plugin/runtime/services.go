package runtime

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"

	"github.com/bojieli/OpenRealtime/plugin"
)

var (
	// ErrUnknownExport means a launcher requested a boundary not declared by
	// the frozen profile.
	ErrUnknownExport = errors.New("unknown plugin realm export")
	// ErrExportUnavailable means the boundary exists but its provider is not
	// currently active.
	ErrExportUnavailable = errors.New("plugin realm export unavailable")
)

type serviceRecord struct {
	contract plugin.Contract
	provider string
	value    any
	revision uint64
}

type serviceStore struct {
	mu       sync.RWMutex
	records  map[string]serviceRecord
	sequence uint64
}

func newServiceStore() *serviceStore {
	return &serviceStore{records: make(map[string]serviceRecord)}
}

func serviceKey(provider, name string) string { return provider + "\x00" + name }

func (store *serviceStore) publish(provider string, contract plugin.Contract, value any) error {
	if nilServiceValue(value) {
		return fmt.Errorf("plugin %s cannot publish nil service %s", provider, contract.Name)
	}
	key := serviceKey(provider, contract.Name)
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.records[key]; exists {
		return fmt.Errorf("plugin %s already published service %s", provider, contract.Name)
	}
	store.sequence++
	store.records[key] = serviceRecord{
		contract: contract, provider: provider, value: value, revision: store.sequence,
	}
	return nil
}

func (store *serviceStore) remove(provider, name string) {
	store.mu.Lock()
	delete(store.records, serviceKey(provider, name))
	store.mu.Unlock()
}

func (store *serviceStore) lookup(provider string, contract plugin.Contract) (serviceRecord, bool) {
	store.mu.RLock()
	record, found := store.records[serviceKey(provider, contract.Name)]
	store.mu.RUnlock()
	if !found || record.contract != contract {
		return serviceRecord{}, false
	}
	return record, true
}

func (store *serviceStore) countProvider(provider string) int {
	store.mu.RLock()
	defer store.mu.RUnlock()
	count := 0
	for _, record := range store.records {
		if record.provider == provider {
			count++
		}
	}
	return count
}

// Export returns one service deliberately exposed by the compiled profile.
// The returned revision changes whenever the provider republishes the service,
// allowing application launchers to detect a replacement without depending on
// implementation details.
func (mounted *Mounted) Export(name string) (
	value any, contract plugin.Contract, provider string, revision uint64, err error,
) {
	var boundary *plugin.PlannedExport
	for index := range mounted.plan.Exports {
		if mounted.plan.Exports[index].Name == name {
			boundary = &mounted.plan.Exports[index]
			break
		}
	}
	if boundary == nil {
		return nil, plugin.Contract{}, "", 0,
			fmt.Errorf("%w: %q", ErrUnknownExport, name)
	}
	record, found := mounted.store.lookup(boundary.Provider, boundary.Service)
	if !found {
		return nil, boundary.Service, boundary.Provider, 0,
			fmt.Errorf("%w: %q provider %q is not active", ErrExportUnavailable, name, boundary.Provider)
	}
	return record.value, record.contract, record.provider, record.revision, nil
}

func nilServiceValue(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type boundServices struct {
	store    *serviceStore
	bindings map[string]plugin.DependencyBinding
}

func (services boundServices) Lookup(name string) (
	any, plugin.Contract, string, uint64, bool,
) {
	binding, declared := services.bindings[name]
	if !declared {
		return nil, plugin.Contract{}, "", 0, false
	}
	record, found := services.store.lookup(binding.Provider, binding.Service)
	if !found {
		return nil, binding.Service, binding.Provider, 0, false
	}
	return record.value, record.contract, record.provider, record.revision, true
}

type servicePublisher struct {
	entry      string
	descriptor plugin.Descriptor
	store      *serviceStore
	scope      *lifecycleScope
}

func (publisher servicePublisher) Provide(contract plugin.Contract, value any) error {
	declared := false
	for _, service := range publisher.descriptor.Provides {
		if service == contract {
			declared = true
			break
		}
	}
	if !declared {
		return fmt.Errorf("plugin %s attempted to publish undeclared service %s",
			publisher.entry, contract.Name)
	}
	if err := publisher.store.publish(publisher.entry, contract, value); err != nil {
		return err
	}
	if err := publisher.scope.Defer("service:"+contract.Name, func(context.Context) error {
		publisher.store.remove(publisher.entry, contract.Name)
		return nil
	}); err != nil {
		publisher.store.remove(publisher.entry, contract.Name)
		return err
	}
	return nil
}
