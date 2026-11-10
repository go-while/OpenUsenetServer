"""Hold Redis db 7 key inn:slot:116 = ous-live (SET NX EX 600), renew every 60 s.
Renews only while the key still holds our value; deletes it on SIGTERM/SIGINT."""
import signal, socket, sys, time
KEY, VAL, TTL = "inn:slot:116", "ous-live", 600
RENEW = ("if redis.call('GET', KEYS[1]) == ARGV[1] then "
         "return redis.call('EXPIRE', KEYS[1], ARGV[2]) else return 0 end")
RELEASE = ("if redis.call('GET', KEYS[1]) == ARGV[1] then "
           "return redis.call('DEL', KEYS[1]) else return 0 end")

def conn():
    s = socket.create_connection(("127.0.0.1", 6379), timeout=5)
    f = s.makefile("rb")
    def rd():
        l = f.readline().rstrip(b"\r\n"); t, r = l[:1], l[1:]
        if t == b"-": raise RuntimeError(r.decode())
        if t in b"+:": return r.decode()
        if t == b"$":
            n = int(r); return None if n < 0 else f.read(n + 2)[:-2].decode()
        if t == b"*": return [rd() for _ in range(int(r))]
    def cmd(*a):
        a = [str(x) for x in a]
        s.sendall(("*%d\r\n" % len(a) + "".join("$%d\r\n%s\r\n" % (len(x.encode()), x) for x in a)).encode())
        return rd()
    cmd("SELECT", 7)
    return cmd

cmd = conn()
if cmd("SET", KEY, VAL, "NX", "EX", TTL) != "OK":
    print(f"{KEY} already held by {cmd('GET', KEY)!r}; not taking it", flush=True); sys.exit(1)
print(f"holding {KEY}={VAL} (EX {TTL}, renew 60s)", flush=True)

def stop(*_):
    try: print("released" if conn()("EVAL", RELEASE, 1, KEY, VAL) == "1" else "not ours anymore", flush=True)
    finally: sys.exit(0)
signal.signal(signal.SIGTERM, stop); signal.signal(signal.SIGINT, stop)

while True:
    time.sleep(60)
    try:
        if conn()("EVAL", RENEW, 1, KEY, VAL, TTL) != "1":
            print(f"{KEY} is no longer ours; stopping", flush=True); sys.exit(1)
    except (OSError, RuntimeError) as e:
        print(f"renew failed: {e}", flush=True)   # retry next minute; key lives 600 s
