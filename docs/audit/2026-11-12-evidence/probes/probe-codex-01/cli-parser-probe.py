import os
import pathlib
import shutil
import subprocess
import tempfile

root = pathlib.Path(tempfile.mkdtemp(
    prefix="codex-parser-", dir="/tmp/rex-audit-20261112/probe-codex-01"))
env = dict(os.environ)
for key in ["REX_BLOCK", "REX_SESSION", "REX_SERVER", "CLAUDE_CODE_SESSION_ID",
            "CODEX_THREAD_ID", "PI_SESSION_ID", "ANTIGRAVITY_CONVERSATION_ID"]:
    env.pop(key, None)
env["CODEX_HOME"] = str(root / "config")
env["REX_AMQ_ADAPTER_HOME"] = str(root / "state")
brief = ("--help=literal user brief\n\n"
         "You were started by claude-01 (summon on send). Report back with AMQ.")
result = subprocess.run([shutil.which("codex"), "--no-daemon", brief],
                        env=env, cwd=root, text=True, capture_output=True, timeout=5)
print("CODEX_PARSE: exit=" + str(result.returncode))
print((result.stdout + result.stderr)[:2500])
if result.returncode != 2 or "unexpected value" not in result.stderr:
    raise SystemExit("Expected safe argument-error proof was not observed")
