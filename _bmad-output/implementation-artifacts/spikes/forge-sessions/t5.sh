#!/bin/bash
# T5: agent pushes to sundae only; the sundae pre-receive hook forwards bookmarks that match
# per-forge rules (forward / deny / noforce / private paths) atomically to the forge.
set -u
T=$(mktemp -d); cd $T; export T
log(){ printf '\n== %s\n' "$*"; }
cat > $T/rules <<R
FORWARD='${FORWARD:-develop push-*}'
DENY='${DENY:-}'
NOFORCE='${NOFORCE:-}'
PRIVATE='${PRIVATE:-AGENTS.md _bmad}'
R
git init -q --bare forge.git
printf '#!/bin/sh\nwhile read o n r; do [ -f "%s/REJECT" ] && grep -qx "${r#refs/heads/}" "%s/REJECT" && { echo "forge: rejected $r"; exit 1; }; done; exit 0\n' "$T" "$T" > forge.git/hooks/pre-receive; chmod +x forge.git/hooks/pre-receive
jj git init --colocate seed >/dev/null 2>&1; (cd seed; echo code > main.go; jj describe -m init >/dev/null 2>&1; jj bookmark set develop -r @ >/dev/null 2>&1; git push -q file://$T/forge.git develop:develop)
git clone -q --bare file://$T/forge.git sundae.git

cat > sundae.git/hooks/pre-receive <<'H'
#!/bin/bash
. "$T/rules"
Z=0000000000000000000000000000000000000000
m(){ local n=$1; shift; for p in $*; do [[ $n == $p ]] && return 0; done; return 1; }
args=(); specs=()
while read old new ref; do
  b=${ref#refs/heads/}; [ "$b" != "$ref" ] || continue
  m "$b" $DENY && { echo "sundae: $b is protected; sessions cannot push it"; exit 1; }
  m "$b" $FORWARD || { echo "sundae: $b kept in sundae only"; continue; }
  if m "$b" $NOFORCE && { [ $new = $Z ] || ! git merge-base --is-ancestor $old $new; }; then
    echo "sundae: $b allows fast-forward only"; exit 1; fi
  if [ $new != $Z ]; then
    for c in $(git rev-list $new --not --all); do
      for f in $(git diff-tree --no-commit-id --name-only --diff-filter=A -r --root $c); do
        for p in $PRIVATE; do [[ $f == $p || $f == $p/* ]] && { echo "sundae: $b: commit ${c:0:8} adds private path $f"; exit 1; }; done
      done
    done
  fi
  up=$(git ls-remote "file://$T/forge.git" "refs/heads/$b" | cut -f1)
  if [ -n "$up" ] && [ "$up" != "$old" ] && ! { [ $new != $Z ] && git merge-base --is-ancestor $up $new 2>/dev/null; }; then
    echo "sundae: forge $b has commits you lack; run jj git fetch, rebase onto $b@origin, push again"; exit 1; fi
  args+=("--force-with-lease=refs/heads/$b:$up")
  specs+=("$([ $new = $Z ] || echo $new):refs/heads/$b")
done
[ ${#specs[@]} -eq 0 ] && exit 0
if out=$(git push --atomic --porcelain --receive-pack="env -u GIT_QUARANTINE_PATH git-receive-pack" "${args[@]}" "file://$T/forge.git" "${specs[@]}" 2>&1); then
  echo "sundae: forwarded ${specs[*]##*/} to forge"
else
  echo "sundae: forge refused; nothing was pushed: $(echo "$out" | rg -o 'rejected.*|stale info|atomic.*' | head -2 | tr '\n' ' ')"; exit 1
fi
H
chmod +x sundae.git/hooks/pre-receive

jj git clone --colocate --remote sundae file://$T/sundae.git ws >/dev/null 2>&1
cd ws
jj git remote add origin file://$T/forge.git
git config remote.origin.pushurl "file:///push-to-sundae-not-origin"
jj config set --repo git.push sundae
jj config set --repo git.fetch '["sundae", "origin"]'
jj config set --repo remotes.sundae.auto-track-bookmarks '"*"'
jj bookmark track develop --remote sundae >/dev/null 2>&1
jj git fetch >/dev/null 2>&1
st(){ printf 'forge[%s] sundae[%s] local[%s]\n' "$(git --git-dir=$T/forge.git for-each-ref --format='%(refname:short)=%(objectname:short)' | tr '\n' ' ')" "$(git --git-dir=$T/sundae.git for-each-ref --format='%(refname:short)=%(objectname:short)' refs/heads | tr '\n' ' ')" "$(jj log -r develop --no-graph -T 'commit_id.short(7)' 2>/dev/null)"; }
change(){ jj new develop -m "$1" >/dev/null 2>&1; echo "$1" > "${2:-f}"; jj bookmark set develop -r @ >/dev/null 2>&1; }
push(){ jj git push "$@" 2>&1 | rg 'sundae:|Error|bookmark:|Creating' ; echo "  exit=${PIPESTATUS[0]}"; }
