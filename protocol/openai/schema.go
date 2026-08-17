package openai

import _ "embed"

// SchemaBundle is the pinned Realtime-only JSON Schema closure generated from
// OpenAI's official OpenAPI specification.
//
//go:embed openai-realtime-events.schema.json
var SchemaBundle []byte
