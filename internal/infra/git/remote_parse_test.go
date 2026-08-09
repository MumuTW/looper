package git

import "testing"

func TestParseRemoteRepoFromURL(t *testing.T) {
	t.Parallel()

	cases := []struct {
		url      string
		wantHost string
		wantRepo string
	}{
		{url: "git@github.com:MumuTW/looper.git", wantHost: "github.com", wantRepo: "MumuTW/looper"},
		{url: "ssh://git@github.com/MumuTW/looper.git", wantHost: "github.com", wantRepo: "MumuTW/looper"},
		{url: "https://github.com/MumuTW/looper.git", wantHost: "github.com", wantRepo: "MumuTW/looper"},
		{url: "ssh://git@ssh.code.example.net/core/odcrew.git", wantHost: "ssh.code.example.net", wantRepo: "core/odcrew"},
		{url: "ssh://git@ssh.code.example.net:2222/core/odcrew.git", wantHost: "ssh.code.example.net", wantRepo: "core/odcrew"},
		{url: "https://code.example.net/core/odcrew.git", wantHost: "code.example.net", wantRepo: "core/odcrew"},
		{url: "git@code.example.net:core/odcrew.git", wantHost: "code.example.net", wantRepo: "core/odcrew"},
		{url: "forgejo@code.example.net:core/odcrew.git", wantHost: "code.example.net", wantRepo: "core/odcrew"},
		{url: "forgejo@[2001:db8::1]:core/odcrew.git", wantHost: "2001:db8::1", wantRepo: "core/odcrew"},
		{url: "", wantHost: "", wantRepo: ""},
		{url: "not-a-remote", wantHost: "", wantRepo: ""},
	}

	for _, tc := range cases {
		host, repo := parseRemoteRepoFromURL(tc.url)
		if host != tc.wantHost || repo != tc.wantRepo {
			t.Fatalf("parseRemoteRepoFromURL(%q) = (%q, %q), want (%q, %q)", tc.url, host, repo, tc.wantHost, tc.wantRepo)
		}
	}
}
