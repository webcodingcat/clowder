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

func TestALeftoverTemporaryFileDoesNotPassOnItsMode(t *testing.T) {
	skipOnWindows(t)
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path+".new", []byte("half"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path+".new", 0o666); err != nil { // past the umask
		t.Fatal(err)
	}
	if err := Write(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
}

func TestWriteChangedSetsTheModeOfAnUnchangedFile(t *testing.T) {
	skipOnWindows(t)
	path := filepath.Join(t.TempDir(), "secret")
	if err := Write(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := WriteChanged(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// a file is replaced by a rename: the same file means it was left alone
	if !os.SameFile(before, after) {
		t.Error("the unchanged file was written anew")
	}
}

func TestWriteChangedReplacesWhatIsNoRegularFile(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	elsewhere := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(elsewhere, []byte("value"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "secret")
	if err := os.Symlink(elsewhere, path); err != nil {
		t.Fatal(err)
	}
	if err := WriteChanged(path, []byte("value"), 0o600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		t.Errorf("the link stayed: %v, %v", info, err)
	}
	assertMode(t, elsewhere, 0o644)
}

func TestEnsureDirSetsTheModeOfADirectoryThatIsThere(t *testing.T) {
	skipOnWindows(t)
	dir := filepath.Join(t.TempDir(), "secrets")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	assertMode(t, dir, 0o700)
}

func TestEnsureDirRefusesALinkOrAFile(t *testing.T) {
	skipOnWindows(t)
	root := t.TempDir()
	file := filepath.Join(root, "file")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(t.TempDir(), link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{file, link} {
		if err := EnsureDir(path, 0o700); err == nil {
			t.Errorf("%s taken as a directory", path)
		}
	}
}

// skipOnWindows skips a test of modes and links: Windows has neither as
// Linux does, and the agent runs on Linux.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("modes and links work otherwise on Windows")
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != want {
		t.Errorf("%s has mode %v, want %v", path, info.Mode().Perm(), want)
	}
}
