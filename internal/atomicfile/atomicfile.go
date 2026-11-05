// Package atomicfile writes files so that a crash or power loss leaves the
// old content or the new one, never half of one.
//
// Every file it writes gets this process's user and group as owner and
// exactly the mode asked for, whatever was there before. WriteChanged and
// EnsureDir also set owner and mode on what they do not write: an
// unchanged file, a directory that exists.
package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Write writes content to path with mode, creating the directory (mode 0700)
// if needed: a new file next to it, synced, renamed over the old one, and
// the directory synced so the rename lasts. Two writers of one path at once
// are not supported: each may remove the other's temporary file.
func Write(path string, content []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	temporary := path + ".new"
	if err := writeSynced(temporary, content, mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteChanged writes content to path like Write, unless path is a regular
// file that holds content already; then it only sets the file's owner to
// this process's and its mode to mode, as Write would have made them.
func WriteChanged(path string, content []byte, mode os.FileMode) error {
	// whatever cannot be read as a regular file is written anew
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Write(path, content, mode)
	}
	if current, err := os.ReadFile(path); err != nil || !bytes.Equal(current, content) {
		return Write(path, content, mode)
	}
	if err := own(path, mode); err != nil {
		return fmt.Errorf("set the owner and mode of %s: %w", path, err)
	}
	return nil
}

// EnsureDir makes dir with its parents if it is missing, and gives it this
// process as owner and mode, also when it was there already. A symbolic
// link or a file in its place is an error: what goes into dir would land
// elsewhere.
func EnsureDir(dir string, mode os.FileMode) error {
	if err := os.MkdirAll(dir, mode); err != nil {
		return fmt.Errorf("make the directory %s: %w", dir, err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("check the directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("check the directory %s: it is no directory (a link or a file)", dir)
	}
	if err := own(dir, mode); err != nil {
		return fmt.Errorf("set the owner and mode of %s: %w", dir, err)
	}
	return nil
}

// writeSynced makes a new file and waits until it is on disk. One that a
// crash left behind goes first: opened as it is, it would keep its owner and
// mode and pass them on with the rename.
func writeSynced(path string, content []byte, mode os.FileMode) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	// the umask may have taken bits from mode that the readers need, and a
	// setgid directory would give the file its own group
	if err := own(path, mode); err != nil {
		_ = file.Close() // this error is the one to report
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close() // the write error is the one to report
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// own sets path's owner to this process's user and group and its mode to
// mode. Windows has neither (the agent runs on Linux; the tests run
// anywhere), so there it does nothing. Chmod follows a link: the callers
// made path themselves or checked that it is none.
func own(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// syncDir makes a rename in dir durable. Windows cannot sync a directory
// (the agent runs on Linux; the tests run anywhere).
func syncDir(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	return errors.Join(syncErr, directory.Close())
}
