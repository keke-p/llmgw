package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"llmgw/internal/db"
)

// runSQL executes SQL against the database. It is a development aid for seeding
// providers/aliases before the admin API exists (the distroless image has no
// sqlite3 binary). Query results are printed as a table.
func runSQL(args []string) error {
	q := strings.Join(args, " ")
	if strings.TrimSpace(q) == "" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		q = string(b)
	}
	if strings.TrimSpace(q) == "" {
		return fmt.Errorf("usage: llmgw sql \"<SQL>\"")
	}
	path := os.Getenv("LLMGW_DB_PATH")
	if path == "" {
		path = "data/llmgw.db"
	}
	ctx := context.Background()
	d, err := db.Open(ctx, path)
	if err != nil {
		return err
	}
	defer d.Close()

	head := strings.ToUpper(strings.TrimSpace(q))
	if strings.HasPrefix(head, "SELECT") || strings.HasPrefix(head, "PRAGMA") || strings.HasPrefix(head, "WITH") {
		rows, err := d.QueryContext(ctx, q)
		if err != nil {
			return err
		}
		defer rows.Close()
		return printRows(rows)
	}
	res, err := d.ExecContext(ctx, q)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	id, _ := res.LastInsertId()
	fmt.Printf("ok (rows affected: %d, last insert id: %d)\n", n, id)
	return nil
}

func printRows(rows *sql.Rows) error {
	cols, err := rows.Columns()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(cols, "\t"))
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		out := make([]string, len(cols))
		for i, v := range vals {
			switch x := v.(type) {
			case nil:
				out[i] = "NULL"
			case []byte:
				out[i] = string(x)
			default:
				out[i] = fmt.Sprint(x)
			}
		}
		fmt.Fprintln(tw, strings.Join(out, "\t"))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return rows.Err()
}
