#!/bin/bash
# T4: Sundae-only paths kept in refs/sundae/private, outside branch history.
set -u
T=$(mktemp -d); cd $T
log(){ printf '\n== %s\n' "$*"; }
PRIV="AGENTS.md _bmad"
git init -q --bare forge.git
jj git init --colocate seed >/dev/null 2>&1; (cd seed; echo code > main.go; jj describe -m init >/dev/null 2>&1; jj bookmark set develop -r @ >/dev/null 2>&1; git push -q file://$T/forge.git develop:develop)
git clone -q --bare file://$T/forge.git sundae.git

# mkcommit DIR PARENT MSG: commit the private paths of DIR via a temp index (no git commit porcelain).
save(){ ( cd "$1"; export GIT_INDEX_FILE=$(mktemp -u); git read-tree --empty
  for p in $PRIV; do
    if git ls-tree -r --name-only HEAD -- "$p" 2>/dev/null | rg -q .; then echo "warn: $p is tracked upstream; skipped" >&2; continue; fi
    [ -e "$p" ] && git add -f -- "$p"
  done
  tree=$(git write-tree); rm -f "$GIT_INDEX_FILE"
  parent=$(git rev-parse -q --verify refs/sundae/private)
  body="tree $tree"$'\n'"${parent:+parent $parent$'\n'}author s <s@x> $(date +%s) +0000"$'\n'"committer s <s@x> $(date +%s) +0000"$'\n\n'"sundae: save private paths"$'\n'
  c=$(printf '%s' "$body" | git hash-object -t commit -w --stdin)
  git update-ref refs/sundae/private "$c"
  git push -q sundae refs/sundae/private:refs/sundae/private && echo "saved $(git rev-parse --short $c)" ); }
restore(){ ( cd "$1"; git fetch -q sundae '+refs/sundae/private:refs/sundae/private' 2>/dev/null || { echo "no private ref"; return; }
  for p in $PRIV; do echo "/$p" >> .git/info/exclude; done
  git archive refs/sundae/private | tar -x && echo "restored" ); }

log "session A: create private files and save"
jj git clone --colocate --remote sundae file://$T/sundae.git A >/dev/null 2>&1
for p in $PRIV; do echo "/$p" >> A/.git/info/exclude; done
echo "agent rules" > A/AGENTS.md; mkdir -p A/_bmad; echo plan > A/_bmad/plan.md
save A
(cd A; echo "jj status:"; jj status | head -3; echo "jj sees ref?"; jj log -r 'all()' --no-graph -T 'commit_id.short(7) ++ " " ++ description.first_line() ++ "\n"' 2>&1)

log "session B: fresh clone restores"
jj git clone --colocate --remote sundae file://$T/sundae.git B >/dev/null 2>&1
restore B; (cd B; cat AGENTS.md _bmad/plan.md; jj status | head -2)

log "refs on sundae; forge untouched"
git --git-dir=sundae.git for-each-ref --format='%(refname)'; echo "forge:"; git --git-dir=forge.git for-each-ref --format='%(refname)'

log "jj fetch / push do not touch refs/sundae/private"
(cd B; jj git fetch >/dev/null 2>&1; jj new develop -m c1 >/dev/null 2>&1; echo 1 > x; jj bookmark set develop -r @ >/dev/null 2>&1; jj git push -b develop 2>&1 | tail -1; git rev-parse --short refs/sundae/private)
git --git-dir=sundae.git for-each-ref --format='%(refname)'

log "upstream later tracks AGENTS.md; session B moves onto it"
(cd seed; jj new develop -m add-agents >/dev/null 2>&1; echo "upstream rules" > AGENTS.md; jj bookmark set develop -r @ >/dev/null 2>&1; git push -q -f file://$T/forge.git develop:develop)
git --git-dir=sundae.git fetch -q file://$T/forge.git +develop:refs/heads/up
(cd B; echo "B AGENTS.md before: $(cat AGENTS.md)"; jj git fetch >/dev/null 2>&1; jj new up@sundae -m on-up 2>&1 | tail -2; echo "B AGENTS.md after: $(cat AGENTS.md 2>&1)"; jj status 2>&1 | head -4; save B)
