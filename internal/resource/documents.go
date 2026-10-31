package resource

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.yaml.in/yaml/v3"
)

// clowderVersions starts the apiVersion of every document of clowder's, in
// this version and any other.
const clowderVersions = "clowder/"

// ParseDocuments reads the YAML documents of a file (JSON is YAML too) as
// resources. A file holds clowder's resources only, or none of them (a
// Compose file, a Kubernetes manifest next to them): for such a file it
// returns none, so an apply of a directory can skip it. A file that mixes
// both is an error, and so is a resource of another apiVersion of
// clowder's. A resource is read strictly: a field it does not have is an
// error.
func ParseDocuments(data []byte) ([]Object, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var objects []Object
	others := 0
	for index := 1; ; index++ {
		var content any
		err := decoder.Decode(&content)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		fields, isMap := content.(map[string]any)
		version, _ := fields["apiVersion"].(string)
		switch {
		case content == nil:
			continue // an empty document, as after a trailing ---
		case !isMap || !strings.HasPrefix(version, clowderVersions):
			others++
			continue
		case version != APIVersion:
			return nil, fmt.Errorf("document %d: apiVersion %s is not %s, the one this clowder reads",
				index, version, APIVersion)
		}
		object, err := decodeObject(fields)
		if err != nil {
			return nil, fmt.Errorf("document %d: %w", index, err)
		}
		objects = append(objects, object)
	}
	if len(objects) > 0 && others > 0 {
		return nil, errors.New("the file mixes clowder's resources with other documents; keep them apart")
	}
	return objects, nil
}

// decodeObject turns a document read from YAML into a resource, strictly.
func decodeObject(fields map[string]any) (Object, error) {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return Object{}, fmt.Errorf("read the resource: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var object Object
	if err := decoder.Decode(&object); err != nil {
		return Object{}, fmt.Errorf("read the resource: %w", err)
	}
	switch {
	case object.Kind == "":
		return Object{}, errors.New("kind is required")
	case object.Metadata.Name == "":
		return Object{}, fmt.Errorf("%s: metadata.name is required", object.Kind)
	}
	return object, nil
}
