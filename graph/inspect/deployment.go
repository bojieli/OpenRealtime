package inspect

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const maximumDeploymentSecretResolutions = 65_536

// DeploymentEvidence carries deployment identity without carrying deployment
// values. Public is the redacted identity derived from Graph IR. The opaque
// private fingerprints cover the reviewed deployment and secret catalog, but
// no secret reference, provider locator, or credential byte is retained.
type DeploymentEvidence struct {
	Public                       ArtifactIdentity         `json:"public"`
	PrivateDeploymentFingerprint string                   `json:"opaque_private_deployment_fingerprint,omitempty"`
	SecretCatalogFingerprint     string                   `json:"opaque_secret_catalog_fingerprint,omitempty"`
	Secrets                      []SecretProviderEvidence `json:"secret_providers,omitempty"`
}

// SecretProviderEvidence proves the provider artifact selected for one
// node-local secret slot. BindingFingerprint is the catalog's opaque digest of
// provider plus locator; the reference and locator themselves never cross the
// inspection boundary.
type SecretProviderEvidence struct {
	Node               string           `json:"node"`
	Slot               string           `json:"slot"`
	BindingFingerprint string           `json:"binding_fingerprint"`
	Provider           string           `json:"provider"`
	Runtime            ArtifactIdentity `json:"runtime"`
}

func (evidence DeploymentEvidence) Clone() DeploymentEvidence {
	result := evidence
	result.Secrets = slices.Clone(evidence.Secrets)
	return result
}

// CanonicalDeploymentEvidence validates, clones, and deterministically orders
// a deployment evidence value.
func CanonicalDeploymentEvidence(source DeploymentEvidence) (DeploymentEvidence, error) {
	result := source.Clone()
	if err := result.Public.Validate(); err != nil {
		return DeploymentEvidence{}, fmt.Errorf("deployment public identity: %w", err)
	}
	if result.Public.Digest == "" {
		return DeploymentEvidence{}, errors.New("deployment public identity requires a digest")
	}
	if err := canonicalDeploymentLabel("deployment public ID", result.Public.ID); err != nil {
		return DeploymentEvidence{}, err
	}
	if result.Public.Revision != "" {
		if err := canonicalDeploymentLabel("deployment public revision", result.Public.Revision); err != nil {
			return DeploymentEvidence{}, err
		}
	}
	if err := validateOpaqueFingerprint("public deployment", result.Public.Digest); err != nil {
		return DeploymentEvidence{}, err
	}
	if result.PrivateDeploymentFingerprint != "" {
		if err := validateOpaqueFingerprint("private deployment", result.PrivateDeploymentFingerprint); err != nil {
			return DeploymentEvidence{}, err
		}
	}
	if result.SecretCatalogFingerprint != "" {
		if err := validateOpaqueFingerprint("secret catalog", result.SecretCatalogFingerprint); err != nil {
			return DeploymentEvidence{}, err
		}
	}
	if len(result.Secrets) > maximumDeploymentSecretResolutions {
		return DeploymentEvidence{}, fmt.Errorf(
			"deployment evidence has %d secret resolutions; maximum is %d",
			len(result.Secrets), maximumDeploymentSecretResolutions,
		)
	}
	sort.Slice(result.Secrets, func(left, right int) bool {
		if result.Secrets[left].Node != result.Secrets[right].Node {
			return result.Secrets[left].Node < result.Secrets[right].Node
		}
		return result.Secrets[left].Slot < result.Secrets[right].Slot
	})
	for index, secret := range result.Secrets {
		if err := canonicalDeploymentLabel("secret node", secret.Node); err != nil {
			return DeploymentEvidence{}, fmt.Errorf("deployment secret %d: %w", index, err)
		}
		if err := canonicalDeploymentLabel("secret slot", secret.Slot); err != nil {
			return DeploymentEvidence{}, fmt.Errorf("deployment secret %d: %w", index, err)
		}
		if err := validateOpaqueFingerprint("secret binding", secret.BindingFingerprint); err != nil {
			return DeploymentEvidence{}, fmt.Errorf("deployment secret %s.%s: %w", secret.Node, secret.Slot, err)
		}
		if err := canonicalDeploymentLabel("secret provider", secret.Provider); err != nil {
			return DeploymentEvidence{}, fmt.Errorf("deployment secret %s.%s: %w", secret.Node, secret.Slot, err)
		}
		if err := secret.Runtime.Validate(); err != nil {
			return DeploymentEvidence{}, fmt.Errorf(
				"deployment secret %s.%s provider runtime: %w",
				secret.Node,
				secret.Slot,
				err,
			)
		}
		if err := canonicalDeploymentLabel("secret provider runtime ID", secret.Runtime.ID); err != nil {
			return DeploymentEvidence{}, fmt.Errorf("deployment secret %s.%s: %w", secret.Node, secret.Slot, err)
		}
		if secret.Runtime.Revision != "" {
			if err := canonicalDeploymentLabel("secret provider runtime revision", secret.Runtime.Revision); err != nil {
				return DeploymentEvidence{}, fmt.Errorf("deployment secret %s.%s: %w", secret.Node, secret.Slot, err)
			}
		}
		if secret.Runtime.Digest != "" {
			if err := validateOpaqueFingerprint("secret provider runtime", secret.Runtime.Digest); err != nil {
				return DeploymentEvidence{}, fmt.Errorf("deployment secret %s.%s: %w", secret.Node, secret.Slot, err)
			}
		}
		if index > 0 && result.Secrets[index-1].Node == secret.Node &&
			result.Secrets[index-1].Slot == secret.Slot {
			return DeploymentEvidence{}, fmt.Errorf("deployment evidence repeats secret slot %s.%s", secret.Node, secret.Slot)
		}
	}
	if len(result.Secrets) != 0 && result.SecretCatalogFingerprint == "" {
		return DeploymentEvidence{}, errors.New("secret provider evidence requires an opaque secret-catalog fingerprint")
	}
	return result, nil
}

// Validate checks canonical order as well as structure.
func (evidence DeploymentEvidence) Validate() error {
	canonical, err := CanonicalDeploymentEvidence(evidence)
	if err != nil {
		return err
	}
	if !slices.Equal(canonical.Secrets, evidence.Secrets) {
		return errors.New("deployment secret provider evidence is not canonical")
	}
	return nil
}

// ValidateExact additionally requires the private deployment fingerprint used
// by graph-native plan preparation. A secret-catalog fingerprint is optional
// only when the deployment contains no secret slots.
func (evidence DeploymentEvidence) ValidateExact() error {
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.PrivateDeploymentFingerprint == "" {
		return errors.New("deployment evidence requires an opaque private deployment fingerprint")
	}
	return nil
}

func validateOpaqueFingerprint(label, value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 ||
		value != strings.ToLower(value) {
		return fmt.Errorf("%s fingerprint is not canonical SHA-256", label)
	}
	if !validHexadecimal(value[len(prefix):], true) {
		return fmt.Errorf("%s fingerprint is invalid", label)
	}
	return nil
}

func canonicalDeploymentLabel(label, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 64<<10 ||
		strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("%s %q is not canonical", label, value)
	}
	return nil
}
