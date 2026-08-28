import json,sys,urllib.request
cases=json.load(open(__import__('os').path.join(__import__('os').path.dirname(__file__),'cases.json')))
pairs=json.load(open(sys.argv[1])) if len(sys.argv)>1 else []
def ask(messages,n=8):
    outs=[]
    for _ in range(n):
        body=json.dumps({"model":"qwen-fast","messages":messages,"max_tokens":24,"temperature":0}).encode()
        q=urllib.request.Request("http://127.0.0.1:8000/v1/chat/completions",data=body,
            headers={"Content-Type":"application/json","Authorization":"Bearer x"})
        outs.append(json.loads(urllib.request.urlopen(q).read())["choices"][0]["message"]["content"].strip())
    return outs
score=0
for c in cases:
    msgs=[dict(m) for m in c['messages']]
    missing=False
    for old,new in pairs:
        if old in msgs[0]['content']: msgs[0]['content']=msgs[0]['content'].replace(old,new)
        else: missing=True
    if missing:
        print(f"  --   [{c['tag']}] {c['note']} (anchor missing)"); continue
    outs=ask(msgs)
    waits=sum(1 for o in outs if o.startswith('<wait>'))
    got="wait" if waits>len(outs)/2 else "speak"
    ok=got==c['want']; score+=ok
    print(f"  {'ok ' if ok else 'MISS'} want={c['want']:5s} got={got:5s} [{c['tag']}] {c['note']}")
    if not ok:
        for o in set(outs): print(f"        {o[:72]!r}")
print(f"\n  {score}/{len(cases)}")
