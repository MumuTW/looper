package github

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/infra/shell"
)

func TestGatewayRequireCredentialFailsBeforeStartingGH(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"} {
		t.Setenv(key, "")
	}
	calls := 0
	gateway := New(Options{
		GHPath:            "gh",
		RequireCredential: true,
		GHRun: func(context.Context, shell.Options) (shell.Result, error) {
			calls++
			return shell.Result{}, nil
		},
	})

	_, err := gateway.GetCurrentUserLogin(context.Background(), t.TempDir())
	if !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("GetCurrentUserLogin() error = %v, want ErrCredentialUnavailable", err)
	}
	if calls != 0 {
		t.Fatalf("gh calls = %d, want zero anonymous child processes", calls)
	}
}

func TestGatewayRequireCredentialRejectsWrongHostTokenFamily(t *testing.T) {
	for _, tc := range []struct{ name, token, host string }{
		{"public token on GHES", "GH_TOKEN", "ghe.example.test"},
		{"enterprise token on github.com", "GH_ENTERPRISE_TOKEN", "github.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			g := New(Options{Env: map[string]string{tc.token: "token"}, RequireCredential: true, GHRun: func(context.Context, shell.Options) (shell.Result, error) { calls++; return shell.Result{}, nil }})
			err := func() error {
				_, err := g.runGhWithTimeout(context.Background(), "", "", time.Second, "api", "user", "--hostname", tc.host)
				return err
			}()
			if !errors.Is(err, ErrCredentialUnavailable) || calls != 0 {
				t.Fatalf("err=%v calls=%d", err, calls)
			}
		})
	}
}

func TestGatewayDaemonCommandUsesOnlyTheTargetHostCredential(t *testing.T) {
	t.Setenv("GH_TOKEN", "ambient-public")
	t.Setenv("GH_ENTERPRISE_TOKEN", "ambient-enterprise")
	g := New(Options{Env: map[string]string{
		"GH_TOKEN":            "configured-public",
		"GH_ENTERPRISE_TOKEN": "configured-enterprise",
	}, RequireCredential: true})

	command, err := g.DaemonCommand(context.Background(), "ghe.example.test", "webhook", "forward")
	if err != nil {
		t.Fatalf("DaemonCommand() error = %v", err)
	}
	if !containsEnvironment(command.Env, "GH_ENTERPRISE_TOKEN=configured-enterprise") || containsEnvironmentPrefix(command.Env, "GH_TOKEN=") {
		t.Fatalf("child env = %q, want only configured GHES credential", command.Env)
	}

	g.UpdateCredentialEnv(map[string]string{"GH_TOKEN": "rotated-public"})
	if _, err := g.DaemonCommand(context.Background(), "ghe.example.test", "webhook", "forward"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("GHES DaemonCommand() error = %v, want ErrCredentialUnavailable after rotation", err)
	}
}

func containsEnvironment(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}

func containsEnvironmentPrefix(env []string, prefix string) bool {
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func TestGatewayAuthHealthReportsActualLoginAndCoreRateFromOneCall(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	calls := 0
	gateway := New(Options{
		GHPath:             "gh",
		Env:                map[string]string{"GH_TOKEN": "configured-token"},
		RequireCredential:  true,
		AuthHealthCacheTTL: 5 * time.Minute,
		Now:                func() time.Time { return now },
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			calls++
			if got := strings.Join(options.Args, " "); got != "api user --include --jq .login --hostname github.com" {
				t.Fatalf("gh args = %q", got)
			}
			return shell.Result{Stdout: "HTTP/2.0 200 OK\r\nX-Ratelimit-Limit: 5000\r\nX-Ratelimit-Remaining: 4182\r\nX-Ratelimit-Reset: 1785414807\r\n\r\nMumuTW\n"}, nil
		},
	})

	first := gateway.AuthHealth(context.Background(), "", "github.com")
	second := gateway.AuthHealth(context.Background(), "", "github.com")

	if !first.Authenticated || first.Login != "MumuTW" {
		t.Fatalf("AuthHealth() = %#v, want authenticated MumuTW", first)
	}
	if first.CoreRateLimit != 5000 || first.CoreRateRemaining != 4182 {
		t.Fatalf("core rate = %d/%d, want 4182/5000", first.CoreRateRemaining, first.CoreRateLimit)
	}
	if first.CoreRateResetAt != "2026-07-30T12:33:27Z" || first.CheckedAt != "2026-07-30T12:00:00Z" {
		t.Fatalf("health timestamps = reset %q checked %q", first.CoreRateResetAt, first.CheckedAt)
	}
	if second != first {
		t.Fatalf("cached AuthHealth() = %#v, want %#v", second, first)
	}
	if calls != 1 {
		t.Fatalf("gh calls = %d, want one cached probe", calls)
	}
}

func TestGatewayAuthHealthRefreshesAfterCacheTTL(t *testing.T) {
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	calls := 0
	gateway := New(Options{
		Env:                map[string]string{"GH_TOKEN": "configured-token"},
		RequireCredential:  true,
		AuthHealthCacheTTL: time.Minute,
		Now:                func() time.Time { return now },
		GHRun: func(context.Context, shell.Options) (shell.Result, error) {
			calls++
			return shell.Result{Stdout: "HTTP/2.0 200 OK\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nMumuTW\n"}, nil
		},
	})

	_ = gateway.AuthHealth(context.Background(), "", "github.com")
	now = now.Add(time.Minute + time.Second)
	_ = gateway.AuthHealth(context.Background(), "", "github.com")
	if calls != 2 {
		t.Fatalf("gh calls = %d, want refresh after TTL", calls)
	}
}

func TestGatewayAuthHealthConcurrentSameHostReadsCachedResultSafely(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	gateway := New(Options{
		Env:               map[string]string{"GH_TOKEN": "configured-token"},
		RequireCredential: true,
		GHRun: func(context.Context, shell.Options) (shell.Result, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			return shell.Result{Stdout: "HTTP/2.0 200 OK\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nMumuTW\n"}, nil
		},
	})
	if health := gateway.AuthHealth(context.Background(), "", "github.com"); !health.Authenticated {
		t.Fatalf("initial AuthHealth() = %#v, want authenticated", health)
	}

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if health := gateway.AuthHealth(context.Background(), "", "github.com"); !health.Authenticated {
				t.Errorf("cached AuthHealth() = %#v, want authenticated", health)
			}
		}()
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("gh calls = %d, want one initial probe followed by cache reads", calls)
	}
}

func TestGatewayAuthHealthCoalescesConcurrentColdProbes(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	gateway := New(Options{
		Env:               map[string]string{"GH_TOKEN": "configured-token"},
		RequireCredential: true,
		GHRun: func(context.Context, shell.Options) (shell.Result, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			started <- struct{}{}
			<-release
			return shell.Result{Stdout: "HTTP/2 200\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nMumuTW\n"}, nil
		},
	})

	ownerDone := make(chan AuthHealth, 1)
	go func() { ownerDone <- gateway.AuthHealth(context.Background(), "", "github.com") }()
	<-started

	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if health := gateway.AuthHealth(context.Background(), "", "github.com"); !health.Authenticated || health.Login != "MumuTW" {
				t.Errorf("AuthHealth() = %#v, want authenticated MumuTW", health)
			}
		}()
	}
	close(release)
	if health := <-ownerDone; !health.Authenticated {
		t.Fatalf("owner AuthHealth() = %#v, want authenticated", health)
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("gh calls = %d, want one shared cold probe", calls)
	}
}

func TestGatewayAuthHealthDoesNotCacheProbeFromPreviousCredentialGeneration(t *testing.T) {
	oldStarted := make(chan struct{}, 1)
	releaseOld := make(chan struct{})
	var mu sync.Mutex
	calls := 0
	gateway := New(Options{
		Env:               map[string]string{"GH_TOKEN": "old"},
		RequireCredential: true,
		GHRun: func(_ context.Context, options shell.Options) (shell.Result, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			if options.Env["GH_TOKEN"] == "old" {
				oldStarted <- struct{}{}
				<-releaseOld
				return shell.Result{Stdout: "HTTP/2 200\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nold-user\n"}, nil
			}
			return shell.Result{Stdout: "HTTP/2 200\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nnew-user\n"}, nil
		},
	})

	oldDone := make(chan AuthHealth, 1)
	go func() { oldDone <- gateway.AuthHealth(context.Background(), "", "github.com") }()
	<-oldStarted
	gateway.UpdateCredentialEnv(map[string]string{"GH_TOKEN": "new"})
	close(releaseOld)
	if health := <-oldDone; health.Login != "old-user" {
		t.Fatalf("in-flight AuthHealth() = %#v, want old probe result", health)
	}

	fresh := gateway.AuthHealth(context.Background(), "", "github.com")
	cached := gateway.AuthHealth(context.Background(), "", "github.com")
	if fresh.Login != "new-user" || cached.Login != "new-user" {
		t.Fatalf("fresh=%#v cached=%#v, want new credential identity", fresh, cached)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("gh calls = %d, want old probe plus one fresh probe", calls)
	}
}

func TestGatewayAuthHealthDoesNotCacheCanceledProbe(t *testing.T) {
	calls := 0
	gateway := New(Options{
		Env:               map[string]string{"GH_TOKEN": "configured-token"},
		RequireCredential: true,
		GHRun: func(ctx context.Context, _ shell.Options) (shell.Result, error) {
			calls++
			if err := ctx.Err(); err != nil {
				return shell.Result{}, err
			}
			return shell.Result{Stdout: "HTTP/2.0 200 OK\nX-Ratelimit-Limit: 5000\nX-Ratelimit-Remaining: 4000\nX-Ratelimit-Reset: 1785414807\n\nMumuTW\n"}, nil
		},
	})

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	first := gateway.AuthHealth(canceled, "", "github.com")
	second := gateway.AuthHealth(context.Background(), "", "github.com")
	if first.Error == "" {
		t.Fatalf("canceled AuthHealth() = %#v, want explicit error", first)
	}
	if !second.Authenticated || calls != 2 {
		t.Fatalf("second AuthHealth() = %#v calls=%d, want fresh successful probe", second, calls)
	}
}

func TestGatewayCredentialUpdateClearsHealthCacheAndReplacesChildEnv(t *testing.T) {
	calls := 0
	g := New(Options{Env: map[string]string{"GH_TOKEN": "old"}, RequireCredential: true, GHRun: func(_ context.Context, o shell.Options) (shell.Result, error) {
		calls++
		if o.Env["GH_TOKEN"] != "new" && calls > 1 {
			t.Fatalf("env=%q", o.Env["GH_TOKEN"])
		}
		return shell.Result{Stdout: "HTTP/2 200\nX-Ratelimit-Limit: 1\nX-Ratelimit-Remaining: 1\nX-Ratelimit-Reset: 1\n\nuser\n"}, nil
	}})
	_ = g.AuthHealth(context.Background(), "", "github.com")
	g.UpdateCredentialEnv(map[string]string{"GH_TOKEN": "new"})
	_ = g.AuthHealth(context.Background(), "", "github.com")
	if calls != 2 {
		t.Fatalf("calls=%d, want cache cleared", calls)
	}
}
