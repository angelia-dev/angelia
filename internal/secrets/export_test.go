package secrets

import (
	"context"
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite" // the database/sql driver of the test store
)

// renameSecret renames a row behind the API's back, as an attacker with
// database write access could: the API has no rename.
func renameSecret(ctx context.Context, path, from, to string) error {
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	res, err := db.ExecContext(ctx, `UPDATE secret SET name = ? WHERE name = ?`, to, from)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("renamed %d rows", n)
	}
	return nil
}
