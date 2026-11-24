package openshell

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
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
//     /tmp: not in /sandbox, so never in .git/config, ~/.gitconfig or the
//     session's storage, and gone when the sandbox's runtime is replaced.
//   - ~/.gitconfig only gains `include.path = <that file>`; git skips an
//     include whose file is missing.
//   - It is sent as an Authorization header scoped to the clone URL, as
//     before, so it works with any git operation against Sundae.
//   - It travels into the sandbox base64-encoded in the exec environment,
//     never as an argument of a command Sundae builds. (The gateway
//     assembles the environment into the shell command line it runs, so it
//     is briefly visible in that shell's /proc/<pid>/cmdline inside the
//     sandbox, as the token always was.)
const (
	// gitCredentialDir holds the credential file.
	gitCredentialDir = "/tmp/sundae"
	// gitCredentialFile is the git config file holding the token.
	gitCredentialFile = gitCredentialDir + "/gitconfig"
)

const (
	gitCredentialsName = "sundae-git-credentials"
	// $1: dir, $2: credential file, $3: forge env file. Contents (base64) in
	// SUNDAE_GITCONFIG/SUNDAE_FORGE_ENV, or as two stdin lines when SUNDAE_CREDENTIALS_STDIN=1.
	gitCredentialsScript = `set -e
umask 077
mkdir -p "$1"
[ -O "$1" ] || { echo "$1 is not owned by uid $(id -u)" >&2; exit 1; }
chmod 0700 "$1"
if [ "$SUNDAE_CREDENTIALS_STDIN" = 1 ]; then
	IFS= read -r SUNDAE_GITCONFIG
	IFS= read -r SUNDAE_FORGE_ENV
fi
printf '%s' "$SUNDAE_GITCONFIG" | base64 -d > "$2.part"
mv -f "$2.part" "$2"
printf '%s' "$SUNDAE_FORGE_ENV" | base64 -d > "$3.part"
mv -f "$3.part" "$3"
git config --global --get-all include.path 2>/dev/null | grep -qxF "$2" || git config --global --add include.path "$2"`
)

// writeGitCredentials writes the credential and forge env files. Pod and docker
// get them on stdin, kept out of exec URLs and audit logs; OpenShell has no stdin.
func (m *Manager) writeGitCredentials(ctx context.Context, sandbox, runtime, username, token string, forges []forgeCredential, progress io.Writer) error {
	forgeConfig, warnings := forgeGitConfig(forges)
	env, envWarnings := forgeEnv(forges)
	for _, w := range append(warnings, envWarnings...) {
		fmt.Fprintf(progress, "Warning: %s\n", w) //nolint:errcheck
	}
	gitconfig := execEnvValue(gitCredentialConfig(m.cloneURL, username, token) + forgeConfig)
	forgeEnvValue := execEnvValue(env)
	if runtime == config.RuntimeOpenShell {
		_, err := m.script(ctx, sandbox, gitCredentialsName, gitCredentialsScript,
			map[string]string{"SUNDAE_GITCONFIG": gitconfig, "SUNDAE_FORGE_ENV": forgeEnvValue},
			gitCredentialDir, gitCredentialFile, forgeEnvFile)
		return err
	}
	_, err := m.scriptStdin(ctx, sandbox, gitCredentialsName, gitCredentialsScript,
		map[string]string{"SUNDAE_CREDENTIALS_STDIN": "1"},
		strings.NewReader(gitconfig+"\n"+forgeEnvValue+"\n"),
		gitCredentialDir, gitCredentialFile, forgeEnvFile)
	return err
}

// gitCredentialConfig returns a git config file that makes git send the
// token as basic-auth credentials to cloneURL only.
func gitCredentialConfig(cloneURL, username, token string) string {
	return "# Written by Sundae on every session start and attach.\n" +
		httpExtraHeader(cloneURL+"/", username, token)
}
