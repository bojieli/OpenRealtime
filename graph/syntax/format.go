package syntax

import (
	"strconv"
	"strings"
)

// Format returns the canonical textual representation of a parsed graph. It
// preserves declaration order and leading comments while normalizing spacing.
func Format(file File) string {
	var output strings.Builder
	for _, imported := range file.Imports {
		writeComments(&output, "", imported.Comments)
		output.WriteString("import ")
		output.WriteString(strconv.Quote(imported.Path))
		if imported.Alias != "" {
			output.WriteString(" as ")
			output.WriteString(imported.Alias)
		}
		output.WriteString(";\n")
	}
	if len(file.Imports) > 0 {
		output.WriteByte('\n')
	}
	writeComments(&output, "", file.Graph.Comments)
	output.WriteString("graph ")
	output.WriteString(file.Graph.Name)
	output.WriteString(" {\n")
	for _, statement := range file.Graph.Statements {
		switch {
		case statement.Node != nil:
			node := statement.Node
			writeComments(&output, "    ", node.Comments)
			output.WriteString("    ")
			output.WriteString(node.Element)
			output.WriteString(" :: ")
			output.WriteString(node.Name)
			output.WriteString(";\n")
		case statement.Edge != nil:
			edge := statement.Edge
			writeComments(&output, "    ", edge.Comments)
			output.WriteString("    ")
			if edge.Name != "" {
				output.WriteString("edge ")
				output.WriteString(edge.Name)
				output.WriteString(" = ")
			}
			output.WriteString(edge.From.String())
			if edge.Delivery == Lossy {
				output.WriteString(" => ")
			} else {
				output.WriteString(" -> ")
			}
			output.WriteString(edge.To.String())
			output.WriteString(";\n")
		case statement.Boundary != nil:
			boundary := statement.Boundary
			writeComments(&output, "    ", boundary.Comments)
			output.WriteString("    ")
			output.WriteString(string(boundary.Direction))
			output.WriteByte(' ')
			output.WriteString(boundary.Name)
			output.WriteString(" = ")
			output.WriteString(boundary.Endpoint.String())
			output.WriteString(";\n")
		}
	}
	writeComments(&output, "    ", file.Graph.TrailingComments)
	output.WriteString("}\n")
	return output.String()
}

func writeComments(output *strings.Builder, indentation string, comments []string) {
	for _, comment := range comments {
		output.WriteString(indentation)
		output.WriteString("//")
		if comment != "" {
			output.WriteByte(' ')
			output.WriteString(comment)
		}
		output.WriteByte('\n')
	}
}
