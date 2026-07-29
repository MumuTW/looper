//go:build darwin || linux

package worker

import (
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestReadSpecBlockRejectsFIFOWithoutBlocking(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	specPath := filepath.Join(repo, "spec.fifo")
	if err := unix.Mkfifo(specPath, 0o600); err != nil {
		t.Fatalf("Mkfifo() error = %v", err)
	}

	done := make(chan error, 1)
	go func() {
		_, err := readSpecBlock(repo, "spec.fifo")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("readSpecBlock() error = nil, want FIFO rejection")
		}
	case <-time.After(time.Second):
		t.Fatal("readSpecBlock() blocked while opening FIFO")
	}
}
