//go:build e2e

package e2e

// Regression tests for the findings in ISSUES.md. Each test starts with knownIssue while the
// finding is open on main: it skips by default and fails with OUS_E2E_KNOWN_ISSUES=1 until the
// fix branch named in ISSUES.md is merged. The findings are about bytes the server changes, so
// the articles are the kit's article values (written and compared byte for byte, never
// normalised), and the head of O-05, which the builder cannot write, is raw text.

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/article"
	"github.com/go-while/nntp-testkit/client"
)

// noPending is how long a test waits to see that the server sent nothing more.
const noPending = 300 * time.Millisecond

// dotStuff turns article bytes (CRLF lines, ending in CRLF) into what comes back after
// 220/221/222 (and goes out after IHAVE/TAKETHIS/POST): dot-stuffed, with ".\r\n" appended.
func dotStuff(art []byte) string { return string(client.StuffBytes(art)) }

// nowDate is a Date header value the server's cutoff check accepts.
func nowDate() string { return time.Now().UTC().Format(article.DateLayout) }

// transferred returns the final reply line of c.Ihave, c.Takethis or c.Post and fails t when
// the server answered before the article was sent (no 335 or 340): the caller checks the reply
// to the article itself.
func transferred(t *testing.T, what string) func(code int, line string, sent bool) string {
	return func(_ int, line string, sent bool) string {
		t.Helper()
		if !sent {
			t.Fatalf("%s: refused before the article was sent: %q", what, line)
		}
		return line
	}
}

// rawBlock sends cmd, expects code and returns the block exactly as sent by the server
// (dot-stuffed, terminator included). The client is non-strict while reading, so a bare CR
// in the data is read as data and not reported as a wire error.
func rawBlock(c *client.Conn, code int, cmd string) string {
	c.ExpectCmd(code, "%s", cmd)
	c.Strict = false
	defer func() { c.Strict = true }()
	return string(c.RawUntilDot())
}

// O-19: requirePostAuth sent 480 (not logged in) or 440 (user may not post) and returned the
// nil error of that write, so cmdPost went on and sent 340 as well; the server then read the
// next commands as article text. Right: one reply (480 or 440) and the next command is
// answered as a command.
func TestRegressionO19RefusedPostOneReply(t *testing.T) {
	knownIssue(t, "O-19", "a refused POST also sends 340")
	e := startEnv(t)
	e.addUser("e2e-reader", "e2e-reader-pass", false)

	c := e.dial()
	c.ExpectCmd(480, "POST")
	if err := c.ExpectNoPending(noPending); err != nil {
		t.Fatalf("POST without login: more than the 480 reply: %v", err)
	}
	login(c, user, pass)

	r := e.dial()
	login(r, "e2e-reader", "e2e-reader-pass")
	r.ExpectCmd(440, "POST")
	if err := r.ExpectNoPending(noPending); err != nil {
		t.Fatalf("POST by a user who may not post: more than the 440 reply: %v", err)
	}
	r.ExpectCmd(211, "GROUP %s", group)
}

// O-05: Parse kept the head in a map and Wire() rebuilt it (known fields first, the rest in map
// order, folded lines joined), and that rebuilt head was stored and served. Right (RFC 5537
// 3.2/3.3): the head comes back as received, except our hop prepended to Path and our Xref
// field (O-10) appended at the end: order, case of field names, folding, missing space after
// the colon and unknown X- fields kept. Xref is allowed here, not required (the O-10 test
// requires it), so this test does not depend on the order the two fixes are merged in.
func TestRegressionO05HeadKeptByteExact(t *testing.T) {
	knownIssue(t, "O-05", "the article head is rebuilt from a map")
	e := startEnv(t)
	c := e.dial()
	msgid := "<o05-head@e2e.test>"
	head := "X-Zeta: unknown header first\r\n" +
		"Path: e2e-peer!not-for-mail\r\n" +
		"Subject: a folded\r\n" +
		"\tsubject line\r\n" +
		"From: E2E Tester <e2e@example.invalid>\r\n" +
		"X-Folded: first\r\n" +
		"   second\r\n" +
		"Newsgroups: " + group + "\r\n" +
		"x-lower:no-space\r\n" +
		"Date: " + nowDate() + "\r\n" +
		"Message-ID: " + msgid + "\r\n" +
		"X-Trace: one\r\n" +
		"X-Trace: two\r\n"
	body := "line one\r\n.dot line\r\n\r\nlast\r\n"
	// Parse keeps every byte (Parse(b).Bytes() == b): the article goes out as written.
	art := article.Parse([]byte(head + "\r\n" + body))
	c.ExpectIhave(235, msgid, art.Bytes())

	// PrependPath changes nothing but the text after "Path: ".
	want := art.Clone().PrependPath(pathhost)
	withXref := want.Clone().AddRaw("Xref: " + pathhost + " " + group + ":1")
	got := rawBlock(c, 220, "ARTICLE "+msgid)
	if got != dotStuff(want.Bytes()) && got != dotStuff(withXref.Bytes()) {
		t.Fatalf("ARTICLE %s\n got %q\nwant %q", msgid, got, dotStuff(withXref.Bytes()))
	}
}

// O-10: stored articles had no Xref header (intake drops a peer's, and the new value went
// only into the xref column), while OVER's Xref:full field and HDR Xref showed it, so a
// reader or cache rebuilding the overview from the article lost it. Right (RFC 3977 8.4.1,
// RFC 5536 3.2.14, RFC 5537 3.3): ARTICLE and HEAD, by number and by message-id, carry
// exactly one Xref field, ours and not the peer's, with the value OVER and HDR show; a
// cross-posted article lists every group. BODY is unchanged.
func TestRegressionO10ServedXref(t *testing.T) {
	knownIssue(t, "O-10", "stored articles have no Xref header")
	e := startEnv(t)
	e.addGroup("local.other", "y")
	c := e.dial()
	msgid := "<o10-xref@e2e.test>"
	// The peer's Xref in the middle of the head: after Subject, before Message-ID and Date.
	art := testArticle(t, msgid, group+",local.other").
		Set("Subject", "served xref").
		Insert(4, "Xref: e2e-peer.example "+group+":99 local.other:98").
		SetBodyLines("body line")
	c.ExpectIhave(235, msgid, art.Bytes())
	body := art.Body

	// xrefs returns the Xref field lines of the head in lines (an article or a head).
	xrefs := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			if l == "" {
				break
			}
			if name, _, ok := strings.Cut(l, ":"); ok && strings.EqualFold(name, "Xref") {
				out = append(out, l)
			}
		}
		return out
	}

	c.ExpectCmd(211, "GROUP %s", group)
	c.ExpectCmd(224, "OVER 1")
	over := c.ReadDotLines()
	if len(over) != 1 {
		t.Fatalf("OVER 1: %q, want one line", over)
	}
	f := strings.Split(over[0], "\t")
	want := f[len(f)-1]
	if want != "Xref: "+pathhost+" "+group+":1 local.other:1" {
		t.Fatalf("OVER 1: Xref field %q", want)
	}
	c.ExpectCmd(225, "HDR Xref 1")
	if got := c.ReadDotLines(); len(got) != 1 || "Xref: "+strings.TrimPrefix(got[0], "1 ") != want {
		t.Errorf("HDR Xref 1: %q, want the OVER value %q", got, want)
	}
	for _, sel := range []string{"1", msgid} {
		c.ExpectCmd(220, "ARTICLE %s", sel)
		if got := xrefs(c.ReadDotLines()); len(got) != 1 || got[0] != want {
			t.Errorf("ARTICLE %s: Xref lines %q, want [%q] as in OVER", sel, got, want)
		}
		c.ExpectCmd(221, "HEAD %s", sel)
		if got := xrefs(c.ReadDotLines()); len(got) != 1 || got[0] != want {
			t.Errorf("HEAD %s: Xref lines %q, want [%q] as in OVER", sel, got, want)
		}
		if got := rawBlock(c, 222, "BODY "+sel); got != dotStuff(body) {
			t.Errorf("BODY %s\n got %q\nwant %q", sel, got, dotStuff(body))
		}
	}
	c.ExpectCmd(211, "GROUP local.other")
	c.ExpectCmd(220, "ARTICLE 1")
	if got := xrefs(c.ReadDotLines()); len(got) != 1 || got[0] != want {
		t.Errorf("ARTICLE 1 in local.other: Xref lines %q, want [%q]", got, want)
	}
}

// O-15: Parse began with normalizeNewlines, which turned every bare CR into a line break, so
// a body line "col1\rcol2" came back as two lines (IHAVE and POST). Right: the bare CR is
// kept as data (or the article refused, which the fix does not do).
func TestRegressionO15BareCRKept(t *testing.T) {
	knownIssue(t, "O-15", "a bare CR becomes a line break")
	e := startEnv(t)
	c := e.dial()
	// The body is "col1\rcol2 on one line\r\nnext line\r\n": the CR stays inside the line.
	lines := []string{"col1\rcol2 on one line", "next line"}

	msgid := "<o15-ihave@e2e.test>"
	art := testArticle(t, msgid, group).Set("Subject", "bare CR by IHAVE").SetBodyLines(lines...)
	c.ExpectIhave(235, msgid, art.Bytes())
	if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(art.Body); got != want {
		t.Errorf("BODY %s (IHAVE)\n got %q\nwant %q", msgid, got, want)
	}

	login(c, user, pass)
	msgid = "<o15-post@e2e.test>"
	art = testPost(t, msgid, "bare CR by POST", lines...)
	c.ExpectPost(240, art.Bytes())
	if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(art.Body); got != want {
		t.Errorf("BODY %s (POST)\n got %q\nwant %q", msgid, got, want)
	}
}

// O-16: parseHeaders cut each header line at its first colon, so the mbox line
// "From nobody Thu Dec  8 12:55:04 2016" became a field "From nobody Thu Dec  8 12" and was
// written back as "From nobody Thu Dec  8 12: 55:04 2016". Right: a field name may not hold a
// space, the head is invalid and the article is refused (fix: "invalid header block", 437 for
// IHAVE, 439 for TAKETHIS, 441 for POST) and not stored.
func TestRegressionO16ColonlessHeaderRefused(t *testing.T) {
	knownIssue(t, "O-16", "a header line without a colon is mangled")
	e := startEnv(t)
	c := e.dial()
	// The mbox line goes first, byte for byte (Insert writes it as given; the article package
	// itself names it "From nobody Thu Dec  8 12", and ValidName is false).
	art := func(msgid string) []byte {
		return testArticle(t, msgid, group).
			Set("Subject", "head with an mbox From line").
			Insert(0, "From nobody Thu Dec  8 12:55:04 2016").
			SetBodyLines("body").Bytes()
	}
	check := func(what, line, want string) {
		t.Helper()
		if !strings.HasPrefix(line, want) {
			t.Errorf("%s: %q, want prefix %q", what, line, want)
		}
	}

	// transferred: each refusal must answer the article (after 335/340), not the command.
	ihaveID := "<o16-ihave@e2e.test>"
	line := transferred(t, "IHAVE")(c.Ihave(ihaveID, art(ihaveID)))
	check("IHAVE", line, "437 invalid header block")

	c.ExpectCmd(203, "MODE STREAM")
	takeID := "<o16-takethis@e2e.test>"
	line = transferred(t, "TAKETHIS")(c.Takethis(takeID, art(takeID)))
	check("TAKETHIS", line, "439 "+takeID+" invalid header block")

	login(c, user, pass)
	postID := "<o16-post@e2e.test>"
	line = transferred(t, "POST")(c.Post(art(postID)))
	check("POST", line, "441 invalid header block")

	for _, id := range []string{ihaveID, takeID, postID} {
		if code, line := c.Cmd("STAT %s", id); code != 430 {
			t.Errorf("STAT %s: %q, want 430 (refused article must not be stored)", id, line)
		}
	}
}

// O-06: the TAKETHIS reply was "239 <id> article transferred <id>": replyTransfer puts the id
// first (RFC 4644) and the text added it again. Right: the id once, as the first argument.
func TestRegressionO06TakethisReplyIDOnce(t *testing.T) {
	knownIssue(t, "O-06", "the 239 reply text repeats the message-id")
	e := startEnv(t)
	c := e.dial()
	c.ExpectCmd(203, "MODE STREAM")
	msgid := "<o06-takethis@e2e.test>"
	line := c.ExpectTakethis(239, msgid, testArticle(t, msgid, group).Bytes())
	if !strings.HasPrefix(line, "239 "+msgid+" ") || strings.Count(line, msgid) != 1 {
		t.Fatalf("TAKETHIS reply %q, want \"239 %s <text>\" with the id once", line, msgid)
	}
}

// O-11: OVER <message-id> built a stand-in article from five headers and the body and counted
// :bytes from that, so every other header was missing from the count (by number it was
// right). A TAB in a header value also went out as is and shifted the fields. Right: OVER by
// message-id gives the same line as by number with article number 0 (RFC 3977 8.3.2), :bytes
// is the size of the article as served, and TAB in Subject is replaced by a space.
func TestRegressionO11OverByMessageID(t *testing.T) {
	knownIssue(t, "O-11", "OVER by message-id reports the wrong :bytes")
	e := startEnv(t)
	c := e.dial()
	msgid := "<o11-over@e2e.test>"
	art := testArticle(t, msgid, group,
		"X-Extra-1: a header OVER by message-id did not count",
		"X-Extra-2: another one").
		Set("Subject", "a\ttab in the subject").
		SetBodyLines("body line 1", "body line 2")
	c.ExpectIhave(235, msgid, art.Bytes())

	// The article as served: its size in octets (CRLF counted, dot-stuffing not).
	c.ExpectCmd(220, "ARTICLE %s", msgid)
	size := 0
	for _, l := range c.ReadDotLines() {
		size += len(l) + 2
	}

	c.ExpectCmd(211, "GROUP %s", group)
	c.ExpectCmd(224, "OVER 1")
	byNum := c.ReadDotLines()
	c.ExpectCmd(224, "OVER %s", msgid)
	byID := c.ReadDotLines()
	if len(byNum) != 1 || len(byID) != 1 {
		t.Fatalf("OVER: by number %q, by message-id %q, want one line each", byNum, byID)
	}
	for what, line := range map[string]string{"OVER 1": byNum[0], "OVER " + msgid: byID[0]} {
		f := strings.Split(line, "\t")
		if len(f) != 9 {
			t.Errorf("%s: %d fields, want 9: %q", what, len(f), line)
			continue
		}
		if f[1] != "a tab in the subject" {
			t.Errorf("%s: subject %q, want TAB replaced by a space", what, f[1])
		}
		if f[6] != strconv.Itoa(size) {
			t.Errorf("%s: :bytes %s, want %d (size of the article as served)", what, f[6], size)
		}
	}
	if want := "0" + strings.TrimPrefix(byNum[0], "1"); byID[0] != want {
		t.Errorf("OVER %s\n got %q\nwant %q (the OVER 1 line with number 0)", msgid, byID[0], want)
	}
}

// O-12: InjectForPost wrote posting-account="openusenet" for every post, whoever was logged
// in. Right: Injection-Info names the logged-in user.
func TestRegressionO12PostingAccount(t *testing.T) {
	knownIssue(t, "O-12", "posting-account is hardcoded")
	e := startEnv(t)
	c := e.dial()
	login(c, user, pass)
	msgid := "<o12-post@e2e.test>"
	c.ExpectPost(240, testPost(t, msgid, "posting account", "body").Bytes())
	c.ExpectCmd(221, "HEAD %s", msgid)
	var got []string
	for _, l := range c.ReadDotLines() {
		if len(l) > 15 && strings.EqualFold(l[:15], "Injection-Info:") {
			got = append(got, l)
		}
	}
	want := `Injection-Info: ` + pathhost + `; posting-account="` + user + `"`
	if len(got) != 1 || got[0] != want {
		t.Fatalf("Injection-Info lines %q, want [%q]", got, want)
	}
}

// O-23: HDR without a range is about the current article (RFC 3977 8.5.2). In a group without
// one, OUS answered "423 no articles in that range"; right is 420, as OVER answers. Fixed by
// fix/hdr-no-current-article. nntp-testkit's no-current-article check covers it too since
// v0.12.0 (conformance_test.go allows that failure while O-23 is open).
func TestRegressionO23HdrWithoutCurrentArticle(t *testing.T) {
	knownIssue(t, "O-23", "HDR without a range and no current article answers 423")
	e := startEnv(t)
	e.addGroup("local.empty", "y")
	c := e.dial()
	c.ExpectCmd(211, "GROUP local.empty")
	c.ExpectCmd(420, "HDR Subject")
	c.ExpectCmd(420, "OVER")

	// A current article cancelled after GROUP selected it is an invalid current article
	// too; an explicit range only finds the number missing (423).
	const msgid = "<o23-cancelled@e2e.test>"
	login(c, user, pass)
	c.ExpectPost(240, testArticle(t, msgid, group).Bytes())
	c.ExpectCmd(211, "GROUP %s", group)
	if removed, err := e.Store.CancelMessageID(context.Background(), msgid); err != nil || !removed {
		t.Fatalf("cancel the current article: %v, %v", removed, err)
	}
	c.ExpectCmd(420, "HDR Subject")
	c.ExpectCmd(423, "HDR Subject 1")
	c.ExpectCmd(420, "OVER")
	c.ExpectCmd(423, "OVER 1")
}
