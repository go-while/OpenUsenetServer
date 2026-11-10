package store

import (
	"context"
	"testing"

	"openusenet/internal/article"
)

// The hourly recount reads Path from the stored head. With heads kept as
// received, Path can be folded; the recount must count every site, as the
// intake did. Found by ous-sol's review.
func TestRefreshContentStatsFoldedPath(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	if err := m.EnsureGroup(ctx, "local.test", "", "y"); err != nil {
		t.Fatal(err)
	}
	a, err := article.Parse([]byte("Path: peer1\r\n\t!peer2!not-for-mail\r\nSubject: folded path\r\n\r\nbody\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Post(ctx, a.RawHeaders(), a.Body, "<folded@test>", "folded path", "a@b", "now", "", "news", a.Bytes(), a.Lines(), []string{"local.test"}, false); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordContentStats(ctx, ContentStatsEvent{Groups: []string{"local.test"}, From: "a@b", Path: a.Get("Path")}); err != nil {
		t.Fatal(err)
	}
	before, err := m.ContentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.RefreshContentStats(ctx, nil); err != nil {
		t.Fatal(err)
	}
	after, err := m.ContentStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before.TopProvidersTotal) != 2 || len(after.TopProvidersTotal) != 2 {
		t.Fatalf("providers before %+v, after recount %+v; want peer1 and peer2", before.TopProvidersTotal, after.TopProvidersTotal)
	}
}
