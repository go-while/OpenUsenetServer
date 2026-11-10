package store

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Two servers upgrading the same database at once: before the fix both read
// TEXT before any lock, the first converted, and the second ran convert_to on
// bytea and failed startup (SQLSTATE 42883). Found by ous-sol's review.
func TestPostgresArticleBytesConcurrentMigration(t *testing.T) {
	dsn := testPostgresURL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, `CREATE TABLE articles (headers text NOT NULL, body text NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO articles VALUES ('Subject: s', 'body')`); err != nil {
		t.Fatal(err)
	}
	// Hold the table until both migrations are past their first look and
	// waiting for a lock, so the race happens every time.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = hold.Rollback(ctx) }()
	if _, err := hold.Exec(ctx, `LOCK TABLE articles IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	p := &Postgres{pool: pool}
	result := make(chan error, 2)
	for range 2 {
		go func() { result <- p.migrateArticleBytes(ctx) }()
	}
	for {
		var waiting int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting >= 2 {
			break
		}
		select {
		case err := <-result:
			t.Fatalf("migration returned while the table was locked: %v", err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := <-result; err != nil {
			t.Errorf("concurrent migration: %v", err)
		}
	}
	var head, body []byte
	if err := pool.QueryRow(ctx, `SELECT headers, body FROM articles`).Scan(&head, &body); err != nil {
		t.Fatal(err)
	}
	if string(head) != "Subject: s" || string(body) != "body" {
		t.Fatalf("bytes changed: %q %q", head, body)
	}
}
