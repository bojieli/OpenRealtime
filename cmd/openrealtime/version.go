package main

import (
	"runtime/debug"
	"strings"
)

// release is the declared version, and the VERSION file at the repository root
// is the same fact written where a release process can read it without
// building anything. TestReleaseMatchesTheVERSIONFile keeps the two from
// drifting: a constant nothing checks is a constant that will eventually name
// a tree it does not describe.
//
// Builds from a tagged source tree report it alongside the revision the binary
// was actually built from, because a version string that cannot be traced to a
// revision is a version string that will eventually be wrong.
const release = "0.1.0"

func version() string {
	parts := []string{"openrealtime " + release}
	if info, ok := debug.ReadBuildInfo(); ok {
		revision, modified := "", ""
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				if setting.Value == "true" {
					modified = " (modified)"
				}
			}
		}
		if revision != "" {
			parts = append(parts, "revision "+revision+modified)
		}
		parts = append(parts, "go "+info.GoVersion)
	}
	return strings.Join(parts, ", ")
}
