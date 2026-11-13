import os,subprocess,pathlib,json,hashlib
base=pathlib.Path('/tmp/rex-audit-20261112/probe-codex-02')
bin=base/'rex-amq-adapter'; state=base/'cli-state'; (state/'records').mkdir(parents=True,exist_ok=True)
fake=base/'fake-rex'; fake.write_text('#!/bin/sh\nprintf "%s\\n" "$*" >> "'+str(base/'fake-rex.log')+'"\ncase "$*" in\n*"block rename "*) exit 0 ;;\n*) exit 3 ;;\nesac\n'); fake.chmod(0o700)
env={k:v for k,v in os.environ.items() if not k.startswith(('REX_','AM_','CLAUDE')) and k not in ['CODEX_THREAD_ID','PI_SESSION_ID','ANTIGRAVITY_CONVERSATION_ID']}
env.update(REX_AMQ_ADAPTER_HOME=str(state),REX_AMQ_REX=str(fake),REX_AMQ_AMQ='/usr/bin/false',REX_BLOCK='block:qa-probe',REX_SERVER='unix:///tmp/rex-audit-20261112/not-a-server.sock')
record=state/'records'/'codex-qa-probe.json'; initial={'handle':'codex-qa-probe','kind':'codex','block_id':'block:qa-probe','agent_pid':os.getpid(),'task':'audit candidate','role':'qa','server':env['REX_SERVER']}; record.write_text(json.dumps(initial))
results=[]
for args in [['--help'],['crews','--help'],['crews','--json'],['who','--json'],['title','--help'],['join','--help'],['events','--help'],['view','--help']]:
 p=subprocess.run([str(bin)]+args,env=env,stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True,timeout=15)
 results.append({'args':args,'exit':p.returncode,'stdout':p.stdout,'stderr':p.stderr,'record_task':json.loads(record.read_text())['task']})
(base/'cli-results.json').write_text(json.dumps({'binary_sha256':hashlib.sha256(bin.read_bytes()).hexdigest(),'probes':results},indent=2,ensure_ascii=False))
for r in results: print(json.dumps({**r,'stdout':r['stdout'][:500]},ensure_ascii=False))
