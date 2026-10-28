package clusterdir

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

// Schema returns the JSON schema of cluster.yaml, made from the Go types so it
// never drifts from what Load accepts. Editors use it for completion.
func Schema() ([]byte, error) {
	reflector := jsonschema.Reflector{
		FieldNameTag:   "yaml",
		DoNotReference: true,
	}
	schema := reflector.Reflect(&Config{})
	schema.Title = "clowder cluster.yaml"
	encoded, err := json.MarshalIndent(schema, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode the schema: %w", err)
	}
	return encoded, nil
}
