package nntp_test

import (
	"bufio"
	"fmt"
	"strings"
	"testing"
)

// eightBitBodies are article bodies that are not valid UTF-8 or contain NUL.
// They must be accepted and served back byte for byte.
func eightBitBodies() map[string]string {
	var yenc strings.Builder
	yenc.WriteString("=ybegin line=128 size=252 name=bytes.bin\r\n")
	for c := 1; c < 256; c++ {
		switch c {
		case '\r', '\n', '=':
			continue
		}
		yenc.WriteByte(byte(c))
	}
	yenc.WriteString("\r\n=yend size=252\r\n")
	return map[string]string{
		"latin1": "caf\xe9 noir, na\xefve\r\n",
		"nul":    "before\x00after\r\n",
		"yenc":   yenc.String(),
	}
}

// readBytesBlock reads a multi-line response and returns the status line and
// the payload with dot-stuffing undone, keeping every byte.
func readBytesBlock(t *testing.T, r *bufio.Reader) (string, string) {
	t.Helper()
	first := readLine(t, r)
	var b strings.Builder
	for {
		l, err := r.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if l == ".\r\n" {
			break
		}
		b.WriteString(strings.TrimPrefix(l, "."))
	}
	return first, b.String()
}

func TestIHaveStoresEightBitBodyExactly(t *testing.T) {
	for name, body := range eightBitBodies() {
		t.Run(name, func(t *testing.T) {
			c, _ := startTestServer(t)
			r := bufio.NewReader(c)
			_ = readLine(t, r)
			msgid := "<ihave-" + name + "@news.test>"
			_, _ = c.Write([]byte("IHAVE " + msgid + "\r\n"))
			if l := readLine(t, r); !strings.HasPrefix(l, "335 ") {
				t.Fatalf("ihave cont %q", l)
			}
			art := "Path: other!not-for-mail\r\n" +
				"From: a@b.c\r\n" +
				"Newsgroups: local.test\r\n" +
				"Subject: caf\xe9\r\n" +
				"Date: Mon, 01 Jan 2024 00:00:00 +0000\r\n" +
				"Message-ID: " + msgid + "\r\n" +
				"\r\n" + body + ".\r\n"
			_, _ = c.Write([]byte(art))
			if l := readLine(t, r); !strings.HasPrefix(l, "235 ") {
				t.Fatalf("ihave %q", l)
			}
			_, _ = c.Write([]byte("BODY " + msgid + "\r\n"))
			first, got := readBytesBlock(t, r)
			if !strings.HasPrefix(first, "222 ") {
				t.Fatalf("body %q", first)
			}
			if got != body {
				t.Fatalf("body changed:\n got %q\nwant %q", got, body)
			}
			_, _ = c.Write([]byte("HEAD " + msgid + "\r\n"))
			first, head := readBytesBlock(t, r)
			if !strings.HasPrefix(first, "221 ") {
				t.Fatalf("head %q", first)
			}
			if !strings.Contains(head, "Subject: caf\xe9\r\n") {
				t.Fatalf("subject bytes changed: %q", head)
			}
		})
	}
}

func TestPostStoresEightBitBodyExactly(t *testing.T) {
	for name, body := range eightBitBodies() {
		t.Run(name, func(t *testing.T) {
			c, _ := startTestServer(t)
			r := bufio.NewReader(c)
			_ = readLine(t, r)
			msgid := fmt.Sprintf("<post-%s@news.test>", name)
			_, _ = c.Write([]byte("POST\r\n"))
			if l := readLine(t, r); !strings.HasPrefix(l, "340 ") {
				t.Fatalf("post cont %q", l)
			}
			art := "From: tester@example.com\r\n" +
				"Newsgroups: local.test\r\n" +
				"Subject: hello\r\n" +
				"Message-ID: " + msgid + "\r\n" +
				"\r\n" + body + ".\r\n"
			_, _ = c.Write([]byte(art))
			if l := readLine(t, r); !strings.HasPrefix(l, "240 ") {
				t.Fatalf("post %q", l)
			}
			_, _ = c.Write([]byte("ARTICLE " + msgid + "\r\n"))
			first, got := readBytesBlock(t, r)
			if !strings.HasPrefix(first, "220 ") {
				t.Fatalf("article %q", first)
			}
			if !strings.HasSuffix(got, "\r\n\r\n"+body) {
				t.Fatalf("article body changed:\n got %q\nwant suffix %q", got, body)
			}
		})
	}
}
