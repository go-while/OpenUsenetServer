package nntp_test

import (
	"bufio"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// O-27: a malformed argument gets 501 even when no group is selected;
// RFC 3977 3.2.1 checks syntax before state (412).
func TestMalformedArgumentBeforeNoGroup(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"ARTICLE \x01<wsprobe-1@e2e.test>", "501 "},
		{"HEAD \x00<wsprobe-1@e2e.test>", "501 "},
		{"BODY abc", "501 "},
		{"STAT 0", "501 "},
		{"STAT -1", "501 "},
		{"ARTICLE <no-at-sign>", "501 "},
		{"OVER \x01<wsprobe-1@e2e.test>", "501 "},
		{"OVER <wsprobe-1@e2e.test", "501 "},
		{"OVER x-y", "501 "},
		{"XOVER -5", "501 "},
		{"HDR Subject \x02<wsprobe-1@e2e.test>", "501 "},
		{"HDR Subject <wsprobe-1@e2e.test", "501 "},
		{"XHDR Subject 1-x", "501 "},
		// Article numbers are digit strings: no signs, at most 16 digits.
		{"ARTICLE +1", "501 "},
		{"STAT +01", "501 "},
		{"STAT 12345678901234567", "501 "},
		{"OVER +1", "501 "},
		{"OVER 1--2", "501 "},
		{"HDR Subject 1-+2", "501 "},
		{"XOVER +1-", "501 "},
		{"XHDR Subject +1", "501 "},
		// Well-formed arguments still get the state and lookup codes.
		{"ARTICLE 1", "412 "},
		{"STAT", "412 "},
		{"OVER", "412 "},
		{"OVER 1-", "412 "},
		{"HDR Subject 3", "412 "},
		{"ARTICLE <missing@e2e.test>", "430 "},
		{"OVER <missing@e2e.test>", "430 "},
		{"HDR Subject <missing@e2e.test>", "430 "},
	}
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	for _, tc := range cases {
		if _, err := c.Write([]byte(tc.cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if l := readLine(t, r); !strings.HasPrefix(l, tc.want) {
			t.Errorf("%q: got %q, want %s", tc.cmd, l, strings.TrimSpace(tc.want))
		}
	}
}

// With a group selected, malformed arguments are still 501 and well-formed
// ones reach the article lookup.
func TestMalformedArgumentInGroup(t *testing.T) {
	cases := []struct {
		cmd  string
		want string
	}{
		{"ARTICLE \x01<wsprobe-1@e2e.test>", "501 "},
		{"STAT abc", "501 "},
		{"STAT +1", "501 "},
		{"OVER 1-+2", "501 "},
		{"OVER <wsprobe-1@e2e.test", "501 "},
		{"HDR Subject x", "501 "},
		{"STAT 99", "423 "},
		{"OVER 99", "423 "},
		{"HDR Subject 99", "423 "},
	}
	c, _ := startTestServer(t)
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	_, _ = c.Write([]byte("GROUP local.test\r\n"))
	if l := readLine(t, r); !strings.HasPrefix(l, "211 ") {
		t.Fatalf("GROUP: %q", l)
	}
	for _, tc := range cases {
		if _, err := c.Write([]byte(tc.cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if l := readLine(t, r); !strings.HasPrefix(l, tc.want) {
			t.Errorf("%q: got %q, want %s", tc.cmd, l, strings.TrimSpace(tc.want))
		}
	}
}

// A range whose upper bound is below its lower bound holds no articles (RFC 3977 8.3.2,
// 8.5.2: 423; 6.1.2: LISTGROUP lists nothing). In particular N-0 must not reach the store as
// hi=0, which the stores read as "no upper bound".
func TestEmptyRange(t *testing.T) {
	c, st := startTestServer(t)
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("<empty-range-%d@test>", i)
		if _, err := st.Post(context.Background(), "Subject: s"+strconv.Itoa(i), "body\r\n", id, "s", "a@b", "now", "", "news.test", 30, 1, []string{"local.test"}, false); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	send := func(cmd, want string) {
		t.Helper()
		if _, err := c.Write([]byte(cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if got := readLine(t, r); !strings.HasPrefix(got, want) {
			t.Fatalf("%s: got %q, want %s...", cmd, got, want)
		}
	}
	send("GROUP local.test", "211 3 ")
	for _, rng := range []string{"2-0", "5-3", "3-2", "1-0"} {
		send("OVER "+rng, "423 ")
		send("XOVER "+rng, "423 ")
		send("HDR Subject "+rng, "423 ")
		send("XHDR Subject "+rng, "423 ")
		send("LISTGROUP local.test "+rng, "211 3 ")
		if l := readLine(t, r); l != "." {
			t.Fatalf("LISTGROUP local.test %s: got %q, want an empty list", rng, l)
		}
	}
	// Non-empty ranges still work.
	send("OVER 2-2", "224 ")
	for l := readLine(t, r); l != "."; l = readLine(t, r) {
	}
	send("LISTGROUP local.test 2-", "211 3 ")
	var nums []string
	for l := readLine(t, r); l != "."; l = readLine(t, r) {
		nums = append(nums, l)
	}
	if strings.Join(nums, ",") != "2,3" {
		t.Fatalf("LISTGROUP local.test 2-: %v", nums)
	}
}

// Syntax before state: a LISTGROUP with a malformed range answers 501 and must not have
// selected the group (a following OVER is still 412).
func TestListgroupBadRangeDoesNotSelectGroup(t *testing.T) {
	c, _ := startTestServer(t)
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(c)
	_ = readLine(t, r)
	for _, tc := range []struct{ cmd, want string }{
		{"LISTGROUP local.test x", "501 "},
		{"OVER", "412 "},
		{"LISTGROUP local.test 1-+2", "501 "},
		{"STAT", "412 "},
	} {
		if _, err := c.Write([]byte(tc.cmd + "\r\n")); err != nil {
			t.Fatal(err)
		}
		if l := readLine(t, r); !strings.HasPrefix(l, tc.want) {
			t.Fatalf("%s: got %q, want %s", tc.cmd, l, tc.want)
		}
	}
}
