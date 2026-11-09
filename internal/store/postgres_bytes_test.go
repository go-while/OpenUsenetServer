package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// testPostgresURL returns a DSN for a scratch schema on the server named by
// OPENUSENET_TEST_POSTGRES, or skips the test. The schema is dropped after.
func testPostgresURL(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("OPENUSENET_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("OPENUSENET_TEST_POSTGRES not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })
	schema := fmt.Sprintf("ous_test_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = conn.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE") })
	if !strings.Contains(dsn, "://") {
		return dsn + " search_path=" + schema
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestPostgresArticleBytes(t *testing.T) {
	dsn := testPostgresURL(t)
	ctx := context.Background()

	// A database created before headers/body became bytea.
	old, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE articles (
			id BIGSERIAL PRIMARY KEY,
			message_id TEXT NOT NULL UNIQUE,
			subject TEXT NOT NULL DEFAULT '',
			from_hdr TEXT NOT NULL DEFAULT '',
			date_hdr TEXT NOT NULL DEFAULT '',
			references_hdr TEXT NOT NULL DEFAULT '',
			bytes INT NOT NULL,
			lines INT NOT NULL,
			headers TEXT NOT NULL,
			body TEXT NOT NULL,
			stored_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			xref TEXT NOT NULL DEFAULT ''
		)`,
		`ALTER TABLE articles ADD COLUMN search_tsv tsvector GENERATED ALWAYS AS (
			setweight(to_tsvector('english', coalesce(subject, '')), 'A') ||
			setweight(to_tsvector('english', coalesce(from_hdr, '')), 'B') ||
			setweight(to_tsvector('english', coalesce(body, '')), 'C')
		) STORED`,
		`INSERT INTO articles (message_id, subject, bytes, lines, headers, body)
		 VALUES ('<old@t>', 'old', 10, 1, 'Subject: old', 'Grüße pineapple' || chr(13) || chr(10))`,
	} {
		if _, err := old.Exec(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	_ = old.Close(ctx)

	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"headers", "body"} {
		var typ string
		if err := p.pool.QueryRow(ctx, `SELECT data_type FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = 'articles' AND column_name = $1`, col).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		if typ != "bytea" {
			t.Fatalf("%s is %s after migration", col, typ)
		}
	}
	a, err := p.GetByMsgID(ctx, "<old@t>")
	if err != nil || a == nil {
		t.Fatalf("old row: %v %v", a, err)
	}
	if a.Headers != "Subject: old" || a.Body != "Grüße pineapple\r\n" {
		t.Fatalf("old row changed: %q %q", a.Headers, a.Body)
	}
	if hits, err := p.SearchArticles(ctx, "pineapple", "", 10, 0); err != nil || len(hits) != 1 {
		t.Fatalf("search migrated row: %v %v", hits, err)
	}
	_ = p.Close()

	// Running the migrations again on a migrated database is a no-op.
	p, err = OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.Close() }()

	if err := p.EnsureGroup(ctx, "local.test", "", "y"); err != nil {
		t.Fatal(err)
	}
	var yenc strings.Builder
	for c := 1; c < 256; c++ {
		if c != '\r' && c != '\n' && c != '=' {
			yenc.WriteByte(byte(c))
		}
	}
	bodies := map[string]string{
		"latin1": "caf\xe9 noir\r\n",
		"nul":    "before\x00after\r\n",
		"yenc":   "=ybegin line=128 size=252 name=b.bin\r\n" + yenc.String() + "\r\n=yend size=252\r\n",
		"empty":  "",
	}
	for name, body := range bodies {
		msgid := "<" + name + "@t>"
		hdr := "Path: x\xe9!not-for-mail\r\nFrom: J\xf6rg <j@t>\r\nSubject: " + name + " caf\xe9\x00\r\nMessage-ID: " + msgid
		if _, err := p.Post(ctx, hdr, body, msgid, name+" caf\xe9\x00", "J\xf6rg <j@t>", "", "", "news.test",
			len(hdr)+len(body), 1, []string{"local.test"}, name == "yenc"); err != nil {
			t.Fatalf("post %s: %v", name, err)
		}
		if err := p.RecordContentStats(ctx, ContentStatsEvent{
			Groups: []string{"local.test"}, From: "J\xf6rg <j@t>", Path: "x\xe9!not-for-mail",
		}); err != nil {
			t.Fatalf("content stats %s: %v", name, err)
		}
		got, err := p.GetByMsgID(ctx, msgid)
		if err != nil || got == nil {
			t.Fatalf("get %s: %v %v", name, got, err)
		}
		if got.Headers != hdr || got.Body != body {
			t.Fatalf("%s changed:\n got %q %q\nwant %q %q", name, got.Headers, got.Body, hdr, body)
		}
		if got.Subject != name+" café" || got.From != "Jörg <j@t>" {
			t.Fatalf("%s overview: %q %q", name, got.Subject, got.From)
		}
	}
	arts, err := p.ArticlesForGroup(ctx, "local.test")
	if err != nil || len(arts) != len(bodies) {
		t.Fatalf("articles for group: %d %v", len(arts), err)
	}
	for _, a := range arts {
		if want := bodies[strings.TrimSuffix(strings.TrimPrefix(a.MessageID, "<"), "@t>")]; a.Body != want {
			t.Fatalf("%s body via group: %q want %q", a.MessageID, a.Body, want)
		}
	}
	for _, q := range []string{"latin1", "noir", "café", "pineapple"} {
		if _, err := p.SearchArticles(ctx, q, "", 10, 0); err != nil {
			t.Fatalf("search %q: %v", q, err)
		}
	}
	hits, err := p.SearchArticles(ctx, "yenc", "", 10, 0)
	if err != nil || len(hits) != 1 || hits[0].MessageID != "<yenc@t>" {
		t.Fatalf("search by subject: %v %v", hits, err)
	}
	if err := p.RefreshContentStats(ctx, []string{"news.test"}); err != nil {
		t.Fatalf("refresh content stats: %v", err)
	}
}
