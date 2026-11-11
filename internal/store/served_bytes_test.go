package store

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"openusenet/internal/article"
)

// servedBytesTestPostgres returns a DSN for a scratch schema on the server
// named by OPENUSENET_TEST_POSTGRES, or skips the test. The schema is dropped
// after. (Named apart from fix/store-articles-bytea's testPostgresURL, so the
// two branches merge.)
func servedBytesTestPostgres(t *testing.T) string {
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
	schema := fmt.Sprintf("ous_test_xref_%d", time.Now().UnixNano())
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

// servedSize is the size of the article as ARTICLE serves it.
func servedSize(head, body, xref string) int {
	return len(article.ServedHead(head, xref)) + 4 + len(body)
}

// legacyRestart turns the rows into rows stored before the served-size count
// (bytes = the stored size) and opens the store again, as an upgraded server
// starts on an old database.
func legacyRestart(t *testing.T, p *Postgres, dsn string, sizes map[string]int) *Postgres {
	t.Helper()
	ctx := context.Background()
	for id, n := range sizes {
		if _, err := p.pool.Exec(ctx, `UPDATE articles SET bytes=$1 WHERE message_id=$2`, n, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.pool.Exec(ctx, `DELETE FROM store_migrations WHERE name=$1`, servedBytesMigration); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	p2, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p2.Close() })
	return p2
}

// TestServedBytesOldAndNewRows is ous-sol's review fixture for O-10: GetByMsgID,
// GetByNumber, Overview and HDR :bytes report the size ARTICLE serves (with our
// Xref line, replacing one an imported head kept) for new rows, and for rows
// stored before the change once the Postgres store has started on them. The
// stored payload is unchanged. The memory store is not persistent, so it has
// no old rows.
func TestServedBytesOldAndNewRows(t *testing.T) {
	ctx := context.Background()
	for _, backend := range []string{"memory", "postgres"} {
		t.Run(backend, func(t *testing.T) {
			var st Store
			restart := func(map[string]int) {}
			if backend == "memory" {
				st = NewMemory()
			} else {
				dsn := servedBytesTestPostgres(t)
				p, err := OpenPostgres(ctx, dsn)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = p.Close() })
				st = p
				restart = func(sizes map[string]int) {
					p = legacyRestart(t, p, dsn, sizes)
					st = p
				}
			}
			for i, tc := range []struct {
				name              string
				legacy, priorXref bool
			}{
				{"new-without-xref", false, false},
				{"new-imported-xref", false, true},
				{"old-without-xref", true, false},
				{"old-imported-xref", true, true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					if tc.legacy && backend == "memory" {
						t.Skip("the memory store has no rows from before the change")
					}
					group := fmt.Sprintf("local.review%d", i)
					if err := st.EnsureGroup(ctx, group, "fixture", "y"); err != nil {
						t.Fatal(err)
					}
					id := fmt.Sprintf("<xref-review-%d@test>", i)
					base := "Path: source!not-for-mail\r\nFrom: fixture@example.invalid\r\nSubject: bytes\r\nMessage-ID: " + id
					head := base
					if tc.priorXref {
						head += "\r\nXref: old.peer.example local.other:123456\r\n\talt.group:999"
					}
					body := ".body\r\n"
					rawBytes := len(head) + 4 + len(body)
					res, err := st.Post(ctx, head, body, id, "bytes", "fixture@example.invalid", "now", "", "news.test", rawBytes, 1, []string{group}, false)
					if err != nil {
						t.Fatal(err)
					}
					if tc.legacy {
						restart(map[string]int{id: rawBytes}) // pre-O10 rows persisted the unserved size
					}
					wantHead := base + "\r\nXref: news.test " + group + ":1"
					want := len(wantHead) + 4 + len(body)
					if res.Xref != "news.test "+group+":1" {
						t.Fatalf("xref=%q", res.Xref)
					}
					byID, err := st.GetByMsgID(ctx, id)
					if err != nil || byID == nil {
						t.Fatalf("GetByMsgID=%v %v", byID, err)
					}
					if byID.Headers != head || byID.Body != body {
						t.Fatal("stored payload changed")
					}
					if byID.Bytes != want {
						t.Errorf("GetByMsgID bytes=%d, want served ARTICLE size %d", byID.Bytes, want)
					}
					byNum, err := st.GetByNumber(ctx, group, 1)
					if err != nil || byNum == nil || byNum.Bytes != want {
						t.Errorf("GetByNumber=%+v err=%v, want bytes %d", byNum, err, want)
					}
					over, err := st.Overview(ctx, group, 1, 1)
					if err != nil || len(over) != 1 || over[0].Bytes != want {
						t.Errorf("Overview=%+v err=%v, want bytes %d", over, err, want)
					}
					hdr, err := st.Header(ctx, group, 1, 1, ":bytes")
					if err != nil || len(hdr) != 1 || hdr[0].Header != strconv.Itoa(want) {
						t.Errorf("HDR :bytes=%+v err=%v, want %d", hdr, err, want)
					}
				})
			}
		})
	}
}

// TestPostgresServedBytesMigration: on an old database the first start
// recounts bytes as the served size, rows with and without an Xref field in
// the head (folded, twice) alike, without touching the stored article; the
// next start changes nothing (the marker), and a recount finds nothing to do.
func TestPostgresServedBytesMigration(t *testing.T) {
	dsn := servedBytesTestPostgres(t)
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if err := p.EnsureGroup(ctx, "local.test", "", "y"); err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureGroup(ctx, "local.other", "", "y"); err != nil {
		t.Fatal(err)
	}
	type art struct{ id, head, body string }
	arts := []art{
		{"<mig-plain@test>", "Path: a!b\r\nFrom: f@x\r\nSubject: plain\r\nMessage-ID: <mig-plain@test>", "body\r\n"},
		{"<mig-folded@test>", "Path: a!b\r\nXref: old.peer.example local.test:123456\r\n\tlocal.other:99\r\n" +
			"From: f@x\r\nSubject: folded\r\nMessage-ID: <mig-folded@test>", "b\r\n\r\nc\r\n"},
		{"<mig-twice@test>", "XREF: one a:1\r\nPath: a!b\r\nFrom: f@x\r\nSubject: twice\r\n" +
			"Message-ID: <mig-twice@test>\r\nxref: two b:2", ".dot\r\n"},
	}
	raw := map[string]int{}
	xref := map[string]string{}
	for _, a := range arts {
		res, err := p.Post(ctx, a.head, a.body, a.id, "s", "f@x", "now", "", "news.test",
			len(a.head)+4+len(a.body), 1, []string{"local.test", "local.other"}, false)
		if err != nil {
			t.Fatal(err)
		}
		raw[a.id] = len(a.head) + 4 + len(a.body)
		xref[a.id] = res.Xref
	}

	p = legacyRestart(t, p, dsn, raw)
	check := func(when string) {
		t.Helper()
		for _, a := range arts {
			got, err := p.GetByMsgID(ctx, a.id)
			if err != nil || got == nil {
				t.Fatalf("%s: GetByMsgID %s: %v %v", when, a.id, got, err)
			}
			if got.Headers != a.head || got.Body != a.body || got.Xref != xref[a.id] {
				t.Errorf("%s: %s stored article changed: %q %q %q", when, a.id, got.Headers, got.Body, got.Xref)
			}
			if want := servedSize(a.head, a.body, xref[a.id]); got.Bytes != want {
				t.Errorf("%s: %s bytes %d, want %d (served size; stored size %d)", when, a.id, got.Bytes, want, raw[a.id])
			}
		}
	}
	check("first start")

	// The marker is set: a second start leaves bytes alone, even a wrong value.
	if _, err := p.pool.Exec(ctx, `UPDATE articles SET bytes=7 WHERE message_id=$1`, arts[0].id); err != nil {
		t.Fatal(err)
	}
	_ = p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got, err := p.GetByMsgID(ctx, arts[0].id); err != nil || got == nil || got.Bytes != 7 {
		t.Fatalf("second start changed bytes: %+v %v", got, err)
	}
	if _, err := p.pool.Exec(ctx, `UPDATE articles SET bytes=$1 WHERE message_id=$2`,
		servedSize(arts[0].head, arts[0].body, xref[arts[0].id]), arts[0].id); err != nil {
		t.Fatal(err)
	}
	check("second start")

	// A recount on served sizes changes nothing.
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if n, err := recountServedBytes(ctx, tx); err != nil || n != 0 {
		t.Fatalf("recount on served sizes: %d rows changed, %v", n, err)
	}
}
