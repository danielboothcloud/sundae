#!/bin/bash
case "${CASE}" in
main)
. "$(dirname "$0")/t5.sh"
log "1 forward develop (matches FORWARD)"; change c1; push -b develop; st
log "2 jj git push -c @ with FORWARD=develop only -> sundae only"
sed -i '' "s/^FORWARD=.*/FORWARD='develop'/" $T/rules; jj new develop -m wip >/dev/null 2>&1; echo w > w; push -c @; st
log "3 rewrite develop (force) -> forwarded with lease"; jj describe -r develop -m c1-reworded >/dev/null 2>&1; push -b develop; st
log "4 forge moved by someone else -> lease refuses"
(cd $T/seed; jj git fetch >/dev/null 2>&1; jj git remote add f file://$T/forge.git 2>/dev/null; jj git fetch --remote f >/dev/null 2>&1; jj bookmark track develop --remote f >/dev/null 2>&1; jj new develop@f -m other >/dev/null 2>&1; echo o > o; jj bookmark set develop -r @ >/dev/null 2>&1; jj git push --remote f -b develop 2>&1 | tail -1)
change c2; push -b develop; st
log "5 sundae sync fast-forwards nothing (replica diverged); agent fetches, rebases, pushes"
jj git fetch 2>&1 | tail -2; jj bookmark list --all-remotes develop
jj rebase -b develop -d develop@origin >/dev/null 2>&1; push -b develop; st; jj bookmark list --all-remotes develop | head -1
log "6 atomic: forge rejects develop while push-* also forwarded -> nothing moves"
sed -i '' "s/^FORWARD=.*/FORWARD='develop push-*'/" $T/rules; echo develop > $T/REJECT
change c3; jj new develop -m side >/dev/null 2>&1; echo s > s; jj git push -b develop -c @ 2>&1 | rg 'sundae:|Error'; st; rm $T/REJECT
log "7 private path guard"; jj new develop -m add-agents >/dev/null 2>&1; echo secret > AGENTS.md; jj bookmark set develop -r @ >/dev/null 2>&1; push -b develop; st
log "8 push to origin"; jj git push --remote origin -b develop 2>&1 | tail -2
;;
protect)
DENY=main NOFORCE=develop . "$(dirname "$0")/t5.sh"
log "9 NOFORCE develop: fast-forward ok"; change c1; push -b develop; st
log "10 NOFORCE develop: rewrite refused"; jj describe -r develop -m c1-reworded >/dev/null 2>&1; push -b develop; st
log "11 DENY develop"; sed -i '' "s/^DENY=.*/DENY='develop'/" $T/rules; change c2; push -b develop; st
;;
esac
