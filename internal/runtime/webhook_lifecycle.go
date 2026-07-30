package runtime

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/processidentity"
	"github.com/nexu-io/looper/internal/storage"
)

const adoptedForwarderPollInterval = 2 * time.Second

type forwarderExitClass string

const (
	forwarderExitTransient forwarderExitClass = "transient"
	forwarderExitTerminal  forwarderExitClass = "terminal"
)

type forwarderExitClassification struct {
	Class          forwarderExitClass
	MatchedPattern string
}

func classifyForwarderExit(stderrTail []string, exitErr error) forwarderExitClassification {
	text := strings.ToLower(strings.Join(stderrTail, "\n"))
	patterns := []string{
		"Hook already exists on this repository",
		"HTTP 401",
		"authentication required",
		"gh auth login",
		"HTTP 403",
		"Resource not accessible by integration",
		"HTTP 404",
	}
	for _, pattern := range patterns {
		if strings.Contains(text, strings.ToLower(pattern)) {
			return forwarderExitClassification{Class: forwarderExitTerminal, MatchedPattern: pattern}
		}
	}
	if strings.Contains(text, "validation failed") && strings.Contains(text, "hook") {
		return forwarderExitClassification{Class: forwarderExitTerminal, MatchedPattern: "Validation Failed"}
	}
	return forwarderExitClassification{Class: forwarderExitTransient}
}

func commandFingerprint(ghPath, repo string, events []string, endpoint string) (string, string) {
	canonicalEvents := canonicalWebhookEvents(events)
	parts := []string{strings.TrimSpace(ghPath), strings.TrimSpace(repo), strings.Join(canonicalEvents, ","), strings.TrimSpace(endpoint)}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:]), strings.Join(canonicalEvents, ",")
}

func canonicalWebhookEvents(events []string) []string {
	canonical := make([]string, 0, len(events))
	seen := map[string]struct{}{}
	for _, event := range events {
		event = strings.ToLower(strings.TrimSpace(event))
		if event == "" {
			continue
		}
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		canonical = append(canonical, event)
	}
	sort.Strings(canonical)
	return canonical
}

func newDaemonID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

func webhookForwarderLockPath(cfgStorageDBPath string) string {
	return daemonLockPath(cfgStorageDBPath, "looperd.lock")
}

// databaseAttachLockPath returns the path of the database-lifetime attach lock
// held by every daemon while it is attached to a SQLite database. Unlike the
// webhook forwarder lock, this lock is acquired unconditionally on boot so that
// only one daemon at a time can be attached to a given database. That prevents
// two daemons that both implement this lock from racing the same schema: a
// second daemon cannot acquire this lock (and therefore cannot boot or migrate)
// until the first stops and releases it.
//
// Cross-version scope (this lock is new in this change): it coordinates only
// daemons that both acquire it. A release predating this lock never opens it,
// so acquiring it proves nothing about whether such a release is still
// attached — the lock cannot prevent a pre-lock release from remaining active
// while a newer daemon migrates. There is no database-lifetime lifecycle
// signal that preceding releases already honor (they open SQLite in WAL mode,
// which permits concurrent readers, and only take the webhook forwarder lock
// when webhooks are enabled). The cross-version contract for a rollout from a
// pre-lock release is therefore operational, not lock-enforced: stop the old
// daemon completely before starting the new one (no overlap). ValidateCompatibility
// remains the authority for the downgrade direction (old binary vs. newer
// schema); this lock is the authority for new-daemon-vs-new-daemon attachment.
//
// The lock filename is derived from a canonical identity of the database path
// (see attachLockFileName) so two independent daemons using different databases
// in the same directory — e.g. /srv/a.sqlite and /srv/b.sqlite — get distinct
// lock files instead of colliding on a single parent-directory lock.
//
// Trade-off analysis (AGENTS.md "New concepts require an explicit trade-off"):
//
//   - Concrete failure prevented: two daemons that both implement this lock and
//     share one database can no longer run at the same time. Without it, a
//     daemon that passes the point-in-time ValidateCompatibility check can have
//     a concurrent daemon apply an additional migration while it remains active
//     against the changing schema. The attach lock makes that new-vs-new
//     interleaving impossible — the second daemon cannot attach (and so cannot
//     migrate) until the first detaches.
//
//   - Costs / new failure modes:
//
//   - Concurrent daemons sharing one database can no longer run at the same
//     time. Previously only webhook-enabled daemons were serialized (via the
//     webhook lock); non-webhook daemons could overlap. SQLite is a
//     single-writer database not designed for two daemons to manage the same
//     schema concurrently, so this serializes an already-hazardous setup, but
//     it is a behavior change for deployments that relied on it.
//
//   - Boot now fails if another daemon holds the attach lock. flock is
//     kernel-managed and auto-released when the holder process exits, so a
//     crashed daemon never leaves a stale lock; a rolling update must wait for
//     the previous daemon to stop before the new one can start (no overlap).
//
//   - The lock cannot see pre-lock releases (see "Cross-version scope" above),
//     so it does not by itself make a mixed-version rollout with a pre-lock
//     binary safe. That is an inherent limit of introducing a new lock: only
//     daemons that take it are coordinated. The cost is a documented boundary,
//     not a silent overclaim.
//
//   - The looper CLI does not open the storage database, so this lock never
//     blocks CLI commands run against a live daemon.
//
//   - Why simpler alternatives are insufficient:
//
//   - "Rely on ValidateCompatibility alone" is a point-in-time check; it
//     cannot see a migration applied by a concurrent daemon after the check
//     passes.
//
//   - "Make the existing webhook lock unconditional" conflates webhook
//     forwarder ownership with database attachment and would change the
//     semantics of an existing, named lock. A dedicated attach lock keeps the
//     two concerns separate and the webhook lock's behavior intact.
//
// The authority for refusing a second attach is the running daemon's
// database-lifetime hold on this lock, not agent output or schema inference.
func databaseAttachLockPath(cfgStorageDBPath string) string {
	return daemonLockPath(cfgStorageDBPath, attachLockFileName(cfgStorageDBPath))
}

// attachLockFileName derives the attach-lock filename from a canonical identity
// of the database path so two independent daemons using different databases in
// the same directory (e.g. /srv/a.sqlite and /srv/b.sqlite) get distinct lock
// files. Keying only by the parent directory — as a fixed "looperd.attach.lock"
// name would — collides both daemons into one lock and rejects the second even
// though it cannot race the first daemon's schema. The identity is a short hash
// of the absolute, slash-normalized database path; the lock file content still
// records pid/daemon_id/started for operability.
func attachLockFileName(cfgStorageDBPath string) string {
	return "looperd.attach." + canonicalDatabaseIdentity(cfgStorageDBPath) + ".lock"
}

// resolvedDatabasePath returns the effective physical path of the configured
// database. It is the single source of truth for both the attach lock's
// directory and its identity hash: deriving those separately let the two
// disagree about which database a lock guards, which defeats the lock in two
// ways. A lexical identity hashes an alias (a symlinked file or parent
// directory) differently from its real path, so two daemons on one physical
// database take different locks and both migrate it. And an identity that
// resolves an empty DBPath differently from the lock directory produces
// different lock filenames in the same directory, with the same result.
//
// Resolution order: empty means the conventional ~/.looper/looper.sqlite, any
// path is made absolute, then symlinks are resolved.
func resolvedDatabasePath(cfgStorageDBPath string) string {
	dbPath := strings.TrimSpace(cfgStorageDBPath)
	if dbPath == "" {
		dbPath = "looper.sqlite"
		if home, err := os.UserHomeDir(); err == nil {
			dbPath = filepath.Join(home, ".looper", "looper.sqlite")
		}
	}
	if absPath, err := filepath.Abs(dbPath); err == nil {
		dbPath = absPath
	}
	return resolvePathSymlinks(dbPath)
}

// resolvePathSymlinks resolves the deepest existing ancestor of path and
// rejoins the components below it. filepath.EvalSymlinks fails outright when
// the leaf does not exist, which is the normal case on a first boot — the
// database file is created after the lock is taken — so resolving the existing
// prefix is what makes aliased spellings collapse before the file exists.
func resolvePathSymlinks(path string) string {
	var trailing []string
	current := path
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, trailing...)...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path
		}
		trailing = append([]string{filepath.Base(current)}, trailing...)
		current = parent
	}
}

// canonicalDatabaseIdentity returns a stable short identifier for the physical
// database a config points at. Aliased spellings of one database resolve to the
// same identity.
func canonicalDatabaseIdentity(cfgStorageDBPath string) string {
	sum := sha256.Sum256([]byte(filepath.ToSlash(resolvedDatabasePath(cfgStorageDBPath))))
	return hex.EncodeToString(sum[:8])
}

func daemonLockPath(cfgStorageDBPath, name string) string {
	return filepath.Join(filepath.Dir(resolvedDatabasePath(cfgStorageDBPath)), name)
}

type daemonLock struct {
	path string
	file *os.File
}

func acquireDaemonLock(path, daemonID string, now time.Time) (*daemonLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := os.ReadFile(path)
		_ = file.Close()
		return nil, fmt.Errorf("another looperd already holds %s (%s): %w", path, strings.TrimSpace(string(holder)), err)
	}
	if err := file.Truncate(0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := file.Seek(0, 0); err != nil {
		_ = file.Close()
		return nil, err
	}
	if _, err := fmt.Fprintf(file, "pid=%d daemon_id=%s started=%s\n", os.Getpid(), daemonID, now.UTC().Format(time.RFC3339)); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &daemonLock{path: path, file: file}, nil
}

func (l *daemonLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

type processProbe interface {
	IsAlive(pid int) (bool, error)
	StartTime(pid int) (int64, error)
	Argv(pid int) ([]string, error)
	ExecutablePath(pid int) (string, error)
}

type defaultProcessProbe struct{}

func (defaultProcessProbe) IsAlive(pid int) (bool, error) {
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func (defaultProcessProbe) StartTime(pid int) (int64, error) {
	return processidentity.StartTime(pid)
}

func (defaultProcessProbe) Argv(pid int) ([]string, error) {
	if runtime.GOOS == "linux" {
		data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
		if err != nil {
			return nil, err
		}
		trimmed := strings.TrimRight(string(data), "\x00")
		if trimmed == "" {
			return nil, nil
		}
		return strings.Split(trimmed, "\x00"), nil
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	if err != nil {
		return nil, err
	}
	return strings.Fields(strings.TrimSpace(string(out))), nil
}

func (defaultProcessProbe) ExecutablePath(pid int) (string, error) {
	if runtime.GOOS == "linux" {
		return os.Readlink(fmt.Sprintf("/proc/%d/exe", pid))
	}
	argv, err := (defaultProcessProbe{}).Argv(pid)
	if err != nil || len(argv) == 0 {
		return "", err
	}
	return argv[0], nil
}

type adoptedForwarderProcess struct {
	pid          int
	processStart int64
	probe        processProbe
	pollInterval time.Duration
}

func (p *adoptedForwarderProcess) Wait() error {
	interval := p.pollInterval
	if interval <= 0 {
		interval = adoptedForwarderPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		alive, err := p.probe.IsAlive(p.pid)
		if err != nil {
			continue
		}
		if !alive {
			return nil
		}
		start, err := p.probe.StartTime(p.pid)
		if err != nil {
			continue
		}
		if start != p.processStart {
			return fmt.Errorf("adopted process identity changed")
		}
	}
	return nil
}

func (p *adoptedForwarderProcess) Stop() error { return syscall.Kill(p.pid, syscall.SIGTERM) }
func (p *adoptedForwarderProcess) Kill() error { return syscall.Kill(p.pid, syscall.SIGKILL) }

func webhookForwarderRecordFromState(repo string, pid int, processStart int64, command []string, daemonID string, now time.Time) storage.WebhookForwarderRecord {
	ghPath, endpoint, events := commandIdentityParts(command)
	fingerprint, eventsCSV := commandFingerprint(ghPath, repo, events, endpoint)
	nanos := now.UTC().UnixNano()
	return storage.WebhookForwarderRecord{Repo: repo, PID: int64(pid), ProcessStart: processStart, Fingerprint: fingerprint, Endpoint: endpoint, Events: eventsCSV, GHPath: ghPath, DaemonID: daemonID, SpawnedAt: nanos, UpdatedAt: nanos}
}

func commandIdentityParts(command []string) (string, string, []string) {
	ghPath := ""
	endpoint := ""
	events := []string{}
	if len(command) > 0 {
		ghPath = command[0]
	}
	for i := 0; i < len(command); i++ {
		arg := command[i]
		if strings.HasPrefix(arg, "--url=") {
			endpoint = strings.TrimPrefix(arg, "--url=")
		} else if arg == "--url" && i+1 < len(command) {
			endpoint = command[i+1]
			i++
		}
		if strings.HasPrefix(arg, "--events=") {
			events = strings.Split(strings.TrimPrefix(arg, "--events="), ",")
		} else if arg == "--events" && i+1 < len(command) {
			events = strings.Split(command[i+1], ",")
			i++
		}
	}
	return ghPath, endpoint, events
}

func argvMatchesWebhookForward(argv []string, repo string, events []string, endpoint string) bool {
	if len(argv) < 3 || argv[1] != "webhook" || argv[2] != "forward" {
		return false
	}
	foundRepo := ""
	foundURL := ""
	foundEvents := []string{}
	for i := 3; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case strings.HasPrefix(arg, "--repo="):
			foundRepo = strings.TrimPrefix(arg, "--repo=")
		case arg == "--repo" && i+1 < len(argv):
			foundRepo = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--url="):
			foundURL = strings.TrimPrefix(arg, "--url=")
		case arg == "--url" && i+1 < len(argv):
			foundURL = argv[i+1]
			i++
		case strings.HasPrefix(arg, "--events="):
			foundEvents = strings.Split(strings.TrimPrefix(arg, "--events="), ",")
		case arg == "--events" && i+1 < len(argv):
			foundEvents = strings.Split(argv[i+1], ",")
			i++
		}
	}
	return foundRepo == repo && foundURL == endpoint && strings.Join(canonicalWebhookEvents(foundEvents), ",") == strings.Join(canonicalWebhookEvents(events), ",")
}
