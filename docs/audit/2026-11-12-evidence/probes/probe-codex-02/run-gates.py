import os,subprocess,time,pathlib,hashlib,tempfile,json
repo='/Users/est9/EstProjects/rex-amq-adapter'
base=pathlib.Path('/tmp/rex-audit-20261112')
probe=base/'probe-codex-02'
def run(cmd):
 return subprocess.run(cmd,cwd=repo,stdout=subprocess.PIPE,stderr=subprocess.STDOUT,text=True).stdout
identity={'branch':run(['git','branch','--show-current']).strip(),'head':run(['git','rev-parse','HEAD']).strip(),'tracked_diff_sha256':hashlib.sha256(subprocess.check_output(['git','diff','HEAD'],cwd=repo)).hexdigest(),'status':run(['git','status','--short']),'go':run(['go','version']),'rex':run(['rex','version']),'amq':run(['amq','--version']),'os':run(['sw_vers'])}
(probe/'candidate.json').write_text(json.dumps(identity,indent=2))
commands=[['go','build','./...'],['go','vet','./...'],['go','test','-short','./...'],['go','test','-short','-race','./...'],['go','test','-short','-coverprofile='+str(base/'cover.out'),'./...'],['go','test','-json','./internal/e2e/...'],['go','run','golang.org/x/vuln/cmd/govulncheck@latest','./...']]
with (base/'gates.md').open('w') as gate:
 gate.write('# Audit gates\n\nCandidate: main '+identity['head']+'; tracked diff SHA-256 '+identity['tracked_diff_sha256']+'. Untracked `.goal/` belongs to orchestrator and is excluded. Environment: '+identity['go'].strip()+'; '+identity['os'].replace('\n','; ')+'; '+identity['amq'].strip()+' amq; '+identity['rex'].replace('\n','; ')+'.\n\nPlan: build must compile; vet must report no diagnostics; short tests and race tests must pass; coverage must identify package/function gaps; e2e must actually run private Rex/amq delivery scenarios with no skipped tests; vulnerability scan must complete and report no reachable vulnerabilities. Run suites serially, record first failures, and never retry e2e. E2e uses `-json` solely to retain executed/skipped test evidence.\n\n| Check | Status | Exit | Seconds | Command | Log |\n|---|---|---:|---:|---|---|\n')
 gate.flush()
 for i,cmd in enumerate(commands,1):
  env=os.environ.copy(); env['REX_AMQ_ADAPTER_HOME']=tempfile.mkdtemp(prefix='state-',dir=probe)
  log=probe/('gate-'+str(i)+'.log'); start=time.monotonic()
  with log.open('w') as output: result=subprocess.run(cmd,cwd=repo,env=env,stdout=output,stderr=subprocess.STDOUT)
  duration=time.monotonic()-start
  content=log.read_text(); failures=[]; skips=[]
  if i==6:
   for line in content.splitlines():
    try: item=json.loads(line)
    except ValueError: continue
    if item.get('Action')=='fail' and item.get('Test'): failures.append(item['Test'])
    if item.get('Action')=='skip' and item.get('Test'): skips.append(item['Test'])
  status='Passed' if result.returncode==0 else 'Failed'
  if i==6 and skips: status='Blocked' if len(skips)>0 else status
  gate.write('| '+str(i)+' | '+status+' | '+str(result.returncode)+' | '+f'{duration:.2f}'+' | `REX_AMQ_ADAPTER_HOME=$(mktemp -d) '+' '.join(cmd)+'` | '+str(log)+' |\n')
  gate.flush()
  if failures or skips: gate.write('\nFailures: '+repr(failures)+'; skipped/blocked: '+repr(skips)+'\n\n');gate.flush()
  print(json.dumps({'gate':i,'command':cmd,'exit':result.returncode,'seconds':round(duration,2),'status':status,'failures':failures,'skips':skips,'tail':content[-1800:]}),flush=True)
 subprocess.run(['go','tool','cover','-func='+str(base/'cover.out')],cwd=repo,stdout=(probe/'coverage-functions.log').open('w'))
 (probe/'final-identity.txt').write_text(run(['git','rev-parse','HEAD'])+run(['git','status','--short'])+hashlib.sha256(subprocess.check_output(['git','diff','HEAD'],cwd=repo)).hexdigest())
