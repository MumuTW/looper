package main

import (
	"errors"
	"testing"
)

func TestApplyDirtySuffix(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		value string
		dirty bool
		want  string
	}{
		{name: "clean tree keeps the exact stamp", value: "0.11.3", dirty: false, want: "0.11.3"},
		{name: "dirty tree is marked", value: "0.0.0-dev+g1e13c67", dirty: true, want: "0.0.0-dev+g1e13c67-dirty"},
		{name: "empty value stays empty", value: "", dirty: true, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := applyDirtySuffix(tc.value, tc.dirty); got != tc.want {
				t.Fatalf("applyDirtySuffix(%q, %v) = %q, want %q", tc.value, tc.dirty, got, tc.want)
			}
		})
	}
}

func TestWorktreeDirty(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		output string
		err    error
		want   *bool
	}{
		{name: "no output is clean", output: "", want: boolPtr(false)},
		{name: "whitespace only is clean", output: "\n  \n", want: boolPtr(false)},
		{name: "modified file is dirty", output: " M internal/version/version.go\n", want: boolPtr(true)},
		{name: "untracked file is dirty", output: "?? internal/version/new.go\n", want: boolPtr(true)},
		{name: "probe failure is unknown", err: errors.New("exec: git not found"), want: nil},
		{name: "probe failure ignores output", output: " M x.go\n", err: errors.New("boom"), want: nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			run := func(string, ...string) ([]byte, error) { return []byte(tc.output), tc.err }
			if got := worktreeDirty(run); !equalBoolPtr(got, tc.want) {
				t.Fatalf("worktreeDirty() = %v, want %v (output=%q err=%v)", got, tc.want, tc.output, tc.err)
			}
		})
	}
}

func boolPtr(value bool) *bool { return &value }

func equalBoolPtr(left, right *bool) bool {
	return left == nil && right == nil || left != nil && right != nil && *left == *right
}

func TestWorktreeDirtyProbesGitStatusPorcelain(t *testing.T) {
	t.Parallel()

	var gotName string
	var gotArgs []string
	run := func(name string, args ...string) ([]byte, error) {
		gotName, gotArgs = name, args
		return nil, nil
	}

	worktreeDirty(run)

	if gotName != "git" {
		t.Fatalf("command = %q, want git", gotName)
	}
	// --untracked-files=all defeats a user's status.showUntrackedFiles=no; the
	// exclude pathspec keeps the dashboard bundles that ci.yml and release.yml
	// generate before this probe from stamping every artifact -dirty.
	want := []string{"status", "--porcelain", "--untracked-files=all",
		"--", ":(exclude)internal/dashboard/assets"}
	if len(gotArgs) != len(want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
	for i := range want {
		if gotArgs[i] != want[i] {
			t.Fatalf("args = %#v, want %#v", gotArgs, want)
		}
	}
}
