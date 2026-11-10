//go:build e2e

package e2e

// Regression tests for the findings in ISSUES.md. Each test starts with knownIssue while the
// finding is open on main: it skips by default and fails with OUS_E2E_KNOWN_ISSUES=1 until the
// fix branch named in ISSUES.md is merged. Articles are written as raw bytes here, because the
// findings are about bytes the server changes.

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-while/nntp-testkit/client"
)

// noPending is how long a test waits to see that the server sent nothing more.
const noPending = 300 * time.Millisecond

// dotStuff turns an article (CRLF lines, ending in CRLF) into what goes on the wire after
// IHAVE/TAKETHIS/POST or comes back after 220/221/222: dot-stuffed, with ".\r\n" appended.
func dotStuff(art string) string {
	if strings.HasPrefix(art, ".") {
		art = "." + art
	}
	return strings.ReplaceAll(art, "\r\n.", "\r\n..") + ".\r\n"
}

// nowDate is a Date header value the server's cutoff check accepts.
func nowDate() string { return time.Now().UTC().Format(time.RFC1123Z) }

// ihaveRaw offers art (exact bytes, CRLF lines) by IHAVE and expects 335, then want.
func ihaveRaw(c *client.Conn, msgid, art string, want int) string {
	c.ExpectCmd(335, "IHAVE %s", msgid)
	c.MustSend(dotStuff(art))
	return c.ExpectStatus(want, "IHAVE "+msgid)
}

// postArticle posts art (exact bytes, CRLF lines) and expects 340, then want.
// TODO(nntp-testkit): replace with the kit's POST helper (code + line) once it exists.
func postArticle(c *client.Conn, art string, want int) string {
	c.ExpectCmd(340, "POST")
	c.MustSend(dotStuff(art))
	return c.ExpectStatus(want, "POST")
}

// readStatus reads the next status line (any code); it fails the test on a read error.
func readStatus(t *testing.T, c *client.Conn) string {
	t.Helper()
	_, line, err := c.ReadStatus()
	if err != nil {
		t.Fatalf("status line: %v (%q)", err, line)
	}
	return line
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
// 3.2/3.3): the head comes back as received, except our hop prepended to Path: order, case of
// field names, folding, missing space after the colon and unknown X- fields kept.
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
	ihaveRaw(c, msgid, head+"\r\n"+body, 235)

	wantHead := strings.Replace(head, "Path: e2e-peer!", "Path: "+pathhost+"!e2e-peer!", 1)
	if got, want := rawBlock(c, 220, "ARTICLE "+msgid), dotStuff(wantHead+"\r\n"+body); got != want {
		t.Fatalf("ARTICLE %s\n got %q\nwant %q", msgid, got, want)
	}
}

// O-15: Parse began with normalizeNewlines, which turned every bare CR into a line break, so
// a body line "col1\rcol2" came back as two lines (IHAVE and POST). Right: the bare CR is
// kept as data (or the article refused, which the fix does not do).
func TestRegressionO15BareCRKept(t *testing.T) {
	knownIssue(t, "O-15", "a bare CR becomes a line break")
	e := startEnv(t)
	c := e.dial()
	body := "col1\rcol2 on one line\r\nnext line\r\n"

	msgid := "<o15-ihave@e2e.test>"
	ihaveRaw(c, msgid, strings.Join([]string{
		"Path: e2e-peer!not-for-mail",
		"From: E2E Tester <e2e@example.invalid>",
		"Newsgroups: " + group,
		"Subject: bare CR by IHAVE",
		"Date: " + nowDate(),
		"Message-ID: " + msgid,
		"", body}, "\r\n"), 235)
	if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(body); got != want {
		t.Errorf("BODY %s (IHAVE)\n got %q\nwant %q", msgid, got, want)
	}

	login(c, user, pass)
	msgid = "<o15-post@e2e.test>"
	postArticle(c, strings.Join([]string{
		"From: E2E Tester <e2e@example.invalid>",
		"Newsgroups: " + group,
		"Subject: bare CR by POST",
		"Message-ID: " + msgid,
		"", body}, "\r\n"), 240)
	if got, want := rawBlock(c, 222, "BODY "+msgid), dotStuff(body); got != want {
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
	head := func(msgid string) string {
		return "From nobody Thu Dec  8 12:55:04 2016\r\n" +
			"Path: e2e-peer!not-for-mail\r\n" +
			"From: E2E Tester <e2e@example.invalid>\r\n" +
			"Newsgroups: " + group + "\r\n" +
			"Subject: head with an mbox From line\r\n" +
			"Date: " + nowDate() + "\r\n" +
			"Message-ID: " + msgid + "\r\n"
	}
	check := func(what, line, want string) {
		t.Helper()
		if !strings.HasPrefix(line, want) {
			t.Errorf("%s: %q, want prefix %q", what, line, want)
		}
	}

	ihaveID := "<o16-ihave@e2e.test>"
	c.ExpectCmd(335, "IHAVE %s", ihaveID)
	c.MustSend(dotStuff(head(ihaveID) + "\r\nbody\r\n"))
	line := readStatus(t, c)
	check("IHAVE", line, "437 invalid header block")

	c.ExpectCmd(203, "MODE STREAM")
	takeID := "<o16-takethis@e2e.test>"
	c.MustSend("TAKETHIS " + takeID + "\r\n" + dotStuff(head(takeID)+"\r\nbody\r\n"))
	line = readStatus(t, c)
	check("TAKETHIS", line, "439 "+takeID+" invalid header block")

	login(c, user, pass)
	postID := "<o16-post@e2e.test>"
	c.ExpectCmd(340, "POST")
	c.MustSend(dotStuff(head(postID) + "\r\nbody\r\n"))
	line = readStatus(t, c)
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
	c.MustSend("TAKETHIS " + msgid + "\r\n" + client.Stuff(testArticle(msgid, group)))
	line := c.ExpectStatus(239, "TAKETHIS "+msgid)
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
	art := strings.Join([]string{
		"Path: e2e-peer!not-for-mail",
		"From: E2E Tester <e2e@example.invalid>",
		"Newsgroups: " + group,
		"Subject: a\ttab in the subject",
		"Date: " + nowDate(),
		"Message-ID: " + msgid,
		"X-Extra-1: a header OVER by message-id did not count",
		"X-Extra-2: another one",
		"", "body line 1", "body line 2", ""}, "\r\n")
	ihaveRaw(c, msgid, art, 235)

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
	postArticle(c, strings.Join([]string{
		"From: E2E Tester <e2e@example.invalid>",
		"Newsgroups: " + group,
		"Subject: posting account",
		"Message-ID: " + msgid,
		"", "body", ""}, "\r\n"), 240)
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
