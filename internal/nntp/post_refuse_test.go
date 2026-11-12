package nntp_test

import (
	"bufio"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"openusenet/internal/config"
	"openusenet/internal/nntp"
	"openusenet/internal/ops"
	"openusenet/internal/store"
)

// Refusals inside cmdPost (paused server, cleanfeed reject, article too old) wrote a 441
// and returned nil, so cmdPost carried on: the article was stored and acknowledged with
// 240, or a second 441 followed, or 340 was sent after the 441.

const refuseArticle = "From: tester@example.com\r\nNewsgroups: local.test\r\nSubject: refuse me\r\n" +
	"Date: %s\r\n\r\nbody\r\n.\r\n"

func refuseStartServer(t *testing.T, mutate func(*config.Config), ctl *ops.Controller) (net.Conn, *store.Memory) {
	t.Helper()
	a, b := net.Pipe()
	st := store.NewMemory()
	if err := st.EnsureGroup(context.Background(), "local.test", "Local test group", "y"); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Server.Hostname = "news.test"
	cfg.Server.Pathhost = "news.test"
	cfg.Limits.IdleSeconds = 30
	cfg.Limits.ArtCutoffDays = 0
	if mutate != nil {
		mutate(&cfg)
	}
	go nntp.Serve(nntp.NewConn(b, 30*time.Second), st, nil, cfg, nil, nil, nil, ctl)
	t.Cleanup(func() { _ = a.Close() })
	return a, st
}

// refuseSession sends command lines and checks each gets exactly the expected reply code.
type refuseSession struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func newRefuseSession(t *testing.T, c net.Conn) *refuseSession {
	_ = c.SetDeadline(time.Now().Add(10 * time.Second)) // hang guard
	s := &refuseSession{t: t, c: c, r: bufio.NewReader(c)}
	if l := readLine(t, s.r); !strings.HasPrefix(l, "200 ") && !strings.HasPrefix(l, "201 ") {
		t.Fatalf("greeting %q", l)
	}
	return s
}

func (s *refuseSession) send(raw string) {
	s.t.Helper()
	if _, err := s.c.Write([]byte(raw)); err != nil {
		s.t.Fatalf("write: %v (server out of sync?)", err)
	}
}

func (s *refuseSession) expect(prefix string) string {
	s.t.Helper()
	l := readLine(s.t, s.r)
	if !strings.HasPrefix(l, prefix) {
		s.t.Fatalf("got %q, want %s...", l, prefix)
	}
	return l
}

// inSync checks the session still answers commands in order: a stray reply left
// over from the refused POST would answer DATE instead.
func (s *refuseSession) inSync() {
	s.t.Helper()
	s.send("DATE\r\n")
	s.expect("111 ")
}

func refuseStored(t *testing.T, st *store.Memory) {
	t.Helper()
	if n, err := st.CountArticles(context.Background()); err != nil || n != 0 {
		t.Fatalf("refused article stored: count=%d err=%v", n, err)
	}
}

func TestPostTooOldGetsOneReply(t *testing.T) {
	for _, remember := range []bool{false, true} {
		name := "remember_rejects_off"
		if remember {
			name = "remember_rejects_on"
		}
		t.Run(name, func(t *testing.T) {
			c, st := refuseStartServer(t, func(cfg *config.Config) {
				cfg.Limits.ArtCutoffDays = 10
				cfg.Limits.RememberRejects = remember
			}, nil)
			s := newRefuseSession(t, c)
			s.send("POST\r\n")
			s.expect("340 ")
			s.send(strings.Replace(refuseArticle, "%s", "Mon, 01 Jan 2001 00:00:00 +0000", 1))
			if l := s.expect("441 "); !strings.Contains(l, "too old") {
				t.Fatalf("got %q, want too old", l)
			}
			s.inSync()
			refuseStored(t, st)
		})
	}
}

func TestPostCleanfeedRejectGetsOneReply(t *testing.T) {
	c, st := refuseStartServer(t, func(cfg *config.Config) {
		cfg.Cleanfeed = config.Cleanfeed{Enabled: true, Mode: "reject", Command: "false"}
	}, nil)
	s := newRefuseSession(t, c)
	s.send("POST\r\n")
	s.expect("340 ")
	s.send(strings.Replace(refuseArticle, "%s", time.Now().UTC().Format(time.RFC1123Z), 1))
	s.expect("441 ")
	s.inSync()
	refuseStored(t, st)
}

func TestPostPausedGetsOneReply(t *testing.T) {
	ctl := ops.New()
	ctl.Pause("maintenance", "test")
	c, st := refuseStartServer(t, nil, ctl)
	s := newRefuseSession(t, c)
	s.send("POST\r\n")
	if l := s.expect("441 "); !strings.Contains(l, "maintenance") {
		t.Fatalf("got %q, want pause reason", l)
	}
	s.inSync()
	// Further commands must still not be swallowed as article text.
	s.send("POST\r\n")
	s.expect("441 ")
	s.inSync()
	refuseStored(t, st)
}
