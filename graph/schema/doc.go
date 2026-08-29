// Package schema derives a strict JSON Schema for the values artifact of one
// frozen graph.
//
// Generation is pure with respect to the filesystem and network. Element
// descriptor ConfigSchema strings are opaque contract references: the package
// never guesses configuration fields from them. Callers that own the actual
// schema documents may provide a Resolver. Otherwise the generated schema is
// an honest, incomplete envelope and node-index schema.
package schema
