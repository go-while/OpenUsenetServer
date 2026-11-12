package feed

import (
	"context"
	"log"
	"strings"
	"sync/atomic"

	"openusenet/internal/config"
	"openusenet/internal/store"
)

// RequirePathIdentity wraps the feeder's article source so that it hands out no queue items
// while this server's name cannot identify it to peers (O-08). Articles go out with the stored
// Path, which starts with the server's own name; a placeholder such as news.example.org makes
// every server started from the example config look like the same site, and a peer that sees it
// in Path skips articles it never had. The startup check covers the peers configured at boot;
// this covers peers added later (portal, CLI, direct DB). Queued items stay queued until the
// name is fixed and the server restarted. The refusal is logged once.
//
// It is a wrapper, not a check inside the Feeder, so it stays out of the drain code.
func RequirePathIdentity(cfg config.Config, arts ArticleSource, lg *log.Logger) ArticleSource {
	if arts == nil {
		return nil
	}
	if lg == nil {
		lg = log.Default()
	}
	return &identityGuard{ArticleSource: arts, cfg: cfg, log: lg}
}

type identityGuard struct {
	ArticleSource
	cfg    config.Config
	log    *log.Logger
	logged atomic.Bool
}

func (g *identityGuard) ClaimFeedDue(ctx context.Context, limit int) ([]store.FeedQueueItem, error) {
	if problems := g.cfg.PathIdentityProblems(); len(problems) > 0 {
		if g.logged.CompareAndSwap(false, true) {
			msgs := make([]string, len(problems))
			for i, p := range problems {
				msgs[i] = p.String()
			}
			g.log.Printf("feed: refusing to feed peers: %s; set it to this server's FQDN and restart (queued articles are kept)",
				strings.Join(msgs, "; "))
		}
		return nil, nil
	}
	return g.ArticleSource.ClaimFeedDue(ctx, limit)
}
