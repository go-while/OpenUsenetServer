package feed

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync/atomic"
	"testing"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// claimCounter counts ClaimFeedDue calls: a feeder that refuses to send never claims queue items.
type claimCounter struct {
	ArticleSource
	claims atomic.Int32
}

func (c *claimCounter) ClaimFeedDue(ctx context.Context, limit int) ([]store.FeedQueueItem, error) {
	c.claims.Add(1)
	return nil, nil
}

func pathIdentCfg(host, path string) config.Config {
	return config.Config{Server: config.Server{Hostname: host, Pathhost: path}}
}

// O-08 follow-up: peers added after startup (portal, CLI, direct DB) must not make the feeder
// send articles whose Path carries a placeholder identity.
func TestFeederRefusesPlaceholderPathIdentity(t *testing.T) {
	var logs bytes.Buffer
	lg := log.New(&logs, "", 0)
	for _, tc := range []struct{ host, path string }{
		{"news.example.org", "news.example.org"},
		{"news.localhost", "news.localhost"},
		{"127.0.0.1", "127.0.0.1"},
		{"news.real-site.org", "news.example.org"}, // pathhost only
		{"news.example.org", "news.real-site.org"}, // hostname only
	} {
		cfg := pathIdentCfg(tc.host, tc.path)
		arts := &claimCounter{}
		f := New(cfg, nil, RequirePathIdentity(cfg, arts, lg), lg)
		f.drainOnce(context.Background())
		f.drainOnce(context.Background())
		if n := arts.claims.Load(); n != 0 {
			t.Errorf("%s/%s: the queue was claimed %d times with a placeholder identity", tc.host, tc.path, n)
		}
	}
	if n := strings.Count(logs.String(), "refusing to feed"); n != 5 {
		t.Errorf("want one log line per feeder (5), got %d:\n%s", n, logs.String())
	}

	cfg := pathIdentCfg("news.real-site.org", "news.real-site.org")
	arts := &claimCounter{}
	New(cfg, nil, RequirePathIdentity(cfg, arts, lg), lg).drainOnce(context.Background())
	if n := arts.claims.Load(); n != 1 {
		t.Errorf("real identity: want the queue claimed once, got %d", n)
	}
}

// Queued items survive the refusal.
func TestFeederKeepsQueueWhilePlaceholderPathIdentity(t *testing.T) {
	ctx := context.Background()
	lg := log.New(&bytes.Buffer{}, "", 0)
	st := store.NewMemory()
	if err := st.EnqueueFeed(ctx, 1, "<q@example>"); err != nil {
		t.Fatal(err)
	}
	cfg := pathIdentCfg("news.example.org", "news.example.org")
	New(cfg, st, RequirePathIdentity(cfg, st, lg), lg).drainOnce(ctx)
	items, err := st.ClaimFeedDue(ctx, 10)
	if err != nil || len(items) != 1 {
		t.Fatalf("queued item should still be there: %v %v", items, err)
	}
}
