//go:build !windows

package cliproxy

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestUpstreamModelCopyRejectsFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "upstream.json")
	if errMkfifo := syscall.Mkfifo(path, 0o600); errMkfifo != nil {
		t.Skipf("mkfifo: %v", errMkfifo)
	}
	done := make(chan error, 1)
	go func() {
		_, errLoad := loadPersistedUpstreamModels(path)
		done <- errLoad
	}()
	select {
	case errLoad := <-done:
		if errLoad == nil {
			t.Fatal("FIFO accepted as the upstream model copy")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loading a FIFO blocked")
	}
}
