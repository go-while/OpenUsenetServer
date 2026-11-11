package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"openusenet/internal/article"
)

// servedBytesMigration names the one-time recount of articles.bytes in
// store_migrations. (An earlier development version used
// "served-xref-bytes" and did not count LF-only line ends as the writer sends
// them; the new name makes databases it ran on recount.)
const servedBytesMigration = "served-wire-bytes"

// servedBytesBatch is how many articles one round of the recount reads. The
// rows carry the body, so it is kept small.
const servedBytesBatch = 200

// migrateServedBytes recounts articles.bytes once as the size ARTICLE sends,
// article.ServedSize: the stored head with our Xref line, the empty line and
// the body, with the line ends nntp.Conn.WriteDot writes (LF-only line ends
// of older rows go out as CRLF). Post stores new rows that way; rows stored
// before kept the stored size without Xref. Only the bytes column changes,
// the stored article is not touched.
//
// It runs once: under an advisory lock, the table and the marker row in
// store_migrations are created in the same transaction as the updates, so a
// second start (or a second server starting at the same time, which waits on
// the lock) does nothing. Upgrade all servers that share a database together:
// a server still running older code stores new rows without the Xref line. The new
// value is computed from the row, not added to it, so even a second run would
// not count anything twice. Reading headers and body into []byte works for
// them as TEXT and as BYTEA.
func (p *Postgres) migrateServedBytes(ctx context.Context) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Servers starting at the same time on a fresh database would race in
	// CREATE TABLE IF NOT EXISTS (duplicate pg_type, SQLSTATE 23505): take a
	// transaction-scoped advisory lock first, so table creation, marker and
	// recount run one server at a time.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('openusenet:store_migrations'))`); err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	if _, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS store_migrations (
		name TEXT PRIMARY KEY,
		done_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	tag, err := tx.Exec(ctx, `INSERT INTO store_migrations (name) VALUES ($1) ON CONFLICT (name) DO NOTHING`,
		servedBytesMigration)
	if err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return tx.Commit(ctx) // done before
	}
	if _, err := recountServedBytes(ctx, tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// recountServedBytes sets articles.bytes to the served size for every row
// where it differs and returns how many rows it changed.
func recountServedBytes(ctx context.Context, tx pgx.Tx) (int64, error) {
	type update struct {
		id    int64
		bytes int
	}
	var changed int64
	var last int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT id, headers, body, bytes, xref
			FROM articles WHERE id > $1 ORDER BY id LIMIT $2`, last, servedBytesBatch)
		if err != nil {
			return 0, fmt.Errorf("served bytes migrate: %w", err)
		}
		var todo []update
		n := 0
		for rows.Next() {
			var id int64
			var head, body []byte
			var bytes int
			var xref string
			if err := rows.Scan(&id, &head, &body, &bytes, &xref); err != nil {
				rows.Close()
				return 0, fmt.Errorf("served bytes migrate: %w", err)
			}
			n++
			last = id
			if served := article.ServedSize(string(head), string(body), xref); served != bytes {
				todo = append(todo, update{id, served})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("served bytes migrate: %w", err)
		}
		if len(todo) > 0 {
			b := &pgx.Batch{}
			for _, u := range todo {
				b.Queue(`UPDATE articles SET bytes = $1 WHERE id = $2`, u.bytes, u.id)
			}
			if err := tx.SendBatch(ctx, b).Close(); err != nil {
				return 0, fmt.Errorf("served bytes migrate: %w", err)
			}
			changed += int64(len(todo))
		}
		if n < servedBytesBatch {
			return changed, nil
		}
	}
}
