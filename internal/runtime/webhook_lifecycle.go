package runtime

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/nexu-io/looper/internal/config"
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

// webhookForwarderLockPath derives the singleton webhook forwarder lock path.
// It is deliberately rollout-stable and lexical: it resolves the configured
// DBPath only with filepath.Abs (no symlink resolution) and places looperd.lock
// beside that spelling. A pre-change webhook-enabled daemon derives the very
// same path, so during a rolling upgrade the old and new daemons contend on one
// lock file instead of each acquiring their own (the old daemon beside the
// configured symlink, a canonicalizing new daemon beside the resolved target)
// and running competing forwarders. Canonical symlink resolution is reserved
// for the new database attach lock (databaseAttachLockPath), which has no
// pre-change holder to stay consistent with.
func webhookForwarderLockPath(cfgStorageDBPath string) string {
	dbPath := strings.TrimSpace(cfgStorageDBPath)
	if dbPath != "" {
		if absPath, err := filepath.Abs(dbPath); err == nil {
			dbPath = absPath
		}
	}
	dir := filepath.Dir(dbPath)
	if dir == "." || dir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			dir = filepath.Join(home, ".looper")
		}
	}
	return filepath.Join(dir, "looperd.lock")
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
// Identity. The lock filename is keyed by the database file's filesystem
// identity (device + inode), not by its path spelling. Two daemons reach the
// same physical file through any of the spellings OpenSQLiteDB supports — a
// bare path, a file: URI, a symlink, a hard link, or a differently cased name
// on a case-insensitive volume — and the inode is the same for all of them, so
// they collapse to one lock. Two daemons on different databases get distinct
// inodes and therefore distinct lock files. The database file is created if it
// does not yet exist (first boot) so the inode is stable before and after
// SQLite opens it; an empty file is a valid empty SQLite database, so creating
// it ahead of SQLite changes nothing about what SQLite subsequently opens.
//
// Location. The lock lives in a private, daemon-owned directory (~/.looper/locks,
// mode 0700) rather than beside the database file. A database directory such
// as /tmp or a shared volume may be writable by another user, who could
// pre-create a deterministic lock pathname beside the database as a symlink to
// a file the daemon can write; the previous beside-database derivation followed
// that symlink and truncated the target. A private 0700 directory denies any
// other user the ability to create entries in it, so no symlink can be planted
// for the daemon to follow. acquireDaemonLock additionally opens the lock with
// O_NOFOLLOW and verifies the open file is a regular file owned by the daemon
// user before truncating, as defense in depth.
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
//   - The database file is created on first boot before the lock is taken. If
//     lock acquisition then fails the empty file remains behind; this is
//     harmless because SQLite would have created it on the next attempt
//     anyway, and an empty file is a valid empty database.
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
//   - "Key by resolved path spelling" (the prior design) was replaced because
//     filepath.EvalSymlinks only replaces symlinks: hard links and
//     case-insensitive aliases of one file resolve to different strings and
//     took different locks. Filesystem identity (inode) collapses all
//     spellings of one file and removes the separate dangling-symlink
//     resolution layer that the spelling approach required.
//
// The authority for refusing a second attach is the running daemon's
// database-lifetime hold on this lock, not agent output or schema inference.
func databaseAttachLockPath(cfgStorageDBPath string) (string, error) {
	identity, err := databaseFilesystemIdentity(cfgStorageDBPath)
	if err != nil {
		return "", err
	}
	lockDir := attachLockDir()
	if err := os.MkdirAll(lockDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(lockDir, "looperd.attach."+identity+".lock"), nil
}

// attachLockDir returns the private, daemon-owned directory that holds every
// attach lock. It lives under the daemon's state directory (the same
// ~/.looper that holds looper.sqlite, backups/, and logs/, resolved via
// config.DefaultLooperHome so LOOPER_HOME overrides it for tests and second
// instances) and is created with mode 0700 so only the daemon user can place
// entries in it. A database directory such as /tmp or a shared volume may be
// writable by another user, who could pre-create a deterministic lock pathname
// beside the database as a symlink; placing the lock in a private 0700
// directory under the daemon's own state directory denies any other user the
// ability to create entries there, so no symlink can be planted for the daemon
// to follow and truncate.
func attachLockDir() string {
	looperHome, err := config.DefaultLooperHome()
	if err != nil || strings.TrimSpace(looperHome) == "" {
		return "looperd.locks"
	}
	return filepath.Join(looperHome, "locks")
}

// databaseFilesystemPath returns the filesystem path SQLite will open for the
// configured dbPath: the conventional ~/.looper/looper.sqlite default for an
// empty value, and the path component of a file: URI (both path and opaque
// forms) for the file: form OpenSQLiteDB supports. It does not resolve
// symlinks or make the path absolute — os.Stat follows symlinks, and the
// filesystem identity (inode) is what disambiguates databases, not the
// spelling. The value is not trimmed, matching OpenSQLiteDB/sqliteDSN: a
// value like "  :memory:  " is not the exact ":memory:" SQLite recognizes, so
// it is treated as a disk filename here and by SQLite alike.
func databaseFilesystemPath(cfgStorageDBPath string) string {
	dbPath := cfgStorageDBPath
	if strings.TrimSpace(dbPath) == "" {
		if looperHome, err := config.DefaultLooperHome(); err == nil && strings.TrimSpace(looperHome) != "" {
			return filepath.Join(looperHome, "looper.sqlite")
		}
		return "looper.sqlite"
	}
	if strings.HasPrefix(dbPath, "file:") {
		if parsed, err := url.Parse(dbPath); err == nil {
			switch {
			case parsed.Path != "":
				dbPath = parsed.Path
			case parsed.Opaque != "":
				dbPath = parsed.Opaque
			}
		}
	}
	return dbPath
}

// databaseFilesystemIdentity returns a stable short identifier for the physical
// database a config points at, keyed by the file's device and inode. Any
// spelling of one physical file — bare path, file: URI, symlink, hard link, or
// a differently cased name on a case-insensitive volume — yields the same
// identity, because os.Stat follows symlinks and reports the target's inode.
// The database file is created if it does not yet exist so the identity is
// stable across the first-boot boundary (before and after SQLite creates it).
func databaseFilesystemIdentity(cfgStorageDBPath string) (string, error) {
	path := databaseFilesystemPath(cfgStorageDBPath)
	if err := ensureDatabaseFileExists(path); err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		// Non-Unix fallback: hash the absolute path. This build already
		// requires syscall.Flock (Unix), so this branch is unreachable in
		// supported deployments but keeps the function total.
		abs, _ := filepath.Abs(path)
		sum := sha256.Sum256([]byte(filepath.ToSlash(abs)))
		return hex.EncodeToString(sum[:8]), nil
	}
	h := sha256.New()
	fmt.Fprintf(h, "%d:%d", stat.Dev, stat.Ino)
	return hex.EncodeToString(h.Sum(nil)[:8]), nil
}

// ensureDatabaseFileExists creates the database file at path if it does not
// already exist, including its parent directory. The file is created the same
// way SQLite would create it — following a symlink at the leaf to its target —
// so the inode os.Stat reports is the inode SQLite will subsequently open. An
// empty file is a valid empty SQLite database, so creating it ahead of SQLite
// does not change what SQLite opens.
func ensureDatabaseFileExists(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	parent := filepath.Dir(path)
	if parent != "." && parent != "" {
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return err
		}
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	return file.Close()
}

type daemonLock struct {
	path string
	file *os.File
}

// acquireDaemonLock opens the lock file at path, takes an exclusive flock, and
// writes holder metadata. The lock directory is private (0700, daemon-owned,
// see databaseAttachLockPath) so another user cannot plant a symlink at the
// lock pathname. As defense in depth the file is opened with O_NOFOLLOW so a
// symlink leaf is rejected rather than followed, and after opening the file is
// verified to be a regular file owned by the daemon user before it is
// truncated — refusing to truncate a file the daemon does not own or that is
// not a regular file.
func acquireDaemonLock(path, daemonID string, now time.Time) (*daemonLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o644)
	if err != nil {
		return nil, err
	}
	if err := verifyLockFile(file); err != nil {
		_ = file.Close()
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

// verifyLockFile confirms the open lock file is a regular file owned by the
// daemon user, so a planted symlink or a file owned by another user is never
// truncated. O_NOFOLLOW already rejects a symlink leaf at open time; this also
// covers the case where the lock file was replaced between open and use, and
// the case of a non-regular file (e.g. a device node) a 0700 directory does
// not by itself rule out for the daemon user.
func verifyLockFile(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Mode().Type() != 0 {
		return fmt.Errorf("lock file %s is not a regular file (mode %s)", file.Name(), info.Mode().String())
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	if stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("lock file %s is owned by uid %d, not the daemon uid %d", file.Name(), stat.Uid, os.Getuid())
	}
	return nil
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
