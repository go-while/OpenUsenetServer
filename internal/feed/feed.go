package feed

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
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
	window  int           // CHECK/TAKETHIS commands outstanding at once on a streaming connection
	idleFor time.Duration // a peer's connection stays open this long after a drain (O-17)
	running atomic.Bool   // a drain is running (one at a time, O-20)
	pending atomic.Bool   // a drain was requested; the running one goes once more
	active  sync.Mutex    // held by the running drain; shutdown waits for it

	// base ends when RunWorker returns: every drain (also one an Offer started) stops, its
	// connection is cut, and nothing more is reported.
	base     context.Context
	shutdown context.CancelFunc

	connMu sync.Mutex
	idle   map[int64]*session // parked connections by peer ID, for the next drain
	closed bool               // RunWorker returned: nothing is parked any more

	offered atomic.Int64
	ok      atomic.Int64
	fail    atomic.Int64
	last    atomic.Value // string
}

func New(cfg config.Config, peers PeerSource, arts ArticleSource, lg *log.Logger) *Feeder {
	if lg == nil {
		lg = log.Default()
	}
	base, shutdown := context.WithCancel(context.Background())
	return &Feeder{cfg: cfg, peers: peers, arts: arts, log: lg, timeout: 15 * time.Second,
		tick: 5 * time.Second, lease: 2 * time.Minute, window: 16, idleFor: time.Minute,
		base: base, shutdown: shutdown}
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

// RunWorker drains the durable feed queue until ctx is done. Then it shuts the feeder
// down: a drain still running (started by an Offer) is cancelled and awaited, the
// connections kept open for the next drain are closed, and no drain runs any more.
func (f *Feeder) RunWorker(ctx context.Context) {
	if f == nil || f.arts == nil {
		return
	}
	defer f.stop()
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
		f.active.Lock()
		// The drain also ends with the feeder (shutdown cancels base): its connection is
		// cut and nothing more is reported (ous-sol's review: a drain an Offer started
		// used to run on after RunWorker returned).
		dctx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(f.base, cancel)
		for dctx.Err() == nil && f.base.Err() == nil && f.pending.Swap(false) {
			if f.drainOnce(dctx) {
				// The batch was full, so more may be due: go on without waiting for the
				// next Offer or worker tick (a burst of Offers collapses into pending).
				f.pending.Store(true)
			}
		}
		ended := dctx.Err() != nil || f.base.Err() != nil
		stop()
		cancel()
		f.active.Unlock()
		f.running.Store(false)
		// A request that came after the last Swap but before running was cleared saw a
		// running drain and returned; pick it up here (unless another drain already did).
		// A cancelled ctx leaves it pending for the next drain.
		if ended || !f.pending.Load() {
			return
		}
	}
}

// stop shuts the feeder down (RunWorker's end): it cancels a running drain and waits for
// it, then closes the parked connections. Nothing is parked or drained afterwards.
func (f *Feeder) stop() {
	f.shutdown()
	f.active.Lock() // waits for the running drain to end
	f.active.Unlock()
	f.closeIdle()
}

// feedBatch is the number of items one claim takes.
const feedBatch = 32

// drainOnce claims and delivers one batch. It reports whether the batch was full and every
// peer's items were handled, i.e. whether another claim may find more due items.
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
	// connection is cut at cutoff, and once less than one step timeout is left no new item
	// is started; the rest stays claimed and is due again when the lease ends.
	cutoff := claimed.Add(f.lease - f.lease/8)
	var order []int64
	byPeer := map[int64][]store.FeedQueueItem{}
	for _, it := range items {
		if _, ok := byPeer[it.PeerID]; !ok {
			order = append(order, it.PeerID)
		}
		byPeer[it.PeerID] = append(byPeer[it.PeerID], it)
	}
	for _, id := range order {
		if ctx.Err() != nil || time.Until(cutoff) < f.timeout {
			return false
		}
		f.deliverPeer(ctx, id, byPeer[id], cutoff)
	}
	return len(items) == feedBatch && ctx.Err() == nil
}

// deliverPeer delivers the claimed items of one peer over one connection (O-17): the one
// parked by the last drain when it is still open and the peer's address and credentials
// are unchanged, else a new one; afterwards it is parked for the next drain. Every result
// is reported as the peer answers, with report's rules: store calls end at cutoff, and
// nothing is reported once cutoff has passed. A transport error fails the items of the
// peer not answered yet and drops the connection; the next drain connects again.
func (f *Feeder) deliverPeer(ctx context.Context, peerID int64, items []store.FeedQueueItem, cutoff time.Time) {
	ctx, cancel := context.WithDeadline(ctx, cutoff)
	defer cancel()
	peer, err := f.arts.GetPeer(ctx, peerID)
	if err != nil {
		for _, it := range items {
			f.report(ctx, it, cutoff, false, "", err)
		}
		return
	}
	label := fmt.Sprintf("peer %d", peerID)
	if peer != nil && peer.Name != "" {
		label = peer.Name
	}
	if peer == nil || !peer.Enabled {
		f.dropSession(peerID)
		for _, it := range items {
			f.report(ctx, it, cutoff, false, fmt.Sprintf("not sent %s -> %s (peer disabled)", it.MessageID, label), nil)
		}
		return
	}
	where := peer.Addr()

	// next hands the session the next article to offer: first the jobs taken back after a
	// failed attempt, then the items in claim order, each loaded and checked against the
	// peer's patterns and flags just before it is offered. Once less than one step timeout
	// is left before cutoff it starts nothing new.
	var queued []*job
	idx := 0
	next := func() *job {
		if len(queued) > 0 {
			j := queued[0]
			queued = queued[1:]
			return j
		}
		for idx < len(items) && ctx.Err() == nil && time.Until(cutoff) >= f.timeout {
			it := items[idx]
			idx++
			wire, skip, err := f.prepare(ctx, *peer, label, it)
			if err != nil || skip != "" {
				f.report(ctx, it, cutoff, false, skip, err)
				continue
			}
			return &job{msgid: it.MessageID, wire: wire, done: func(accepted bool, err error) {
				switch {
				case err != nil:
					f.report(ctx, it, cutoff, false, "", err)
				case accepted:
					f.report(ctx, it, cutoff, true, fmt.Sprintf("accepted %s -> %s (%s)", it.MessageID, label, where), nil)
				default:
					f.report(ctx, it, cutoff, false, fmt.Sprintf("refused %s -> %s (%s)", it.MessageID, label, where), nil)
				}
			}}
		}
		return nil
	}
	first := next()
	if first == nil {
		return // nothing to offer: no connection needed
	}
	queued = append(queued, first)

	s := f.takeSession(peerID, f.sessionKey(*peer))
	for {
		fresh := s == nil
		if fresh {
			if s, err = f.dial(ctx, *peer); err != nil {
				break
			}
		}
		var left []*job
		var replies int
		var usable bool
		left, replies, usable, err = f.exchange(ctx, s, fresh, *peer, next)
		if err == nil {
			if fresh {
				mode := "IHAVE"
				if s.streaming {
					mode = "streaming"
				}
				f.log.Printf("feed %s: connected to %s (%s)", label, where, mode)
			}
			if usable {
				f.park(peerID, s)
			} else {
				_ = s.nc.Close()
			}
			return
		}
		_ = s.nc.Close()
		queued = append(left, queued...)
		if fresh || replies > 0 || ctx.Err() != nil {
			break
		}
		// The parked connection broke since the last drain (it passed the liveness check,
		// but the peer closed it meanwhile): nothing was answered on it, so the same jobs
		// go out once more on a new connection.
		f.log.Printf("feed %s: parked connection to %s lost (%v), reconnecting", label, where, err)
		s = nil
	}
	for _, j := range queued {
		j.done(false, err)
	}
	for _, it := range items[idx:] {
		f.report(ctx, it, cutoff, false, "", err)
	}
}

// report records the result of one claimed item, before cutoff: ctx (which ends at cutoff)
// bounds the store call, and once cutoff has passed nothing is reported. The item is then
// left for the next claim (a second offer is refused by the peer as a duplicate, the safe
// side), instead of completing or rescheduling an item that another process may have
// claimed again. The same goes once ctx is cancelled (the feeder shuts down). err
// reschedules the item with a backoff; else it is done, and summary (when set) becomes
// Stats.Last.
func (f *Feeder) report(ctx context.Context, it store.FeedQueueItem, cutoff time.Time, accepted bool, summary string, err error) {
	if !time.Now().Before(cutoff) {
		f.log.Printf("feed %s peer=%d: claim deadline passed, left for the next claim (%v)", it.MessageID, it.PeerID, err)
		return
	}
	if ctx.Err() != nil {
		f.log.Printf("feed %s peer=%d: stopped (%v), left for the next claim (%v)", it.MessageID, it.PeerID, ctx.Err(), err)
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

// prepare loads the article of it and decides whether peer gets it: the wire form to offer,
// or a summary why not (the item is then done), or the store's error.
func (f *Feeder) prepare(ctx context.Context, peer store.Peer, label string, it store.FeedQueueItem) (wire []byte, skip string, err error) {
	art, err := f.arts.GetByMsgID(ctx, it.MessageID)
	if err != nil {
		return nil, "", err
	}
	if art == nil {
		return nil, fmt.Sprintf("not sent %s -> %s (article missing)", it.MessageID, label), nil
	}
	wire = []byte(art.Headers + "\r\n\r\n" + art.Body)
	parsed, _ := article.Parse(wire)
	view := ViewFromArticle(parsed, len(wire))
	if parsed == nil {
		view = ArticleView{Bytes: len(wire)}
	}
	view.GroupStatus = f.groupStatus(ctx, view.Groups)
	if !PeerWantsArticle(peer, view) {
		return nil, fmt.Sprintf("not sent %s -> %s (patterns/flags)", it.MessageID, label), nil
	}
	return wire, "", nil
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
