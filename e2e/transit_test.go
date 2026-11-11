//go:build e2e

package e2e

// Kit-based counterparts of the transit, control and moderation cases in
// internal/nntp/session_test.go, plus internal/server's TestPOSTFeedsIHAVE.
// These use real TCP listeners. Keep the moderation tests non-parallel: the
// existing delivery-capture hook is process-wide and restored at cleanup.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	testkit "github.com/go-while/nntp-testkit"
	"github.com/go-while/nntp-testkit/article"

	"openusenet/internal/config"
	"openusenet/internal/mail"
	"openusenet/internal/moderate"
	"openusenet/internal/store"
)

func TestTransitIHaveAcceptAndDuplicate(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	const msgid = "<transit-ihave@e2e.invalid>"
	c.ExpectIhave(235, msgid, testArticle(t, msgid, group).Bytes())
	c.ExpectCmd(220, "ARTICLE %s", msgid)
	lines := c.ReadDotLines()
	if !strings.Contains(strings.Join(lines, "\r\n"), "Path: "+pathhost+"!e2e-peer!not-for-mail\r\n") {
		t.Fatalf("local Path hop missing from article: %q", lines)
	}
	c.ExpectCmd(435, "IHAVE %s", msgid)
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 1 {
		t.Fatalf("duplicate changed article count: %d, %v", n, err)
	}
}

func TestTransitIHavePathLoop(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	const msgid = "<transit-path-loop@e2e.invalid>"
	art := testArticle(t, msgid, group).Set("Path", pathhost+"!other!not-for-mail")
	c.ExpectIhave(437, msgid, art.Bytes()) // 437 after the article (335 first)
	c.ExpectCmd(430, "ARTICLE %s", msgid)
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 0 {
		t.Fatalf("path-loop article was stored: %d, %v", n, err)
	}
}

func TestTransitCheckAndTakeThis(t *testing.T) {
	e := startEnv(t)
	c := e.dial()
	const msgid = "<transit-stream@e2e.invalid>"
	c.ExpectCmd(203, "MODE STREAM")
	if line := c.ExpectCmd(238, "CHECK %s", msgid); !strings.HasPrefix(line, "238 "+msgid+" ") {
		t.Fatalf("CHECK reply does not identify the article: %q", line)
	}
	if line := c.ExpectTakethis(239, msgid, testArticle(t, msgid, group).Bytes()); !strings.HasPrefix(line, "239 "+msgid+" ") {
		t.Fatalf("TAKETHIS reply does not identify the article: %q", line)
	}
	if line := c.ExpectCmd(438, "CHECK %s", msgid); !strings.HasPrefix(line, "438 "+msgid+" ") {
		t.Fatalf("duplicate CHECK reply does not identify the article: %q", line)
	}
	c.ExpectCmd(220, "ARTICLE %s", msgid)
	if lines := c.ReadDotLines(); !strings.Contains(strings.Join(lines, "\n"), "e2e body line 1\ne2e body line 2") {
		t.Fatalf("streamed article body missing: %q", lines)
	}
}

func TestTransitIHaveBannedGroup(t *testing.T) {
	e := startEnv(t)
	e.addGroup("alt.test", "y")
	if err := e.Store.AddGroupBan(context.Background(), "local.*"); err != nil {
		t.Fatal(err)
	}
	c := e.dial()
	for i, groups := range []string{group, group + ",alt.test"} {
		msgid := fmt.Sprintf("<transit-banned-%d@e2e.invalid>", i)
		c.ExpectIhave(437, msgid, testArticle(t, msgid, groups).Bytes()) // banned group or crosspost
		c.ExpectCmd(430, "ARTICLE %s", msgid)
		if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 0 {
			t.Fatalf("%s was stored despite a banned group: %d, %v", groups, n, err)
		}
	}
}

func TestTransitDeniedPeerTransferSendsOneReply(t *testing.T) {
	e := startEnv(t, func(cfg *config.Config) {
		closed := false
		cfg.Inbound.Open = &closed
		cfg.Inbound.Allow = []string{"192.0.2.0/24"} // excludes the loopback client
	})
	c := e.dial()
	const msgid = "<transit-denied@e2e.invalid>"
	line := c.ExpectCmd(438, "check %s", msgid)
	if line != "438 "+msgid+" transfer not permitted from your address" {
		t.Fatalf("CHECK denial: %q", line)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("extra reply after denied CHECK: %v", err)
	}
	c.ExpectCmd(111, "DATE")
	if line = c.ExpectCmd(502, "IHAVE %s", msgid); line != "502 transfer not permitted from your address" {
		t.Fatalf("IHAVE denial: %q", line)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("extra reply after denied IHAVE: %v", err)
	}
	c.ExpectCmd(111, "DATE")
	// TAKETHIS must consume its article even when admission will be refused.
	if line = c.ExpectTakethis(439, msgid, testArticle(t, msgid, group).Bytes()); line != "439 "+msgid+" transfer not permitted from your address" {
		t.Fatalf("TAKETHIS denial: %q", line)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("extra reply after denied TAKETHIS: %v", err)
	}
	c.ExpectCmd(111, "DATE")
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 0 {
		t.Fatalf("denied transfer was stored: %d, %v", n, err)
	}
}

func TestTransitCheckAuthRequiredSendsOneReply(t *testing.T) {
	e := startEnv(t)
	e.addPeer(store.Peer{Host: "127.0.0.1", IncomingHost: "127.0.0.1", Enabled: true, IncomingPassword: "peer-secret"})
	c := e.dial()
	if line := c.ExpectCmd(480, "CHECK <transit-auth@e2e.invalid>"); line != "480 Authentication required for IHAVE" {
		t.Fatalf("CHECK requiring peer authentication: %q", line)
	}
	if err := c.ExpectNoPending(50 * time.Millisecond); err != nil {
		t.Fatalf("extra reply after CHECK requiring authentication: %v", err)
	}
	c.ExpectCmd(111, "DATE")
}

func TestTransitIHaveNewgroupControl(t *testing.T) {
	e := startEnv(t)
	e.addGroup("control.newgroup", "y")
	c := e.dial()
	const msgid = "<transit-newgroup@e2e.invalid>"
	const newGroup = "demo.control.test"
	art := testArticle(t, msgid, "control.newgroup", "Control: newgroup "+newGroup).
		SetBodyLines("For your newsgroups file:", newGroup+"\tDemo control-created group")
	c.ExpectIhave(235, msgid, art.Bytes())
	g, err := e.Store.GetGroup(context.Background(), newGroup)
	if err != nil || g == nil {
		t.Fatalf("control-created group missing: %+v, %v", g, err)
	}
	if g.Status != "y" || g.Origin != store.OriginControl || g.Description != "Demo control-created group" {
		t.Fatalf("control-created group metadata: %+v", g)
	}
	c.ExpectCmd(211, "GROUP %s", newGroup)
}

func TestTransitPostMailsModeratedGroup(t *testing.T) {
	e := startEnv(t)
	const moderatedGroup = "misc.test.moderated"
	e.addGroup(moderatedGroup, "m")
	c := e.dial()
	login(c, user, pass)
	plain := article.ParseLines([]string{"From: tester@example.invalid", "Newsgroups: " + moderatedGroup, "Subject: please approve", "", "hello"}).Bytes()
	// unconfigured moderator mail: 441 after the article
	if line := c.ExpectPost(441, plain); !strings.Contains(line, "mail server is not configured") {
		t.Fatalf("unconfigured mail error: %q", line)
	}
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 0 {
		t.Fatalf("unmailed moderated article was stored: %d, %v", n, err)
	}
	if err := e.Store.SaveMailSettings(context.Background(), mail.Settings{Host: "127.0.0.1", Port: 25, From: "news@e2e.invalid", Security: "plain"}); err != nil {
		t.Fatal(err)
	}
	type submission struct {
		settings mail.Settings
		to       []string
		wire     string
	}
	mailed := make(chan submission, 4)
	// Capture at the existing SMTP delivery seam, without a fake NNTP/SMTP
	// server or an external mail connection. NNTP and moderation run for real.
	restore := moderate.SetDeliver(func(_ context.Context, s mail.Settings, to []string, body []byte) error {
		select {
		case mailed <- submission{s, append([]string(nil), to...), string(body)}:
			return nil
		default:
			return fmt.Errorf("unexpected extra moderator mail")
		}
	})
	t.Cleanup(restore)
	if line := c.ExpectPost(240, plain); !strings.Contains(line, "mailed to moderator for "+moderatedGroup) {
		t.Fatalf("moderator submission reply: %q", line)
	}
	select {
	case got := <-mailed:
		if got.settings.From != "news@e2e.invalid" || len(got.to) != 1 || got.to[0] != "misc-test-moderated@moderators.isc.org" {
			t.Fatalf("moderator envelope: from=%q to=%q", got.settings.From, got.to)
		}
		if !strings.Contains(got.wire, "From: tester@example.invalid\r\n") || !strings.Contains(got.wire, "Subject: please approve\r\n") || strings.Contains(got.wire, "Auto-Submitted:") {
			t.Fatalf("moderator mail changed article headers: %q", got.wire)
		}
	default:
		t.Fatal("successful moderated submission did not call mail delivery")
	}
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 0 {
		t.Fatalf("mailed article was also stored: %d, %v", n, err)
	}
	const approvedID = "<transit-approved@e2e.invalid>"
	approved := article.ParseLines([]string{"From: tester@example.invalid", "Newsgroups: " + moderatedGroup, "Subject: approved", "Approved: moderator@example.invalid", "Message-ID: " + approvedID, "", "kept"})
	if line := c.ExpectPost(240, approved.Bytes()); strings.Contains(line, "mailed") {
		t.Fatalf("approved article was mailed: %q", line)
	}
	c.ExpectCmd(220, "ARTICLE %s", approvedID)
	c.ReadDotLines()
	if n, err := e.Store.CountArticles(context.Background()); err != nil || n != 1 {
		t.Fatalf("approved article not stored exactly once: %d, %v", n, err)
	}
	dup := article.ParseLines([]string{"From: tester@example.invalid", "Newsgroups: " + moderatedGroup, "Subject: again", "Message-ID: " + approvedID, "", "again"})
	// duplicate moderated article: 441 after the article
	if line := c.ExpectPost(441, dup.Bytes()); !strings.Contains(line, "duplicate") {
		t.Fatalf("duplicate moderation reply: %q", line)
	}
	select {
	case extra := <-mailed:
		t.Fatalf("approved or duplicate article generated extra moderator mail: %+v", extra)
	default:
	}
}

func TestTransitIHaveUnapprovedModeratedStaysStored(t *testing.T) {
	e := startEnv(t)
	const moderatedGroup = "misc.test.moderated"
	e.addGroup(moderatedGroup, "m")
	if err := e.Store.SaveMailSettings(context.Background(), mail.Settings{Host: "127.0.0.1", Port: 25, From: "news@e2e.invalid", Security: "plain"}); err != nil {
		t.Fatal(err)
	}
	var mailed atomic.Bool
	t.Cleanup(moderate.SetDeliver(func(context.Context, mail.Settings, []string, []byte) error {
		mailed.Store(true)
		return fmt.Errorf("transit article was mailed to a moderator")
	}))
	c := e.dial()
	const msgid = "<transit-unapproved@e2e.invalid>"
	c.ExpectIhave(235, msgid, testArticle(t, msgid, moderatedGroup).Bytes()) // unapproved, in transit
	if mailed.Load() {
		t.Fatal("transit article was mailed to a moderator")
	}
	c.ExpectCmd(220, "ARTICLE %s", msgid)
	if lines := c.ReadDotLines(); !strings.Contains(strings.Join(lines, "\n"), "Newsgroups: "+moderatedGroup+"\n") {
		t.Fatalf("unapproved transit article missing: %q", lines)
	}
}

func TestTransitPOSTFeedsIHAVE(t *testing.T) {
	// Exercise two full OUS servers, not a fake peer. Like the original test,
	// this allows the feeder to negotiate STREAMING when the receiver offers it.
	receiver := startEnv(t, func(cfg *config.Config) {
		cfg.Server.Hostname, cfg.Server.Pathhost = "transit-b.test", "transit-b.test"
	})
	sender := startEnv(t, func(cfg *config.Config) {
		cfg.Server.Hostname, cfg.Server.Pathhost = "transit-a.test", "transit-a.test"
	})
	host, portText, err := net.SplitHostPort(receiver.Addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	sender.addPeer(store.Peer{Host: host, Port: port, Enabled: true, Patterns: group})
	c := sender.dial()
	login(c, user, pass)
	const msgid = "<transit-two-servers@e2e.invalid>"
	art := article.ParseLines([]string{"From: tester@example.invalid", "Newsgroups: " + group, "Subject: mesh", "Message-ID: " + msgid, "", "hello peers", ".dot-stuffed peer body"})
	c.ExpectPost(240, art.Bytes()) // to the sending server
	testkit.WaitFor(t, 0, "article relayed to receiving OUS server", func() bool {
		ok, err := receiver.Store.HasMessageID(context.Background(), msgid)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	})
	reader := receiver.dial()
	reader.ExpectCmd(220, "ARTICLE %s", msgid)
	lines := reader.ReadDotLines()
	wire := strings.Join(lines, "\r\n")
	if !strings.Contains(wire, "Path: transit-b.test!transit-a.test!") {
		t.Fatalf("both OUS Path hops missing: %q", lines)
	}
	if !strings.HasSuffix(wire, "\r\n\r\nhello peers\r\n.dot-stuffed peer body") {
		t.Fatalf("relayed article body changed: %q", lines)
	}
}
