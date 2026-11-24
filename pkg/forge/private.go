package forge

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// privatePathKey is the repository git config key whose values replace the
// forge's private_paths for that repository.
const privatePathKey = "sundae.privatepath"

// Where a repository's private paths come from.
const (
	// PrivatePathsFromRepo: the repository's sundae.privatepath override.
	PrivatePathsFromRepo = "repo"
	// PrivatePathsFromForge: the forge's private_paths.
	PrivatePathsFromForge = "forge"
)

// PrivatePaths is a repository's effective private paths and their source.
type PrivatePaths struct {
	Paths []string
	// Source is PrivatePathsFromRepo or PrivatePathsFromForge.
	Source string
}

// effectivePrivatePaths returns the sundae.privatepath values of the bare
// repository in dir when any is set, else forgePaths. The forward guard
// and sessions both use it, so they agree.
func effectivePrivatePaths(ctx context.Context, dir string, forgePaths []string) (PrivatePaths, error) {
	// -z: a value with a newline must stay one (invalid) value, not split
	// into several paths.
	got, err := runGit(ctx, dir, nil, localTimeout, "config", "--file", "config", "-z", "--get-all", privatePathKey)
	if err != nil && exitCode(err) != 1 {
		return PrivatePaths{}, err
	}
	var paths []string
	for _, p := range strings.Split(got, "\x00") {
		if p = strings.TrimSpace(p); p == "" {
			continue
		}
		// An invalid value never matches rather than refusing every push.
		if n, err := config.NormalizePrivatePath(p); err == nil {
			paths = append(paths, n)
		}
	}
	if len(paths) > 0 {
		return PrivatePaths{Paths: paths, Source: PrivatePathsFromRepo}, nil
	}
	return PrivatePaths{Paths: forgePaths, Source: PrivatePathsFromForge}, nil
}

// linkedForge returns r's link and its forge's config; ErrNotLinked when r
// has no link or its forge is no longer in cfg.
func linkedForge(cfg *config.Config, r proto.Repository) (*Link, config.ForgeConfig, error) {
	l, err := readLink(r)
	if err != nil {
		return nil, config.ForgeConfig{}, fmt.Errorf("read the forge link of %s: %w", r.Name(), err)
	}
	if l != nil && cfg != nil {
		for _, fc := range cfg.Forges {
			if fc.Name == l.Forge {
				return l, fc, nil
			}
		}
	}
	return nil, config.ForgeConfig{}, fmt.Errorf("%w: %s", ErrNotLinked, r.Name())
}

// PrivatePathsOf returns the effective private paths of r; ErrNotLinked
// when r is not linked to a forge in cfg.
func PrivatePathsOf(ctx context.Context, cfg *config.Config, r proto.Repository) (PrivatePaths, error) {
	l, fc, err := linkedForge(cfg, r)
	if err != nil {
		return PrivatePaths{}, err
	}
	return effectivePrivatePaths(ctx, l.Dir, fc.PrivatePaths)
}

// SetPrivatePaths replaces r's sundae.privatepath override with paths,
// which must be valid (config.NormalizePrivatePath); none removes it, so
// the forge's private_paths apply. r must be linked to a forge in cfg.
func SetPrivatePaths(ctx context.Context, cfg *config.Config, r proto.Repository, paths []string) error {
	l, _, err := linkedForge(cfg, r)
	if err != nil {
		return err
	}
	normalized := make([]string, 0, len(paths))
	for _, p := range paths {
		n, err := config.NormalizePrivatePath(p)
		if err != nil {
			return fmt.Errorf("private path %q: %w", p, err)
		}
		if !slices.Contains(normalized, n) {
			normalized = append(normalized, n)
		}
	}
	// Exit status 5: nothing to unset.
	if _, err := runGit(ctx, l.Dir, nil, localTimeout, "config", "--file", "config", "--unset-all", privatePathKey); err != nil && exitCode(err) != 5 {
		return err
	}
	for _, p := range normalized {
		if _, err := runGit(ctx, l.Dir, nil, localTimeout, "config", "--file", "config", "--add", privatePathKey, p); err != nil {
			return err
		}
	}
	return nil
}
