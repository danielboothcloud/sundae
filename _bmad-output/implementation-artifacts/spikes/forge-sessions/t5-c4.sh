. "$(dirname "$0")/t5.sh"
change c1; push -b develop >/dev/null; st
log "other writer"
(cd $T/seed; jj git remote add f file://$T/forge.git; jj git fetch --remote f 2>&1 | tail -1; jj bookmark track develop --remote f 2>&1|tail -1; jj new develop@f -m other >/dev/null 2>&1; echo o > o; jj bookmark set develop -r @ 2>&1|tail -1; jj git push --remote f -b develop 2>&1 | tail -3)
st
log "agent pushes stale develop"; change c2; push -b develop; st
log "fetch: local follows sundae, origin informational"; jj git fetch 2>&1 | tail -2; jj bookmark list --all-remotes develop
log "rebase onto develop@origin and push"; jj rebase -b develop -d develop@origin 2>&1 | tail -1; push -b develop; st
