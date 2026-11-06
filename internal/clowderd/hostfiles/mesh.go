package hostfiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/webcodingcat/clowder/internal/atomicfile"
	"github.com/webcodingcat/clowder/internal/network"
)

// WriteMesh keeps the mesh in net.json. The join writes the first one, and
// the agent's network reconciler each one it applied.
func (f Files) WriteMesh(mesh network.Mesh) error {
	content, err := json.MarshalIndent(mesh, "", "  ")
	if err != nil {
		return fmt.Errorf("write %s: %w", f.MeshFile(), err)
	}
	content = append(content, '\n')
	if old, err := os.ReadFile(f.MeshFile()); err == nil && bytes.Equal(old, content) {
		return nil
	}
	return atomicfile.Write(f.MeshFile(), content, 0o600)
}

// ReadMesh reads net.json. Without the file, the error is os.ErrNotExist.
func (f Files) ReadMesh() (network.Mesh, error) {
	var mesh network.Mesh
	content, err := os.ReadFile(f.MeshFile())
	if err != nil {
		return mesh, fmt.Errorf("read %s: %w", f.MeshFile(), err)
	}
	if err := json.Unmarshal(content, &mesh); err != nil {
		return mesh, fmt.Errorf("read %s: %w", f.MeshFile(), err)
	}
	return mesh, nil
}
