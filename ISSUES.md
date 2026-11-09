# Known Issues

Findings about OpenUsenetServer 0.8.0 since 2026-11-09. They come from the first blind test
runs against an unmodified server (`config.example.yml`, PostgreSQL 16, test VM), by other
projects' tools, and from reading the code those runs pointed at.
[REVIEW.md](REVIEW.md) tracks how much of the code has been reviewed. Both files are kept up to
date on branch `dev-gw` of the test fork
[go-while/OpenUsenetServer](https://github.com/go-while/OpenUsenetServer/tree/dev-gw), where the
tests below live.

Security problems do not go here: report them privately as described in
[SECURITY.md](SECURITY.md).

Line numbers refer to `main` at `abe8543` unless stated otherwise. "Verified" means checked
against the code by reading it; "reported" means a test or a user reported it and it has not
been reproduced or checked yet.

Status values: **open**, **PR** (fix waiting for review, with its number), **fixed** (with
the PR that fixed it), **decision** (needs the maintainer to decide), **won't fix**.

## Test runs so far

| Run | Tool | Result |
|---|---|---|
| Conformance, in-process | `e2e/conformance_test.go` (`make e2e`; on the fork's `dev-gw`, not in `main` yet), nntp-testkit v0.11.2, memory store | 30 pass, 4 fail, 7 skip (empty group) |
| Conformance, live | `nntp-conformance` v0.11.2 against `openusenet serve` with PostgreSQL | 1st pass 30/4/7, 2nd pass 37 pass, 4 fail, 0 skip |
| pynntp integration tests | pynntp `main` + portable tests (greenbender/pynntp#144), anonymous | 4 pass, 22 fail, 8 skip; one server-side failure (O-01), the rest pynntp or test assumptions |
| Diablo feed interop | Diablo `dnewslink`, 5 articles per mode, then resent | streaming and IHAVE work; duplicates get 438/435; one finding (O-05) |
| Build report | OCTADE, `news.software.nntp` 815 (2026-10-06) | does not build on Debian; Docker and Podman builds fail on package conflicts (O-07) |
| NewsCache interop | NewsCache 1.3.23 with OUS as upstream, 203 commands direct vs through the cache, twice; 3 posts | reads, error codes and COMPRESS fallback match; posted bodies byte-exact; three findings (O-10 to O-12) |

## Overview

| ID | Severity | Summary | Status |
|---|---|---|---|
| [O-01](#o-01-wrong-password-gets-480-not-481) | Medium | A wrong AUTHINFO password gets 480 instead of 481 | open |
| [O-02](#o-02-authinfo-pass-before-user-gets-501-not-482) | Low | AUTHINFO PASS before USER gets 501 instead of 482 | open |
| [O-03](#o-03-authinfo-after-a-login-is-accepted-and-drops-the-login) | Low | AUTHINFO after a successful login is accepted (381) instead of 502, and logs the session out | open |
| [O-04](#o-04-capabilities-still-lists-authinfo-after-a-login) | Low | CAPABILITIES still lists `AUTHINFO USER` after a login | open |
| [O-05](#o-05-the-article-head-is-rebuilt-not-kept) | Medium | The article head is rebuilt, not kept: header order changes, unknown headers come out in random order, folding is lost | open |
| [O-06](#o-06-235-and-239-reply-text-repeats-the-message-id) | Low | 239 (and 235) reply text repeats the message-id | open |
| [O-07](#o-07-build-fails-on-debian-and-with-docker-or-podman) | Medium | Build fails on Debian, Docker and Podman builds fail on package conflicts | open (reported) |
| [O-08](#o-08-the-example-path-host-reaches-peers) | Low | Running with the example config prepends `news.localhost!` to Path | decision |
| [O-09](#o-09-test-dependency-raises-the-go-version) | Low | The e2e test dependency raises `go.mod` to Go 1.26 | fixed on the fork (`e2e/` is its own module) |
| [O-10](#o-10-stored-articles-have-no-xref-header) | Medium | Stored articles have no Xref header, but OVER's `Xref:full` field has one | open |
| [O-11](#o-11-over-by-message-id-reports-the-wrong-bytes) | Low | OVER by message-id reports the wrong `:bytes` | open |
| [O-12](#o-12-injection-info-always-says-posting-accountopenusenet) | Low | Injection-Info always says `posting-account="openusenet"`, not the logged-in user | open |
| [O-13](#o-13-8-bit-articles-cannot-be-stored-and-get-436-forever) | High | Articles with non-UTF-8 bytes or NUL (Latin-1 text, yEnc) cannot be stored; IHAVE answers 436, so peers retry forever | open |
| [O-14](#o-14-go-vet-fails-on-main) | Low | `go vet ./...` fails on `main`: self-assignment in `internal/mail/settings.go` | open |

## Details

### O-01 Wrong password gets 480, not 481
RFC 4643 section 2.3.2: a rejected password gets 481 ("Authentication failed/rejected"); 480
means "authentication required" and RFC 4643 section 2.3.1 forbids it as a reply to AUTHINFO.
`cmdAuthinfo` answers `480 Authentication failed` (`internal/nntp/session.go:647`,
`FailAuthNeeded`). Clients that treat 480 as "log in first" may retry the same credentials in
a loop. Verified. Found by nntp-testkit (`authinfo-wrong-pass`) and pynntp's tests.
Fix: a 481 constant in `internal/nntp/codes.go` and use it here.

### O-02 AUTHINFO PASS before USER gets 501, not 482
RFC 4643 section 2.3.2: AUTHINFO PASS without a preceding USER is a sequence error, 482.
The server answers `501 AUTHINFO USER required first` (`internal/nntp/session.go:626`).
Verified. Found by nntp-testkit (`authinfo-pass-without-user`).

### O-03 AUTHINFO after a login is accepted and drops the login
RFC 4643 section 2.2: once a session is authenticated, every further AUTHINFO gets 502. The
server answers a second `AUTHINFO USER` with 381 (`internal/nntp/session.go:612-620`), and
that branch also clears `authOK`, `authUser` and `authUserID`, so a logged-in session that
sends AUTHINFO USER again is logged out even if it never sends PASS. The same applies after a
peer login through `tryFeedAuth` (`feedAuthOK` is never checked here). Verified. Found by
nntp-testkit (`authinfo-after-auth`).

### O-04 CAPABILITIES still lists AUTHINFO after a login
RFC 4643 section 2.2: after a successful login the server must not advertise AUTHINFO any
more. `cmdCapabilities` returns a fixed list that always contains `AUTHINFO USER`
(`internal/nntp/session.go:150-170`). Verified. Found by nntp-testkit
(`authinfo-capabilities`). Diablo fixed O-01 to O-04 together (its D-46, diablo-nntp #67) and
pins them with the six RFC 4643 checks of nntp-testkit; the fork's `e2e/conformance_test.go` can do the
same once they are fixed.

### O-05 The article head is rebuilt, not kept
RFC 5537 section 3.2 (relaying agents) and 3.3 (serving agents): an agent "MUST NOT alter,
delete, or rearrange any part of an article" except Path and Xref. `article.Parse` splits the
head into `map[string][]string` (`internal/article/article.go:15-84`), and `Wire()`
(`article.go:122-164`) writes it back in its own order: a fixed list of known headers first,
then all other headers in Go map iteration order, which is random. Effects:
- the received order is lost (Diablo sent `Subject, Message-ID, Date`; HEAD returned
  `Subject, Date, Message-ID`);
- headers outside the list (`X-*`, `Cancel-Lock`, `NNTP-Posting-Host`, `Lines`, ...) can come
  out in a different order every time `Wire()` runs;
- folded header lines are joined into one line and their whitespace trimmed
  (`parseHeaders`, `article.go:51-84`), so long `References` or `Path` lines can exceed the
  998-character line limit of RFC 5536 section 3.2.4 (via RFC 5322);
- two spellings of one header name (`Message-ID`, `Message-Id`) are separate map keys.

`Wire()` is what IHAVE/TAKETHIS (`internal/nntp/session.go:1139`, `:1175`), POST
(`internal/posting/posting.go:139`), moderation, the mbox archive and `Bytes()` use, so the
rebuilt head is what gets stored, served and fed to peers. Signed headers (for example the
PGP signature of control messages, which covers listed header values) can break when
whitespace changes. Verified. Found by the Diablo feed interop run.
Fix direction: keep `RawHeaders` (already parsed, `article.go:16`) as the source of truth and
change only Path (prepend) and Xref (replace) in place.

### O-06 235 and 239 reply text repeats the message-id
For TAKETHIS the reply is `239 <id> article transferred <id>`: `replyTransfer` puts the id
first as RFC 4644 requires, and the text passed in by `acceptPeerArticle` adds it again
(`internal/nntp/session.go:1206`). Legal, only redundant; for IHAVE (`235 article
transferred <id>`) the id in the text is fine. Verified. Found by the Diablo feed interop run.

### O-07 Build fails on Debian and with Docker or Podman
Reported in `news.software.nntp` (article 815, 2026-10-06): "it does not want to compile on
Debian (package conflicts). Also trying docker and podman exposes package conflicts." Not
reproduced yet. Likely parts, checked on a Debian 13 VM:
- `go.mod` needs Go 1.25 (`main`); Debian 13 ships `golang-go` 1.24, which refuses the
  module unless it may download a newer toolchain;
- the runtime image is Ubuntu 24.04 because cleanfeed-ng needs Perl 5.38 or later
  (`Dockerfile`); Debian 13 has Perl 5.40, Debian 12 only 5.36.
Next step: build `main` with the distribution's Go on the test VMs (Debian, Ubuntu, Fedora,
Alpine, ...) and run `docker build` and `podman build`, and record what fails.

### O-08 The example path host reaches peers
With `config.example.yml`, `server.pathhost` is `news.localhost`, and every article the
server relays gets `news.localhost!` prepended to Path (seen in the Diablo interop run). A
server started from the example and peered as-is sends that name to its peers. Behaviour as
configured; a startup warning when `pathhost` (or `hostname`) is still the example value would
help. Decision for the maintainer.

### O-09 Test dependency raises the Go version
The e2e conformance test (`e2e/` on the fork's branch `dev-gw`) imports nntp-testkit, which needs Go 1.26, so
`go mod tidy` raises the `go` line of the main module from 1.25 to 1.26 and the Dockerfile
builder image has to follow. That makes O-07 worse for distribution builds. Alternative: give
`e2e/` its own `go.mod` (with a `replace openusenet => ..`), so only the tests need Go 1.26.
Done on `dev-gw`: `e2e/go.mod` is module `openusenet/e2e` (Go 1.26, nntp-testkit v0.11.2,
`replace openusenet => ../`); the server's `go.mod` and the Dockerfile stay at Go 1.25.
`go test ./...` in the root does not enter `e2e/`; `make e2e` runs it.

### O-10 Stored articles have no Xref header
RFC 3977 section 8.4.1: the `Xref:full` overview field is the article's Xref header, so the
two should agree (INN writes Xref into the stored article). `InjectForIHave` and
`InjectForPost` strip any incoming Xref (`internal/article/article.go:231`, `stripXref`), and
after `Post` the new Xref is set on the article only for the mbox copy
(`internal/nntp/session.go:1234`, `internal/posting/posting.go:157`, both inside
`if mbox != nil`); the head stored in the database has none. OVER still prints
`Xref: news.localhost local.test:1` from the stored `xref` column. Effect seen with
NewsCache: it rebuilds its overview from the article once it caches it, so through the cache
the field goes empty. Verified. Found by the NewsCache interop run.

### O-11 OVER by message-id reports the wrong bytes
`OVER <message-id>` builds a stand-in `Article` with only Subject, From, Date, Message-ID and
References plus the body (`internal/nntp/session.go:498-504`) and lets `OverviewLine` count
`:bytes` from that (`Bytes()` is `len(Wire())`, `internal/article/article.go:165`), so every
other header is missing from the count. The same article gave `:bytes` 365 with OVER by number
and 300 by message-id. The stored `bytes` column (`a.Bytes`, already loaded) is the right
value. Verified. Found by the NewsCache interop run.

### O-12 Injection-Info always says posting-account="openusenet"
`InjectForPost` writes `Injection-Info: <host>; posting-account="openusenet"` for every post
(`internal/article/article.go:229`); `InjectOpts` has no field for the user, although the
session knows it (`s.authUser`). A post by `nc-interop` carried `openusenet`. RFC 5536
section 3.2.8 (Injection-Info) describes `posting-account` as identifying the poster's
account for abuse handling; a fixed value makes it useless (a hashed or plain user name, as
INN and others do, would be the fix). Verified. Found by the NewsCache interop run.

### O-13 8-bit articles cannot be stored and get 436 forever
The `articles` table keeps `headers` and `body` as PostgreSQL `TEXT`
(`internal/store/postgres.go:37-38`), and the transfer and posting paths pass the raw bytes
without conversion (`SanitizeUTF8` in `internal/article/utf8.go` is only used for display, the
ISC list and archive import). PostgreSQL rejects any byte sequence that is not valid UTF-8, and
any NUL. Reproduced on the test server (IHAVE, `local.test`):
- body `caf\xe9` (Latin-1): `335`, then `436 try again later`; log: `peer transfer store
  <...>: ERROR: invalid byte sequence for encoding "UTF8": 0xe9 0x20 0x6e (SQLSTATE 22021)`;
- body with a NUL byte: the same, `0x00`;
- the same text in UTF-8: `235`, stored.

436 tells the peer to offer the article again later, so every such article is retried
indefinitely and never stored. Usenet carries a lot of 8-bit text that is not UTF-8 (Latin-1,
KOI8-R, Shift-JIS, ...) and yEnc binaries are 8-bit by design, so this probably affects a large
part of a real feed, binary groups included. Converting with `SanitizeUTF8` would store the
article but change its bytes (and RFC 5537 forbids that); storing head and body as `bytea`
keeps them byte-exact. Until then, a permanent reject (437/439) is better than 436.
Verified (reproduced). Found while answering usenet_archiver's questions.

### O-14 go vet fails on main
`internal/mail/settings.go:113` is `s.Password = s.Password` (next to the `TrimSpace` lines for
the other fields), and `go vet ./...` stops there with "self-assignment of s.Password". If the
point is that passwords are not trimmed, a comment instead of the assignment says so and keeps
`go vet` clean, so it can run in CI. Verified.

## Checked, not a bug

From the pynntp and conformance runs; recorded so they are not reported again.

- `DATE` answers `111 20261109141713 server date and time`: text after the argument is allowed
  (RFC 3977 section 3.2); pynntp's parser rejects it and is being fixed there.
- `GROUP` of an empty group answers `211 0 0 0 <group>`: one of the three empty-group forms
  RFC 3977 section 6.1.1 allows (INN answers `211 0 1 0`).
- POST without a login gets 480 once at least one user exists (`requirePostAuth`,
  `internal/nntp/session.go:653`): by design; with no users posting is open.
- Reading without a login is allowed (nntp-testkit `authinfo-480`: "not restricted"): by
  design.
- Replies to commands by message-id carry article number 0 (`223 0 <id>`): correct per
  RFC 3977 section 6.2.
