package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/nexu-io/looper/internal/config"
	"github.com/nexu-io/looper/internal/storage"
)

func mkfifo(path string) error {
	return syscall.Mkfifo(path, 0o644)
}

// withPrivateLooperHome points LOOPER_HOME at a per-test directory so the
// attach lock's private directory (~/.looper/locks) is created under the test
// scratch space rather than the operator's real home. These tests cannot be
// t.Parallel because t.Setenv is incompatible with parallel tests.
func withPrivateLooperHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("LOOPER_HOME", home)
	return home
}

// mustAttachLockPath computes the attach lock path for dbPath, failing the test
// on the new error return (filesystem identity derivation can fail if the
// database file cannot be created/stated).
func mustAttachLockPath(t *testing.T, dbPath string) string {
	t.Helper()
	p, err := databaseAttachLockPath(dbPath)
	if err != nil {
		t.Fatalf("databaseAttachLockPath(%q) error = %v", dbPath, err)
	}
	return p
}

// TestDatabaseAttachLockPathDerivedPerDatabase covers the attach-lock naming
// contract: two independent daemons using different databases must get distinct
// lock files so the second is not rejected for racing a schema it cannot reach.
// The identity is the database file's filesystem identity (device + inode), so
// different files produce different lock filenames while the lock directory is
// shared and private.
func TestDatabaseAttachLockPathDerivedPerDatabase(t *testing.T) {
	withPrivateLooperHome(t)

	dir := t.TempDir()
	pathA := mustAttachLockPath(t, filepath.Join(dir, "a.sqlite"))
	pathB := mustAttachLockPath(t, filepath.Join(dir, "b.sqlite"))
	if pathA == pathB {
		t.Fatalf("attach lock paths for different databases collide: %q == %q", pathA, pathB)
	}
	// Both locks live in the same private lock directory; only the identity
	// hash in the filename differs.
	if filepath.Dir(pathA) != filepath.Dir(pathB) {
		t.Fatalf("attach locks for different databases live in different directories %q != %q; they should share one private lock dir", filepath.Dir(pathA), filepath.Dir(pathB))
	}
	// Same database must resolve to the same lock path deterministically.
	if mustAttachLockPath(t, filepath.Join(dir, "a.sqlite")) != pathA {
		t.Fatal("attach lock path for the same database is not deterministic")
	}
	// The webhook forwarder lock is intentionally still keyed by the configured
	// database's directory (it predates this change and has rollout-stable
	// naming); it must collide for the two databases, confirming the attach
	// lock is the one that diverges.
	if webhookForwarderLockPath(filepath.Join(dir, "a.sqlite")) != webhookForwarderLockPath(filepath.Join(dir, "b.sqlite")) {
		t.Fatal("webhook forwarder lock path diverged for different databases; it is intentionally directory-keyed")
	}
}

// TestDatabaseAttachLockPathCollapsesAliases covers the identity-collapse
// contract: two spellings of one physical database must produce one lock path.
// The identity is the file's inode, so symlinks, hard links, and (on
// case-insensitive volumes) differently cased names of one file all collapse.
// A lexical or EvalSymlinks-based identity fails this for hard links and
// case-insensitive aliases, so two daemons would take different locks against
// the same file and both migrate it.
func TestDatabaseAttachLockPathCollapsesAliases(t *testing.T) {
	withPrivateLooperHome(t)

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", realDir, err)
	}

	t.Run("symlinked parent directory", func(t *testing.T) {
		linkDir := filepath.Join(root, "link-dir")
		if err := os.Symlink(realDir, linkDir); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		// The database file deliberately does not exist yet: the attach lock is
		// taken before the database is created on a first boot, so the file is
		// created and the inode is stable from the existing parent.
		viaReal := mustAttachLockPath(t, filepath.Join(realDir, "looper.sqlite"))
		viaLink := mustAttachLockPath(t, filepath.Join(linkDir, "looper.sqlite"))
		if viaReal != viaLink {
			t.Fatalf("aliased parent directories produced different attach locks:\n real: %q\n link: %q", viaReal, viaLink)
		}
	})

	t.Run("symlinked database file", func(t *testing.T) {
		realDB := filepath.Join(realDir, "existing.sqlite")
		if err := os.WriteFile(realDB, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", realDB, err)
		}
		linkDB := filepath.Join(root, "link-db.sqlite")
		if err := os.Symlink(realDB, linkDB); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		viaReal := mustAttachLockPath(t, realDB)
		viaLink := mustAttachLockPath(t, linkDB)
		if viaReal != viaLink {
			t.Fatalf("aliased database files produced different attach locks:\n real: %q\n link: %q", viaReal, viaLink)
		}
	})

	t.Run("hard-linked database file", func(t *testing.T) {
		// filepath.EvalSymlinks does not collapse hard links: two names for one
		// inode resolve to different strings. The inode-based identity collapses
		// them, so two daemons opening the same file through two hard-link names
		// take one lock instead of both migrating it.
		realDB := filepath.Join(realDir, "hardlinked.sqlite")
		if err := os.WriteFile(realDB, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", realDB, err)
		}
		linkDB := filepath.Join(root, "hardlink.sqlite")
		if err := os.Link(realDB, linkDB); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		viaReal := mustAttachLockPath(t, realDB)
		viaLink := mustAttachLockPath(t, linkDB)
		if viaReal != viaLink {
			t.Fatalf("hard-linked database files produced different attach locks:\n real: %q\n link: %q", viaReal, viaLink)
		}
	})

	t.Run("identity and lock directory agree for an empty DBPath", func(t *testing.T) {
		// An empty DBPath defaults to <looperHome>/looper.sqlite, and the lock
		// lives in <looperHome>/locks. The identity is the inode of the
		// defaulted database file, so the lock filename carries that identity.
		lockPath := mustAttachLockPath(t, "")
		if got, want := filepath.Dir(lockPath), filepath.Join(os.Getenv("LOOPER_HOME"), "locks"); got != want {
			t.Fatalf("attach lock directory %q, want %q", got, want)
		}
	})
}

// TestDatabaseAttachLockPathStableAcrossDanglingSymlinkFirstBoot covers the
// dangling-symlink first-boot race: when storage.dbPath is a symlink whose
// target does not exist yet, the first daemon creates the target (the way
// SQLite would) and stats it for its inode. Once the target exists, a second
// daemon with the same config stats the same inode, so both derive the same
// lock. The inode identity is stable across the first-boot boundary by
// construction, removing the separate dangling-symlink resolution layer the
// prior spelling-based identity required.
func TestDatabaseAttachLockPathStableAcrossDanglingSymlinkFirstBoot(t *testing.T) {
	withPrivateLooperHome(t)

	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", realDir, err)
	}
	realDB := filepath.Join(realDir, "created-later.sqlite")
	linkDB := filepath.Join(root, "link.sqlite")
	// The target deliberately does NOT exist yet: this is the first-boot case
	// where the lock is taken before SQLite creates the database file.
	if err := os.Symlink(realDB, linkDB); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	beforeTargetExists := mustAttachLockPath(t, linkDB)

	// Now SQLite creates the target, as a second daemon would observe. The
	// inode is the same because the first daemon's ensureDatabaseFileExists
	// already created the target through the symlink.
	if err := os.WriteFile(realDB, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", realDB, err)
	}
	afterTargetExists := mustAttachLockPath(t, linkDB)

	if beforeTargetExists != afterTargetExists {
		t.Fatalf("dangling-symlink lock identity changed once the target appeared:\n before: %q\n after:  %q", beforeTargetExists, afterTargetExists)
	}
	// And it must equal the lock derived from the real path directly, proving
	// the identity is the physical target rather than the alias spelling.
	if got, want := beforeTargetExists, mustAttachLockPath(t, realDB); got != want {
		t.Fatalf("dangling-symlink lock %q != real-path lock %q", got, want)
	}
}

// TestDatabaseAttachLockPathCollapsesFileURIVariants covers file: URI handling:
// OpenSQLiteDB explicitly supports the file: form, but treating the URI as an
// ordinary filesystem name makes /tmp/shared.sqlite and file:/tmp/shared.sqlite
// (which open the same database) produce unrelated lock identities. The URI is
// parsed to its path component, both spellings create and stat the same file,
// and the inode collapses them. Query variants that reference the same file
// likewise collapse.
func TestDatabaseAttachLockPathCollapsesFileURIVariants(t *testing.T) {
	withPrivateLooperHome(t)

	dir := t.TempDir()
	bare := filepath.Join(dir, "shared.sqlite")
	uri := "file:" + bare
	uriQuery := "file:" + bare + "?cache=shared"

	bareLock := mustAttachLockPath(t, bare)
	uriLock := mustAttachLockPath(t, uri)
	uriQueryLock := mustAttachLockPath(t, uriQuery)

	if bareLock != uriLock {
		t.Fatalf("bare path and file: URI produced different attach locks:\n bare: %q\n uri:  %q", bareLock, uriLock)
	}
	if bareLock != uriQueryLock {
		t.Fatalf("bare path and file: URI with query produced different attach locks:\n bare: %q\n uriq: %q", bareLock, uriQueryLock)
	}
}

// TestDatabaseAttachLockLivesInPrivateDirectory covers the symlink-attack
// mitigation: the attach lock lives in a private, daemon-owned directory under
// the looper state directory (mode 0700), NOT beside the database file. A
// database directory such as /tmp may be writable by another user, who could
// pre-create a deterministic lock pathname beside the database as a symlink to
// a file the daemon can write; the prior beside-database derivation followed
// that symlink and truncated the target. A private 0700 directory denies any
// other user the ability to create entries in it.
func TestDatabaseAttachLockLivesInPrivateDirectory(t *testing.T) {
	withPrivateLooperHome(t)

	dir := t.TempDir()
	lockPath := mustAttachLockPath(t, filepath.Join(dir, "looper.sqlite"))

	// The lock must NOT live beside the database (the attackable location).
	if filepath.Dir(lockPath) == dir {
		t.Fatalf("attach lock lives beside the database in %q; it must live in a private directory", dir)
	}
	// It must live in the private lock directory under the looper state dir.
	wantDir := filepath.Join(os.Getenv("LOOPER_HOME"), "locks")
	if filepath.Dir(lockPath) != wantDir {
		t.Fatalf("attach lock directory %q, want private %q", filepath.Dir(lockPath), wantDir)
	}
	// The private directory must be 0700 so only the daemon user can create
	// entries (and therefore plant no symlink for the daemon to follow).
	info, err := os.Stat(wantDir)
	if err != nil {
		t.Fatalf("Stat(%q) error = %v", wantDir, err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("private lock directory perm %o, want 0700", got)
	}
}

// TestAcquireDaemonLockRefusesSymlinkAtLockPath covers the defense-in-depth
// symlink refusal: even if a symlink were planted at the lock pathname,
// acquireDaemonLock opens it with O_NOFOLLOW and refuses to follow it, so the
// target file is never truncated. The lock directory is private (0700) so this
// should not be reachable in practice, but the open-time guard is the last line
// against a planted symlink.
func TestAcquireDaemonLockRefusesSymlinkAtLockPath(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("precious"), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", target, err)
	}
	lockPath := filepath.Join(dir, "looperd.attach.symlink.lock")
	if err := os.Symlink(target, lockPath); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := acquireDaemonLock(lockPath, "daemon", time.Now()); err == nil {
		// If acquisition wrongly succeeded, the target must not have been
		// truncated — verify the guard held.
		got, readErr := os.ReadFile(target)
		if readErr != nil {
			t.Fatalf("acquireDaemonLock followed the symlink and the target is unreadable: %v", readErr)
		}
		t.Fatalf("acquireDaemonLock(%q) error = nil, want refusal; target contents = %q", lockPath, string(got))
	}

	// The target must be untouched.
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q) error = %v", target, err)
	}
	if string(got) != "precious" {
		t.Fatalf("lock symlink target was truncated: got %q, want %q", string(got), "precious")
	}
}

// TestAcquireDaemonLockRefusesNonRegularFile covers the regular-file
// verification: a non-regular file (e.g. a FIFO) at the lock path is rejected
// rather than truncated, so the daemon never writes holder metadata into a
// non-regular file.
func TestAcquireDaemonLockRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "looperd.attach.fifo.lock")
	// Create a FIFO at the lock path. Opening a FIFO with O_RDWR blocks until
	// a peer opens it, so open it non-blockingly via a pipe created here.
	if err := mkfifo(lockPath); err != nil {
		t.Skipf("FIFOs unavailable: %v", err)
	}
	if _, err := acquireDaemonLock(lockPath, "daemon", time.Now()); err == nil {
		t.Fatalf("acquireDaemonLock(%q) on a FIFO error = nil, want refusal", lockPath)
	}
}

// TestWebhookForwarderLockPathStaysBesideConfiguredSymlink covers the
// rollout-stability requirement for the legacy webhook forwarder lock: when the
// configured database is a symlink whose target lives in another directory, a
// pre-change webhook-enabled daemon opens looperd.lock beside the configured
// symlink spelling. The new daemon must derive the very same path (lexical
// filepath.Abs only, no symlink canonicalization) so the two contend on one
// lock during a rolling upgrade instead of each acquiring their own and running
// competing forwarders. The attach lock, by contrast, is keyed by filesystem
// identity and lives in a private directory, so the two derivations are
// deliberately different.
func TestWebhookForwarderLockPathStaysBesideConfiguredSymlink(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", realDir, err)
	}
	realDB := filepath.Join(realDir, "looper.sqlite")
	if err := os.WriteFile(realDB, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", realDB, err)
	}
	linkDir := filepath.Join(root, "link-dir")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	configuredDB := filepath.Join(linkDir, "looper.sqlite")

	// The webhook lock stays beside the configured (symlinked) spelling.
	webhookLock := webhookForwarderLockPath(configuredDB)
	if filepath.Dir(webhookLock) != linkDir {
		t.Fatalf("webhook lock lives beside resolved target %q, want beside configured symlink %q", filepath.Dir(webhookLock), linkDir)
	}
	// It must NOT migrate to the resolved target's directory, which is what a
	// canonicalizing derivation would do and what breaks the rolling upgrade.
	if filepath.Dir(webhookLock) == realDir {
		t.Fatalf("webhook lock migrated to resolved target dir %q; it must stay rollout-stable beside the configured spelling", realDir)
	}
}

// TestRuntimeFailedStartClosesConnectionBeforeReleasingAttachLock covers the
// failed-start cleanup invariant (AGENTS.md review item): when a startup step
// after opening the coordinator but before publishing resources fails — here,
// ValidateCompatibility rejecting an unknown applied migration — the deferred
// cleanup must close the SQLite connection before releasing the attach lock.
// Releasing the lock first would let a concurrent startup retry acquire it and
// begin migration while the failed instance still owns its SQLite connection,
// violating the database-lifetime invariant the lock exists to enforce.
//
// The test exercises the failed-start lifecycle path and asserts the invariant
// holds at the point a retry can proceed: after the failed Start returns, the
// captured coordinator's *sql.DB is closed (Ping fails with "database is
// closed"), and a second daemon with a manifest that recognizes the seeded
// migration boots cleanly — which requires acquiring the attach lock the first
// daemon released and opening SQLite against the connection the first daemon
// closed. This catches regressions that drop either cleanup operation (a
// missing coordinator.Close leaks the connection; a missing attachLock.Release
// wedges the retry). The close-before-release ordering itself is enforced by
// the deferred cleanup sequence in start(); observing the intra-defer window
// directly is not feasible without a production Close seam, so the test pins
// the end-state invariant the ordering guarantees.
func TestRuntimeFailedStartClosesConnectionBeforeReleasingAttachLock(t *testing.T) {
	withPrivateLooperHome(t)

	workingDir := t.TempDir()
	dbPath := filepath.Join(workingDir, "shared.sqlite")
	backupDir := filepath.Join(workingDir, "backups")

	const unknownID = "9999_unknown_future"
	extraMigration := storage.EmbeddedMigration{
		ID:       unknownID,
		FileName: unknownID + ".sql",
		SQL:      "CREATE TABLE unknown_future_marker (id TEXT PRIMARY KEY);",
	}
	// Pre-seed the database with an applied migration the current binary does
	// not know, so daemonA's ValidateCompatibility fails after the coordinator
	// opens. Only the ledger row is seeded (not the table), so daemonB's
	// RunPending sees the migration as applied and skips it.
	seedDB, err := storage.OpenSQLiteDB(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("seed OpenSQLiteDB(%q) error = %v", dbPath, err)
	}
	if _, err := seedDB.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (id TEXT PRIMARY KEY, applied_at TEXT NOT NULL);`); err != nil {
		t.Fatalf("seed CREATE schema_migrations error = %v", err)
	}
	if _, err := seedDB.Exec(`INSERT INTO schema_migrations (id, applied_at) VALUES (?, ?);`, unknownID, "2026-07-30T00:00:00Z"); err != nil {
		t.Fatalf("seed INSERT schema_migrations error = %v", err)
	}
	if err := seedDB.Close(); err != nil {
		t.Fatalf("seed Close error = %v", err)
	}

	baseConfig := func() (config.Config, error) {
		cfg, err := config.DefaultConfig(workingDir)
		if err != nil {
			return config.Config{}, err
		}
		cfg.Storage.DBPath = dbPath
		cfg.Storage.BackupDir = &backupDir
		cfg.Package.AutoMigrateOnStartup = true
		return cfg, nil
	}

	// daemonA uses the current manifest (no extra migration), so
	// ValidateCompatibility rejects the seeded unknown migration. The hook
	// captures the coordinator's *sql.DB so the test can assert it is closed
	// after the failed start.
	var capturedDB interface {
		PingContext(context.Context) error
	}
	cfgA, err := baseConfig()
	if err != nil {
		t.Fatalf("baseConfig() A error = %v", err)
	}
	daemonA := New(Options{
		Config:           cfgA,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
		OpenSQLiteCoordinator: func(ctx context.Context, p string, options storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error) {
			coordinator, openErr := storage.OpenSQLiteCoordinator(ctx, p, options)
			if openErr == nil {
				capturedDB = coordinator.DB()
			}
			return coordinator, openErr
		},
	})
	startErr := daemonA.Start(context.Background())
	if startErr == nil {
		daemonA.Stop("test cleanup")
		t.Fatal("daemonA.Start() with an unknown applied migration = nil, want ValidateCompatibility failure")
	}
	if !strings.Contains(startErr.Error(), "unknown") {
		t.Fatalf("daemonA.Start() error = %q, want it to reference unknown migrations", startErr.Error())
	}

	// The captured coordinator connection must be closed: the deferred cleanup
	// ran coordinator.Close() before the attach lock was released.
	if capturedDB == nil {
		t.Fatal("OpenSQLiteCoordinator hook did not capture the coordinator DB")
	}
	if pingErr := capturedDB.PingContext(context.Background()); pingErr == nil {
		t.Fatal("captured coordinator DB Ping = nil after failed start, want it closed before the attach lock was released")
	}

	// A concurrent startup retry must be able to boot and migrate: it acquires
	// the attach lock daemonA released and opens SQLite against the connection
	// daemonA closed. daemonB's manifest recognizes the seeded migration.
	cfgB, err := baseConfig()
	if err != nil {
		t.Fatalf("baseConfig() B error = %v", err)
	}
	daemonB := New(Options{
		Config:           cfgB,
		Logger:           &testLogger{},
		RunSchedulerTick: func(context.Context, Services) error { return nil },
		OpenSQLiteCoordinator: func(ctx context.Context, p string, options storage.SQLiteCoordinatorOptions) (*storage.SQLiteCoordinator, error) {
			options.Migrations = append(append([]storage.EmbeddedMigration{}, storage.EmbeddedMigrations...), extraMigration)
			return storage.OpenSQLiteCoordinator(ctx, p, options)
		},
	})
	if err := daemonB.Start(context.Background()); err != nil {
		t.Fatalf("daemonB.Start() after daemonA's failed start = %v, want boot to succeed (attach lock released and connection closed)", err)
	}
	t.Cleanup(func() { daemonB.Stop("test cleanup") })
}
