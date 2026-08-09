package gatekeeper

import "testing"

func TestProtectedPathMatchSupportsRepositoryGlobs(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		{pattern: "internal/gatekeeper/**", path: "internal/gatekeeper/runner.go", want: true},
		{pattern: ".github/workflows/*", path: ".github/workflows/ci.yml", want: true},
		{pattern: "*.sql", path: "internal/storage/migrations/0001_init.sql", want: true},
		{pattern: "internal/gatekeeper/**", path: "internal/reviewer/runner.go", want: false},
		{pattern: "internal/gatekeeper/*", path: "internal/gatekeeper/sub/runner.go", want: false},
	}
	for _, test := range tests {
		if got := protectedPathMatch(test.pattern, test.path); got != test.want {
			t.Errorf("protectedPathMatch(%q, %q) = %v, want %v", test.pattern, test.path, got, test.want)
		}
	}
}

func TestMatchedProtectedPathsPreservesProviderOrderAndDeduplicates(t *testing.T) {
	got := matchedProtectedPaths([]string{"b.go", "internal/gatekeeper/runner.go", "b.go", "README.md"}, []string{"internal/gatekeeper/**", "*.go"})
	want := []string{"b.go", "internal/gatekeeper/runner.go"}
	if len(got) != len(want) {
		t.Fatalf("matched paths = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("matched paths = %#v, want %#v", got, want)
		}
	}
}
