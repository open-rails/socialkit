package socialkit

import (
	"embed"
	"io/fs"
)

// migratekit's LoadFromFS does not recurse, so expose a sub-FS rooted at
// "postgres/" (same shape searchkit uses).
//
//go:embed migrations/postgres/*.sql
var migrationsFS embed.FS

// PostgresMigrations is the socialkit migration source. A host that runs a
// central migrate step can register it with migratekit alongside its other
// sources; Runtime also applies it itself at construction.
var PostgresMigrations fs.FS = mustSub(migrationsFS, "migrations/postgres")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}
