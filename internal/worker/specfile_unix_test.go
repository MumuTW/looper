//go:build darwin || linux

package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestReadSpecBlockNeverFollowsRacingSymlinkAncestor(t *testing.T) {
	t.Parallel()

	repo := t.TempDir()
	outside := t.TempDir()
	specsPath := filepath.Join(repo, "specs")
	parkedPath := filepath.Join(repo, "specs.parked")
	if err := os.Mkdir(specsPath, 0o755); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(specsPath, "spec.md"), []byte("inside spec"), 0o644); err != nil {
		t.Fatalf("WriteFile(inside) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(outside, "spec.md"), []byte("outside secret"), 0o644); err != nil {
		t.Fatalf("WriteFile(outside) error = %v", err)
	}

	stop := make(chan struct{})
	done := make(chan struct{})
	swapErr := make(chan error, 1)
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := os.Rename(specsPath, parkedPath); err != nil {
				swapErr <- fmt.Errorf("park spec directory: %w", err)
				return
			}
			if err := os.Symlink(outside, specsPath); err != nil {
				swapErr <- fmt.Errorf("install outside symlink: %w", err)
				return
			}
			runtime.Gosched()
			if err := os.Remove(specsPath); err != nil {
				swapErr <- fmt.Errorf("remove outside symlink: %w", err)
				return
			}
			if err := os.Rename(parkedPath, specsPath); err != nil {
				swapErr <- fmt.Errorf("restore spec directory: %w", err)
				return
			}
			runtime.Gosched()
		}
	}()
	defer func() {
		close(stop)
		<-done
		select {
		case err := <-swapErr:
			t.Error(err)
		default:
		}
	}()

	for range 2_000 {
		block, err := readSpecBlock(repo, "specs/spec.md")
		if err == nil && strings.Contains(block, "outside secret") {
			t.Fatalf("readSpecBlock() followed racing ancestor symlink: %q", block)
		}
	}
}
