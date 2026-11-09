package article

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestCheckHeaderBlock(t *testing.T) {
	good := [][]string{
		nil,
		{"From: a@b.c", "Subject: hi"},
		{"Subject: folded", "\tsecond", " third"},
		{"X-Empty:", "Subject:no-space"},
		{"Received: one", "Received: two", "X-Dup: 1", "x-dup: 2"},
		{"Subject: ws-only continuation", " "},
		{"Subject: high octets \xc3\xa4 and \x1b esc"},
	}
	for _, lines := range good {
		for _, lenient := range []bool{false, true} {
			got, err := checkHeaderBlock(lines, lenient)
			if err != nil {
				t.Errorf("checkHeaderBlock(%q, %v): %v", lines, lenient, err)
				continue
			}
			if !slices.Equal(got, lines) {
				t.Errorf("checkHeaderBlock(%q, %v) changed lines: %q", lines, lenient, got)
			}
		}
	}

	// rejected in both modes
	unsafe := [][]string{
		{"Subject: nul \x00 byte"},
		{"Subject: bare \r cr"},
		{"Subject: bare \n lf"},
		{"Subject: a", "Subject: b"},
		{"From: a", "FROM: b"},
		{"Message-ID: <1@x>", "Path: x", "message-id: <2@x>"},
		{"Date: x", "Newsgroups: a", "Path: p", "newsgroups: b"},
	}
	for _, lines := range unsafe {
		for _, lenient := range []bool{false, true} {
			if _, err := checkHeaderBlock(lines, lenient); !errors.Is(err, ErrInvalidHeaderBlock) {
				t.Errorf("checkHeaderBlock(%q, %v) = %v, want ErrInvalidHeaderBlock", lines, lenient, err)
			}
		}
	}

	// rejected in strict mode, dropped in lenient mode
	malformed := []struct {
		lines, lenient []string
	}{
		{[]string{" leading continuation", "Subject: x"}, []string{"Subject: x"}},
		{[]string{"From nobody Thu Dec  8 12:55:04 2016", "Subject: x"}, []string{"Subject: x"}},
		{[]string{"Subject: x", "no colon here", "\tits continuation", "From: y"}, []string{"Subject: x", "From: y"}},
		{[]string{": empty name", "Subject: x"}, []string{"Subject: x"}},
		{[]string{"Bad Name: x", "Subject: x"}, []string{"Subject: x"}},
		{[]string{"Bad\x7fName: x"}, []string{}},
		{[]string{"Subject: x", "."}, []string{"Subject: x"}},
	}
	for _, tc := range malformed {
		if _, err := checkHeaderBlock(tc.lines, false); !errors.Is(err, ErrInvalidHeaderBlock) {
			t.Errorf("strict checkHeaderBlock(%q) = %v, want ErrInvalidHeaderBlock", tc.lines, err)
		}
		in := slices.Clone(tc.lines)
		got, err := checkHeaderBlock(tc.lines, true)
		if err != nil {
			t.Errorf("lenient checkHeaderBlock(%q): %v", tc.lines, err)
			continue
		}
		if !slices.Equal(got, tc.lenient) {
			t.Errorf("lenient checkHeaderBlock(%q) = %q, want %q", tc.lines, got, tc.lenient)
		}
		if !slices.Equal(tc.lines, in) {
			t.Errorf("lenient checkHeaderBlock modified its input: %q", tc.lines)
		}
	}
}

func TestHeaderValue(t *testing.T) {
	a := &Article{Head: []string{
		"Subject:   padded value  ",
		"X-Folded: first",
		"\tsecond",
		"   third  ",
		"X-Late-Start:",
		" value on next line",
		"Received: one",
		"Received: two",
		"X-Empty:",
		"From nobody: not a From field",
	}}
	cases := map[string]string{
		"Subject":      "padded value",
		"subject":      "padded value",
		"X-FOLDED":     "first second third",
		"X-Late-Start": "value on next line",
		"Received":     "one",
		"X-Empty":      "",
		"From":         "",
		"Missing":      "",
	}
	for name, want := range cases {
		if got := a.Get(name); got != want {
			t.Errorf("Get(%q) = %q, want %q", name, got, want)
		}
	}
	if a.Has("X-Empty") || a.Has("Missing") || !a.Has("received") {
		t.Error("Has: wrong result")
	}
	head := strings.Join(a.Head, "\r\n")
	for name, want := range cases {
		if got := HeaderValue(head, name); got != want {
			t.Errorf("HeaderValue(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestSetReplacesInPlaceAndAppends(t *testing.T) {
	a := &Article{Head: []string{
		"Path: x!y",
		"message-id:  <old@x>",
		"\t(folded comment)",
		"X-Other: keep",
		"Organization: one",
		"organization: two",
		" two continued",
		"X-Tail: keep",
	}}
	a.Set("Message-ID", "<new@x>")
	a.Set("Organization", "single")
	a.Set("Injection-Date", "now")
	want := []string{
		"Path: x!y",
		"message-id: <new@x>",
		"X-Other: keep",
		"Organization: single",
		"X-Tail: keep",
		"Injection-Date: now",
	}
	if !slices.Equal(a.Head, want) {
		t.Fatalf("head after Set:\n%q\nwant\n%q", a.Head, want)
	}
}

func TestDeleteRemovesContinuationLines(t *testing.T) {
	a := &Article{Head: []string{
		"Xref: a b:1",
		"\tc:2",
		"Subject: x",
		"XREF: again",
		"Xref-Like: keep",
		" continued",
	}}
	a.Delete("xref")
	want := []string{"Subject: x", "Xref-Like: keep", " continued"}
	if !slices.Equal(a.Head, want) {
		t.Fatalf("head after Delete: %q, want %q", a.Head, want)
	}
	a.Delete("Missing")
	if !slices.Equal(a.Head, want) {
		t.Fatalf("Delete of an absent field changed the head: %q", a.Head)
	}
}

func TestPrependPath(t *testing.T) {
	cases := []struct {
		head []string
		want []string
	}{
		{[]string{"From: a", "Path: x!y"}, []string{"From: a", "Path: me!x!y"}},
		{[]string{"PATH:x!y"}, []string{"PATH:me!x!y"}},
		{[]string{"Path: x!", " y"}, []string{"Path: me!x!", " y"}},
		{[]string{"Path:", " x!y"}, []string{"Path: me!x!y"}},
		{[]string{"Path: ", "From: a"}, []string{"Path: me!not-for-mail", "From: a"}},
		{[]string{"From: a"}, []string{"Path: me!not-for-mail", "From: a"}},
	}
	for _, tc := range cases {
		a := &Article{Head: slices.Clone(tc.head)}
		a.prependPath("me")
		if !slices.Equal(a.Head, tc.want) {
			t.Errorf("prependPath(%q) = %q, want %q", tc.head, a.Head, tc.want)
		}
	}
}

// wireArticles are raw articles that must survive Parse + Wire byte-exact.
var wireArticles = map[string]string{
	"unknown headers, odd order": "X-Zeta: z\r\n" +
		"Subject: s\r\n" +
		"X-Alpha: a\r\n" +
		"Newsgroups: local.test\r\n" +
		"Message-ID: <1@x>\r\n" +
		"From: a@b.c\r\n" +
		"Path: p!not-for-mail\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"x-lower: l\r\n" +
		"\r\nbody\r\n",
	"folded headers": "Path: p!not-for-mail\r\n" +
		"Subject: a long\r\n" +
		"\tsubject folded with a tab\r\n" +
		"References: <1@x>\r\n" +
		"  <2@x>\r\n" +
		" <3@x>\r\n" +
		"X-NoSpace:v\r\n" +
		"\r\nbody\r\n",
	"duplicate non-singletons": "Received: from a\r\n" +
		"Subject: s\r\n" +
		"Received: from b\r\n" +
		"X-Trace: 1\r\n" +
		"X-Trace: 2\r\n" +
		"\r\nbody\r\n",
	"bare CR in body": "Subject: s\r\n\r\ncol1\rcol2\r\nend\r\r\n",
	"dot lines":       "Subject: s\r\n\r\n.\r\n..\r\n.leading\r\n\r\nlast\r\n",
	"empty body":      "Subject: s\r\n\r\n",
	"8bit and tabs":   "Subject: gr\xc3\xbc\xc3\x9fe\t tab\r\n\r\n\tindented\r\n",
}

func TestParseWireRoundTrip(t *testing.T) {
	for name, raw := range wireArticles {
		a, err := Parse([]byte(raw))
		if err != nil {
			t.Errorf("%s: Parse: %v", name, err)
			continue
		}
		if got := string(a.Wire()); got != raw {
			t.Errorf("%s: Wire()\n%q\nwant\n%q", name, got, raw)
		}
		if a.Bytes() != len(raw) {
			t.Errorf("%s: Bytes() = %d, want %d", name, a.Bytes(), len(raw))
		}
	}
	a, _ := Parse([]byte(wireArticles["bare CR in body"]))
	if a.Lines() != 2 {
		t.Errorf("bare CR must not count as a line: Lines() = %d", a.Lines())
	}
}

func TestParseLFLineEnds(t *testing.T) {
	a, err := Parse([]byte("Subject: s\n\tfolded\nFrom: a\n\nline1\nmixed\r\ncol1\rcol2\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "Subject: s\r\n\tfolded\r\nFrom: a\r\n\r\nline1\r\nmixed\r\ncol1\rcol2\r\n"
	if got := string(a.Wire()); got != want {
		t.Fatalf("Wire() = %q, want %q", got, want)
	}
	if a.Lines() != 3 {
		t.Fatalf("Lines() = %d, want 3", a.Lines())
	}
}

func TestParseRejectsInvalidHeaderBlock(t *testing.T) {
	raw := "From nobody Thu Dec  8 12:55:04 2016\r\n" +
		"From: a@b.c\r\n" +
		"Subject: s\r\n" +
		"\r\nbody\r\n"
	if _, err := Parse([]byte(raw)); !errors.Is(err, ErrInvalidHeaderBlock) {
		t.Fatalf("Parse = %v, want ErrInvalidHeaderBlock", err)
	}
	if _, err := Parse([]byte("Subject: a\rb\r\n\r\nbody\r\n")); !errors.Is(err, ErrInvalidHeaderBlock) {
		t.Fatalf("Parse with CR in a header line = %v, want ErrInvalidHeaderBlock", err)
	}
	a, err := ParseLenient([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"From: a@b.c", "Subject: s"}; !slices.Equal(a.Head, want) {
		t.Fatalf("lenient head %q, want %q", a.Head, want)
	}
}

func TestInjectForIHaveChangesOnlyPathAndXref(t *testing.T) {
	raw := "X-First: 1\r\n" +
		"Path: news-a!not-for-mail\r\n" +
		"From: a@b.c\r\n" +
		"Subject: a folded\r\n" +
		"\tsubject\r\n" +
		"Xref: news-a local.test:1\r\n" +
		"\tlocal.other:2\r\n" +
		"Newsgroups: local.test\r\n" +
		"Message-ID:   <1@news-a>\r\n" +
		"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
		"Received: one\r\n" +
		"Received: two\r\n" +
		"\r\n" +
		"col1\rcol2\r\n.dot\r\n"
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := InjectForIHave(a, InjectOpts{Pathhost: "news-b", Hostname: "news-b"}); err != nil {
		t.Fatal(err)
	}
	want := strings.Replace(raw, "Path: news-a!", "Path: news-b!news-a!", 1)
	want = strings.Replace(want, "Xref: news-a local.test:1\r\n\tlocal.other:2\r\n", "", 1)
	if got := string(a.Wire()); got != want {
		t.Fatalf("Wire() after InjectForIHave\n%q\nwant\n%q", got, want)
	}
	if a.Get("Message-ID") != "<1@news-a>" {
		t.Fatalf("Message-ID %q", a.Get("Message-ID"))
	}
}

func TestInjectForPostKeepsHeadOrder(t *testing.T) {
	raw := "X-First: 1\r\n" +
		"Newsgroups: local.test\r\n" +
		"Subject: s\r\n" +
		"\tfolded\r\n" +
		"From: a@b.c\r\n" +
		"Xref: stale local.test:9\r\n" +
		"\r\nbody\r\n"
	a, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := InjectForPost(a, InjectOpts{Pathhost: "news.example", Hostname: "news.example"}); err != nil {
		t.Fatal(err)
	}
	if a.Head[0] != "Path: news.example!not-for-mail" {
		t.Fatalf("Path not inserted first: %q", a.Head)
	}
	want := []string{"X-First: 1", "Newsgroups: local.test", "Subject: s", "\tfolded", "From: a@b.c"}
	if !slices.Equal(a.Head[1:6], want) {
		t.Fatalf("poster's lines changed: %q", a.Head)
	}
	for _, name := range []string{"Message-ID", "Date", "Injection-Date", "Injection-Info"} {
		if !a.Has(name) {
			t.Errorf("missing %s", name)
		}
	}
	if a.Has("Xref") {
		t.Error("Xref not stripped")
	}
}
