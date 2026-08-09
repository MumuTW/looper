package daemonservice

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPreferReleaseCurrentExecutableRewritesReleaseTreePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", "1.2.3")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(releaseDir, "looperd")
	if err := os.WriteFile(daemon, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1.2.3"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	got := PreferReleaseCurrentExecutable(daemon)
	want := filepath.Join(root, "current", "looperd")
	if got != want {
		t.Fatalf("PreferReleaseCurrentExecutable() = %q, want %q", got, want)
	}
}

func TestPreferReleaseCurrentExecutableLeavesNonReleasePaths(t *testing.T) {
	t.Parallel()
	path := "/usr/local/bin/looperd"
	if got := PreferReleaseCurrentExecutable(path); got != path {
		t.Fatalf("got %q, want unchanged", got)
	}
}

func TestPreferReleaseCurrentExecutableWithoutCurrentKeepsReleasePath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	daemon := filepath.Join(root, "releases", "1.2.3", "looperd")
	if err := os.MkdirAll(filepath.Dir(daemon), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(daemon, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := PreferReleaseCurrentExecutable(daemon); got != daemon {
		t.Fatalf("got %q, want concrete release path when current is missing", got)
	}
}

func TestPreferReleaseCurrentExecutableUsesInnermostReleasesMarker(t *testing.T) {
	t.Parallel()
	// /tmp/.../releases/host/releases/1.2.3/looperd — outer "releases" is not the tree.
	outer := t.TempDir()
	root := filepath.Join(outer, "releases", "host")
	releaseDir := filepath.Join(root, "releases", "1.2.3")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(releaseDir, "looperd")
	if err := os.WriteFile(daemon, []byte("bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1.2.3"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	got := PreferReleaseCurrentExecutable(daemon)
	want := filepath.Join(root, "current", "looperd")
	if got != want {
		t.Fatalf("PreferReleaseCurrentExecutable() = %q, want innermost %q", got, want)
	}
}

func TestPreferReleaseCurrentExecutableRequiresRegularExecutableLeaf(t *testing.T) {
	for _, test := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "not executable", mode: 0o644},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			releaseDir := filepath.Join(root, "releases", "1.2.3")
			if err := os.MkdirAll(releaseDir, 0o755); err != nil {
				t.Fatal(err)
			}
			daemon := filepath.Join(releaseDir, "looperd")
			if err := os.WriteFile(daemon, []byte("bin"), test.mode); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join("releases", "1.2.3"), filepath.Join(root, "current")); err != nil {
				t.Fatal(err)
			}
			if got := PreferReleaseCurrentExecutable(daemon); got != daemon {
				t.Fatalf("got %q, want unchanged non-executable path", got)
			}
		})
	}

	root := t.TempDir()
	releaseDir := filepath.Join(root, "releases", "1.2.3")
	if err := os.MkdirAll(releaseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "looperd")
	if err := os.WriteFile(external, []byte("external"), 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(releaseDir, "looperd")
	if err := os.Symlink(external, daemon); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1.2.3"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if got := PreferReleaseCurrentExecutable(daemon); got != daemon {
		t.Fatalf("got %q, want unchanged leaf-symlink path", got)
	}
}

func TestPreferReleaseCurrentExecutableRequiresMatchingCurrentRelease(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "releases", "1.2.3")
	second := filepath.Join(root, "releases", "1.2.4")
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	daemon := filepath.Join(first, "looperd")
	if err := os.WriteFile(daemon, []byte("first"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "looperd"), []byte("second"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("releases", "1.2.4"), filepath.Join(root, "current")); err != nil {
		t.Fatal(err)
	}
	if got := PreferReleaseCurrentExecutable(daemon); got != daemon {
		t.Fatalf("got %q, want unchanged path when current selects another release", got)
	}
}
