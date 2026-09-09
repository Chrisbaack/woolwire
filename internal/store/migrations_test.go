package store

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"
)

// migrationDigests pins each migration's content to its position. A database
// records the index it has applied, so an index must never change meaning.
//
// This exists because it went wrong: a new migration inserted in the middle
// renumbered every later one, and a database that had already applied the
// original numbering replayed the shifted migrations against a schema that
// already had them ("duplicate column name: filename"). A fresh database
// migrates cleanly either way, so nothing else catches it.
//
// Appending a migration appends a digest. Changing an existing digest means an
// already-released migration was edited or moved, which is almost always the
// bug above rather than the intent.
var migrationDigests = []string{
	"1905a6beb346be80",
	"02a80103a562b6bc",
	"3b23e547bde98b1b",
	"bd305345b54d4a31",
	"770bac438d2a3941",
	"1dfad690f2dc0790",
	"61bea0dc4c8dce88",
	"ec424382fa6d6c2d",
	"9e00d776a7e5b126",
	"a59b921c1005b76a",
	"964b2e68f7e8072a",
}

func TestMigrationsAreAppendOnly(t *testing.T) {
	if len(migrations) < len(migrationDigests) {
		t.Fatalf("%d migrations for %d pinned digests: a released migration was removed",
			len(migrations), len(migrationDigests))
	}
	for i, want := range migrationDigests {
		sum := sha256.Sum256([]byte(migrations[i]))
		if got := hex.EncodeToString(sum[:8]); got != want {
			t.Errorf("migration %d changed (%s, pinned %s). Inserting or editing a\n"+
				"migration renumbers the ones after it, and a database that already\n"+
				"applied the old numbering will replay the wrong statements. Append\n"+
				"instead, and add the new digest.", i, got, want)
		}
	}
	if len(migrations) > len(migrationDigests) {
		t.Logf("%d new migrations appended; add their digests to migrationDigests",
			len(migrations)-len(migrationDigests))
	}
}

// TestEveryReleaseUpgradesCleanly walks a database forward from each earlier
// point in the migration list, which is what an existing install does. A
// migration that only works on a database built from scratch fails here.
func TestEveryReleaseUpgradesCleanly(t *testing.T) {
	full := migrations
	t.Cleanup(func() { migrations = full })

	for cut := 1; cut <= len(full); cut++ {
		path := filepath.Join(t.TempDir(), "woolwire.db")

		migrations = full[:cut]
		older, err := Open(path)
		if err != nil {
			t.Fatalf("a release with %d migrations cannot create a database: %v", cut, err)
		}
		_ = older.Close()

		migrations = full
		upgraded, err := Open(path)
		if err != nil {
			t.Fatalf("upgrading a database left by a release with %d migrations: %v", cut, err)
		}
		if _, err := upgraded.GetHostLimits(); err != nil {
			t.Fatalf("host limits after upgrading from %d migrations: %v", cut, err)
		}
		if _, err := upgraded.ListHostedModels(); err != nil {
			t.Fatalf("hosted models after upgrading from %d migrations: %v", cut, err)
		}
		_ = upgraded.Close()
	}
}
