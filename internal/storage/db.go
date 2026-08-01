package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	sqliteBusyTimeoutMilliseconds = 5000
	sqliteMaxOpenConnections      = 4
)

type SQLiteCoordinatorOptions struct {
	Migrations []EmbeddedMigration
	BackupDir  string
	Now        func() time.Time
}

type SQLiteCoordinator struct {
	db *sql.DB
	// path is the absolute filesystem path frozen at open (symlink parents
	// resolved once). Upgrade backup records this as Source.DatabasePath so a
	// later retarget of a path component cannot rename the restore destination
	// away from the open inode.
	path   string
	runner *MigrationRunner
}

type txBeginner interface {
	BeginTx(context.Context, *sql.TxOptions) (*sql.Tx, error)
}

func OpenSQLiteCoordinator(ctx context.Context, dbPath string, options SQLiteCoordinatorOptions) (*SQLiteCoordinator, error) {
	// Freeze restore metadata path before open so a parent symlink cannot be
	// retargeted between open/ping and path resolution.
	openedPath, err := resolveOpenedDatabasePath(dbPath)
	if err != nil {
		return nil, err
	}
	// Open with frozen filesystem path but preserve original file: URI query
	// options (cache, mode, _busy_timeout, …).
	openPath := openPathWithPreservedURIOptions(dbPath, openedPath)
	db, err := OpenSQLiteDB(ctx, openPath)
	if err != nil {
		return nil, err
	}

	coordinator := &SQLiteCoordinator{
		db:   db,
		path: openedPath,
		runner: NewMigrationRunner(db, MigrationRunnerOptions{
			Migrations: options.Migrations,
			BackupDir:  options.BackupDir,
			Now:        options.Now,
		}),
	}

	return coordinator, nil
}

// DatabasePath returns the absolute filesystem path bound when the coordinator
// opened SQLite. Empty for non-filesystem databases.
func (c *SQLiteCoordinator) DatabasePath() string {
	if c == nil {
		return ""
	}
	return c.path
}

// openPathWithPreservedURIOptions returns the DSN used to open SQLite: if
// original was a file: URI, rebuild it with the frozen filesystem path and the
// original query string; otherwise use the frozen path (or original if empty).
func openPathWithPreservedURIOptions(original, frozenFS string) string {
	if strings.TrimSpace(frozenFS) == "" {
		return original
	}
	trimmed := strings.TrimSpace(original)
	if len(trimmed) < 5 || !strings.EqualFold(trimmed[:5], "file:") {
		return frozenFS
	}
	rest := trimmed[5:]
	query := ""
	if i := strings.Index(rest, "?"); i >= 0 {
		query = rest[i:]
	}
	return "file:" + filepath.ToSlash(frozenFS) + query
}

// resolveOpenedDatabasePath freezes storage.dbPath for restore metadata: file:
// URIs are unwrapped, the path is made absolute, and existing symlink parents
// are resolved once at open so later retargets do not rename the open database.
func resolveOpenedDatabasePath(dbPath string) (string, error) {
	path, isFile, err := SQLiteFilesystemPath(dbPath)
	if err != nil {
		return "", err
	}
	if !isFile || strings.TrimSpace(path) == "" {
		return "", nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve database path: %w", err)
	}
	abs = filepath.Clean(abs)
	if info, err := os.Lstat(abs); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("database path %s is a leaf symlink; open the real file path so restore metadata cannot diverge", abs)
		}
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			return filepath.Clean(resolved), nil
		}
		return abs, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat database path: %w", err)
	}
	// Leaf does not exist yet (first start). Resolve existing parents so a
	// retarget of e.g. .../current/ cannot rename the eventual open inode.
	parent := filepath.Dir(abs)
	base := filepath.Base(abs)
	if parent == "" || parent == "." {
		return abs, nil
	}
	if resolvedParent, err := filepath.EvalSymlinks(parent); err == nil {
		return filepath.Join(filepath.Clean(resolvedParent), base), nil
	}
	return abs, nil
}

func OpenSQLiteDB(ctx context.Context, dbPath string) (*sql.DB, error) {
	if err := ensureSQLiteParentDir(dbPath); err != nil {
		return nil, err
	}

	db, err := sql.Open(DriverName, sqliteDSN(dbPath))
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	maxConns := sqliteMaxOpenConnections
	if dbPath == ":memory:" {
		maxConns = 1
	}
	db.SetMaxOpenConns(maxConns)
	db.SetMaxIdleConns(maxConns)

	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping sqlite database: %w", err)
	}

	if err := applySQLitePragmas(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}

	return db, nil
}

// CompatibleSQLiteDB keeps the shared migration fence until its caller has
// finished all compatibility-dependent authority reads.
type CompatibleSQLiteDB struct {
	DB   *sql.DB
	lock *DatabaseLock
}

func (db *CompatibleSQLiteDB) Close() error {
	if db == nil {
		return nil
	}
	var closeErr error
	if db.DB != nil {
		closeErr = db.DB.Close()
	}
	if lockErr := db.lock.Release(); lockErr != nil {
		return errors.Join(closeErr, lockErr)
	}
	return closeErr
}

// OpenSQLiteDBWithCompatibilityCheck opens the SQLite database at dbPath and
// validates that its schema_migrations ledger contains no migration IDs
// unknown to this binary's manifest. It is the shared database boundary for
// supported CLI consumers — `looper review submit` and its authority lookups —
// that open the daemon's database directly without a coordinator, so a
// mixed-version CLI cannot read migration-sensitive authority state or publish
// a review against a schema created or migrated by a newer looper binary.
//
// The validation reuses MigrationRunner.ValidateCompatibility and is read-only:
// a database without a schema_migrations table (a freshly created file, or one
// that has never been migrated) is treated as compatible, so this never
// performs DDL and never blocks a CLI probing an empty or optional database.
// The handle is closed before returning on a compatibility failure so the
// caller never receives a connection to a database it cannot prove it
// understands. The authority is the running binary's EmbeddedMigrations
// manifest, not agent output or infra inference.
func OpenSQLiteDBWithCompatibilityCheck(ctx context.Context, dbPath string) (*CompatibleSQLiteDB, error) {
	lock, err := AcquireDatabaseLock(dbPath, DatabaseLockShared)
	if err != nil {
		return nil, err
	}
	db, err := OpenSQLiteDB(ctx, dbPath)
	if err != nil {
		_ = lock.Release()
		return nil, err
	}
	if err := NewMigrationRunner(db, MigrationRunnerOptions{}).ValidateCompatibility(ctx); err != nil {
		_ = db.Close()
		_ = lock.Release()
		return nil, err
	}
	return &CompatibleSQLiteDB{DB: db, lock: lock}, nil
}

func (c *SQLiteCoordinator) DB() *sql.DB {
	return c.db
}

func (c *SQLiteCoordinator) MigrationRunner() *MigrationRunner {
	return c.runner
}

func (c *SQLiteCoordinator) Backup(ctx context.Context) (string, error) {
	if c == nil || c.runner == nil {
		return "", fmt.Errorf("sqlite coordinator is not initialized")
	}

	return c.runner.Backup(ctx)
}

func (c *SQLiteCoordinator) Close() error {
	if c == nil || c.db == nil {
		return nil
	}

	return c.db.Close()
}

func (c *SQLiteCoordinator) WithTransaction(ctx context.Context, fn func(*sql.Tx) error) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("sqlite coordinator is not initialized")
	}

	return WithTransaction(ctx, c.db, nil, fn)
}

func WithTransaction(ctx context.Context, db txBeginner, options *sql.TxOptions, fn func(*sql.Tx) error) (err error) {
	if db == nil {
		return fmt.Errorf("transaction starter is nil")
	}

	if fn == nil {
		return fmt.Errorf("transaction callback is nil")
	}

	tx, err := db.BeginTx(ctx, options)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			_ = tx.Rollback()
			panic(recovered)
		}

		if err != nil {
			_ = tx.Rollback()
			return
		}

		if commitErr := tx.Commit(); commitErr != nil {
			err = fmt.Errorf("commit transaction: %w", commitErr)
		}
	}()

	err = fn(tx)
	return err
}

func WithTransactionValue[T any](ctx context.Context, db txBeginner, options *sql.TxOptions, fn func(*sql.Tx) (T, error)) (result T, err error) {
	if fn == nil {
		return result, fmt.Errorf("transaction callback is nil")
	}

	err = WithTransaction(ctx, db, options, func(tx *sql.Tx) error {
		var runErr error
		result, runErr = fn(tx)
		return runErr
	})

	return result, err
}

func ensureSQLiteParentDir(dbPath string) error {
	if dbPath == "" || dbPath == ":memory:" || strings.HasPrefix(dbPath, "file:") {
		return nil
	}

	parentDir := filepath.Dir(dbPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return fmt.Errorf("create sqlite parent directory: %w", err)
	}

	return nil
}

func sqliteDSN(dbPath string) string {
	params := map[string]string{
		"_foreign_keys": "on",
		"_busy_timeout": fmt.Sprintf("%d", sqliteBusyTimeoutMilliseconds),
		"_journal_mode": "WAL",
		"_txlock":       "immediate",
	}
	if dbPath == ":memory:" {
		values := url.Values{}
		values.Set("mode", "memory")
		for key, value := range params {
			values.Set(key, value)
		}
		return "file::memory:?" + values.Encode()
	}

	if strings.HasPrefix(dbPath, "file:") {
		parsed, err := url.Parse(dbPath)
		if err != nil {
			return dbPath
		}

		query := parsed.Query()
		for key, value := range params {
			query.Set(key, value)
		}
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}

	values := url.Values{}
	for key, value := range params {
		values.Set(key, value)
	}
	return dbPath + "?" + values.Encode()
}

func applySQLitePragmas(ctx context.Context, db *sql.DB) error {
	if err := execPragma(ctx, db, `PRAGMA journal_mode = WAL;`); err != nil {
		return fmt.Errorf("set sqlite pragma journal_mode=WAL: %w", err)
	}

	if err := execPragma(ctx, db, `PRAGMA foreign_keys = ON;`); err != nil {
		return fmt.Errorf("set sqlite pragma foreign_keys=ON: %w", err)
	}

	if err := execPragma(ctx, db, fmt.Sprintf(`PRAGMA busy_timeout = %d;`, sqliteBusyTimeoutMilliseconds)); err != nil {
		return fmt.Errorf("set sqlite pragma busy_timeout=%d: %w", sqliteBusyTimeoutMilliseconds, err)
	}

	return nil
}

func execPragma(ctx context.Context, db *sql.DB, statement string) error {
	_, err := db.ExecContext(ctx, statement)
	return err
}
