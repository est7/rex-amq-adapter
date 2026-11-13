import subprocess,pathlib,json,time,re,os
base=pathlib.Path('/tmp/rex-audit-20261112/probe-codex-02')
out=base/'e2e-surface'; out.mkdir(exist_ok=True)
os.environ.pop('REX_SESSION',None)
os.environ.pop('REX_BLOCK',None)
seen=set(); gotpid=False; end=time.monotonic()+720
while time.monotonic()<end:
 ps=subprocess.check_output(['ps','-axo','pid=,ppid=,command='],text=True)
 rows=[line.strip().split(None,2) for line in ps.splitlines() if line.strip()]
 tests={int(r[0]) for r in rows if len(r)==3 and re.match(r'^/[^ ]*/e2e\.test(?: |$)',r[2])}
 if tests: gotpid=True
 if gotpid and not tests: break
 for row in rows:
  if len(row)<3 or int(row[1]) not in tests or 'server run --listen unix:///tmp/rae' not in row[2]: continue
  m=re.search(r'--listen unix://(/tmp/rae[^/ ]+)/s',row[2])
  if not m: continue
  d=pathlib.Path(m.group(1)); dest=out/d.name; dest.mkdir(exist_ok=True)
  (dest/'ownership.txt').write_text('e2e_test_pid='+row[1]+'\nserver_pid='+row[0]+'\n'+row[2]+'\n')
  for f in list(d.glob('*.got'))+list(d.glob('*.hook')):
   try: (dest/f.name).write_bytes(f.read_bytes())
   except FileNotFoundError: pass
  try: state=list(d.glob('*.hook'))
  except FileNotFoundError: continue
  # Capture the private server only, and keep its structured sessions beside VT.
  key=str(d)
  if key not in seen:
   p=subprocess.run(['rex','--autostart=false','-S','unix://'+str(d/'s'),'ls','--json'],capture_output=True,text=True,timeout=3)
   if p.returncode==0:
    (dest/'sessions.json').write_text(p.stdout)
    # Session command layouts have changed; block ls returns the concrete ids.
    try: doc=json.loads(p.stdout)
    except ValueError: doc={}
    anycapture=False
    for s in doc.get('sessions',[]):
     sid=s.get('session_id','')
     if not sid: continue
     b=subprocess.run(['rex','--autostart=false','-S','unix://'+str(d/'s'),'block','ls','-s',sid,'--json'],capture_output=True,text=True,timeout=3)
     if b.returncode: continue
     (dest/'blocks.json').write_text(b.stdout)
     try: blocks=json.loads(b.stdout).get('blocks',[])
     except ValueError: blocks=[]
     for blk in blocks:
      bid=blk.get('block_id','')
      if not bid: continue
      cap=subprocess.run(['rex','--autostart=false','-S','unix://'+str(d/'s'),'capture','-b',bid,'--format=vt'],capture_output=True,text=True,timeout=3)
      if cap.returncode==0 and '❯' in cap.stdout:
       (dest/(bid.replace(':','-')+'.vt')).write_text(cap.stdout);anycapture=True
    if anycapture: seen.add(key)
 time.sleep(0.15)
(base/'e2e-collector.json').write_text(json.dumps({'observed_e2e_process':gotpid,'captured_servers':len(seen),'end':time.time()}))
print('E2E collector complete:',gotpid,len(seen),flush=True)
