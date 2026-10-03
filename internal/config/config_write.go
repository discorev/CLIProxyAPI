package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	log "github.com/sirupsen/logrus"
)

// writeAndSync writes data to f and flushes it to disk. Tests replace it to inject failures.
var writeAndSync = func(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// WriteConfigFile replaces the contents of path with data in place, keeping the
// file's inode so single-file bind mounts keep working. The new contents are staged
// in a temp file first when the directory allows it, and the original bytes are
// restored if the in-place write fails. A file that did not exist is removed if its
// write fails. perm only applies when path does not exist yet.
func WriteConfigFile(path string, data []byte, perm os.FileMode) error {
	original, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		if errWrite := writeFileInPlace(path, data, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm); errWrite != nil {
			if errRemove := os.Remove(path); errRemove != nil && !errors.Is(errRemove, fs.ErrNotExist) {
				log.Warnf("failed to remove partial config %s: %v", path, errRemove)
			}
			return fmt.Errorf("write config: %w", errWrite)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	// Staging is best-effort: the directory may be read-only while the file itself is writable.
	tmpPath := ""
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.yaml.tmp")
	if err != nil {
		log.Warnf("config staging unavailable for %s: %v", path, err)
	} else {
		tmpPath = tmp.Name()
		errStage := writeAndSync(tmp, data)
		if errClose := tmp.Close(); errStage == nil {
			errStage = errClose
		}
		if errStage != nil {
			removeStagedConfig(tmpPath)
			return fmt.Errorf("stage config: %w", errStage)
		}
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		// Open failed before truncation, so the live file is untouched.
		removeStagedConfig(tmpPath)
		return fmt.Errorf("open config: %w", err)
	}
	errWrite := writeAndSync(f, data)
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite == nil {
		removeStagedConfig(tmpPath)
		return nil
	}
	if errRestore := writeFileInPlace(path, original, os.O_WRONLY|os.O_TRUNC, 0); errRestore != nil {
		if tmpPath == "" {
			return fmt.Errorf("write config: %w; restoring original failed: %v", errWrite, errRestore)
		}
		return fmt.Errorf("write config: %w; restoring original failed: %v; new config kept at %s", errWrite, errRestore, tmpPath)
	}
	removeStagedConfig(tmpPath)
	return fmt.Errorf("write config (original restored): %w", errWrite)
}

func writeFileInPlace(path string, data []byte, flag int, perm os.FileMode) error {
	f, err := os.OpenFile(path, flag, perm)
	if err != nil {
		return err
	}
	errWrite := writeAndSync(f, data)
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	return errWrite
}

func removeStagedConfig(path string) {
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("failed to remove staged config %s: %v", path, err)
	}
}
