package openshell

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
)

// A forge-linked checkout pushes to sundae (the hook forwards to the forge)
// and only fetches origin; see "Forge-linked repositories" in README.md.
const (
	// ForgeRemoteSundae is the remote of the Sundae replica.
	ForgeRemoteSundae = "sundae"
	// ForgeOriginPushURL is origin's pushurl: git cannot push there, and
	// its error names the path.
	ForgeOriginPushURL = "file:///sundae-forwards-pushes/use-jj-git-push-to-sundae"

	forgeRemotesName = "sundae-forge-remotes"
	// $1: checkout, $2: Sundae URL, $3: forge URL, $4: pushurl, $5: 1 to fetch origin.
	// Idempotent; an origin that is not the forge is an older clone's Sundae remote: renamed.
	forgeRemotesScript = `set -e
cd "$1"
[ -e .git ] || { echo "$1 is not a git or colocated jj checkout" >&2; exit 3; }
if [ -d .jj ]; then
	remote() { jj --ignore-working-copy git remote "$@"; }
else
	remote() { git remote "$@"; }
fi
url() { git config --get "remote.$1.url" 2>/dev/null || true; }
if [ -z "$(url sundae)" ]; then
	cur=$(url origin)
	if [ -n "$cur" ] && [ "$cur" != "$3" ]; then remote rename origin sundae; else remote add sundae "$2"; fi
fi
[ "$(url sundae)" = "$2" ] || remote set-url sundae "$2"
cur=$(url origin)
if [ -z "$cur" ]; then remote add origin "$3"; elif [ "$cur" != "$3" ]; then remote set-url origin "$3"; fi
git config remote.origin.pushurl "$4"
[ -d .jj ] || exit 0
jj --ignore-working-copy config set --repo git.push '"sundae"'
if [ "$5" = 1 ]; then fetch='["sundae", "origin"]'; else fetch='["sundae"]'; fi
jj --ignore-working-copy config set --repo git.fetch "$fetch"
jj --ignore-working-copy config set --repo remotes.sundae.auto-track-bookmarks '"*"'`
)

// ForgeCloneCommand returns the command that clones a forge-linked
// repository from url into dir: a colocated jj clone whose remote is
// sundae.
func ForgeCloneCommand(url, dir string) []string {
	return []string{"jj", "git", "clone", "--colocate", "--remote", ForgeRemoteSundae, url, dir}
}

// setupForgeRemotes gives the checkout in dir its sundae and origin remotes
// and jj config. NOTE: jj git fetch fails as a whole when one remote fails, so
// origin is fetched only when the session has the forge's token.
func (m *Manager) setupForgeRemotes(ctx context.Context, sandbox, dir, repo string, f *ForgeRemote, fetchOrigin bool) error {
	fetch := "0"
	if fetchOrigin {
		fetch = "1"
	}
	_, err := m.script(ctx, sandbox, forgeRemotesName, forgeRemotesScript, nil, dir, m.repoURL(repo), f.CloneURL, ForgeOriginPushURL, fetch)
	return err
}

// forgeCredential is a forge token a session gets.
type forgeCredential struct {
	forge string
	kind  string
	// url is the forge's web base URL.
	url   string
	token string
}

// gitUser is the basic-auth user git sends with the token, as the server's
// forge sync does.
func (c forgeCredential) gitUser() string {
	if c.kind == config.ForgeKindGitHub {
		return "x-access-token"
	}
	return "oauth2"
}

// forgeCredentials re-reads the token of each forge of specs; unreadable ones
// are skipped with a warning so the session still starts.
func (m *Manager) forgeCredentials(specs []RepoSpec, progress io.Writer) []forgeCredential {
	var (
		out  []forgeCredential
		seen = map[string]bool{}
	)
	for _, sp := range specs {
		if sp.Forge == nil || seen[sp.Forge.Forge] {
			continue
		}
		seen[sp.Forge.Forge] = true
		i := slices.IndexFunc(m.cfg.Forges, func(fc config.ForgeConfig) bool { return fc.Name == sp.Forge.Forge })
		if i < 0 {
			continue
		}
		token, err := m.cfg.Forges[i].Token()
		if err != nil {
			m.logger.Warn("cannot read a forge token for a session", "forge", sp.Forge.Forge, "err", err)
			fmt.Fprintf(progress, "Warning: no %s token in the session: %v\n", sp.Forge.Forge, err) //nolint:errcheck
			continue
		}
		if token == "" {
			continue
		}
		out = append(out, forgeCredential{forge: sp.Forge.Forge, kind: sp.Forge.Kind, url: strings.TrimSuffix(sp.Forge.URL, "/"), token: token})
	}
	return out
}

// forgeGitConfig scopes each forge's token to its web URL. A second forge on
// the same URL is skipped: git would send both headers.
func forgeGitConfig(creds []forgeCredential) (string, []string) {
	var (
		b        strings.Builder
		warnings []string
		seen     = map[string]string{}
	)
	for _, c := range creds {
		if prev, ok := seen[c.url]; ok {
			warnings = append(warnings, fmt.Sprintf("forges %s and %s share %s; git in the session uses the %s token", prev, c.forge, c.url, prev))
			continue
		}
		seen[c.url] = c.forge
		b.WriteString(httpExtraHeader(c.url+"/", c.gitUser(), c.token))
	}
	return b.String(), warnings
}

// httpExtraHeader returns a git config section that sends user:token as
// basic auth to urls under prefix.
func httpExtraHeader(prefix, user, token string) string {
	cred := base64.StdEncoding.EncodeToString([]byte(user + ":" + token))
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(prefix)
	return "[http \"" + quoted + "\"]\n" +
		"\textraHeader = Authorization: Basic " + cred + "\n"
}

// forgeEnvFile holds gh/glab tokens as export lines, sourced by the session's
// shell; under /tmp so it never reaches the /sandbox volume.
const forgeEnvFile = gitCredentialDir + "/forge-env"

// forgeEnv returns forgeEnvFile's content. gh reads GH_TOKEN only for github.com
// and *.ghe.com, GH_ENTERPRISE_TOKEN for other hosts; the first forge wins.
func forgeEnv(creds []forgeCredential) (string, []string) {
	vars := map[string]string{}
	owner := map[string]string{}
	var (
		order    []string
		warnings []string
	)
	set := func(forge string, kv ...string) {
		for i := 0; i < len(kv); i += 2 {
			if prev, ok := owner[kv[i]]; ok {
				warnings = append(warnings, fmt.Sprintf("%s is %s's; forge %s does not get it", kv[i], prev, forge))
				return
			}
		}
		for i := 0; i < len(kv); i += 2 {
			vars[kv[i]], owner[kv[i]] = kv[i+1], forge
			order = append(order, kv[i])
		}
	}
	for _, c := range creds {
		host := c.url
		if u, err := url.Parse(c.url); err == nil && u.Host != "" {
			host = u.Host
		}
		lower := strings.ToLower(host)
		switch {
		case c.kind == config.ForgeKindGitHub && lower == "github.com":
			set(c.forge, "GH_TOKEN", c.token)
		case c.kind == config.ForgeKindGitHub && strings.HasSuffix(lower, ".ghe.com"):
			set(c.forge, "GH_TOKEN", c.token, "GH_HOST", host)
		case c.kind == config.ForgeKindGitHub:
			set(c.forge, "GH_ENTERPRISE_TOKEN", c.token, "GH_HOST", host)
		case c.kind == config.ForgeKindGitLab:
			set(c.forge, "GITLAB_TOKEN", c.token, "GITLAB_HOST", host)
		}
	}
	var b strings.Builder
	b.WriteString("# Written by Sundae on every session start and attach.\n")
	for _, k := range order {
		fmt.Fprintf(&b, "export %s=%s\n", k, shellQuote(vars[k]))
	}
	return b.String(), warnings
}

// shellQuote quotes s for sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
