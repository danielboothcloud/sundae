package forge

import (
	"strings"

	"github.com/danielboothcloud/sundae/pkg/config"
	"github.com/danielboothcloud/sundae/pkg/proto"
)

// SessionRemote is what an agent session needs of a forge link: its origin
// URL and which forge's token it uses.
type SessionRemote struct {
	// Forge is the forge name; Kind is config.ForgeKindGitHub or ForgeKindGitLab.
	Forge string
	Kind  string
	// URL is the forge's web base URL, without a trailing slash.
	URL string
	// Path is the forge repository path, e.g. owner/repo.
	Path string
	// DefaultBranch is the forge's default branch as of the last sync.
	DefaultBranch string
	// CloneURL is the HTTPS clone URL: URL/Path.git.
	CloneURL string
}

// SessionRemoteOf returns the session remote of r, or nil when r has no
// forge link or its forge is no longer in cfg.
func SessionRemoteOf(cfg *config.Config, r proto.Repository) (*SessionRemote, error) {
	if cfg == nil {
		return nil, nil //nolint:nilnil
	}
	l, err := readLink(r)
	if err != nil || l == nil {
		return nil, err
	}
	for _, fc := range cfg.Forges {
		if fc.Name != l.Forge {
			continue
		}
		base := strings.TrimSuffix(fc.URL, "/")
		// NOTE: always HTTPS, even when the server syncs over ssh_key_file: the session only has the token.
		return &SessionRemote{
			Forge:         fc.Name,
			Kind:          fc.Kind,
			URL:           base,
			Path:          l.Path,
			DefaultBranch: l.DefaultBranch,
			CloneURL:      base + "/" + l.Path + ".git",
		}, nil
	}
	return nil, nil //nolint:nilnil
}
