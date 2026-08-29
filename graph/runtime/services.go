package runtime

import (
	"fmt"
	"strings"
	"sync"
)

type serviceEntry struct {
	value    any
	revision uint64
}

// ServiceSet is a concurrency-safe dependency context. Revision increases on
// replacement so future reconciliation can detect coeffect changes.
type ServiceSet struct {
	mu       sync.RWMutex
	services map[string]serviceEntry
}

func NewServiceSet() *ServiceSet { return &ServiceSet{services: make(map[string]serviceEntry)} }

func (services *ServiceSet) Set(name string, value any) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" || value == nil {
		return 0, fmt.Errorf("service registration requires a name and non-nil value")
	}
	services.mu.Lock()
	defer services.mu.Unlock()
	if services.services == nil {
		services.services = make(map[string]serviceEntry)
	}
	entry := services.services[name]
	entry.revision++
	entry.value = value
	services.services[name] = entry
	return entry.revision, nil
}

func (services *ServiceSet) Remove(name string) (uint64, bool) {
	services.mu.Lock()
	defer services.mu.Unlock()
	entry, found := services.services[name]
	if !found {
		return 0, false
	}
	delete(services.services, name)
	return entry.revision + 1, true
}

func (services *ServiceSet) Lookup(name string) (any, uint64, bool) {
	if services == nil {
		return nil, 0, false
	}
	services.mu.RLock()
	defer services.mu.RUnlock()
	entry, found := services.services[name]
	return entry.value, entry.revision, found
}
