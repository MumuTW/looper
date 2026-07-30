package storage

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type DatabaseLockMode int

const (
	DatabaseLockShared DatabaseLockMode = iota
	DatabaseLockExclusive
)

// DatabaseLock fences schema compatibility reads from migration writes for one
// canonical SQLite file. Shared holders may read a compatible schema together;
// a migration requires exclusive ownership.
type DatabaseLock struct {
	file *os.File
}

// AcquireDatabaseLock takes a non-blocking advisory lock for dbPath. It
// resolves existing file and directory symlinks before deriving the lock path,
// so aliases of one database contend while separate files in one directory do
// not. In-memory SQLite databases are process-local and need no file lock.
func AcquireDatabaseLock(dbPath string, mode DatabaseLockMode) (*DatabaseLock, error) {
	path, ok, err := databaseLockPath(dbPath)
	if err != nil || !ok {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open database compatibility lock: %w", err)
	}
	operation := syscall.LOCK_SH
	if mode == DatabaseLockExclusive {
		operation = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(file.Fd()), operation|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("database compatibility lock is held for %s: %w", path, err)
	}
	return &DatabaseLock{file: file}, nil
}

func (l *DatabaseLock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	err := l.file.Close()
	l.file = nil
	return err
}

func databaseLockPath(dbPath string) (string, bool, error) {
	canonical, ok, err := canonicalDatabasePath(dbPath)
	if err != nil || !ok {
		return "", ok, err
	}
	return canonical + ".lock", true, nil
}

func canonicalDatabasePath(dbPath string) (string, bool, error) {
	dbPath, isFile, err := SQLiteFilesystemPath(dbPath)
	if err != nil || !isFile {
		return "", isFile, err
	}
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", false, fmt.Errorf("make SQLite database path absolute: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(absPath); err == nil {
		return resolved, true, nil
	} else if !os.IsNotExist(err) {
		return "", false, fmt.Errorf("resolve SQLite database path: %w", err)
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absPath))
	if err != nil && !os.IsNotExist(err) {
		return "", false, fmt.Errorf("resolve SQLite database directory: %w", err)
	}
	if err == nil {
		absPath = filepath.Join(parent, filepath.Base(absPath))
	}
	return absPath, true, nil
}

// SQLiteFilesystemPath returns the underlying filesystem path for an on-disk
// SQLite configuration value. It recognizes the file: URI form accepted by
// sqliteDSN, while memory databases have no path to stat or lock.
func SQLiteFilesystemPath(dbPath string) (string, bool, error) {
	dbPath = strings.TrimSpace(dbPath)
	if dbPath == "" || dbPath == ":memory:" {
		return "", false, nil
	}
	if strings.HasPrefix(dbPath, "file:") {
		parsed, err := url.Parse(dbPath)
		if err != nil {
			return "", false, fmt.Errorf("parse SQLite file URI: %w", err)
		}
		if strings.EqualFold(parsed.Query().Get("mode"), "memory") {
			return "", false, nil
		}
		if parsed.Path == "" {
			return "", false, fmt.Errorf("SQLite file URI has no filesystem path")
		}
		dbPath = parsed.Path
	}
	return dbPath, true, nil
}
