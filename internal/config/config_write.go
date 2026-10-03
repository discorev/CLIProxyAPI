package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
)

const stagedConfigPattern = ".config-*.yaml.tmp"

// configWriteMu serializes in-process config writers, so a rollback cannot
// overwrite a save made by another goroutine. Cross-process writers are not covered.
var configWriteMu sync.Mutex

// writeAndSync writes data to f and flushes it to disk. Tests replace it to inject failures.
var writeAndSync = func(f *os.File, data []byte) error {
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Sync()
}

// WriteConfigFile replaces the contents of path with data in place, keeping the
// file's inode so single-file bind mounts keep working. The new contents are first
// staged in a recovery copy, in the config directory or else in os.TempDir(); the
// save is refused if neither can hold it. The original bytes are restored if the
// in-place write fails, and the recovery copy is kept if that restore fails too.
// A file this call created is emptied if its write fails. perm only applies when
// path does not exist yet.
func WriteConfigFile(path string, data []byte, perm os.FileMode) error {
	configWriteMu.Lock()
	defer configWriteMu.Unlock()

	original, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		f, errCreate := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if errCreate == nil {
			return writeNewConfig(f, path, data)
		}
		if !errors.Is(errCreate, fs.ErrExist) {
			return fmt.Errorf("create config: %w", errCreate)
		}
		// Another process created the file meanwhile; replace it like an existing one.
		original, err = os.ReadFile(path)
	}
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	tmpPath, err := stageConfig(path, data)
	if err != nil {
		return err
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
	if errRestore := restoreConfig(path, original); errRestore != nil {
		return fmt.Errorf("write config: %w; restoring original failed: %v; new config kept at %s", errWrite, errRestore, tmpPath)
	}
	removeStagedConfig(tmpPath)
	return fmt.Errorf("write config (original restored): %w", errWrite)
}

// writeNewConfig writes data to f, a file this call just created at path. On failure
// it empties the file through f instead of removing path, so a config another process
// installed at path meanwhile cannot be deleted. Optional loads treat an empty config
// like a missing one.
func writeNewConfig(f *os.File, path string, data []byte) error {
	errWrite := writeAndSync(f, data)
	if errWrite != nil {
		if errTruncate := f.Truncate(0); errTruncate != nil {
			log.Warnf("failed to empty partial config %s: %v", path, errTruncate)
		}
	}
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	if errWrite != nil {
		return fmt.Errorf("write config: %w", errWrite)
	}
	return nil
}

// stageConfig writes data to a recovery copy and returns its path. The copy is never
// renamed into place, so falling back to os.TempDir() on another filesystem is fine.
func stageConfig(path string, data []byte) (string, error) {
	tmp, errDir := os.CreateTemp(filepath.Dir(path), stagedConfigPattern)
	if errDir != nil {
		var errTemp error
		if tmp, errTemp = os.CreateTemp(os.TempDir(), stagedConfigPattern); errTemp != nil {
			return "", fmt.Errorf("stage config: %w; fallback to temp dir: %w", errDir, errTemp)
		}
	}
	errStage := writeAndSync(tmp, data)
	if errClose := tmp.Close(); errStage == nil {
		errStage = errClose
	}
	if errStage != nil {
		removeStagedConfig(tmp.Name())
		return "", fmt.Errorf("stage config: %w", errStage)
	}
	return tmp.Name(), nil
}

func restoreConfig(path string, original []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	errWrite := writeAndSync(f, original)
	if errClose := f.Close(); errWrite == nil {
		errWrite = errClose
	}
	return errWrite
}

func removeStagedConfig(path string) {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Warnf("failed to remove staged config %s: %v", path, err)
	}
}
