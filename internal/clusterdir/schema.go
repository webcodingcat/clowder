package clusterdir

import (
	"encoding/json"
	"fmt"

	"github.com/invopop/jsonschema"
)

// Schema returns the JSON schema of cluster.yaml for editors, made from the Go
// types that Load decodes into. Check enforces its required fields and enums
// at every level, plus rules a schema cannot express (the fields of each alert
// type, pool names, sizes); a test compares the two at the top level.
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
