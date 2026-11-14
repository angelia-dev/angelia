// Package migrations holds the versioned goose migrations of the store schema,
// embedded into the binary, one directory per engine.
//
// Both engines use the same version numbers: version N is the same schema on
// SQLite and on Postgres (ADR 0028 §7). Postgres starts at the SQLite head of
// the day it was added (00020) with one initial migration, and every later
// migration ships both engines.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/*.sql postgres/*.sql
var files embed.FS

// SQLite returns the SQLite migrations, named NNNNN_description.sql.
func SQLite() fs.FS { return sub("sqlite") }

// Postgres returns the Postgres migrations, named NNNNN_description.sql.
func Postgres() fs.FS { return sub("postgres") }

func sub(dir string) fs.FS {
	// fs.Sub fails only for an invalid path, and dir is a constant valid one.
	s, _ := fs.Sub(files, dir)
	return s
}
