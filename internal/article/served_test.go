package article

import "testing"

func TestServedHead(t *testing.T) {
	const x = "news.test local.test:1 local.other:2"
	cases := []struct {
		name, head, xref, want string
	}{
		{"empty xref keeps head", "Subject: s\r\nXref: peer a:1", "", "Subject: s\r\nXref: peer a:1"},
		{"empty head", "", x, "Xref: " + x},
		{"appended", "Path: a!b\r\nSubject: s\r\n\tfolded", x,
			"Path: a!b\r\nSubject: s\r\n\tfolded\r\nXref: " + x},
		{"LF head appended with LF", "Path: a!b\nSubject: s", x,
			"Path: a!b\nSubject: s\nXref: " + x},
		{"replaced in place keeping spelling", "Path: a\r\nxref:peer a:1\r\nSubject: s", x,
			"Path: a\r\nxref: " + x + "\r\nSubject: s"},
		{"folded xref and second field dropped",
			"Path: a\r\nXref: peer a:1\r\n\tb:2\r\nSubject: s\r\nXREF: again\r\n c:3\r\nX-Xref: keep", x,
			"Path: a\r\nXref: " + x + "\r\nSubject: s\r\nX-Xref: keep"},
		{"last field dropped leaves no terminator",
			"Xref: one\r\nSubject: s\r\nXref: two\r\n\tmore", x,
			"Xref: " + x + "\r\nSubject: s"},
		{"last field replaced", "Subject: s\nXref: peer\n x:1", x,
			"Subject: s\nXref: " + x},
		{"continuation of other field untouched", "X-A: Xref: no\r\n Xref: no", x,
			"X-A: Xref: no\r\n Xref: no\r\nXref: " + x},
	}
	for _, c := range cases {
		if got := ServedHead(c.head, c.xref); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}
