package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteReplacesTheFileWholeAndKeepsItsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "key.txt")
	for _, content := range []string{"old\n", "new\n"} {
		if err := Write(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "new\n" {
		t.Errorf("read %q, %v", got, err)
	}
	if _, err := os.Stat(path + ".new"); !os.IsNotExist(err) {
		t.Errorf("the temporary file is left: %v", err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", info.Mode().Perm())
	}
}
