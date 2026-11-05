package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
	"unicode/utf8"
)

// Output formats of the CLI (-o).
const (
	formatTable = "table"
	formatJSON  = "json"
)

// parseFormat validates the -o value.
func parseFormat(s string) (string, error) {
	switch s {
	case formatTable, formatJSON:
		return s, nil
	}
	return "", fmt.Errorf("unknown output format %q: want %s or %s", s, formatTable, formatJSON)
}

// table is what a command prints in the default format: a header and rows of
// already formatted cells.
type table struct {
	Header []string
	Rows   [][]string
}

// detail builds the two-column table of a single resource.
func detail(pairs ...string) table {
	t := table{Header: []string{"FIELD", "VALUE"}}
	for i := 0; i+1 < len(pairs); i += 2 {
		t.Rows = append(t.Rows, []string{pairs[i], pairs[i+1]})
	}
	return t
}

// writeTable prints t with aligned columns. An empty table prints its header.
func writeTable(w io.Writer, t table) error {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, strings.Join(t.Header, "\t")); err != nil {
		return err
	}
	for _, r := range t.Rows {
		cells := make([]string, len(r))
		for i, c := range r {
			cells[i] = cell(c)
		}
		if _, err := fmt.Fprintln(tw, strings.Join(cells, "\t")); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// cell keeps a value on one line, so that it cannot break the table.
func cell(s string) string {
	if s == "" {
		return "-"
	}
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' || r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}

// writeJSON prints v as indented JSON, without escaping <, > and &.
func writeJSON(w io.Writer, v any) error {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("encode output: %w", err)
	}
	_, err := w.Write(b.Bytes())
	return err
}

// render prints v as JSON, or t as a table, by format. v is what the API
// returned, so that `-o json` shows the API's own shape.
func render(w io.Writer, format string, v any, t table) error {
	if format == formatJSON {
		return writeJSON(w, v)
	}
	return writeTable(w, t)
}

// page is the JSON shape of a list: that of the API's pages, for a whole
// listing too.
type page[T any] struct {
	Items      []T     `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

// timeCell formats an optional timestamp as RFC 3339 UTC with milliseconds,
// the form the API sends.
func timeCell(t *time.Time) string {
	if t == nil {
		return ""
	}
	return fmtTime(*t)
}

func fmtTime(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000Z") }

// truncate shortens s to at most n runes.
func truncate(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}
