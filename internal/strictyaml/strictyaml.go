// Package strictyaml rejects YAML features that make signed configuration
// ambiguous while retaining ordinary scalar types needed by schemas.
package strictyaml

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

type Error struct {
	Path    string
	Line    int
	Column  int
	Message string
}

func (failure *Error) Error() string {
	location := fmt.Sprintf("%d:%d", failure.Line, failure.Column)
	if failure.Path != "" {
		location = failure.Path + ":" + location
	}
	return location + ": " + failure.Message
}

// Decode reads exactly one document, rejects aliases, anchors, merge keys,
// custom tags, duplicate mapping keys, and unknown struct fields, and returns
// its syntax tree for callers that need more precise source mapping.
func Decode(path string, source []byte, target any) (*yaml.Node, error) {
	var tree yaml.Node
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	if err := decoder.Decode(&tree); err != nil {
		return nil, at(path, nil, "decode YAML: "+err.Error())
	}
	if len(tree.Content) == 0 {
		return nil, at(path, nil, "empty YAML document")
	}
	var trailing yaml.Node
	if err := decoder.Decode(&trailing); err == nil {
		return nil, at(path, &trailing, "multiple YAML documents are not allowed")
	} else if !errors.Is(err, io.EOF) {
		return nil, at(path, nil, "decode trailing YAML: "+err.Error())
	}
	if err := validate(path, tree.Content[0]); err != nil {
		return nil, err
	}
	strict := yaml.NewDecoder(bytes.NewReader(source))
	strict.KnownFields(true)
	if err := strict.Decode(target); err != nil {
		return nil, at(path, nil, "decode YAML: "+err.Error())
	}
	return tree.Content[0], nil
}

func validate(path string, node *yaml.Node) error {
	if node.Anchor != "" || node.Kind == yaml.AliasNode {
		return at(path, node, "YAML anchors and aliases are not allowed")
	}
	if (len(node.Tag) > 0 && node.Tag[0] == '!' && len(node.Tag) < 2) ||
		(len(node.Tag) >= 2 && node.Tag[:2] != "!!") {
		return at(path, node, fmt.Sprintf("custom YAML tag %q is not allowed", node.Tag))
	}
	switch node.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, child := range node.Content {
			if err := validate(path, child); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		seen := make(map[string]struct{}, len(node.Content)/2)
		for index := 0; index < len(node.Content); index += 2 {
			key := node.Content[index]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return at(path, key, "YAML mapping keys must be strings")
			}
			if key.Value == "<<" {
				return at(path, key, "YAML merge keys are not allowed")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return at(path, key, fmt.Sprintf("duplicate YAML key %q", key.Value))
			}
			seen[key.Value] = struct{}{}
			if err := validate(path, node.Content[index+1]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
	default:
		return at(path, node, fmt.Sprintf("unsupported YAML node kind %d", node.Kind))
	}
	return nil
}

func at(path string, node *yaml.Node, message string) error {
	line, column := 1, 1
	if node != nil {
		if node.Line > 0 {
			line = node.Line
		}
		if node.Column > 0 {
			column = node.Column
		}
	}
	return &Error{Path: path, Line: line, Column: column, Message: message}
}
