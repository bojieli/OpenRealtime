package management

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"

	"github.com/bojieli/OpenRealtime/graph/schema"
	"github.com/bojieli/OpenRealtime/internal/strictjson"
)

const ValuesSchemaResourceFormatVersion uint64 = 1

// ValuesSchemaResource is the lossless HTTP representation of a generated
// values-schema bundle. Schema is a JSON string rather than an embedded JSON
// value because standard JSON encoders are allowed to compact RawMessage
// values. Compaction would make Bundle.Digest unverifiable and would discard
// the generator's canonical trailing newline.
type ValuesSchemaResource struct {
	FormatVersion uint64                      `json:"format_version"`
	Schema        string                      `json:"schema"`
	Digest        string                      `json:"digest"`
	Complete      bool                        `json:"complete"`
	Nodes         []schema.NodeContract       `json:"nodes"`
	Contracts     []schema.ContractResolution `json:"contracts"`
	Unresolved    []string                    `json:"unresolved"`
}

func NewValuesSchemaResource(bundle schema.Bundle) (ValuesSchemaResource, error) {
	resource := ValuesSchemaResource{
		FormatVersion: ValuesSchemaResourceFormatVersion,
		Schema:        string(bundle.Schema), Digest: bundle.Digest, Complete: bundle.Complete,
		Nodes: slices.Clone(bundle.Nodes), Contracts: cloneSchemaContracts(bundle.Contracts),
		Unresolved: slices.Clone(bundle.Unresolved),
	}
	if err := resource.Validate(); err != nil {
		return ValuesSchemaResource{}, err
	}
	return resource, nil
}

func (resource ValuesSchemaResource) Validate() error {
	if resource.FormatVersion != ValuesSchemaResourceFormatVersion ||
		len(resource.Schema) == 0 || len(resource.Schema) > 64<<20 ||
		!CanonicalDigest(resource.Digest) {
		return fmt.Errorf("%w: invalid values-schema resource identity", ErrConflict)
	}
	if err := strictjson.Validate([]byte(resource.Schema)); err != nil {
		return fmt.Errorf("%w: invalid values-schema JSON", ErrConflict)
	}
	digest := sha256.Sum256([]byte(resource.Schema))
	if "sha256:"+hex.EncodeToString(digest[:]) != resource.Digest {
		return fmt.Errorf("%w: values-schema content digest mismatch", ErrConflict)
	}
	if resource.Complete != (len(resource.Unresolved) == 0) {
		return fmt.Errorf("%w: values-schema completeness mismatch", ErrConflict)
	}
	return nil
}

func (resource ValuesSchemaResource) Bundle() (schema.Bundle, error) {
	if err := resource.Validate(); err != nil {
		return schema.Bundle{}, err
	}
	return schema.Bundle{
		Schema: []byte(resource.Schema), Digest: resource.Digest, Complete: resource.Complete,
		Nodes: slices.Clone(resource.Nodes), Contracts: cloneSchemaContracts(resource.Contracts),
		Unresolved: slices.Clone(resource.Unresolved),
	}, nil
}

func cloneSchemaContracts(source []schema.ContractResolution) []schema.ContractResolution {
	result := make([]schema.ContractResolution, len(source))
	for index, contract := range source {
		result[index] = contract
		result[index].NodeIDs = slices.Clone(contract.NodeIDs)
	}
	return result
}
