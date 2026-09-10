package integration

import (
	"context"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"

	"github.com/artemavrin/broker/migrations"
)

// TestEmbeddedMigrationsMatchDisk guards the packaging promise: the binary
// carries every migration, so a release artefact needs no files beside it. A
// migration added outside the embedded directory would break a deployed broker
// while still passing the tests that migrate from a working copy.
func TestEmbeddedMigrationsMatchDisk(t *testing.T) {
	embedded, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatalf("glob embedded: %v", err)
	}
	if len(embedded) == 0 {
		t.Fatal("no migrations embedded in the binary")
	}

	paths, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil {
		t.Fatalf("glob on disk: %v", err)
	}
	onDisk := make([]string, 0, len(paths))
	for _, p := range paths {
		onDisk = append(onDisk, filepath.Base(p))
	}

	sort.Strings(embedded)
	sort.Strings(onDisk)
	if len(embedded) != len(onDisk) {
		t.Fatalf("embedded %v, on disk %v", embedded, onDisk)
	}
	for i := range embedded {
		if embedded[i] != onDisk[i] {
			t.Fatalf("embedded %v, on disk %v", embedded, onDisk)
		}
	}
}

// TestMigrateFromEmbeddedFS exercises the path a deployed broker actually
// takes: an empty directory, migrations read out of the binary.
func TestMigrateFromEmbeddedFS(t *testing.T) {
	env := newEnv(t)
	ctx := context.Background()

	if err := env.db.Migrate(ctx, ""); err != nil {
		t.Fatalf("migrate from embedded FS: %v", err)
	}
	for _, table := range []string{"initiators", "receivers", "messages"} {
		var n int
		err := env.db.Pool().QueryRow(ctx,
			`SELECT count(*) FROM information_schema.tables
			 WHERE table_schema = 'public' AND table_name = $1`, table).Scan(&n)
		if err != nil {
			t.Fatalf("check table %s: %v", table, err)
		}
		if n != 1 {
			t.Fatalf("table %s missing after migrating from the embedded FS", table)
		}
	}
}

// TestMigrateFromDiskStillWorks keeps the override honest: development and the
// rest of the suite point at a working copy, and that must not regress when
// the embedded copy is the default.
func TestMigrateFromDiskStillWorks(t *testing.T) {
	env := newEnv(t)
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatalf("abs: %v", err)
	}
	if err := env.db.Migrate(context.Background(), dir); err != nil {
		t.Fatalf("migrate from disk: %v", err)
	}
}
