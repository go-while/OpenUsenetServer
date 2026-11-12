package feed

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"openusenet/internal/article"
	"openusenet/internal/config"
	"openusenet/internal/store"
)

// PeerSource supplies outbound feed destinations (usually the DB).
type PeerSource interface {
	ListEnabledPeers(ctx context.Context) ([]store.Peer, error)
}

// ArticleSource loads stored articles for outbound transfer.
type ArticleSource interface {
	GetByMsgID(ctx context.Context, msgid string) (*store.StoredArticle, error)
	EnqueueFeed(ctx context.Context, peerID int64, msgid string) error
	ClaimFeedDue(ctx context.Context, limit int) ([]store.FeedQueueItem, error)
	CompleteFeed(ctx context.Context, id int64) error
	FailFeed(ctx context.Context, id int64, errMsg string, retryAfter time.Duration) error
	GetPeer(ctx context.Context, id int64) (*store.Peer, error)
	FeedQueueStats(ctx context.Context) (store.FeedQueueStats, error)
	GetGroup(ctx context.Context, name string) (*store.Group, error)
}

// Feeder offers newly accepted articles to configured peers via durable queue
// and CHECK/TAKETHIS (streaming) or IHAVE.
type Feeder struct {
	cfg     config.Config
	peers   PeerSource
	arts    ArticleSource
	log     *log.Logger
	timeout time.Duration
	tick    time.Duration // worker drain interval
	lease   time.Duration // claim lease of ClaimFeedDue (store: 2 minutes)
	running atomic.Bool   // a drain is running (one at a time, O-20)
	pending atomic.Bool   // a drain was requested; the running one goes once more
	offered atomic.Int64
	ok      atomic.Int64
	fail    atomic.Int64
	last    atomic.Value // string
}

func New(cfg config.Config, peers PeerSource, arts ArticleSource, lg *log.Logger) *Feeder {
	if lg == nil {
		lg = log.Default()
	}
	return &Feeder{cfg: cfg, peers: peers, arts: arts, log: lg, timeout: 15 * time.Second,
		tick: 5 * time.Second, lease: 2 * time.Minute}
}

// Offer enqueues the article for peers whose Path/patterns/flags allow it.
func (f *Feeder) Offer(msgid, path string, groups []string, wire []byte) {
	if f == nil || msgid == "" || f.arts == nil {
		return
	}
	var list []store.Peer
	if f.peers != nil {
		var err error
		list, err = f.peers.ListEnabledPeers(context.Background())
		if err != nil {
			f.log.Printf("feed list peers: %v", err)
			return
		}
	}
	view := ArticleView{Groups: groups, Path: path, Bytes: len(wire)}
	if art, err := article.Parse(wire); err == nil && art != nil {
		view = ViewFromArticle(art, len(wire))
		if path != "" {
			view.Path = path
		}
		if len(groups) > 0 {
			view.Groups = groups
		}
	}
	view.GroupStatus = f.groupStatus(context.Background(), view.Groups)

	for _, p := range list {
		if skipPeer(f.cfg, p, view.Path) {
			continue
		}
		if !PeerWantsArticle(p, view) {
			continue
		}
		if err := f.arts.EnqueueFeed(context.Background(), p.ID, msgid); err != nil {
			f.log.Printf("feed enqueue %s -> %d: %v", msgid, p.ID, err)
			continue
		}
		f.offered.Add(1)
	}
	go f.drain(context.Background())
}

func (f *Feeder) groupStatus(ctx context.Context, groups []string) map[string]string {
	if f.arts == nil || len(groups) == 0 {
		return nil
	}
	out := make(map[string]string, len(groups))
	for _, g := range groups {
		gr, err := f.arts.GetGroup(ctx, g)
		if err != nil || gr == nil {
			continue
		}
		out[g] = gr.Status
	}
	return out
}

// RunWorker drains the durable feed queue until ctx is done.
func (f *Feeder) RunWorker(ctx context.Context) {
	if f == nil || f.arts == nil {
		return
	}
	t := time.NewTicker(f.tick)
	defer t.Stop()
	for {
		f.drain(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// drain runs drainOnce unless a drain is already running. Only one drain runs at a time
// (O-20: every Offer used to start its own drain, each with its own connections, and
// overlapping drains re-claimed each other's items once the lease ran out, O-21). A request
// that arrives while a drain runs is not lost: it sets pending, and the running drain goes
// once more before it returns.
func (f *Feeder) drain(ctx context.Context) {
	f.pending.Store(true)
	for f.running.CompareAndSwap(false, true) {
		for ctx.Err() == nil && f.pending.Swap(false) {
			if f.drainOnce(ctx) {
				// The batch was full, so more may be due: go on without waiting for the
				// next Offer or worker tick (a burst of Offers collapses into pending).
				f.pending.Store(true)
			}
		}
		f.running.Store(false)
		// A request that came after the last Swap but before running was cleared saw a
		// running drain and returned; pick it up here (unless another drain already did).
		// A cancelled ctx leaves it pending for the next drain.
		if ctx.Err() != nil || !f.pending.Load() {
			return
		}
	}
}

// feedBatch is the number of items one claim takes.
const feedBatch = 32

// drainOnce claims and delivers one batch. It reports whether the batch was full and was
// delivered to its end, i.e. whether another claim may find more due items.
func (f *Feeder) drainOnce(ctx context.Context) (more bool) {
	// The store leases claimed items for f.lease from its own clock; taking the time
	// before the claim keeps this estimate at or before the store's lease end.
	claimed := time.Now()
	items, err := f.arts.ClaimFeedDue(ctx, feedBatch)
	if err != nil {
		f.log.Printf("feed claim: %v", err)
		return false
	}
	// No item may still be in delivery when its lease runs out, or a drain elsewhere
	// (another process on the same queue) could claim and send it again (O-21). Each
	// delivery is cut at cutoff, and once less than one step timeout is left the batch
	// stops; the rest stays claimed and is due again when the lease ends.
	cutoff := claimed.Add(f.lease - f.lease/8)
	for _, it := range items {
		if time.Until(cutoff) < f.timeout {
			return false
		}
		f.deliverClaimed(ctx, it, cutoff)
	}
	return len(items) == feedBatch && ctx.Err() == nil
}

// deliverClaimed delivers one claimed item and reports the result, all before cutoff: the
// store lookups and the report get a context that ends at cutoff, and once cutoff has
// passed no result is reported. The item is then left for the next claim (a second offer
// is refused by the peer as a duplicate, the safe side), instead of completing or
// rescheduling an item that another process may have claimed again.
func (f *Feeder) deliverClaimed(ctx context.Context, it store.FeedQueueItem, cutoff time.Time) {
	ctx, cancel := context.WithDeadline(ctx, cutoff)
	defer cancel()
	accepted, summary, err := f.deliver(ctx, it, cutoff)
	if !time.Now().Before(cutoff) {
		f.log.Printf("feed %s peer=%d: claim deadline passed, left for the next claim (%v)", it.MessageID, it.PeerID, err)
		return
	}
	if err != nil {
		f.fail.Add(1)
		backoff := retryBackoff(it.Attempts)
		_ = f.arts.FailFeed(ctx, it.ID, err.Error(), backoff)
		f.last.Store(fmt.Sprintf("error %s -> peer %d: %s", it.MessageID, it.PeerID, err.Error()))
		f.log.Printf("feed %s peer=%d: %v (retry in %s)", it.MessageID, it.PeerID, err, backoff)
		return
	}
	_ = f.arts.CompleteFeed(ctx, it.ID)
	if summary != "" {
		f.last.Store(summary)
	}
	if accepted {
		f.ok.Add(1)
	}
}

func retryBackoff(attempts int) time.Duration {
	d := time.Duration(attempts) * time.Minute
	if d < 30*time.Second {
		d = 30 * time.Second
	}
	if d > 30*time.Minute {
		d = 30 * time.Minute
	}
	return d
}

func (f *Feeder) deliver(ctx context.Context, it store.FeedQueueItem, deadline time.Time) (accepted bool, summary string, err error) {
	peer, err := f.arts.GetPeer(ctx, it.PeerID)
	if err != nil {
		return false, "", err
	}
	label := fmt.Sprintf("peer %d", it.PeerID)
	if peer != nil && peer.Name != "" {
		label = peer.Name
	}
	if peer == nil || !peer.Enabled {
		// deliverClaimed completes the item (before the claim deadline).
		return false, fmt.Sprintf("not sent %s -> %s (peer disabled)", it.MessageID, label), nil
	}
	art, err := f.arts.GetByMsgID(ctx, it.MessageID)
	if err != nil {
		return false, "", err
	}
	if art == nil {
		return false, fmt.Sprintf("not sent %s -> %s (article missing)", it.MessageID, label), nil
	}
	wire := []byte(art.Headers + "\r\n\r\n" + art.Body)
	parsed, _ := article.Parse(wire)
	view := ViewFromArticle(parsed, len(wire))
	if parsed == nil {
		view = ArticleView{Bytes: len(wire)}
	}
	view.GroupStatus = f.groupStatus(ctx, view.Groups)
	if !PeerWantsArticle(*peer, view) {
		return false, fmt.Sprintf("not sent %s -> %s (patterns/flags)", it.MessageID, label), nil
	}
	accepted, err = f.deliverArticleBy(*peer, it.MessageID, wire, deadline)
	if err != nil {
		return false, "", err
	}
	where := peer.Addr()
	if accepted {
		return true, fmt.Sprintf("accepted %s -> %s (%s)", it.MessageID, label, where), nil
	}
	return false, fmt.Sprintf("refused %s -> %s (%s)", it.MessageID, label, where), nil
}

func skipPeer(cfg config.Config, p store.Peer, path string) bool {
	host := strings.TrimSpace(p.Host)
	if host == "" {
		return true
	}
	token := strings.TrimSpace(p.PathToken)
	if token == "" {
		token = host
	}
	if strings.EqualFold(host, cfg.Server.Hostname) || strings.EqualFold(host, cfg.Server.Pathhost) {
		return true
	}
	flags := ParseFlags(p.Flags)
	for _, token := range strings.Split(token, ",") {
		if article.PathContains(path, token) {
			return true
		}
	}
	if !flags.PathOnlyExclude {
		// Without Ap, also treat sitename and host as Path exclusions.
		if article.PathContains(path, host) {
			return true
		}
		if name := strings.TrimSpace(p.Name); name != "" && article.PathContains(path, name) {
			return true
		}
	} else if article.PathContains(path, host) && strings.EqualFold(host, token) {
		// Host doubles as path identity.
		return true
	}
	if article.PathContains(path, cfg.Server.Pathhost) && strings.EqualFold(host, cfg.Server.Pathhost) {
		return true
	}
	return false
}

type Stats struct {
	Offered int64  `json:"offered"`
	OK      int64  `json:"ok"`
	Fail    int64  `json:"fail"`
	Last    string `json:"last"`
}

func (f *Feeder) Stats() Stats {
	if f == nil {
		return Stats{}
	}
	s := Stats{Offered: f.offered.Load(), OK: f.ok.Load(), Fail: f.fail.Load()}
	if v, ok := f.last.Load().(string); ok {
		s.Last = v
	}
	return s
}
