package config

import (
	"errors"
	"path"
	"strings"
	"unicode"
)

// DefaultForgeForward is the forward pattern of a forge without forward.
const DefaultForgeForward = "**"

// BranchRule is how a session push of one branch is handled.
type BranchRule struct {
	// Forward pushes the branch on to the forge.
	Forward bool
	// Deny refuses the push.
	Deny bool
	// FastForwardOnly refuses a rewrite or delete of the branch.
	FastForwardOnly bool
}

// ProtectsDefaultBranch reports whether sessions are refused pushes of the
// forge repository's default branch. Unset means true.
func (f ForgeConfig) ProtectsDefaultBranch() bool {
	return f.ProtectDefaultBranch == nil || *f.ProtectDefaultBranch
}

// BranchRule returns how a session push of branch is handled. defaultBranch
// is the forge repository's default branch, "" when unknown.
func (f ForgeConfig) BranchRule(branch, defaultBranch string) BranchRule {
	forward := f.Forward
	if len(forward) == 0 {
		forward = []string{DefaultForgeForward}
	}
	deny := matchBranch(f.Protect.Deny, branch) ||
		(f.ProtectsDefaultBranch() && defaultBranch != "" && branch == defaultBranch)
	return BranchRule{
		Forward:         !deny && matchBranch(forward, branch) && !matchBranch(f.Keep, branch),
		Deny:            deny,
		FastForwardOnly: matchBranch(f.Protect.FastForwardOnly, branch),
	}
}

// matchBranch reports whether branch matches any of patterns, case-sensitively.
func matchBranch(patterns []string, branch string) bool {
	for _, p := range patterns {
		// Patterns are checked at config load, so an invalid one never matches.
		g, err := compileGlob(p, true)
		if err == nil && g.Match(branch) {
			return true
		}
	}
	return false
}

// MatchPrivatePath reports whether the repository-relative file is one of
// paths or under one of them.
func MatchPrivatePath(paths []string, file string) bool {
	for _, p := range paths {
		if file == p || strings.HasPrefix(file, p+"/") {
			return true
		}
	}
	return false
}

// NormalizePrivatePath checks a private path and returns it without its
// trailing "/".
func NormalizePrivatePath(p string) (string, error) {
	trimmed := strings.TrimSuffix(p, "/")
	switch {
	case trimmed == "" || trimmed == ".":
		return "", errors.New("path is empty")
	case strings.ContainsFunc(p, unicode.IsControl):
		// A newline would split one value into several in the git config.
		return "", errors.New("path must not contain a control character")
	case strings.Contains(p, ","):
		// Lists travel to hook subprocesses as comma-separated environment
		// variables (see Environ).
		return "", errors.New("path must not contain a comma")
	case path.IsAbs(trimmed):
		return "", errors.New("path must be relative to the repository root")
	}
	first, _, _ := strings.Cut(trimmed, "/")
	for _, seg := range strings.Split(trimmed, "/") {
		if seg == ".." {
			return "", errors.New(`path must not contain ".."`)
		}
	}
	if first == ".git" || first == ".jj" {
		return "", errors.New("path must not be inside .git or .jj")
	}
	if path.Clean(trimmed) != trimmed {
		return "", errors.New(`path must be clean: no "//", "." segments or more than one trailing "/"`)
	}
	return trimmed, nil
}

// validateForgeRules validates the push-rule and private-path keys of a
// forge and fills in their defaults.
func validateForgeRules(f *ForgeConfig, fail func(format string, args ...any)) {
	if len(f.Forward) == 0 {
		f.Forward = []string{DefaultForgeForward}
	}
	for _, list := range []struct {
		key      string
		patterns []string
	}{
		{"forward", f.Forward},
		{"keep", f.Keep},
		{"protect.deny", f.Protect.Deny},
		{"protect.fast_forward_only", f.Protect.FastForwardOnly},
	} {
		for j, p := range list.patterns {
			if err := validateForgeGlob(p, true); err != nil {
				fail("%s[%d] %q: %v", list.key, j, p, err)
			}
		}
	}
	for j, p := range f.PrivatePaths {
		normalized, err := NormalizePrivatePath(p)
		if err != nil {
			fail("private_paths[%d] %q: %v", j, p, err)
			continue
		}
		f.PrivatePaths[j] = normalized
	}
}
