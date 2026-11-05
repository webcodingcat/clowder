package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// otherUser owns what the tests hand over to another user.
const otherUser = 4242

func TestTheOwnerIsSetAgain(t *testing.T) {
	if os.Getuid() != 0 {
		t.Skip("only root gives a file to another user")
	}
	dir := filepath.Join(t.TempDir(), "secrets")
	file := filepath.Join(dir, "secret")
	if err := Write(file, []byte("value"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, file} {
		if err := os.Chown(path, otherUser, otherUser); err != nil {
			t.Fatal(err)
		}
	}
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WriteChanged(file, []byte("value"), 0o444); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, file} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		owner := info.Sys().(*syscall.Stat_t)
		if int(owner.Uid) != os.Getuid() || int(owner.Gid) != os.Getgid() {
			t.Errorf("%s belongs to %d:%d, want %d:%d", path, owner.Uid, owner.Gid, os.Getuid(), os.Getgid())
		}
	}
}
