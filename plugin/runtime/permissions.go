package runtime

import (
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/plugin"
)

type permissionSet struct {
	grants []plugin.Permission
}

func bindPermissions(descriptor plugin.Descriptor, grants []plugin.Permission) (permissionSet, error) {
	result := permissionSet{grants: make([]plugin.Permission, 0, len(grants))}
	seen := make(map[string]struct{}, len(grants))
	for _, grant := range grants {
		key := grant.Kind + "\x00" + grant.Resource
		if _, duplicate := seen[key]; duplicate {
			return permissionSet{}, fmt.Errorf("duplicate grant %s/%s", grant.Kind, grant.Resource)
		}
		seen[key] = struct{}{}
		var ceiling *plugin.Permission
		for index := range descriptor.Permissions {
			candidate := &descriptor.Permissions[index]
			if candidate.Kind == grant.Kind && candidate.Resource == grant.Resource {
				ceiling = candidate
				break
			}
		}
		if ceiling == nil {
			return permissionSet{}, fmt.Errorf("grant %s/%s exceeds the descriptor ceiling",
				grant.Kind, grant.Resource)
		}
		if grant.Authority != ceiling.Authority {
			return permissionSet{}, fmt.Errorf("grant %s/%s changes required authority from %q to %q",
				grant.Kind, grant.Resource, ceiling.Authority, grant.Authority)
		}
		if len(grant.Operations) == 0 {
			return permissionSet{}, fmt.Errorf("grant %s/%s declares no operations", grant.Kind, grant.Resource)
		}
		operations := slices.Clone(grant.Operations)
		slices.Sort(operations)
		for index, operation := range operations {
			if operation == "" || (index > 0 && operations[index-1] == operation) {
				return permissionSet{}, fmt.Errorf("grant %s/%s has invalid or duplicate operation %q",
					grant.Kind, grant.Resource, operation)
			}
			if !slices.Contains(ceiling.Operations, operation) {
				return permissionSet{}, fmt.Errorf("grant %s/%s operation %s exceeds the descriptor ceiling",
					grant.Kind, grant.Resource, operation)
			}
		}
		copy := grant
		copy.Operations = operations
		result.grants = append(result.grants, copy)
	}
	slices.SortFunc(result.grants, func(left, right plugin.Permission) int {
		if left.Kind != right.Kind {
			if left.Kind < right.Kind {
				return -1
			}
			return 1
		}
		if left.Resource < right.Resource {
			return -1
		}
		if left.Resource > right.Resource {
			return 1
		}
		return 0
	})
	return result, nil
}

func (permissions permissionSet) Allows(kind, resource, operation string) bool {
	for _, grant := range permissions.grants {
		if grant.Kind == kind && grant.Resource == resource && slices.Contains(grant.Operations, operation) {
			return true
		}
	}
	return false
}

func (permissions permissionSet) Snapshot() []plugin.Permission {
	result := make([]plugin.Permission, len(permissions.grants))
	for index := range permissions.grants {
		result[index] = permissions.grants[index]
		result[index].Operations = slices.Clone(permissions.grants[index].Operations)
	}
	return result
}

func (permissions permissionSet) Clone() permissionSet {
	return permissionSet{grants: permissions.Snapshot()}
}
