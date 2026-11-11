# End-to-end tests

These tests talk to a real OpenUsenetServer listener over TCP, the way a newsreader or a
peer does. The server runs in the test process on `127.0.0.1:0` with the in-memory store,
so no PostgreSQL is needed (except for the one Postgres test below). They use
[nntp-testkit](https://github.com/go-while/nntp-testkit) v0.12.1: a strict NNTP client that
fails on bare LF line ends, bad dot-stuffing or stray replies, the `article` builder, fake
transit peers, and the conformance checks.

`e2e/` is its own Go module (Go 1.26, for nntp-testkit), so the server module keeps its Go
version and gets no test dependency. The server's own unit tests in `internal/*` keep their
local helpers; the tests here are their kit-based counterparts plus regression and feeder
tests.

## Running

```sh
make e2e                                   # or: cd e2e && go test -tags e2e -count=1 -v .
cd e2e && go test -tags e2e -count=1 -run TestFeed -v .
```

| Variable | Effect |
|---|---|
| `OUS_E2E_KNOWN_ISSUES=1` | also run the tests that pin findings still open on `main` (ISSUES.md); they fail until the fix is merged |
| `OUS_E2E_SLOW=1` | also run tests that need minutes (the feed claim-lease test, about 155 s; use `-timeout 15m`) |
| `OPENUSENET_TEST_POSTGRES=postgres://...` | run the Postgres test (O-13) in a fresh schema, dropped afterwards |
| `NNTP_TEST_DEADLINE` | nntp-testkit's hang guard per blocking step (default 60 s); CI elsewhere uses 30s |

Without any of them the suite passes on `main`.

## Files

| File | What it covers |
|---|---|
| `harness_test.go` | `startEnv` (server, store, group `local.test`, user `e2euser`), `dial`, `login`, `addUser`/`addGroup`/`addPeer`, `knownIssue`, `testArticle` (a transit article, on the kit's `article.New`), `testPost` (a newsreader's POST: no Path, no Date) |
| `conformance_test.go` | nntp-testkit's 58 conformance checks (RFC 3977, 4642, 4643, 4644, 8054; the STARTTLS and COMPRESS ones skip, OUS offers neither); known issues: the four AUTHINFO checks of O-01 to O-04 (skipped) and the HDR 423 of O-23 in `no-current-article` (allowed with `Options.Allow`, the rest of that check still runs) |
| `reader_test.go`, `post_test.go` | greeting and CAPABILITIES, unknown commands, GROUP/LIST/ARTICLE/STAT/OVER, pipelining, POST without From |
| `transit_test.go` | IHAVE, CHECK/TAKETHIS, duplicates, Path loops, banned groups, denied peers, peer authentication, newgroup control, moderated groups, two servers feeding each other |
| `regressions_test.go`, `regressions_postgres_test.go` | one test per finding (O-05, O-06, O-10, O-11, O-12, O-13, O-15, O-16, O-19, O-23), each a known issue until its fix branch is merged |
| `feed_test.go` | the outgoing feeder against nntp-testkit's fake peer (`fakeserver.NewPeer`; the body checked byte for byte on the wire with `ArticlesRaw`): streaming, IHAVE fallback, Path and pattern exclusion, articles the peer has; known issues O-17, O-18 (a Diablo-like peer: `CapabilitiesReply`, `ModeStreamReply`), O-20 (unbounded parallel connections; `HoldGreeting`), O-21 (resends after the claim lease; `GreetingDelay`) |
| `interop/` | lab scripts from the INN and Diablo interop rounds (not run by `go test`) |

## Writing tests

- Assert on replies and counts, never on timing; wait for asynchronous effects with
  `testkit.WaitFor` and a generous bound.
- A test that pins an open finding starts with a comment (what was wrong, what is right),
  then `knownIssue(t, "O-nn", "...")`. Show it failing on `main` and passing with the fix
  branch merged; when the fix is merged, remove the `knownIssue` call.
- Send articles with `c.ExpectPost`, `c.ExpectIhave` or `c.ExpectTakethis` (they fail when
  the server refuses before the article, so an expected 441/437 cannot come from the command
  alone), or `c.Post`/`Ihave`/`Takethis` and their `sent` result when a test checks the reply
  itself. Build articles with `testArticle`/`testPost` or the kit's `article` package; keep
  raw text only for what the builder cannot write (the O-05 head), and parse it with
  `article.Parse`, which keeps every byte.
- Helpers that every NNTP project needs belong in nntp-testkit; a local stand-in is marked
  `TODO(nntp-testkit)` until the kit has it (there are none at the moment).
