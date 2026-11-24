package openshell

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
)

const (
	sharedLinksName = "sundae-shared-links"
	// sharedLinksScript takes (link, mount path, target) triples. It needs only
	// sh, awk, mkdir, cd, pwd, readlink, cp, ln, mv, rm and date (Fedora base).
	sharedLinksScript = `warn() { printf 'Warning: link %s: %s\n' "$1" "$2"; }
# NOTE: tests fake the mount table; validation keeps mountinfo's escaped characters out of mount paths.
mountinfo=${SUNDAE_MOUNTINFO:-/proc/self/mountinfo}
mounted() { MOUNT=$1 awk '$5 == ENVIRON["MOUNT"] { found = 1 } END { exit !found }' "$mountinfo" 2>/dev/null; }
# set_aside LINK TARGET: the volume's copy wins, whatever its age.
set_aside() {
	bak=$1.sundae-bak.$(date +%Y%m%d%H%M%S).$$
	if [ -e "$bak" ] || [ -L "$bak" ]; then
		warn "$1" "$bak already exists; left the local file in place"
		return 1
	fi
	if ! out=$(mv "$1" "$bak" 2>&1); then
		warn "$1" "$out"
		return 1
	fi
	warn "$1" "the shared volume already has $2; kept the local file as $bak"
}
# unique PREFIX sets u to a new empty file PREFIX.N: O_EXCL, so no other session's (pids repeat across pods).
unique() {
	n=$$
	until (set -C && : >"$1.$n") 2>/dev/null; do
		[ -e "$1.$n" ] || [ -L "$1.$n" ] || return 1
		n=$((n + 1))
	done
	u=$1.$n
}
# claim LINK TARGET moves a login made before the volume had one onto it.
# A hard link never replaces a file, so a concurrent start's login is kept.
claim() {
	if ! unique "$2.sundae-tmp"; then
		warn "$1" "cannot write to ${2%/*}"
		return 1
	fi
	tmp=$u
	if ! out=$(cp -p "$1" "$tmp" 2>&1); then
		rm -f "$tmp"
		warn "$1" "$out"
		return 1
	fi
	if [ -L "$2" ] && [ ! -e "$2" ] && unique "$2.sundae-old"; then
		# Moved aside, not removed: another session may have just put its login there.
		if ! mv "$2" "$u" 2>/dev/null || { [ -L "$u" ] && [ ! -e "$u" ]; }; then
			rm -f "$u"
		else
			warn "$1" "kept $u, which another session put on the volume meanwhile"
		fi
	fi
	if out=$(ln "$tmp" "$2" 2>&1); then
		rm -f "$tmp"
		if ! out=$(rm -f "$1" 2>&1); then
			warn "$1" "$out"
			return 1
		fi
		printf '%s: moved to the shared volume (%s)\n' "$1" "$2"
		return 0
	fi
	rm -f "$tmp"
	if [ -e "$2" ] || [ -L "$2" ]; then
		set_aside "$1" "$2"
		return
	fi
	warn "$1" "$out"
	return 1
}
while [ "$#" -ge 3 ]; do
	link=$1 root=$2 target=$3
	shift 3
	if ! mounted "$root"; then
		warn "$link" "$root is not mounted in this session; recreate the session to mount it"
		continue
	fi
	tdir=${target%/*}
	# New directories on the shared volume are the user's alone.
	if ! out=$( (umask 077 && mkdir -p "$tdir") 2>&1); then
		warn "$link" "$out"
		continue
	fi
	dir=${link%/*}
	dir=${dir:-/}
	if ! out=$(mkdir -p "$dir" 2>&1); then
		warn "$link" "$out"
		continue
	fi
	# A linked parent directory can make the link path the target itself.
	if ! pdir=$(cd -P "$dir" 2>&1 && pwd -P); then
		warn "$link" "$pdir"
		continue
	fi
	if ! ptdir=$(cd -P "$tdir" 2>&1 && pwd -P); then
		warn "$link" "$ptdir"
		continue
	fi
	case ${pdir%/}/${link##*/} in
	"${ptdir%/}/${target##*/}")
		[ -L "$link" ] && warn "$link" "is $target itself (through a linked directory) but a symlink; remove it"
		continue
		;;
	"${ptdir%/}/${target##*/}"/*)
		warn "$link" "is inside $target (through a linked directory); not linked"
		continue
		;;
	esac
	replaced=
	if [ -L "$link" ]; then
		replaced=$(readlink "$link")
		[ "$replaced" = "$target" ] && continue
	elif [ -f "$link" ]; then
		if [ -d "$target" ]; then
			warn "$link" "is a file but $target is a directory; move one of them away"
			continue
		fi
		if [ -e "$target" ]; then
			set_aside "$link" "$target" || continue
		else
			claim "$link" "$target" || continue
		fi
	elif [ -d "$link" ]; then
		warn "$link" "is a directory; remove it to link it to $target"
		continue
	elif [ -e "$link" ]; then
		warn "$link" "is not a regular file; remove it to link it to $target"
		continue
	fi
	if ! out=$(ln -sfn "$target" "$link" 2>&1); then
		warn "$link" "$out"
	elif [ -n "$replaced" ]; then
		printf 'link %s: replaced a link to %s\n' "$link" "$replaced"
	fi
done
exit 0
`
)

// sharedLinkArgs returns sharedLinksScript's arguments for vols' links;
// nil when there are none.
func sharedLinkArgs(vols []config.SharedVolume) []string {
	var args []string
	for _, v := range vols {
		for _, l := range v.Links {
			args = append(args, l.Path, v.MountPath, v.TargetPath(l))
		}
	}
	return args
}

// applySharedLinks makes the shared volume links of a pod session of
// profile. A failure is a warning: the session works without them.
func (m *Manager) applySharedLinks(ctx context.Context, sandbox, runtime, profile string, progress io.Writer) {
	if runtime != config.RuntimePod {
		return
	}
	args := sharedLinkArgs(m.cfg.PodSettingsFor(profile).SharedVolumes)
	if len(args) == 0 {
		return
	}
	out, err := m.script(ctx, sandbox, sharedLinksName, sharedLinksScript, nil, args...)
	if err != nil {
		m.logger.Warn("cannot make the shared volume links", "sandbox", sandbox, "err", err)
		fmt.Fprintf(progress, "Warning: could not make the shared volume links: %v\n", err) //nolint:errcheck
		return
	}
	if out = strings.TrimSpace(out); out != "" {
		fmt.Fprintln(progress, out) //nolint:errcheck
	}
}
