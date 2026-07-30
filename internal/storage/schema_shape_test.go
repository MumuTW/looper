package storage

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func migratedTestDB(t *testing.T) *SQLiteCoordinator {
	t.Helper()
	ctx := context.Background()
	coordinator, err := OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "shape.sqlite"), SQLiteCoordinatorOptions{Migrations: EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err := coordinator.MigrationRunner().RunPending(ctx); err != nil {
		t.Fatalf("RunPending() error = %v", err)
	}
	return coordinator
}

// Contract: the written-down shape is the migrated shape. A migration that adds
// a column without updating scannedTableColumns fails here, at the commit that
// introduces the drift, rather than in production against an older binary.
func TestScannedTableColumnsMatchMigratedSchema(t *testing.T) {
	ctx := context.Background()
	db := migratedTestDB(t).DB()

	for table, expected := range scannedTableColumns {
		actual, err := tableColumns(ctx, db, table)
		if err != nil {
			t.Fatalf("tableColumns(%s) error = %v", table, err)
		}
		if len(actual) == 0 {
			t.Fatalf("table %s does not exist in the migrated schema", table)
		}
		if len(actual) != len(expected) {
			t.Fatalf("%s columns = %v, want %v", table, actual, expected)
		}
		for i := range actual {
			if actual[i] != expected[i] {
				t.Fatalf("%s column %d = %q, want %q (full: %v)", table, i, actual[i], expected[i], actual)
			}
		}
	}
}

func TestValidateSchemaShapeAcceptsTheMigratedSchema(t *testing.T) {
	if err := ValidateSchemaShape(context.Background(), migratedTestDB(t).DB()); err != nil {
		t.Fatalf("ValidateSchemaShape() error = %v, want nil for the schema this build was compiled against", err)
	}
}

// Contract: a column this build does not know is refused at boot, named.
//
// This is the #188 incident in one assertion: `runs` gained a fifteenth column
// while the binary scanned fourteen, and the only symptom was a repeated
// `sql: expected 15 destination arguments in Scan, not 14` from an arbitrary
// call site, once per launchd restart.
func TestValidateSchemaShapeRejectsAnUnknownColumn(t *testing.T) {
	ctx := context.Background()
	db := migratedTestDB(t).DB()
	if _, err := db.ExecContext(ctx, `ALTER TABLE runs ADD COLUMN future_field TEXT`); err != nil {
		t.Fatalf("add drift column: %v", err)
	}

	err := ValidateSchemaShape(ctx, db)
	if err == nil {
		t.Fatal("ValidateSchemaShape() = nil, want a refusal naming the unknown column")
	}
	for _, want := range []string{"runs", "future_field", "unknown column"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// Contract: a column this build scans and the database lacks is equally fatal,
// and equally named — the same mismatch read from the other side.
func TestValidateSchemaShapeRejectsAMissingColumn(t *testing.T) {
	ctx := context.Background()
	db := migratedTestDB(t).DB()
	if _, err := db.ExecContext(ctx, `ALTER TABLE runs DROP COLUMN summary`); err != nil {
		t.Fatalf("drop column: %v", err)
	}

	err := ValidateSchemaShape(ctx, db)
	if err == nil {
		t.Fatal("ValidateSchemaShape() = nil, want a refusal naming the missing column")
	}
	for _, want := range []string{"runs", "summary", "missing column"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to contain %q", err, want)
		}
	}
}

// Contract: an empty database is the migration runner's business, not a shape
// violation. Reporting every table as missing when migration is simply pending
// would make the check fire on the one case it has nothing to say about.
func TestValidateSchemaShapeIgnoresTablesThatDoNotExistYet(t *testing.T) {
	ctx := context.Background()
	coordinator, err := OpenSQLiteCoordinator(ctx, filepath.Join(t.TempDir(), "empty.sqlite"), SQLiteCoordinatorOptions{Migrations: EmbeddedMigrations})
	if err != nil {
		t.Fatalf("OpenSQLiteCoordinator() error = %v", err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })

	if err := ValidateSchemaShape(ctx, coordinator.DB()); err != nil {
		t.Fatalf("ValidateSchemaShape() error = %v, want nil before migrations have run", err)
	}
}

// Contract: every mismatched table is reported in one message. An operator
// restoring a database should not have to fix one table, restart, and discover
// the next.
func TestValidateSchemaShapeReportsEveryMismatchedTable(t *testing.T) {
	ctx := context.Background()
	db := migratedTestDB(t).DB()
	for _, statement := range []string{
		`ALTER TABLE runs ADD COLUMN future_field TEXT`,
		`ALTER TABLE loops ADD COLUMN other_future_field TEXT`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}

	err := ValidateSchemaShape(ctx, db)
	if err == nil {
		t.Fatal("ValidateSchemaShape() = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "runs") || !strings.Contains(err.Error(), "loops") {
		t.Fatalf("error = %q, want both drifted tables named", err)
	}
}
