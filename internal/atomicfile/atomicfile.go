// Package atomicfile writes files so that a crash or power loss leaves the
// old content or the new one, never half of one. The agent keeps its keys
// and host.json this way.
package atomicfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Write writes content to path with mode, creating the directory (mode 0700)
// if needed: a new file next to it, synced, renamed over the old one, and
// the directory synced so the rename lasts.
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

// writeSynced writes a file and waits until it is on disk.
func writeSynced(path string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
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
