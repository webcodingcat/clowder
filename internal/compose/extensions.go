package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// extensionKey is the Compose extension that carries clowder's fields.
const extensionKey = "x-clowder"

// The raw x-clowder blocks as written in the file. They are decoded strictly,
// so a misspelled key is an error instead of being ignored.

type rawStackExtension struct {
	Priority any      `json:"priority"`
	Exports  []string `json:"exports"`
	Imports  []string `json:"imports"`
}

type rawServiceExtension struct {
	Priority         any           `json:"priority"`
	HTTP             *rawHTTPRoute `json:"http"`
	RestartUnhealthy *bool         `json:"restart_unhealthy"`
}

type rawHTTPRoute struct {
	Host string `json:"host"`
	Port uint16 `json:"port"`
	Path string `json:"path"`
}

type rawVolumeExtension struct {
	Class    string     `json:"class"`
	Size     string     `json:"size"`
	Replicas *int       `json:"replicas"`
	Backup   *rawBackup `json:"backup"`
}

type rawBackup struct {
	Schedule string         `json:"schedule"`
	Keep     int            `json:"keep"`
	Pre      *rawBackupHook `json:"pre"`
}

type rawBackupHook struct {
	Service string   `json:"service"`
	Command []string `json:"command"`
}

// decodeExtension reads the x-clowder block of extensions into target; a
// missing block leaves target as it is.
func decodeExtension(extensions map[string]any, target any) error {
	raw, ok := extensions[extensionKey]
	if !ok || raw == nil {
		return nil
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", extensionKey, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("%s: %w", extensionKey, err)
	}
	return nil
}

// parsePriority reads a priority: a number from 0 to 1000 or a category name.
// Whether the category exists depends on the cluster (cluster.yaml); it is
// checked when the stack is applied.
func parsePriority(raw any) (Priority, error) {
	switch value := raw.(type) {
	case nil:
		return Priority{}, nil
	case float64:
		number := int(value)
		if float64(number) != value || number < 0 || number > 1000 {
			return Priority{}, fmt.Errorf("priority %v is not a whole number from 0 to 1000", value)
		}
		return Priority{Value: &number}, nil
	case string:
		if number, err := strconv.Atoi(value); err == nil {
			return parsePriority(float64(number))
		}
		if value == "" {
			return Priority{}, fmt.Errorf("priority is empty")
		}
		return Priority{Category: value}, nil
	default:
		return Priority{}, fmt.Errorf("priority %v is neither a number nor a category", raw)
	}
}
