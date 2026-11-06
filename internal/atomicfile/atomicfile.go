// Package atomicfile writes files so that a crash or power loss leaves the
// old content or the new one, never half of one.
//
// Every file it writes gets this process's user and group as owner and
// exactly the requested mode, whatever was there before. WriteChanged and
// EnsureDir also set owner and mode on what they do not write: an
// unchanged file, or a directory that already exists.
package atomicfile

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Write writes content to path with mode, creating the directory (mode
// 0700) if needed. It writes a new file next to the old one, syncs it,
// renames it over the old one, and syncs the directory so that the rename
// lasts. Two writers of one path at the same time are not supported,
// because each may remove the other's temporary file.
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
// file that already holds content. In that case it only sets the file's
// owner to this process's user and group and its mode to mode, as Write
// would have done.
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

// EnsureDir makes dir with its parents if it is missing, and sets its
// owner to this process and its mode to mode, even if it already existed.
// A symbolic link or a file in its place is an error, because what goes
// into dir would land elsewhere.
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

// writeSynced makes a new file and waits until it is on disk. It first
// removes a temporary file that a crash left behind. If writeSynced opened
// that file as it is, the file would keep its owner and mode, and the
// rename would pass them on.
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
// mode. Windows has neither, so there own does nothing; the agent runs on
// Linux, but the tests run anywhere. Chmod follows a link, so the callers
// either made path themselves or checked that it is not a link.
func own(path string, mode os.FileMode) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	if err := os.Lchown(path, os.Getuid(), os.Getgid()); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// syncDir makes a rename in dir durable. Windows cannot sync a directory,
// so there syncDir does nothing.
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
