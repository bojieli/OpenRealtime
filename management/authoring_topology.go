package management

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/editor"
	"github.com/bojieli/OpenRealtime/graph/manifest"
	"github.com/bojieli/OpenRealtime/graph/syntax"
)

func canonicalAuthoringTopology(document AuthoringDocument) (syntax.File, error) {
	file, err := parseAuthoringDocument(document.Path, []byte(document.Source))
	if err != nil {
		return syntax.File{}, err
	}
	canonical, err := formatAuthoringTopology(document.Path, file)
	if err != nil {
		return syntax.File{}, err
	}
	if !bytes.Equal([]byte(document.Source), canonical) {
		return syntax.File{}, fmt.Errorf("topology source is not canonical for %s",
			strings.ToLower(filepath.Ext(document.Path)))
	}
	return file, nil
}

func formatAuthoringTopology(path string, file syntax.File) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ortg":
		return []byte(syntax.Format(file)), nil
	case ".yaml", ".yml":
		return manifest.MarshalYAML(manifest.FromSyntax(file))
	case ".json":
		return manifest.MarshalJSON(manifest.FromSyntax(file))
	default:
		return nil, fmt.Errorf("unsupported topology extension")
	}
}

func normalizedAuthoringDocument(document AuthoringDocument, limits editor.Limits) (*editor.NormalizedDocument, error) {
	return editor.AnalyzeNormalized(document.Path, []byte(document.Source), limits)
}

func authoringTopologyIsORTG(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".ortg")
}
