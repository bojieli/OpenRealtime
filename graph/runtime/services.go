package runtime

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/bojieli/OpenRealtime/element"
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

// InstallIfAbsent atomically publishes a non-empty batch of services if none
// of their canonical names is already registered. Validation and collision
// checks complete before any service is visible. The returned revisions are
// keyed by canonical service name.
//
// The create-only guarantee applies at this method's linearization point. A
// later Set call may deliberately replace an installed service as part of
// reconciliation.
func (services *ServiceSet) InstallIfAbsent(entries map[string]any) (map[string]uint64, error) {
	if services == nil {
		return nil, fmt.Errorf("service batch installation requires a non-nil service set")
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("service batch installation requires at least one service")
	}

	rawNames := make([]string, 0, len(entries))
	for rawName := range entries {
		rawNames = append(rawNames, rawName)
	}
	sort.Strings(rawNames)

	normalized := make(map[string]any, len(entries))
	names := make([]string, 0, len(entries))
	for _, rawName := range rawNames {
		name := strings.TrimSpace(rawName)
		value := entries[rawName]
		if name == "" || nilServiceValue(value) {
			return nil, fmt.Errorf("service batch installation requires canonical names and non-nil values")
		}
		if _, duplicate := normalized[name]; duplicate {
			return nil, fmt.Errorf("service batch installation contains duplicate canonical name %q", name)
		}
		normalized[name] = value
		names = append(names, name)
	}
	sort.Strings(names)

	services.mu.Lock()
	defer services.mu.Unlock()
	for _, name := range names {
		if _, exists := services.services[name]; exists {
			return nil, fmt.Errorf("service %q is already registered", name)
		}
	}
	if services.services == nil {
		services.services = make(map[string]serviceEntry)
	}
	revisions := make(map[string]uint64, len(names))
	for _, name := range names {
		const revision = uint64(1)
		services.services[name] = serviceEntry{value: normalized[name], revision: revision}
		revisions[name] = revision
	}
	return revisions, nil
}

func (services *ServiceSet) Set(name string, value any) (uint64, error) {
	name = strings.TrimSpace(name)
	if name == "" || nilServiceValue(value) {
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

// declaredServices is the element-facing dependency view for one immutable
// descriptor. Mount validation may inspect the complete service set, but an
// element must not acquire an ambient coeffect that it did not declare.
// Revisions and provider loss remain live because successful lookups delegate
// to the underlying mount service set instead of copying values at mount time.
type declaredServices struct {
	base    element.Services
	allowed map[string]struct{}
}

func bindDeclaredServices(
	base element.Services, dependencies []element.Dependency,
) element.Services {
	allowed := make(map[string]struct{}, len(dependencies))
	for _, dependency := range dependencies {
		allowed[dependency.Name] = struct{}{}
	}
	return declaredServices{base: base, allowed: allowed}
}

func (services declaredServices) Lookup(name string) (any, uint64, bool) {
	if _, allowed := services.allowed[name]; !allowed || services.base == nil {
		return nil, 0, false
	}
	return services.base.Lookup(name)
}
