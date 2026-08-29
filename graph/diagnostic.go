// Package graph elaborates frontend-neutral topology into immutable Graph IR.
package graph

import (
	"fmt"
	"strings"

	"github.com/bojieli/OpenRealtime/graph/syntax"
)

// Diagnostic is a stable, source-mapped compiler finding.
type Diagnostic struct {
	Code    string      `json:"code" yaml:"code"`
	Path    string      `json:"path,omitempty" yaml:"path,omitempty"`
	Span    syntax.Span `json:"span" yaml:"span"`
	Message string      `json:"message" yaml:"message"`
	Notes   []string    `json:"notes,omitempty" yaml:"notes,omitempty"`
}

func (diagnostic Diagnostic) Error() string {
	location := fmt.Sprintf("%d:%d", diagnostic.Span.Start.Line, diagnostic.Span.Start.Column)
	if diagnostic.Path != "" {
		location = diagnostic.Path + ":" + location
	}
	if diagnostic.Code != "" {
		return fmt.Sprintf("%s: %s: %s", location, diagnostic.Code, diagnostic.Message)
	}
	return location + ": " + diagnostic.Message
}

// Errors is returned only when elaboration cannot produce a sound graph. It
// can carry several independent diagnostics from one compile.
type Errors struct {
	Diagnostics []Diagnostic
}

func (failures *Errors) Error() string {
	if failures == nil || len(failures.Diagnostics) == 0 {
		return "graph compilation failed"
	}
	var message strings.Builder
	for index, diagnostic := range failures.Diagnostics {
		if index != 0 {
			message.WriteByte('\n')
		}
		message.WriteString(diagnostic.Error())
	}
	return message.String()
}

func (failures *Errors) add(path, code string, span syntax.Span, message string, notes ...string) {
	failures.Diagnostics = append(failures.Diagnostics, Diagnostic{
		Code: code, Path: path, Span: span, Message: message, Notes: notes,
	})
}

func (failures *Errors) any() bool { return len(failures.Diagnostics) != 0 }
