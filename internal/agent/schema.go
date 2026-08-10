package agent

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

// reflector generates flat, self-contained JSON Schema per tool - no shared
// $defs a model has to resolve, no schema $id a tool catalog has no use for.
// It reads the same jsonschema: struct tags decode.go's validateArgs reads,
// which is the whole point: what the model is told and what the validator
// enforces cannot drift (docs/06-agent-spec.md#validation).
var reflector = &jsonschema.Reflector{
	DoNotReference: true,
	Anonymous:      true,
}

// schemaFor renders v's JSON Schema. v is always the zero value of a
// registered tool's argument struct (registrations[i].args), a fixed,
// compile-time-known set - a marshal failure here is a programmer error
// caught the first time cmd/naviseed runs, not a runtime condition worth
// plumbing an error return through every call site for.
func schemaFor(v any) json.RawMessage {
	s := reflector.Reflect(v)
	data, err := json.Marshal(s)
	if err != nil {
		panic(fmt.Sprintf("agent: schema for %T: %v", v, err))
	}
	return data
}
