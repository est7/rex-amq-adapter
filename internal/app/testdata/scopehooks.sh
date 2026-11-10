#!/bin/bash
# test.sh <hooks dir>: exits 1 on any wrong verdict.
H="$1"; fail=0; T=$(mktemp -d)
G() { env GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=core.hooksPath GIT_CONFIG_VALUE_0="$H" "$@"; }
expect() { local want=$1 what=$2; shift 2; if "$@" >/dev/null 2>"$T/err"; then got=ok; else got=refused; fi
  [ "$got" = "$want" ] && echo "PASS $what" || { echo "FAIL $what: $got, want $want: $(tail -1 $T/err)"; fail=1; }; }
cd "$T"; git init -q --bare -b main o.git; git init -q -b main w; cd w; git commit -q --allow-empty -m a
git remote add origin ../o.git; git push -q origin main; git commit -q --allow-empty -m b; git switch -q -c feat/x
# 1. base from the remote's HEAD (no config)
expect refused "push+PR: HEAD:main (no config, base from remote HEAD)" G env GITX_SCOPE=push+PR git push -q origin HEAD:main
expect refused "push+PR: feat/x:main" G env GITX_SCOPE=push+PR git push -q origin feat/x:main
expect refused "push+PR: delete main" G env GITX_SCOPE=push+PR git push -q origin :main
expect ok      "push+PR: own branch feat/x" G env GITX_SCOPE=push+PR git push -q origin feat/x
expect ok      "merge: HEAD:main" G env GITX_SCOPE=merge git push -q origin HEAD:main
git commit -q --allow-empty -m c
expect ok      "unset: HEAD:main" G env -u GITX_SCOPE git push -q origin HEAD:main
expect refused "unknown value local" G env GITX_SCOPE=local git push -q origin feat/x
# 2. config names a base that differs from remote HEAD
mkdir -p .config && printf '[base]\nbranch = "development"\n' > .config/git-command.toml
expect refused "push+PR: config base development" G env GITX_SCOPE=push+PR git push -q origin HEAD:development
expect ok      "push+PR: main is not the base under config" G env GITX_SCOPE=push+PR git push -q origin HEAD:refs/heads/main
rm -rf .config
# 3. submodule: base from the superproject's config, not the submodule's remote HEAD
cd "$T"; git init -q --bare -b master so.git; git init -q -b master s; git -C s commit -q --allow-empty -m s; git -C s remote add origin ../so.git; git -C s push -q origin master
git init -q -b development p; cd p; mkdir .config; printf '[base]\nbranch = "development"\n' > .config/git-command.toml
git -c protocol.file.allow=always submodule -q add ../so.git sub 2>/dev/null; git -C sub remote set-url origin "$T/so.git"
git -C sub commit -q --allow-empty -m t
cd sub; expect refused "submodule push+PR: HEAD:development (superproject base)" G env GITX_SCOPE=push+PR git push -q origin HEAD:development
expect ok "submodule push+PR: HEAD:feat/y" G env GITX_SCOPE=push+PR git push -q origin HEAD:feat/y
# 4. no base anywhere: fail closed
cd "$T"; git init -q --bare -b main nohead.git; git init -q -b main n; git -C n commit -q --allow-empty -m n; cd n; git remote add origin ../nohead.git
git push -q origin HEAD:feat/seed; git -C ../nohead.git symbolic-ref HEAD refs/heads/nothing-here
expect refused "push+PR: no base resolvable -> fail closed" G env GITX_SCOPE=push+PR git push -q origin HEAD:feat/z
grep -q "cannot tell the base branch" "$T/err" && echo "PASS refused by the hook, not by git" || { echo "FAIL refusal did not come from the hook: $(tail -1 $T/err)"; fail=1; }
expect ok "unset: same push succeeds without the cap" G env -u GITX_SCOPE git push -q origin HEAD:feat/z
# 5. the repository's own hooks still run
cd "$T/w"; printf '#!/bin/sh\necho ran > "%s/own-pre-commit"\n' "$T" > .git/hooks/pre-commit; chmod +x .git/hooks/pre-commit
printf '#!/bin/sh\nwhile read l; do echo "$l"; done > "%s/own-pre-push"\nexit 1\n' "$T" > .git/hooks/pre-push; chmod +x .git/hooks/pre-push
G git commit -q --allow-empty -m d; [ -f "$T/own-pre-commit" ] && echo "PASS own pre-commit ran" || { echo "FAIL own pre-commit did not run"; fail=1; }
expect refused "own pre-push still vetoes" G env GITX_SCOPE=push+PR git push -q origin HEAD:feat/w
grep -q 'refs/heads/feat/w' "$T/own-pre-push" && echo "PASS own pre-push got the ref lines" || { echo "FAIL own pre-push got no input"; fail=1; }
exit $fail
