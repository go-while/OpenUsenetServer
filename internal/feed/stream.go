package feed

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"openusenet/internal/nntp"
	"openusenet/internal/store"
)

// session is an open connection to one peer, past the greeting, AUTHINFO and the streaming
// decision. A drain carries all of the peer's items on it (O-17: it used to be one
// connection per article), and between drains it is parked in Feeder.idle for the next one.
type session struct {
	key       string // address and credentials it was opened with (Feeder.sessionKey)
	c         net.Conn
	nc        *nntp.Conn
	streaming bool        // CHECK/TAKETHIS, else IHAVE
	timer     *time.Timer // closes it when it stays parked for Feeder.idleFor
	parked    uint64      // counts its parkings; the timer expires only the parking it was set for
}

// job is one article to offer on a session. done receives the peer's answer: accepted, or
// refused (435/437/438/439), or err when the peer could not take it now (431/436) or
// answered something unexpected while the connection stayed in step. A transport error is
// not reported through done by the session code; the caller fails what is left.
type job struct {
	msgid string
	wire  []byte
	done  func(accepted bool, err error)
}

// offer is a command sent on a streaming session whose reply is outstanding.
type offer struct {
	j    *job
	take bool // TAKETHIS, else CHECK
}

// deliverArticle sends one article to a peer on a connection of its own: STREAMING
// (CHECK/TAKETHIS) when the peer advertises it or accepts MODE STREAM, otherwise IHAVE.
// deliverArticle returns accepted=false when the peer refuses the offer (435/438)
// without a transport error. Callers must not report that as a successful send.
func (f *Feeder) deliverArticle(p store.Peer, msgid string, wire []byte) (accepted bool, err error) {
	ctx := context.Background()
	s, err := f.dial(ctx, p)
	if err != nil {
		return false, err
	}
	var jerr error
	j := &job{msgid: msgid, wire: wire, done: func(a bool, e error) { accepted, jerr = a, e }}
	_, _, usable, err := f.exchange(ctx, s, true, p, once(j))
	if !usable {
		_ = s.nc.Close()
	} else {
		s.quit(f.quitWait())
	}
	if err != nil {
		return false, err
	}
	return accepted, jerr
}

// once returns a job source that yields j and then nothing.
func once(j *job) func() *job {
	return func() *job {
		r := j
		j = nil
		return r
	}
}

// sessionKey identifies the connection a peer's current settings ask for. A parked session
// with another key (address, login or password changed since) is not reused.
func (f *Feeder) sessionKey(p store.Peer) string {
	return p.Addr() + "\x00" + f.peerUser(p) + "\x00" + strings.TrimSpace(p.OutgoingPassword)
}

func (f *Feeder) dial(ctx context.Context, p store.Peer) (*session, error) {
	d := net.Dialer{Timeout: f.timeout}
	c, err := d.DialContext(ctx, "tcp", p.Addr())
	if err != nil {
		return nil, err
	}
	return &session{key: f.sessionKey(p), c: c, nc: nntp.NewConn(c, f.timeout)}, nil
}

// exchange offers the jobs from next on s (after the greeting, AUTHINFO and the streaming
// decision when fresh) until next returns nil. The connection is closed as soon as ctx ends
// (the claim cutoff, or the worker's shutdown), whatever step it is in; f.timeout bounds
// each step. left holds the jobs taken from next whose answer did not come, replies counts
// the replies read after the handshake, and usable reports whether s may carry more jobs
// (no error, not cut).
func (f *Feeder) exchange(ctx context.Context, s *session, fresh bool, p store.Peer, next func() *job) (left []*job, replies int, usable bool, err error) {
	stop := context.AfterFunc(ctx, func() { _ = s.c.Close() })
	defer func() {
		cut := !stop()
		if err != nil && ctx.Err() != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				err = fmt.Errorf("delivery cut at the claim deadline: %w", err)
			} else {
				err = fmt.Errorf("delivery cut by shutdown: %w", err)
			}
		}
		usable = err == nil && !cut
	}()
	if fresh {
		if err = f.handshake(s, p); err != nil {
			return nil, 0, false, err
		}
	}
	if s.streaming {
		left, replies, err = s.stream(f.window, next)
	} else {
		left, replies, err = s.ihave(next, nil)
	}
	return left, replies, false, err
}

// handshake reads the greeting, logs in when the peer has an outgoing password and decides
// between streaming and IHAVE.
func (f *Feeder) handshake(s *session, p store.Peer) error {
	code, line, err := s.nc.ReadReply()
	if err != nil {
		return err
	}
	if code != nntp.OKBannerPost && code != nntp.OKBannerNoPost {
		return fmt.Errorf("greeting %s", line)
	}
	if err := f.authPeer(s.nc, p); err != nil {
		return err
	}
	s.streaming, err = peerSupportsStreaming(s.nc)
	return err
}

// stream offers jobs with CHECK and sends TAKETHIS for each 238, with up to window commands
// (CHECK or TAKETHIS) outstanding. A peer answers commands in the order they came (RFC 3977
// section 3.5), so each reply belongs to the oldest outstanding command; RFC 4644 replies
// (238/431/438/239/439) also carry the message-id, which must be that command's, or the
// connection is out of step. Keeping the outstanding commands to window also bounds the replies the peer may have
// queued, so it never blocks writing them while we write an article. When a write fails
// (the peer closed the connection while we were still sending), nothing more is sent, but
// the replies that came before are still read and reported.
func (s *session) stream(window int, next func() *job) ([]*job, int, error) {
	window = max(window, 1)
	var (
		out     []offer // outstanding, oldest first
		want    []*job  // answered 238, TAKETHIS not yet sent
		nocheck []*job  // CHECK not understood (500/501): offered with IHAVE afterwards
		unsent  *job    // its command could not be written
		werr    error   // that write's error
		replies int
		more    = true
	)
	left := func() []*job {
		l := make([]*job, 0, len(out)+len(want)+len(nocheck)+1)
		for _, o := range out {
			l = append(l, o.j)
		}
		l = append(l, want...)
		l = append(l, nocheck...)
		if unsent != nil {
			l = append(l, unsent)
		}
		return l
	}
	for {
		for werr == nil && len(out) < window {
			var o offer
			switch {
			case len(want) > 0:
				o = offer{j: want[0], take: true}
				want = want[1:]
			case more && len(nocheck) == 0:
				j := next()
				if j == nil {
					more = false
					continue
				}
				o = offer{j: j}
			}
			if o.j == nil {
				break
			}
			out = append(out, o)
			if werr = s.send(o); werr != nil {
				out, unsent = out[:len(out)-1], o.j
			}
		}
		if len(out) == 0 {
			if werr != nil {
				return left(), replies, werr
			}
			break
		}
		code, line, err := s.nc.ReadReply()
		if err != nil {
			if werr != nil {
				err = werr
			}
			return left(), replies, err
		}
		replies++
		o := out[0]
		if id := replyID(line); (id != "" || carriesID(code)) && id != o.j.msgid {
			return left(), replies, fmt.Errorf("reply %q to %s: out of step", line, o.command())
		}
		out = out[1:]
		if o.take {
			switch code {
			case nntp.OKTakeThis:
				o.j.done(true, nil)
			case nntp.FailTakeThisReject:
				o.j.done(false, nil)
			default:
				// 400 (closing) or 5xx: a peer that did not take TAKETHIS may have read
				// the article as commands; the connection is not trusted any more.
				return append([]*job{o.j}, left()...), replies, fmt.Errorf("TAKETHIS %s", line)
			}
			continue
		}
		switch code {
		case nntp.OKCheckWant:
			want = append(want, o.j)
		case nntp.FailCheckRefuse:
			o.j.done(false, nil)
		case nntp.FailCheckDefer:
			o.j.done(false, fmt.Errorf("peer deferred CHECK: %s", line))
		case nntp.ErrCommand, nntp.ErrSyntax:
			nocheck = append(nocheck, o.j)
		default:
			o.j.done(false, fmt.Errorf("CHECK %s", line))
		}
	}
	if len(nocheck) == 0 {
		return nil, replies, nil
	}
	// The peer advertised streaming but does not know CHECK: IHAVE from here on, for the
	// rest of this connection's life.
	s.streaming = false
	l, n, err := s.ihave(next, nocheck)
	return l, replies + n, err
}

func (o offer) command() string {
	if o.take {
		return "TAKETHIS " + o.j.msgid
	}
	return "CHECK " + o.j.msgid
}

func (s *session) send(o offer) error {
	if err := s.nc.ReplyRaw(o.command()); err != nil {
		return err
	}
	if o.take {
		return s.nc.WriteDot(o.j.wire)
	}
	return nil
}

// replyID returns the message-id a reply line carries after its code ("" when none).
func replyID(line string) string {
	if f := strings.Fields(line); len(f) > 1 && strings.HasPrefix(f[1], "<") {
		return f[1]
	}
	return ""
}

// carriesID reports whether a CHECK/TAKETHIS reply with code must name the offered
// message-id as its first argument (RFC 4644 sections 2.4.1 and 2.5.1). A reply without it
// cannot be matched to an article, so it puts the connection out of step like a wrong one
// (ous-sol's review). 500/501 to CHECK (not understood: IHAVE fallback) and other errors
// such as 400 carry none.
func carriesID(code int) bool {
	switch code {
	case nntp.OKCheckWant, nntp.FailCheckDefer, nntp.FailCheckRefuse, nntp.OKTakeThis, nntp.FailTakeThisReject:
		return true
	}
	return false
}

// ihave offers queued and then the jobs from next with IHAVE, one after the other.
func (s *session) ihave(next func() *job, queued []*job) ([]*job, int, error) {
	replies := 0
	for {
		var j *job
		if len(queued) > 0 {
			j, queued = queued[0], queued[1:]
		} else if j = next(); j == nil {
			return nil, replies, nil
		}
		accepted, jerr, n, err := ihaveOne(s.nc, j.msgid, j.wire)
		replies += n
		if err != nil {
			return append([]*job{j}, queued...), replies, err
		}
		j.done(accepted, jerr)
	}
}

// ihaveOne offers one article with IHAVE. jerr is the peer's answer when it cannot take the
// article now (436) or answers something unexpected, the connection still in step; err is a
// transport error. replies counts the reply lines read.
func ihaveOne(nc *nntp.Conn, msgid string, wire []byte) (accepted bool, jerr error, replies int, err error) {
	if err := nc.ReplyRaw("IHAVE " + msgid); err != nil {
		return false, nil, 0, err
	}
	code, line, err := nc.ReadReply()
	if err != nil {
		return false, nil, 0, err
	}
	switch code {
	case nntp.ContIHave:
	case nntp.FailIHaveRefuse, nntp.FailIHaveReject:
		return false, nil, 1, nil
	case nntp.FailIHaveDefer:
		return false, fmt.Errorf("peer deferred: %s", line), 1, nil
	default:
		return false, fmt.Errorf("IHAVE %s", line), 1, nil
	}
	if err := nc.WriteDot(wire); err != nil {
		return false, nil, 1, err
	}
	code, line, err = nc.ReadReply()
	if err != nil {
		return false, nil, 1, err
	}
	switch code {
	case nntp.OKIHave:
		return true, nil, 2, nil
	case nntp.FailIHaveRefuse, nntp.FailIHaveReject:
		return false, nil, 2, nil
	case nntp.FailIHaveDefer:
		return false, fmt.Errorf("peer deferred: %s", line), 2, nil
	default:
		return false, fmt.Errorf("after transfer %s", line), 2, nil
	}
}

// alive reports whether a parked connection is still open: the peer has neither closed it
// (peers time out idle feeds) nor sent anything (a 400 before closing) since the last drain.
// Input already read ahead into the reader counts too (ous-sol's review: a 400 that came
// with the last 239 sits there, not on the socket).
func (s *session) alive() bool {
	if s.nc.Buffered() > 0 {
		return false
	}
	_ = s.c.SetReadDeadline(time.Now().Add(time.Millisecond))
	var b [1]byte
	n, err := s.c.Read(b[:])
	var ne net.Error
	return n == 0 && errors.As(err, &ne) && ne.Timeout()
}

// quit ends the session with QUIT, waiting at most wait for the reply.
func (s *session) quit(wait time.Duration) {
	t := time.AfterFunc(wait, func() { _ = s.c.Close() })
	if s.nc.ReplyRaw("QUIT") == nil {
		_, _, _ = s.nc.ReadReply()
	}
	t.Stop()
	_ = s.nc.Close()
}

func (f *Feeder) quitWait() time.Duration { return min(f.timeout, 2*time.Second) }

// takeSession returns the parked session of peer id when it is still open and was opened
// with key, else nil (a stale one is closed).
func (f *Feeder) takeSession(id int64, key string) *session {
	f.connMu.Lock()
	s := f.idle[id]
	delete(f.idle, id)
	f.connMu.Unlock()
	if s == nil {
		return nil
	}
	s.timer.Stop() // when it already fired, expire finds s gone (or parked anew) and leaves it alone
	if s.key != key {
		s.quit(f.quitWait())
		return nil
	}
	if !s.alive() {
		_ = s.nc.Close()
		return nil
	}
	return s
}

// dropSession closes the parked session of peer id, if any (the peer was disabled): no
// session has the empty key, so takeSession closes it.
func (f *Feeder) dropSession(id int64) { _ = f.takeSession(id, "") }

// park keeps s for the next drain to peer id, for at most f.idleFor. After the worker's
// shutdown (closeIdle) nothing is parked; s is closed instead.
func (f *Feeder) park(id int64, s *session) {
	f.connMu.Lock()
	if f.closed || f.idleFor <= 0 {
		f.connMu.Unlock()
		s.quit(f.quitWait())
		return
	}
	if f.idle == nil {
		f.idle = map[int64]*session{}
	}
	old := f.idle[id] // none: one drain at a time takes and parks
	f.idle[id] = s
	s.parked++
	gen := s.parked
	s.timer = time.AfterFunc(f.idleFor, func() { f.expire(id, s, gen) })
	f.connMu.Unlock()
	if old != nil {
		old.timer.Stop()
		old.quit(f.quitWait())
	}
}

// expire closes s when it is still in its gen-th parking for peer id (its idle time ran
// out). A timer whose callback came late, while a drain took s and parked it again, must
// not expire the new parking (ous-sol's review).
func (f *Feeder) expire(id int64, s *session, gen uint64) {
	f.connMu.Lock()
	if f.idle[id] != s || s.parked != gen {
		f.connMu.Unlock()
		return
	}
	delete(f.idle, id)
	f.connMu.Unlock()
	s.quit(f.quitWait())
}

// closeIdle closes every parked session and parks none from now on (RunWorker's shutdown).
func (f *Feeder) closeIdle() {
	f.connMu.Lock()
	f.closed = true
	idle := f.idle
	f.idle = nil
	f.connMu.Unlock()
	var wg sync.WaitGroup
	for _, s := range idle {
		s.timer.Stop()
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.quit(f.quitWait())
		}()
	}
	wg.Wait()
}

func (f *Feeder) peerUser(p store.Peer) string {
	if user := strings.TrimSpace(p.Name); user != "" {
		return user
	}
	return f.cfg.Server.Pathhost
}

func (f *Feeder) authPeer(nc *nntp.Conn, p store.Peer) error {
	pass := strings.TrimSpace(p.OutgoingPassword)
	if pass == "" {
		return nil
	}
	user := f.peerUser(p)
	if err := nc.ReplyRaw("AUTHINFO USER " + user); err != nil {
		return err
	}
	code, line, err := nc.ReadReply()
	if err != nil {
		return err
	}
	if code != nntp.ContAuthPass {
		return fmt.Errorf("AUTHINFO USER %s", line)
	}
	if err := nc.ReplyRaw("AUTHINFO PASS " + pass); err != nil {
		return err
	}
	code, line, err = nc.ReadReply()
	if err != nil {
		return err
	}
	if code != nntp.OKAuth {
		return fmt.Errorf("AUTHINFO PASS %s", line)
	}
	return nil
}

func peerSupportsStreaming(nc *nntp.Conn) (bool, error) {
	if err := nc.ReplyRaw("CAPABILITIES"); err != nil {
		return false, err
	}
	code, _, err := nc.ReadReply()
	if err != nil {
		return false, err
	}
	if code != nntp.InfoCapabilities {
		// Peer without CAPABILITIES (e.g. Diablo answers 500): fall back
		// to the legacy MODE STREAM probe (RFC 4644 section 2.3).
		return peerAcceptsModeStream(nc)
	}
	streaming := false
	for {
		line, err := nc.ReadLine()
		if err != nil {
			return false, err
		}
		if line == "." {
			break
		}
		line = strings.TrimPrefix(line, ".")
		fields := strings.Fields(line)
		if len(fields) > 0 && strings.EqualFold(fields[0], "STREAMING") {
			streaming = true
		}
	}
	return streaming, nil
}

// peerAcceptsModeStream sends MODE STREAM and reports whether the peer
// answered 203. Any other reply (500/501/...) means IHAVE only; exactly one
// reply line is read so the connection stays usable for IHAVE.
func peerAcceptsModeStream(nc *nntp.Conn) (bool, error) {
	if err := nc.ReplyRaw("MODE STREAM"); err != nil {
		return false, err
	}
	code, _, err := nc.ReadReply()
	if err != nil {
		return false, err
	}
	return code == nntp.OKStreamMode, nil
}
