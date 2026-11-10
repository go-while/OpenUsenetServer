"""Compare articles by message-id on OUS (116:1119) and Diablo's reader (118:46685). Args: message-ids."""
import socket, sys
def conn(a):
    s=socket.create_connection(a,timeout=20); f=s.makefile("rb"); g=f.readline().decode().strip(); return s,f,g
def art(s,f,mid):
    s.sendall(f"ARTICLE {mid}\r\n".encode()); r=f.readline().decode().strip()
    if not r.startswith("220"): return r,None
    out=[]
    while True:
        l=f.readline().rstrip(b"\r\n")
        if l==b".": return r,out
        out.append(l[1:] if l.startswith(b"..") else l)
os_,of,_=conn(("127.0.0.1",1119)); ds,df,dg=conn(("10.10.11.118",46685)); print("diablo reader:",dg)
for mid in sys.argv[1:]:
    ro,ao=art(os_,of,mid); rd,ad=art(ds,df,mid)
    print(f"== {mid}\n   OUS {ro[:3]} | Diablo {rd[:60]}")
    if not ad or not ao: continue
    sp=lambda a:(a[:a.index(b"")],a[a.index(b"")+1:])
    ho,bo=sp(ao); hd,bd=sp(ad)
    st=lambda h:[l for l in h if not l.lower().startswith((b"path:",b"xref:"))]
    print("   body same:",bo==bd,"| head same minus Path/Xref:",st(ho)==st(hd))
    x=[l.decode() for l in st(hd) if l not in st(ho)]; m=[l.decode() for l in st(ho) if l not in st(hd)]
    if x: print("   Diablo added:",x)
    if m: print("   missing at Diablo:",m)
    print("  ",[l.decode() for l in hd if l.lower().startswith(b"path:")])
