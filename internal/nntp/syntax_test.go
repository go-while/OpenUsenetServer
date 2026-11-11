package nntp_test

import (
	"bufio"
	"strings"
	"testing"
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
