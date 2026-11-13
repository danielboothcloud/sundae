package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxGitIdentityLen is the longest git_identity name or email accepted, in
// bytes.
const MaxGitIdentityLen = 256

// GitIdentity is the author and committer name and email that git and jj
// use for commits made in an agent session's checkout.
//
// It is server-wide configuration, like the forge tokens: every user's
// sessions of a repository commit as the same identity.
type GitIdentity struct {
	// Name is user.name.
	Name string `env:"NAME" yaml:"name"`

	// Email is user.email.
	Email string `env:"EMAIL" yaml:"email"`
}

// IsSet reports whether the identity has both a name and an email. A
// validated config never holds an identity with only one of them.
func (g GitIdentity) IsSet() bool {
	return g.Name != "" && g.Email != ""
}

// String renders the identity the way git shows it: "Name <email>".
func (g GitIdentity) String() string {
	return g.Name + " <" + g.Email + ">"
}

// ForgeGitIdentity is the git_identity of a forge. It is GitIdentity under
// another name only so that an unknown key in it is reported as
// forges[].git_identity.<key> rather than sessions.git_identity.<key>.
type ForgeGitIdentity GitIdentity

// emailRe is what a git_identity email must look like: something@something,
// with no spaces, angle brackets or second '@'.
var emailRe = regexp.MustCompile(`^[^\s@<>]+@[^\s@<>]+$`)

// validateGitIdentity trims g and checks it. An identity with neither a name
// nor an email is unset and valid. Each problem is returned naming the key
// after prefix, e.g. "sessions.git_identity.email is required when
// sessions.git_identity is set".
func validateGitIdentity(prefix, key string, g *GitIdentity) []error {
	g.Name = strings.TrimSpace(g.Name)
	g.Email = strings.TrimSpace(g.Email)
	if g.Name == "" && g.Email == "" {
		return nil
	}
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("%s%s."+format, append([]any{prefix, key}, args...)...))
	}
	check := func(field, v string) bool {
		switch {
		case v == "":
			fail("%s is required when %s is set", field, key)
		case len(v) > MaxGitIdentityLen:
			fail("%s is longer than %d bytes", field, MaxGitIdentityLen)
		case !utf8.ValidString(v):
			fail("%s %q is not valid UTF-8", field, v)
		case strings.ContainsFunc(v, unicode.IsControl):
			fail("%s %q must not contain control characters", field, v)
		case strings.ContainsAny(v, "<>"):
			// git drops angle brackets from identities.
			fail("%s %q must not contain '<' or '>'", field, v)
		default:
			return true
		}
		return false
	}
	check("name", g.Name)
	if check("email", g.Email) && !emailRe.MatchString(g.Email) {
		fail("email %q does not look like an email address (user@host)", g.Email)
	}
	return errs
}

// validateGitIdentities validates sessions.git_identity and every
// forges[].git_identity.
func (c *Config) validateGitIdentities() error {
	errs := validateGitIdentity("", "sessions.git_identity", &c.Sessions.GitIdentity)
	for i := range c.Forges {
		f := &c.Forges[i]
		// The same prefix as the forge's other problems.
		prefix := fmt.Sprintf("forges[%d]: ", i)
		if name := strings.TrimSpace(f.Name); name != "" {
			prefix = fmt.Sprintf("forges[%d] (%s): ", i, name)
		}
		g := GitIdentity(f.GitIdentity)
		errs = append(errs, validateGitIdentity(prefix, "git_identity", &g)...)
		f.GitIdentity = ForgeGitIdentity(g)
	}
	return errors.Join(errs...)
}

// SessionGitIdentity returns the git identity for sessions of a repository
// imported from the named forge ("" for a repository linked to no forge):
// that forge's git_identity when it is configured and has one, else
// sessions.git_identity, else none (the zero GitIdentity).
func (c *Config) SessionGitIdentity(forge string) GitIdentity {
	if forge != "" {
		for _, f := range c.Forges {
			if f.Name == forge {
				if g := GitIdentity(f.GitIdentity); g.IsSet() {
					return g
				}
				break
			}
		}
	}
	if c.Sessions.GitIdentity.IsSet() {
		return c.Sessions.GitIdentity
	}
	return GitIdentity{}
}
