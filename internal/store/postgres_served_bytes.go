package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"openusenet/internal/article"
)

// servedBytesMigration names the one-time recount of articles.bytes in
// store_migrations.
const servedBytesMigration = "served-xref-bytes"

// servedBytesBatch is how many articles one round of the recount reads.
const servedBytesBatch = 1000

// migrateServedBytes recounts articles.bytes once as the size ARTICLE serves:
// the stored article (headers, the empty line, body: octet_length(headers) +
// 4 + octet_length(body), as Post's callers count it) plus
// article.ServedXrefDelta for the Xref line ARTICLE adds. Post stores new
// rows that way; rows stored before kept the size without Xref. Only the
// bytes column changes, the stored article is not touched.
//
// It runs once: the marker row in store_migrations is inserted in the same
// transaction as the updates, so a second start (or a second server starting
// at the same time, which waits on the marker row) does nothing. The new
// value is computed from the row, not added to it, so even a second run would
// not count anything twice. Reading headers into []byte and the sizes with
// octet_length works for headers/body as TEXT and as BYTEA.
func (p *Postgres) migrateServedBytes(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS store_migrations (
		name TEXT PRIMARY KEY,
		done_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("served bytes migrate: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	type row struct {
		id   int64
		head []byte
		raw  int64
		xref string
	}
	var changed int64
	var last int64
	for {
		rows, err := tx.Query(ctx, `
			SELECT id, headers, octet_length(headers)::bigint + 4 + octet_length(body)::bigint, bytes, xref
			FROM articles WHERE id > $1 ORDER BY id LIMIT $2`, last, servedBytesBatch)
		if err != nil {
			return 0, fmt.Errorf("served bytes migrate: %w", err)
		}
		var todo []row
		n := 0
		for rows.Next() {
			var r row
			var bytes int64
			if err := rows.Scan(&r.id, &r.head, &r.raw, &bytes, &r.xref); err != nil {
				rows.Close()
				return 0, fmt.Errorf("served bytes migrate: %w", err)
			}
			n++
			last = r.id
			r.raw += int64(article.ServedXrefDelta(string(r.head), r.xref))
			if r.raw != bytes {
				r.head = nil
				todo = append(todo, r)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("served bytes migrate: %w", err)
		}
		if len(todo) > 0 {
			b := &pgx.Batch{}
			for _, r := range todo {
				b.Queue(`UPDATE articles SET bytes = $1 WHERE id = $2`, r.raw, r.id)
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
