package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// failWriteCalls makes the listed writeAndSync calls (1-based) write a partial
// prefix and fail, simulating a disk-full error mid-write.
func failWriteCalls(t *testing.T, failing ...int) {
	t.Helper()
	orig := writeAndSync
	calls := 0
	writeAndSync = func(f *os.File, data []byte) error {
		calls++
		if !slices.Contains(failing, calls) {
			return orig(f, data)
		}
		if len(data) > 0 {
			_, _ = f.Write(data[:1])
		}
		return errors.New("injected write failure")
	}
	t.Cleanup(func() { writeAndSync = orig })
}

func writeTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func readTestConfig(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func stagedConfigs(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".config-*.yaml.tmp"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return matches
}

func TestWriteConfigFileReplacesInPlace(t *testing.T) {
	path := writeTestConfig(t, "port: 1\nlonger: content-that-gets-truncated\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if err = WriteConfigFile(path, []byte("port: 2\n"), 0o600); err != nil {
		t.Fatalf("WriteConfigFile: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("config file was replaced instead of written in place")
	}
	if got := readTestConfig(t, path); got != "port: 2\n" {
		t.Fatalf("content = %q", got)
	}
	if left := stagedConfigs(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}

func TestWriteConfigFileCreatesMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")

	if err := WriteConfigFile(path, []byte("port: 2\n"), 0o600); err != nil {
		t.Fatalf("WriteConfigFile: %v", err)
	}

	if got := readTestConfig(t, path); got != "port: 2\n" {
		t.Fatalf("content = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("perm = %o, want 600", perm)
	}
	if left := stagedConfigs(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}

func TestWriteConfigFileStagingFailureLeavesOriginal(t *testing.T) {
	path := writeTestConfig(t, "port: 1\n")
	failWriteCalls(t, 1)

	if err := WriteConfigFile(path, []byte("port: 2\n"), 0o600); err == nil {
		t.Fatal("expected staging error")
	}

	if got := readTestConfig(t, path); got != "port: 1\n" {
		t.Fatalf("original changed: %q", got)
	}
	if left := stagedConfigs(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}

func TestWriteConfigFileWriteFailureRestoresOriginal(t *testing.T) {
	path := writeTestConfig(t, "port: 1\n")
	// Call 1 stages, call 2 is the in-place write, call 3 restores.
	failWriteCalls(t, 2)

	err := WriteConfigFile(path, []byte("port: 2\n"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "original restored") {
		t.Fatalf("expected restored write error, got %v", err)
	}

	if got := readTestConfig(t, path); got != "port: 1\n" {
		t.Fatalf("original not restored: %q", got)
	}
	if left := stagedConfigs(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("staged files left behind after restore: %v", left)
	}
}

func TestWriteConfigFileRestoreFailureKeepsStagedCopy(t *testing.T) {
	path := writeTestConfig(t, "port: 1\n")
	failWriteCalls(t, 2, 3)

	err := WriteConfigFile(path, []byte("port: 2\n"), 0o600)
	if err == nil {
		t.Fatal("expected write error")
	}

	left := stagedConfigs(t, filepath.Dir(path))
	if len(left) != 1 {
		t.Fatalf("staged files = %v, want one recovery copy", left)
	}
	if !strings.Contains(err.Error(), left[0]) {
		t.Fatalf("error %q does not name recovery copy %s", err, left[0])
	}
	if got := readTestConfig(t, left[0]); got != "port: 2\n" {
		t.Fatalf("recovery copy = %q", got)
	}
}
