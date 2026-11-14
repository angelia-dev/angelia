package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// A catalogue is a schema normalised to lines that read the same on both
// engines, one fact per line:
//
//	table <t>
//	column <t>.<c> <class> [not null] [default <v>]
//	primary key <t> (<cols>)
//	unique <t> (<cols>)
//	foreign key <t> (<cols>) references <rt> (<rcols>) on delete <action>
//	check <t>.<c>
//	index <name> on <t> [unique] (<cols>) [where <predicate>]
//	trigger <name> on <t> <timing> <event>
//
// Type classes follow ADR 0028 §7: text, bytes, int64 and bool. An expression
// in an index is <expr>. A CHECK is recorded by the column it is declared on;
// its expression differs by engine (json_valid against a ::json cast) and is
// checked by behaviour in schema_test.go instead.
type catalogue []string

// sqliteFlags are the SQLite INTEGER 0/1 columns that are BOOLEAN on Postgres,
// without their SQLite CHECK (x IN (0, 1)) (ADR 0028 §7). A new flag column
// is added here and to the Postgres twin as BOOLEAN.
var sqliteFlags = map[string]bool{
	"watch.baseline_done":      true,
	"version.prerelease":       true,
	"version.yanked":           true,
	"delivery.hold_prerelease": true,
}

// excludedTables are bookkeeping tables of SQLite or goose, not of the schema.
func excludedTable(name string) bool {
	return name == migrationsTable || strings.HasPrefix(name, "sqlite_")
}

// diff returns the lines only in a (prefixed "- ") and only in b ("+ "),
// sorted, or "" when the catalogues are equal.
func diff(a, b catalogue) string {
	var out []string
	for _, l := range a {
		if !slices.Contains(b, l) {
			out = append(out, "- "+l)
		}
	}
	for _, l := range b {
		if !slices.Contains(a, l) {
			out = append(out, "+ "+l)
		}
	}
	slices.SortFunc(out, func(x, y string) int { return strings.Compare(x[2:]+x[:1], y[2:]+y[:1]) })
	return strings.Join(out, "\n")
}

var (
	triggerKind   = regexp.MustCompile(`(?i)\b(BEFORE|AFTER|INSTEAD OF)\s+(INSERT|UPDATE|DELETE)\b`)
	sqlComment    = regexp.MustCompile(`--[^\n]*`)
	checkClause   = regexp.MustCompile(`(?i)\bCHECK\s*\(`)
	whereClause   = regexp.MustCompile(`(?is)\bWHERE\s+(.*)$`)
	castSuffix    = regexp.MustCompile(`::[a-z ]+$`)
	tableKeywords = map[string]bool{"CONSTRAINT": true, "PRIMARY": true, "UNIQUE": true, "FOREIGN": true, "CHECK": true}
)

// normPredicate makes an index predicate comparable: lower case, without
// parentheses and repeated spaces.
func normPredicate(p string) string {
	p = strings.NewReplacer("(", "", ")", "").Replace(strings.ToLower(p))
	return strings.Join(strings.Fields(p), " ")
}

func trigger(name, table, def string) string {
	m := triggerKind.FindStringSubmatch(def)
	if m == nil {
		return fmt.Sprintf("trigger %s on %s <unparsed>", name, table)
	}
	return fmt.Sprintf("trigger %s on %s %s %s", name, table, strings.ToLower(m[1]), strings.ToLower(m[2]))
}

func column(table, name, class string, notNull bool, def sql.NullString) string {
	s := fmt.Sprintf("column %s.%s %s", table, name, class)
	if notNull {
		s += " not null"
	}
	if def.Valid {
		s += " default " + def.String
	}
	return s
}

// splitTopLevel splits s at commas outside parentheses and quotes.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
		case r == '(':
			depth++
		case r == ')':
			depth--
		case r == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

// sqliteCheckedColumns returns the columns of a CREATE TABLE statement whose
// definition carries a CHECK, and "<table>" for a table-level CHECK.
func sqliteCheckedColumns(createSQL string) []string {
	s := sqlComment.ReplaceAllString(createSQL, "")
	open, end := strings.Index(s, "("), strings.LastIndex(s, ")")
	if open < 0 || end < open {
		return nil
	}
	var out []string
	for _, def := range splitTopLevel(s[open+1 : end]) {
		fields := strings.Fields(def)
		if len(fields) == 0 || !checkClause.MatchString(def) {
			continue
		}
		if tableKeywords[strings.ToUpper(fields[0])] {
			out = append(out, "<table>")
			continue
		}
		out = append(out, strings.Trim(fields[0], `"`+"`"))
	}
	return out
}

func sqliteClass(table, col, declared string) string {
	switch strings.ToUpper(declared) {
	case "TEXT":
		return "text"
	case "BLOB":
		return "bytes"
	case "INTEGER":
		if sqliteFlags[table+"."+col] {
			return "bool"
		}
		return "int64"
	}
	return "unmapped:" + declared
}

func sqliteDefault(class string, d sql.NullString) sql.NullString {
	if d.Valid && class == "bool" {
		switch d.String {
		case "0":
			d.String = "false"
		case "1":
			d.String = "true"
		}
	}
	return d
}

// queryStrings runs q and returns its single text column.
func queryStrings(ctx context.Context, db *sql.DB, q string, args ...any) ([]string, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// sqliteCatalogue reads the catalogue of a SQLite database.
func sqliteCatalogue(ctx context.Context, db *sql.DB) (catalogue, error) {
	tables, err := queryStrings(ctx, db, `SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var c catalogue
	for _, t := range tables {
		if excludedTable(t) {
			continue
		}
		lines, err := sqliteTable(ctx, db, t)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t, err)
		}
		c = append(c, lines...)
	}
	rows, err := db.QueryContext(ctx, `SELECT name, tbl_name, sql FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, table, def string
		if err := rows.Scan(&name, &table, &def); err != nil {
			return nil, err
		}
		c = append(c, trigger(name, table, def))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	slices.Sort(c)
	return c, nil
}

func sqliteTable(ctx context.Context, db *sql.DB, t string) ([]string, error) {
	out := []string{"table " + t}

	var pk []string
	err := eachRow(ctx, db, `SELECT name, type, "notnull", dflt_value, pk FROM pragma_table_info(?) ORDER BY pk, cid`, []any{t},
		func(rows *sql.Rows) error {
			var name, typ string
			var notNull bool
			var def sql.NullString
			var pkPos int
			if err := rows.Scan(&name, &typ, &notNull, &def, &pkPos); err != nil {
				return err
			}
			if pkPos > 0 {
				pk = append(pk, name)
				notNull = true // primary key columns of STRICT tables are NOT NULL
			}
			class := sqliteClass(t, name, typ)
			out = append(out, column(t, name, class, notNull, sqliteDefault(class, def)))
			return nil
		})
	if err != nil {
		return nil, err
	}
	if len(pk) > 0 {
		out = append(out, fmt.Sprintf("primary key %s (%s)", t, strings.Join(pk, ",")))
	}

	type index struct {
		name, origin string
		unique       bool
	}
	var indexes []index
	err = eachRow(ctx, db, `SELECT name, "unique", origin FROM pragma_index_list(?)`, []any{t}, func(rows *sql.Rows) error {
		var ix index
		if err := rows.Scan(&ix.name, &ix.unique, &ix.origin); err != nil {
			return err
		}
		indexes = append(indexes, ix)
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, ix := range indexes {
		if ix.origin == "pk" {
			continue
		}
		var cols []string
		err := eachRow(ctx, db, `SELECT coalesce(name, '<expr>') FROM pragma_index_info(?) ORDER BY seqno`, []any{ix.name}, func(rows *sql.Rows) error {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			cols = append(cols, c)
			return nil
		})
		if err != nil {
			return nil, err
		}
		if ix.origin == "u" {
			out = append(out, fmt.Sprintf("unique %s (%s)", t, strings.Join(cols, ",")))
			continue
		}
		var def string
		if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'index' AND name = ?`, ix.name).Scan(&def); err != nil {
			return nil, err
		}
		pred := ""
		if m := whereClause.FindStringSubmatch(def); m != nil {
			pred = normPredicate(m[1])
		}
		out = append(out, indexLine(ix.name, t, ix.unique, strings.Join(cols, ","), pred))
	}

	type fk struct{ cols, refTable, refCols, onDelete []string }
	fks := map[int]*fk{}
	var ids []int
	err = eachRow(ctx, db, `SELECT id, "table", "from", "to", on_delete FROM pragma_foreign_key_list(?) ORDER BY id, seq`, []any{t},
		func(rows *sql.Rows) error {
			var id int
			var ref, from, to, onDelete string
			if err := rows.Scan(&id, &ref, &from, &to, &onDelete); err != nil {
				return err
			}
			f, ok := fks[id]
			if !ok {
				f = &fk{refTable: []string{ref}, onDelete: []string{strings.ToLower(onDelete)}}
				fks[id] = f
				ids = append(ids, id)
			}
			f.cols = append(f.cols, from)
			f.refCols = append(f.refCols, to)
			return nil
		})
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		f := fks[id]
		out = append(out, fkLine(t, strings.Join(f.cols, ","), f.refTable[0], strings.Join(f.refCols, ","), f.onDelete[0]))
	}

	var create string
	if err := db.QueryRowContext(ctx, `SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, t).Scan(&create); err != nil {
		return nil, err
	}
	for _, c := range sqliteCheckedColumns(create) {
		if sqliteFlags[t+"."+c] {
			continue // the 0/1 CHECK of a flag is the BOOLEAN type on Postgres
		}
		out = append(out, fmt.Sprintf("check %s.%s", t, c))
	}
	return out, nil
}

func indexLine(name, table string, unique bool, cols, pred string) string {
	s := "index " + name + " on " + table
	if unique {
		s += " unique"
	}
	s += " (" + cols + ")"
	if pred != "" {
		s += " where " + pred
	}
	return s
}

func fkLine(table, cols, refTable, refCols, onDelete string) string {
	return fmt.Sprintf("foreign key %s (%s) references %s (%s) on delete %s", table, cols, refTable, refCols, onDelete)
}

func eachRow(ctx context.Context, db *sql.DB, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

var pgDeleteActions = map[string]string{"a": "no action", "r": "restrict", "c": "cascade", "n": "set null", "d": "set default"}

func pgClass(typ string) string {
	switch typ {
	case "text":
		return "text"
	case "bytea":
		return "bytes"
	case "bigint":
		return "int64"
	case "boolean":
		return "bool"
	}
	return "unmapped:" + typ
}

// postgresCatalogue reads the catalogue of the connection's current schema.
func postgresCatalogue(ctx context.Context, db *sql.DB) (catalogue, error) {
	tables, err := queryStrings(ctx, db, `SELECT relname::text FROM pg_class
		WHERE relnamespace = current_schema()::regnamespace AND relkind = 'r' ORDER BY relname`)
	if err != nil {
		return nil, err
	}
	var c catalogue
	for _, t := range tables {
		if excludedTable(t) {
			continue
		}
		lines, err := postgresTable(ctx, db, t)
		if err != nil {
			return nil, fmt.Errorf("table %s: %w", t, err)
		}
		c = append(c, lines...)
	}
	slices.Sort(c)
	return c, nil
}

// attNames is a subquery giving the comma-separated names of the attributes
// in the int2 array $arr of relation $rel, in array order.
func attNames(rel, arr string) string {
	return `(SELECT string_agg(a.attname, ',' ORDER BY k.ord) FROM unnest(` + arr + `) WITH ORDINALITY k(attnum, ord)
		JOIN pg_attribute a ON a.attrelid = ` + rel + ` AND a.attnum = k.attnum)`
}

func postgresTable(ctx context.Context, db *sql.DB, t string) ([]string, error) {
	out := []string{"table " + t}
	rel := `(current_schema() || '.' || $1)::regclass`

	err := eachRow(ctx, db, `SELECT a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull, pg_get_expr(d.adbin, d.adrelid)
		FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = `+rel+` AND a.attnum > 0 AND NOT a.attisdropped ORDER BY a.attnum`, []any{t},
		func(rows *sql.Rows) error {
			var name, typ string
			var notNull bool
			var def sql.NullString
			if err := rows.Scan(&name, &typ, &notNull, &def); err != nil {
				return err
			}
			if def.Valid {
				def.String = castSuffix.ReplaceAllString(def.String, "")
			}
			out = append(out, column(t, name, pgClass(typ), notNull, def))
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = eachRow(ctx, db, `SELECT con.contype::text, `+attNames("con.conrelid", "con.conkey")+`,
			CASE WHEN con.contype = 'f' THEN con.confrelid::regclass::text ELSE '' END,
			CASE WHEN con.contype = 'f' THEN `+attNames("con.confrelid", "con.confkey")+` ELSE '' END,
			con.confdeltype::text
		FROM pg_constraint con WHERE con.conrelid = `+rel+` ORDER BY con.conname`, []any{t},
		func(rows *sql.Rows) error {
			var typ, cols, refTable, refCols, onDelete string
			if err := rows.Scan(&typ, &cols, &refTable, &refCols, &onDelete); err != nil {
				return err
			}
			switch typ {
			case "p":
				out = append(out, fmt.Sprintf("primary key %s (%s)", t, cols))
			case "u":
				out = append(out, fmt.Sprintf("unique %s (%s)", t, cols))
			case "f":
				out = append(out, fkLine(t, cols, refTable, refCols, pgDeleteActions[onDelete]))
			case "c":
				for _, col := range strings.Split(cols, ",") {
					out = append(out, fmt.Sprintf("check %s.%s", t, col))
				}
			default:
				out = append(out, fmt.Sprintf("constraint %s %s (%s)", typ, t, cols))
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = eachRow(ctx, db, `SELECT i.indexrelid::regclass::text, i.indisunique, coalesce(pg_get_expr(i.indpred, i.indrelid), ''),
			(SELECT string_agg(CASE WHEN k.attnum = 0 THEN '<expr>' ELSE a.attname::text END, ',' ORDER BY k.ord)
			 FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, ord)
			 LEFT JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum)
		FROM pg_index i WHERE i.indrelid = `+rel+`
		AND NOT EXISTS (SELECT 1 FROM pg_constraint c WHERE c.conindid = i.indexrelid AND c.contype IN ('p', 'u'))`, []any{t},
		func(rows *sql.Rows) error {
			var name, pred, cols string
			var unique bool
			if err := rows.Scan(&name, &unique, &pred, &cols); err != nil {
				return err
			}
			out = append(out, indexLine(name, t, unique, cols, normPredicate(pred)))
			return nil
		})
	if err != nil {
		return nil, err
	}

	err = eachRow(ctx, db, `SELECT tgname::text, pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = `+rel+` AND NOT tgisinternal`, []any{t},
		func(rows *sql.Rows) error {
			var name, def string
			if err := rows.Scan(&name, &def); err != nil {
				return err
			}
			out = append(out, trigger(name, t, def))
			return nil
		})
	return out, err
}
