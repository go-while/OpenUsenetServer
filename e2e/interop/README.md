# Interop scripts (lab only)

Throwaway Python scripts (standard library only) from the first interop rounds on
2026-11-09. They are kept for reference until the relay check moves into
nntp-testkit; they are not part of `make e2e`.

The addresses are the go-while test lab (OUS on 10.10.11.116:1119, INN and Diablo test
hosts on 10.10.11.118) and the login is the lab's test user. Change them before use.

| Script | What it does |
|---|---|
| `ous2inn.py` | posts test articles to OUS, which feeds INN (streaming); reads each article back from OUS and from INN's nnrpd and compares the bytes |
| `ous2diablo.py` | the same against Diablo (IHAVE), reading back from dreaderd |
| `readback.py` | reads given message-ids from OUS and Diablo and compares them (run on 116, Diablo's reader admits only the lab net) |
| `inn-slot-lease.py` | holds the lab's shared VM lease (Redis db 7, key `inn:slot:116`, SET NX EX 600, renewed every 60 s, released on exit) while OUS runs on 116 |

Results of those runs are in [ISSUES.md](../../ISSUES.md) (test runs table, O-17, O-18).
