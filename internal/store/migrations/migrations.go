// Package migrations holds the versioned goose migrations of the store schema,
// embedded into the binary, one directory per engine.
package migrations

import (
	"embed"
	"io/fs"
)

//go:embed sqlite/*.sql
var files embed.FS

// SQLite returns the SQLite migrations, named NNNNN_description.sql.
func SQLite() fs.FS {
	// fs.Sub fails only for an invalid path, and "sqlite" is a constant valid one.
	sub, _ := fs.Sub(files, "sqlite")
	return sub
}
