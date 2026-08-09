package github

import (
	"errors"
	"testing"
)

func TestIsAuthorizationError(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "integration scope", err: errors.New("HTTP 403: Resource not accessible by integration"), want: true},
		{name: "bad credentials", err: errors.New("HTTP 401: Bad credentials"), want: true},
		{name: "ordinary forge refusal", err: errors.New("pull request is not mergeable"), want: false},
		{name: "nil", err: nil, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := IsAuthorizationError(test.err); got != test.want {
				t.Fatalf("IsAuthorizationError(%v) = %v, want %v", test.err, got, test.want)
			}
		})
	}
}
