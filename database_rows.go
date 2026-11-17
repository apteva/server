package main

import "database/sql"

// Own the cursor in this read phase. Callers enrich only the returned records,
// after Close has released the pooled connection, including every error path.
func collectSQLRows[T any](rows *sql.Rows, scan func(rowScanner) (T, error)) ([]T, error) {
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		record, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, record)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
