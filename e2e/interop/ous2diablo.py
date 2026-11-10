"""Post test articles to OUS, read them back from OUS and from Diablo's reader, compare."""
import socket, sys, time
OUS=("10.10.11.116",1119); RD=("10.10.11.118",46685)
tag=sys.argv[1]; wait=int(sys.argv[2])
class C:
    def __init__(s, addr, user=None, pw=None):
        s.s=socket.create_connection(addr,timeout=20); s.f=s.s.makefile("rb"); s.greet=s.line()
        if user:
            s.cmd(f"AUTHINFO USER {user}"); r=s.cmd(f"AUTHINFO PASS {pw}"); assert r.startswith("281"), r
    def line(s): return s.f.readline().decode("utf-8","replace").rstrip("\r\n")
    def cmd(s,c): s.s.sendall((c+"\r\n").encode()); return s.line()
    def block(s):
        out=[]
        while True:
            l=s.f.readline().rstrip(b"\r\n")
            if l==b".": return out
            out.append(l[1:] if l.startswith(b"..") else l)
run=time.strftime("%H%M%S")
tests=[("plain",[],["hello Diablo from OUS","line two"]),
       ("xhdrs",["X-Zeta: z","X-Alpha: a"],["unknown headers"]),
       ("dots",[],["..leading dot",".","end"])]
p=C(OUS,"e2euser","e2e-secret-pass"); sent={}
for name,extra,body in tests:
    mid=f"<ous2diablo-{tag}-{name}-{run}@e2e.test>"
    head=["From: ous-1 <ous-1@e2e.test>","Newsgroups: local.test",f"Subject: ous2diablo {tag} {name} {run}",f"Message-ID: {mid}"]+extra
    assert p.cmd("POST").startswith("340")
    p.s.sendall(("\r\n".join(head)+"\r\n\r\n"+"\r\n".join(("."+l if l.startswith(".") else l) for l in body)+"\r\n.\r\n").encode())
    print(f"POST {name}: {p.line()}"); sent[name]=(mid,body)
time.sleep(wait)
o=C(OUS); r=C(RD)
for name,(mid,body) in sent.items():
    ro=o.cmd(f"ARTICLE {mid}"); ao=o.block() if ro.startswith("220") else []
    rr=r.cmd(f"ARTICLE {mid}"); ar=r.block() if rr.startswith("220") else []
    print(f"== {name}: OUS {ro[:4]} | Diablo reader: {rr[:70]}")
    if not ar: continue
    sp=lambda a:(a[:a.index(b"")],a[a.index(b"")+1:])
    ho,bo=sp(ao); hr,br=sp(ar)
    st=lambda h:[l for l in h if not l.lower().startswith((b"path:",b"xref:"))]
    print("   body same:",bo==br,"| head same minus Path/Xref:",st(ho)==st(hr))
    extra=[l.decode() for l in st(hr) if l not in st(ho)]; miss=[l.decode() for l in st(ho) if l not in st(hr)]
    if extra: print("   Diablo added:",extra)
    if miss: print("   missing at Diablo:",miss)
    print("  ",[l.decode() for l in hr if l.lower().startswith(b"path:")])
