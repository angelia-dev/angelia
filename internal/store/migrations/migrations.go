// Package migrations holds the versioned goose migrations of the store schema,
// embedded into the binary.
package migrations

import "embed"

// FS contains the SQL migrations, named NNNNN_description.sql.
//
//go:embed *.sql
var FS embed.FS
