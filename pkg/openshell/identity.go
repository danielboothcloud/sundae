package openshell

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// Git identity in a sandbox.
//
// A session's commits are made as the git identity Sundae resolves for the
// repository (the git_identity of the forge it was imported from, else
// sessions.git_identity; see StartOptions.GitIdentity). It is set in the
// checkout's own repository config, never in the sandbox user's: git's
// .git/config (git -C <checkout> config user.name/user.email), and for jj
// and colocated repositories jj's repo-level config (jj config set --repo,
// which jj keeps per repository). ~/.gitconfig and the jj user config stay
// the user's.
//
// It is set on every start and attach, so a changed identity reaches
// existing sessions, and given to the first clone as well: jj creates the
// checkout's first working-copy commit while cloning, which therefore has
// the identity as its author.
//
// The values travel like the git credentials: base64 in the exec
// environment, decoded by the script, never part of a command line Sundae
// builds. jj reads config values as TOML, so its copies are TOML strings: a
// name such as 1 or true is then still a string.
const (
	gitIdentityCloneName = "sundae-clone"
	// $1: "jj" or "git", $2: the clone URL, $3: the checkout. Clones
	// with the identity: a colocated jj clone for jj, whose working-copy
	// commit is then authored by it, and a git clone that records it in
	// the new .git/config.
	gitIdentityCloneScript = `set -e
dec() { printf '%s' "$1" | base64 -d; }
if [ "$1" = jj ]; then
	name=$(dec "$SUNDAE_JJ_USER_NAME")
	email=$(dec "$SUNDAE_JJ_USER_EMAIL")
	exec jj git clone --colocate --config "user.name=$name" --config "user.email=$email" "$2" "$3"
fi
name=$(dec "$SUNDAE_GIT_USER_NAME")
email=$(dec "$SUNDAE_GIT_USER_EMAIL")
exec git clone -c "user.name=$name" -c "user.email=$email" "$2" "$3"`

	gitIdentityName = "sundae-git-identity"
	// $1: the checkout. Sets the identity in its git repository config
	// when it has a .git, and in its jj repo config when it has a .jj
	// (both for a colocated repository). A checkout that does not exist
	// (yet) or is neither is left alone.
	gitIdentityScript = `set -e
[ -d "$1" ] || exit 0
cd "$1"
dec() { printf '%s' "$1" | base64 -d; }
if [ -e .git ]; then
	name=$(dec "$SUNDAE_GIT_USER_NAME")
	email=$(dec "$SUNDAE_GIT_USER_EMAIL")
	git config --local -- user.name "$name"
	git config --local -- user.email "$email"
fi
if [ -d .jj ]; then
	name=$(dec "$SUNDAE_JJ_USER_NAME")
	email=$(dec "$SUNDAE_JJ_USER_EMAIL")
	jj --ignore-working-copy config set --repo user.name "$name"
	jj --ignore-working-copy config set --repo user.email "$email"
fi`
)

// gitIdentityEnv is the exec environment carrying id to the scripts.
func gitIdentityEnv(id config.GitIdentity) map[string]string {
	return map[string]string{
		"SUNDAE_GIT_USER_NAME":  execEnvValue(id.Name),
		"SUNDAE_GIT_USER_EMAIL": execEnvValue(id.Email),
		"SUNDAE_JJ_USER_NAME":   execEnvValue(tomlString(id.Name)),
		"SUNDAE_JJ_USER_EMAIL":  execEnvValue(tomlString(id.Email)),
	}
}

// tomlString returns s as a TOML basic string.
func tomlString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// cloneTool is the clone script's first argument for vcs.
func cloneTool(vcs proto.VCS) string {
	if vcs == proto.VCSJJ {
		return "jj"
	}
	return "git"
}

// cloneWithIdentity clones url into workDir like CloneCommand, with id as
// the clone's identity.
func (m *Manager) cloneWithIdentity(ctx context.Context, sandbox string, vcs proto.VCS, url, workDir string, id config.GitIdentity) error {
	_, err := m.script(ctx, sandbox, gitIdentityCloneName, gitIdentityCloneScript, gitIdentityEnv(id), cloneTool(vcs), url, workDir)
	return err
}

// writeGitIdentity sets id in the repository config of the checkout in
// workDir, if there is one.
func (m *Manager) writeGitIdentity(ctx context.Context, sandbox, workDir string, id config.GitIdentity) error {
	_, err := m.script(ctx, sandbox, gitIdentityName, gitIdentityScript, gitIdentityEnv(id), workDir)
	return err
}
