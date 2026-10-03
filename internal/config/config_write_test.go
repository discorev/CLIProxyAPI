package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

func TestWriteConfigFileMissingFileWriteFailureEmptiesPartial(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	failWriteCalls(t, 1)

	if err := WriteConfigFile(path, []byte("port: 2\n"), 0o600); err == nil {
		t.Fatal("expected write error")
	}

	if got := readTestConfig(t, path); got != "" {
		t.Fatalf("partial content left behind: %q", got)
	}
}

// readOnlyDir makes dir non-writable for the rest of the test.
func readOnlyDir(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestWriteConfigFileReadOnlyDirStagesInTempDir(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	path := writeTestConfig(t, "port: 1\nlonger: content-that-gets-truncated\n")
	readOnlyDir(t, filepath.Dir(path))

	orig := writeAndSync
	var written []string
	writeAndSync = func(f *os.File, data []byte) error {
		written = append(written, f.Name())
		return orig(f, data)
	}
	t.Cleanup(func() { writeAndSync = orig })

	if err := WriteConfigFile(path, []byte("port: 2\n"), 0o600); err != nil {
		t.Fatalf("WriteConfigFile: %v", err)
	}

	if got := readTestConfig(t, path); got != "port: 2\n" {
		t.Fatalf("content = %q", got)
	}
	if len(written) != 2 || filepath.Dir(written[0]) != tempDir || written[1] != path {
		t.Fatalf("writes = %v, want staged copy in %s then %s", written, tempDir, path)
	}
	if left := stagedConfigs(t, tempDir); len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}

func TestWriteConfigFileNoStagingDirRefusesSave(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	tempDir := t.TempDir()
	readOnlyDir(t, tempDir)
	t.Setenv("TMPDIR", tempDir)
	path := writeTestConfig(t, "port: 1\n")
	readOnlyDir(t, filepath.Dir(path))

	err := WriteConfigFile(path, []byte("port: 2\n"), 0o600)
	if err == nil || !strings.Contains(err.Error(), "stage config") {
		t.Fatalf("expected stage config error, got %v", err)
	}

	if got := readTestConfig(t, path); got != "port: 1\n" {
		t.Fatalf("original changed: %q", got)
	}
}

func TestWriteConfigFileMissingFileKeepsReplacementOnFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	orig := writeAndSync
	writeAndSync = func(f *os.File, data []byte) error {
		// Another writer replaces our new file with its own before we clean up.
		if err := os.Remove(path); err != nil {
			t.Errorf("remove: %v", err)
		}
		if err := os.WriteFile(path, []byte("other: true\n"), 0o600); err != nil {
			t.Errorf("install other: %v", err)
		}
		return errors.New("injected write failure")
	}
	t.Cleanup(func() { writeAndSync = orig })

	if err := WriteConfigFile(path, []byte("port: 2\n"), 0o600); err == nil {
		t.Fatal("expected write error")
	}

	if got := readTestConfig(t, path); got != "other: true\n" {
		t.Fatalf("other writer's file = %q", got)
	}
}

func TestWriteConfigFileConcurrentWriters(t *testing.T) {
	path := writeTestConfig(t, "port: 0\n")
	const writers = 16
	payloads := make([]string, writers)
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		payloads[i] = fmt.Sprintf("port: %d\n", i+1)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = WriteConfigFile(path, []byte(payloads[i]), 0o600)
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("writer %d: %v", i, err)
		}
	}
	if got := readTestConfig(t, path); !slices.Contains(payloads, got) {
		t.Fatalf("final content %q is not one of the payloads", got)
	}
	if left := stagedConfigs(t, filepath.Dir(path)); len(left) != 0 {
		t.Fatalf("staged files left behind: %v", left)
	}
}
