"""Post test articles to OUS, then read them back from OUS and from INN's nnrpd and compare."""
import socket, sys, time
OUS=("10.10.11.116",1119); INN=("10.10.11.118",1220)
class C:
    def __init__(s, addr, user=None, pw=None):
        s.s=socket.create_connection(addr,timeout=20); s.f=s.s.makefile("rb"); s.greet=s.line()
        if user:
            assert s.cmd(f"AUTHINFO USER {user}").startswith("381")
            r=s.cmd(f"AUTHINFO PASS {pw}"); assert r.startswith("281"), r
    def line(s): return s.f.readline().decode("utf-8","replace").rstrip("\r\n")
    def cmd(s,c): s.s.sendall((c+"\r\n").encode()); return s.line()
    def block(s):
        out=[]
        while True:
            l=s.f.readline().rstrip(b"\r\n")
            if l==b".": return out
            out.append(l[1:] if l.startswith(b"..") else l)
run=time.strftime("%H%M%S")
tests=[
 ("plain",  [], ["hello INN from OUS","line two"]),
 ("folded", ["X-Ous-Folded: first part of a folded header,","\tand its second line"], ["folded header test"]),
 ("xhdrs",  ["X-Zeta: z","X-Alpha: a","Cancel-Lock: sha1:abcdefabcdefabcdefabcdefabc="], ["unknown headers test"]),
 ("dots",   [], ["..leading dot line (stuffed on the wire)", ".", "end"]),
 ("utf8",   ["Content-Type: text/plain; charset=UTF-8"], ["café über ✓"]),
]
post=C(OUS,"e2euser","e2e-secret-pass")
sent={}
for name,extra,body in tests:
    mid=f"<ous2inn-{name}-{run}@e2e.test>"
    head=[f"From: ous-1 <ous-1@e2e.test>","Newsgroups: local.test",f"Subject: ous2inn {name} {run}",f"Message-ID: {mid}"]+extra
    r=post.cmd("POST"); assert r.startswith("340"), r
    data="\r\n".join(head)+"\r\n\r\n"+"\r\n".join(("."+l if l.startswith(".") else l) for l in body)+"\r\n.\r\n"
    post.s.sendall(data.encode()); r=post.line(); print(f"POST {name}: {r}")
    sent[name]=(mid,head,body)
print("waiting for the feed..."); time.sleep(int(sys.argv[1]) if len(sys.argv)>1 else 15)
ous=C(OUS); inn=C(INN,"ous","ous-cWOmFSSpiJNJ")
for name,(mid,head,body) in sent.items():
    ro=ous.cmd(f"ARTICLE {mid}"); ao=ous.block() if ro.startswith("220") else []
    ri=inn.cmd(f"ARTICLE {mid}"); ai=inn.block() if ri.startswith("220") else []
    print(f"\n== {name} {mid}\n OUS: {ro[:60]}\n INN: {ri[:60]}")
    if not ai: continue
    def split(a):
        i=a.index(b""); return a[:i],a[i+1:]
    ho,bo=split(ao); hi,bi=split(ai)
    print(" body OUS==INN:", bo==bi, "| body == sent:", [l.decode("utf-8","replace") for l in bi]==body)
    so=[l for l in ho if not l.lower().startswith((b"path:",b"xref:"))]
    si=[l for l in hi if not l.lower().startswith((b"path:",b"xref:"))]
    added=[l.decode() for l in si if l not in so]; missing=[l.decode() for l in so if l not in si]
    print(" head order same (minus Path/Xref):", [l for l in si if l in so]==so)
    if added: print(" INN added:", added)
    if missing: print(" not at INN:", missing)
    print(" INN Path:", [l.decode() for l in hi if l.lower().startswith(b"path:")])
for c in (post,ous,inn): c.cmd("QUIT")
