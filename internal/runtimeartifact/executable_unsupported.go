//go:build !linux && !darwin

package runtimeartifact

import (
	"errors"

	"github.com/bojieli/OpenRealtime/graph/inspect"
)

// Executable refuses to claim that a mutable launch pathname identifies the
// process image. Unsupported production builds must supply a build-time signed
// artifact manifest to their launcher instead of using this helper.
func Executable(string) (inspect.ArtifactIdentity, error) {
	return inspect.ArtifactIdentity{}, errors.New(
		"mapped executable identity is unsupported on this platform; use a verified signed build manifest",
	)
}
