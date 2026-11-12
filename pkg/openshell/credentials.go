package openshell

import (
	"context"
	"encoding/base64"
	"strings"
)

// Git credentials in a sandbox.
//
// A session's program runs in an osewa session, which starts with a clean
// environment and can outlive any one SSH connection, and the token
// expires after openshell.token_ttl. So the credentials cannot travel as
// exec environment, as they did when the program was the exec itself.
// Instead, every start and attach writes a fresh token into a git config
// file outside the workspace, and the sandbox user's ~/.gitconfig includes
// that file:
//
//   - The token is in gitCredentialFile (mode 0600, directory 0700), under
//     /tmp: not in /sandbox, so never in .git/config, ~/.gitconfig or a
//     snapshot of /sandbox, and gone when the sandbox's runtime is replaced.
//   - ~/.gitconfig only gains `include.path = <that file>`; git skips an
//     include whose file is missing.
//   - It is sent as an Authorization header scoped to the clone URL, as
//     before, so it works with any git operation against Soft Serve.
//   - It travels into the sandbox base64-encoded in the exec environment,
//     never as an argument of a command Soft Serve builds. (The gateway
//     assembles the environment into the shell command line it runs, so it
//     is briefly visible in that shell's /proc/<pid>/cmdline inside the
//     sandbox, as the token always was.)
const (
	// gitCredentialDir holds the credential file.
	gitCredentialDir = "/tmp/soft-serve"
	// gitCredentialFile is the git config file holding the token.
	gitCredentialFile = gitCredentialDir + "/gitconfig"
)

const (
	gitCredentialsName = "soft-serve-git-credentials"
	// $1: the credential directory, $2: the credential file.
	// SOFT_SERVE_GITCONFIG: its content, base64. Writes the file and makes sure
	// ~/.gitconfig includes it.
	gitCredentialsScript = `set -e
umask 077
mkdir -p "$1"
[ -O "$1" ] || { echo "$1 is not owned by uid $(id -u)" >&2; exit 1; }
chmod 0700 "$1"
printf '%s' "$SOFT_SERVE_GITCONFIG" | base64 -d > "$2.part"
mv -f "$2.part" "$2"
git config --global --get-all include.path 2>/dev/null | grep -qxF "$2" || git config --global --add include.path "$2"`
)

// writeGitCredentials writes the git credential file for token into the
// sandbox and makes ~/.gitconfig include it.
func (m *Manager) writeGitCredentials(ctx context.Context, sandbox, username, token string) error {
	_, err := m.script(ctx, sandbox, gitCredentialsName, gitCredentialsScript,
		map[string]string{"SOFT_SERVE_GITCONFIG": execEnvValue(gitCredentialConfig(m.cloneURL, username, token))},
		gitCredentialDir, gitCredentialFile)
	return err
}

// gitCredentialConfig returns a git config file that makes git send the
// token as basic-auth credentials to cloneURL only.
func gitCredentialConfig(cloneURL, username, token string) string {
	cred := base64.StdEncoding.EncodeToString([]byte(username + ":" + token))
	url := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(cloneURL + "/")
	return "# Written by Soft Serve on every session start and attach.\n" +
		"[http \"" + url + "\"]\n" +
		"\textraHeader = Authorization: Basic " + cred + "\n"
}
