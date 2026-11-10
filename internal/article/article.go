package article

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Article is a Netnews article on the wire (headers + body, CRLF).
//
// The head is kept as received so that Wire returns the same bytes: RFC 5537
// §3.2/3.3 forbid relaying and serving agents to alter, delete or rearrange
// any part of an article except Path and Xref.
type Article struct {
	// Head holds the header lines in received order, without CRLF. A folded
	// field keeps each continuation line as its own entry, with its leading
	// whitespace.
	Head []string
	// Body is the body as received, with CRLF line ends.
	Body string
}

// ErrInvalidHeaderBlock is wrapped by the Parse errors for a malformed head.
var ErrInvalidHeaderBlock = errors.New("invalid header block")

// singletonHeaders must not appear more than once (lower-case names).
var singletonHeaders = map[string]bool{
	"date": true, "from": true, "message-id": true,
	"newsgroups": true, "path": true, "subject": true,
}

// Parse splits an article into head and body and validates the head strictly,
// as NNTP intake (POST, IHAVE, TAKETHIS) requires; see checkHeaderBlock.
func Parse(raw []byte) (*Article, error) {
	return parse(raw, false)
}

// ParseLenient is Parse for articles that are imported or already stored: a
// malformed header line is dropped (with its continuation lines) instead of
// rejecting the article. NUL, CR or LF inside a header line and a duplicated
// Date, From, Message-ID, Newsgroups, Path or Subject still reject.
func ParseLenient(raw []byte) (*Article, error) {
	return parse(raw, true)
}

func parse(raw []byte, lenient bool) (*Article, error) {
	text := string(crlfLineEnds(raw))
	var head, body string
	if strings.HasPrefix(text, "\r\n") {
		body = text[2:]
	} else {
		var ok bool
		head, body, ok = strings.Cut(text, "\r\n\r\n")
		if !ok {
			// empty body is allowed if trailing CRLF pair present
			if !strings.HasSuffix(text, "\r\n") {
				return nil, fmt.Errorf("article has no header/body separator")
			}
			head, body = strings.TrimSuffix(text, "\r\n"), ""
		}
	}
	var lines []string
	if head != "" {
		lines = strings.Split(head, "\r\n")
	}
	lines, err := checkHeaderBlock(lines, lenient)
	if err != nil {
		return nil, err
	}
	return &Article{Head: lines, Body: body}, nil
}

// crlfLineEnds turns every bare LF into CRLF, as Conn.ReadArticle frames
// lines. A bare CR is not a line end and is kept.
func crlfLineEnds(b []byte) []byte {
	lf := bytes.Count(b, []byte("\n"))
	if lf == bytes.Count(b, []byte("\r\n")) {
		return b
	}
	out := make([]byte, 0, len(b)+lf)
	for i, c := range b {
		if c == '\n' && (i == 0 || b[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, c)
	}
	return out
}

// checkHeaderBlock validates the header lines (without CRLF and without the
// empty separator line). It rejects
//  1. a line containing NUL, CR or LF;
//  2. a continuation line (leading SP or TAB) with no field line before it;
//  3. a field line without a colon, with an empty name, or with a name octet
//     outside RFC 5322 ftext (%x21-39 / %x3B-7E);
//  4. a second Date, From, Message-ID, Newsgroups, Path or Subject (any case).
//
// In lenient mode a line breaking rule 2 or 3 is dropped instead, and so are
// the continuation lines of a line breaking rule 3. The input is not modified.
func checkHeaderBlock(lines []string, lenient bool) ([]string, error) {
	reject := func(i int, reason string) ([]string, error) {
		return nil, fmt.Errorf("%w: line %d: %s", ErrInvalidHeaderBlock, i+1, reason)
	}
	out := lines
	dropped := false
	drop := func(i int) {
		if !dropped {
			dropped = true
			out = slices.Clone(lines[:i])
		}
	}
	keep := func(line string) {
		if dropped {
			out = append(out, line)
		}
	}
	seen := map[string]bool{}
	inField := false
	for i, line := range lines {
		if strings.ContainsAny(line, "\x00\r\n") {
			return reject(i, "NUL, CR or LF in header line")
		}
		if isContinuation(line) {
			if !inField {
				if !lenient {
					return reject(i, "continuation line without a field")
				}
				drop(i)
				continue
			}
			keep(line)
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 || !validFieldName(line[:colon]) {
			if !lenient {
				return reject(i, "malformed header field")
			}
			drop(i)
			inField = false
			continue
		}
		if name := strings.ToLower(line[:colon]); singletonHeaders[name] {
			if seen[name] {
				return reject(i, "duplicate "+line[:colon])
			}
			seen[name] = true
		}
		inField = true
		keep(line)
	}
	return out, nil
}

// validFieldName reports whether name is non-empty RFC 5322 ftext.
func validFieldName(name string) bool {
	for i := 0; i < len(name); i++ {
		if c := name[i]; c < 0x21 || c > 0x7e || c == ':' {
			return false
		}
	}
	return name != ""
}

// isContinuation reports whether line continues the previous header field.
func isContinuation(line string) bool {
	return line != "" && (line[0] == ' ' || line[0] == '\t')
}

// fieldColon returns the index of the colon when line is a field line (not a
// continuation line) named name, compared case-insensitively.
func fieldColon(line, name string) (int, bool) {
	if isContinuation(line) {
		return -1, false
	}
	colon := strings.IndexByte(line, ':')
	if colon < 0 || !strings.EqualFold(strings.TrimSpace(line[:colon]), name) {
		return -1, false
	}
	return colon, true
}

// findField returns the index of the first field line named name and of its
// colon, or -1, -1.
func findField(lines []string, name string) (int, int) {
	for i, line := range lines {
		if colon, ok := fieldColon(line, name); ok {
			return i, colon
		}
	}
	return -1, -1
}

// fieldEnd returns the index after the last continuation line of the field
// that starts at lines[i].
func fieldEnd(lines []string, i int) int {
	i++
	for i < len(lines) && isContinuation(lines[i]) {
		i++
	}
	return i
}

// headerValue returns the value of the first field named name, unfolded for
// reading: continuation lines are joined by one space, the value is trimmed.
func headerValue(lines []string, name string) string {
	i, colon := findField(lines, name)
	if i < 0 {
		return ""
	}
	value := strings.TrimSpace(lines[i][colon+1:])
	for _, cont := range lines[i+1 : fieldEnd(lines, i)] {
		if part := strings.TrimSpace(cont); part != "" {
			if value != "" {
				value += " "
			}
			value += part
		}
	}
	return value
}

// HeaderValue looks up name in a stored head (lines without the separator
// line) the way Article.Get does: case-insensitive, continuation lines
// unfolded. Lines may end in CRLF or LF (older stored heads).
func HeaderValue(head, name string) string {
	lines := strings.Split(head, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return headerValue(lines, name)
}

// Get returns the value of the first field named name (case-insensitive),
// unfolded and trimmed; "" when absent.
func (a *Article) Get(name string) string {
	return headerValue(a.Head, name)
}

// Has reports whether name is present with a non-empty value.
func (a *Article) Has(name string) bool {
	return a.Get(name) != ""
}

// Set replaces the first field named name, with its continuation lines, in
// place by the single line "Name: value", keeping the existing spelling of
// the name, and removes further fields of that name. A field that is absent
// is appended to the head as "name: value".
func (a *Article) Set(name, value string) {
	i, colon := findField(a.Head, name)
	if i < 0 {
		a.Head = append(a.Head, name+": "+value)
		return
	}
	a.Head = slices.Replace(a.Head, i, fieldEnd(a.Head, i), a.Head[i][:colon]+": "+value)
	a.deleteFrom(i+1, name)
}

// Delete removes every field named name together with its continuation lines.
func (a *Article) Delete(name string) {
	a.deleteFrom(0, name)
}

func (a *Article) deleteFrom(start int, name string) {
	out := a.Head[:start]
	skipping := false
	for _, line := range a.Head[start:] {
		if skipping && isContinuation(line) {
			continue
		}
		if _, skipping = fieldColon(line, name); !skipping {
			out = append(out, line)
		}
	}
	clear(a.Head[len(out):])
	a.Head = out
}

// prependPath adds host as the first Path hop and keeps the rest of the Path
// field as it is. A missing Path is inserted as the first header line.
func (a *Article) prependPath(host string) {
	i, colon := findField(a.Head, "Path")
	if i < 0 {
		a.Head = slices.Insert(a.Head, 0, "Path: "+host+"!not-for-mail")
		return
	}
	line := a.Head[i]
	v := colon + 1
	for v < len(line) && (line[v] == ' ' || line[v] == '\t') {
		v++
	}
	switch {
	case v < len(line):
		a.Head[i] = line[:v] + host + "!" + line[v:]
	case a.Has("Path"): // the value starts on a continuation line
		a.Set("Path", host+"!"+a.Get("Path"))
	default:
		a.Set("Path", host+"!not-for-mail")
	}
}

func (a *Article) Newsgroups() []string {
	raw := a.Get("Newsgroups")
	if raw == "" {
		return nil
	}
	var out []string
	for _, g := range strings.Split(raw, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			out = append(out, g)
		}
	}
	return out
}

// RawHeaders returns the head as CRLF-separated lines, without the separator.
func (a *Article) RawHeaders() string {
	return strings.Join(a.Head, "\r\n")
}

// Wire returns the article without dot-stuffing: the head lines in order,
// an empty line, and the body.
func (a *Article) Wire() []byte {
	b := make([]byte, 0, a.Bytes())
	for _, line := range a.Head {
		b = append(b, line...)
		b = append(b, '\r', '\n')
	}
	b = append(b, '\r', '\n')
	b = append(b, a.Body...)
	if a.Body != "" && !strings.HasSuffix(a.Body, "\r\n") {
		b = append(b, '\r', '\n')
	}
	return b
}

// Bytes returns len(a.Wire()).
func (a *Article) Bytes() int {
	n := 2 + len(a.Body)
	for _, line := range a.Head {
		n += len(line) + 2
	}
	if a.Body != "" && !strings.HasSuffix(a.Body, "\r\n") {
		n += 2
	}
	return n
}

func (a *Article) Lines() int {
	if a.Body == "" {
		return 0
	}
	n := strings.Count(a.Body, "\n")
	if !strings.HasSuffix(a.Body, "\n") {
		n++
	}
	return n
}

type InjectOpts struct {
	Pathhost     string
	Hostname     string
	Organization string
	Now          time.Time
}

// InjectForPost validates and fills RFC 5536 injection headers.
func InjectForPost(a *Article, opt InjectOpts) error {
	if opt.Now.IsZero() {
		opt.Now = time.Now().UTC()
	}
	if strings.TrimSpace(a.Get("From")) == "" {
		return fmt.Errorf("missing From")
	}
	if len(a.Newsgroups()) == 0 {
		return fmt.Errorf("missing Newsgroups")
	}
	if strings.TrimSpace(a.Get("Subject")) == "" {
		return fmt.Errorf("missing Subject")
	}
	for _, g := range a.Newsgroups() {
		if !ValidGroupName(g) {
			return fmt.Errorf("bad newsgroup name %q", g)
		}
	}
	if msgid := strings.TrimSpace(a.Get("Message-ID")); msgid != "" {
		if !ValidMessageID(msgid) {
			return fmt.Errorf("bad Message-ID")
		}
		a.Set("Message-ID", msgid)
	} else {
		a.Set("Message-ID", GenerateMessageID(opt.Hostname))
	}
	if !a.Has("Date") {
		a.Set("Date", opt.Now.Format(time.RFC1123Z))
	}
	if !strings.HasPrefix(a.Get("Path"), opt.Pathhost+"!") {
		a.prependPath(opt.Pathhost)
	}
	if opt.Organization != "" && !a.Has("Organization") {
		a.Set("Organization", opt.Organization)
	}
	if !a.Has("Injection-Date") {
		// INN parses this with parsedate_rfc5322_lax and rejects the
		// compact YYYYMMDDHHMMSSZ form, then remembers the message-id.
		a.Set("Injection-Date", opt.Now.UTC().Format(time.RFC1123Z))
	}
	if !a.Has("Injection-Info") {
		a.Set("Injection-Info", fmt.Sprintf("%s; posting-account=\"openusenet\"", opt.Hostname))
	}
	stripXref(a)
	return nil
}

// InjectForIHave validates a transferred article and prepends Path.
func InjectForIHave(a *Article, opt InjectOpts) error {
	if strings.TrimSpace(a.Get("From")) == "" {
		return fmt.Errorf("missing From")
	}
	if len(a.Newsgroups()) == 0 {
		return fmt.Errorf("missing Newsgroups")
	}
	if strings.TrimSpace(a.Get("Subject")) == "" {
		return fmt.Errorf("missing Subject")
	}
	for _, g := range a.Newsgroups() {
		if !ValidGroupName(g) {
			return fmt.Errorf("bad newsgroup name %q", g)
		}
	}
	msgid := strings.TrimSpace(a.Get("Message-ID"))
	if msgid == "" {
		return fmt.Errorf("missing Message-ID")
	}
	if !ValidMessageID(msgid) {
		return fmt.Errorf("bad Message-ID")
	}
	if strings.TrimSpace(a.Get("Date")) == "" {
		return fmt.Errorf("missing Date")
	}
	if PathContains(a.Get("Path"), opt.Pathhost) {
		return fmt.Errorf("path loop")
	}
	a.prependPath(opt.Pathhost)
	stripXref(a)
	return nil
}

func stripXref(a *Article) {
	a.Delete("Xref")
}

// PathContains reports whether token appears as a bang-separated Path hop.
func PathContains(path, token string) bool {
	token = strings.TrimSpace(token)
	if token == "" || path == "" {
		return false
	}
	for _, hop := range strings.Split(path, "!") {
		// A folded Path unfolds to "a !b": the white space is not part of the site.
		if strings.EqualFold(strings.TrimSpace(hop), token) {
			return true
		}
	}
	return false
}

func ValidGroupName(g string) bool {
	if g == "" || len(g) > 255 {
		return false
	}
	for _, r := range g {
		if r > 127 {
			return false
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '+' || r == '-' || r == '_' {
			continue
		}
		return false
	}
	if strings.HasPrefix(g, ".") || strings.HasSuffix(g, ".") || strings.Contains(g, "..") {
		return false
	}
	return true
}

func ValidMessageID(id string) bool {
	if len(id) < 3 || len(id) > MaxMessageID {
		return false
	}
	if id[0] != '<' || id[len(id)-1] != '>' {
		return false
	}
	inner := id[1 : len(id)-1]
	if strings.ContainsAny(inner, "<> \t\r\n") {
		return false
	}
	for i := 0; i < len(inner); i++ {
		if inner[i] < 0x21 || inner[i] > 0x7e {
			return false
		}
	}
	return strings.Contains(inner, "@")
}

const MaxMessageID = 250

func GenerateMessageID(host string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if host == "" {
		host = "localhost"
	}
	return fmt.Sprintf("<%d.%s@%s>", time.Now().UnixNano(), hex.EncodeToString(b[:]), host)
}

func OverviewLine(num int64, a *Article, xref string) string {
	fields := []string{
		strconv.FormatInt(num, 10),
		sanitizeOverview(a.Get("Subject")),
		sanitizeOverview(a.Get("From")),
		sanitizeOverview(a.Get("Date")),
		sanitizeOverview(a.Get("Message-ID")),
		sanitizeOverview(a.Get("References")),
		strconv.Itoa(a.Bytes()),
		strconv.Itoa(a.Lines()),
	}
	if xref != "" {
		// OVERVIEW.FMT lists this column as "Xref:full". RFC 2980 says the
		// :full suffix means the overview value itself is "Xref: <value>",
		// not the literal format token "Xref:full".
		fields = append(fields, "Xref: "+xref)
	}
	return strings.Join(fields, "\t")
}

func sanitizeOverview(s string) string {
	s = strings.ReplaceAll(s, "\t", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return s
}
